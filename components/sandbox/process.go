package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProcessSandbox 基于子进程的沙箱实现
type ProcessSandbox struct {
	config ProcessConfig
}

func NewProcessSandbox(config ProcessConfig) *ProcessSandbox {
	if config.DefaultTimeout <= 0 {
		config.DefaultTimeout = 30 * time.Second
	}
	if config.MaxOutputBytes <= 0 {
		config.MaxOutputBytes = 1024 * 1024
	}
	if config.AllowedLangs == nil {
		config.AllowedLangs = DefaultLangs()
	}
	if config.EnvMode == "" {
		config.EnvMode = EnvModeBlacklist
	}
	return &ProcessSandbox{config: config}
}

func DefaultLangs() map[string]LangConfig {
	return map[string]LangConfig{
		"python": {Command: "python3", Ext: ".py"},
		"node":   {Command: "node", Ext: ".js"},
		"go": {
			Command: "go", Args: []string{"run"}, Ext: ".go",
			InitFiles: map[string]string{"go.mod": "module sandbox\ngo 1.21\n"},
		},
		"shell": {Command: "sh", Args: []string{"-c"}},
	}
}

func (s *ProcessSandbox) Execute(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	lang, ok := s.config.AllowedLangs[req.Language]
	if !ok {
		return nil, fmt.Errorf("sandbox: 不支持的语言 %q，可用: %s", req.Language, strings.Join(s.ListLangs(), ", "))
	}
	if req.Code == "" {
		return nil, fmt.Errorf("sandbox: 代码不能为空")
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = s.config.DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// 使用指定工作目录或创建临时目录
	var workDir string
	var needCleanup bool
	if req.WorkDir != "" {
		absDir, err := filepath.Abs(req.WorkDir)
		if err != nil {
			return nil, fmt.Errorf("sandbox: 解析工作目录: %w", err)
		}
		if err := os.MkdirAll(absDir, 0755); err != nil {
			return nil, fmt.Errorf("sandbox: 创建工作目录: %w", err)
		}
		workDir = absDir
	} else {
		var err error
		workDir, err = os.MkdirTemp("", "pulse-sandbox-*")
		if err != nil {
			return nil, fmt.Errorf("sandbox: 创建工作目录: %w", err)
		}
		needCleanup = true
	}
	if needCleanup {
		defer os.RemoveAll(workDir)
	}

	for name, content := range lang.InitFiles {
		if err := os.WriteFile(filepath.Join(workDir, name), []byte(content), 0644); err != nil {
			return nil, fmt.Errorf("sandbox: 写入初始化文件 %s: %w", name, err)
		}
	}

	for _, f := range req.Files {
		fpath := filepath.Join(workDir, filepath.FromSlash(f.Path))
		// 路径安全校验：确保文件在工作目录内
		absPath, _ := filepath.Abs(fpath)
		absWorkDir, _ := filepath.Abs(workDir)
		if !strings.HasPrefix(absPath, absWorkDir+string(os.PathSeparator)) && absPath != absWorkDir {
			return nil, fmt.Errorf("sandbox: 文件路径 %q 超出工作目录范围", f.Path)
		}
		if err := os.MkdirAll(filepath.Dir(fpath), 0755); err != nil {
			return nil, fmt.Errorf("sandbox: 创建文件目录: %w", err)
		}
		if err := os.WriteFile(fpath, []byte(f.Content), 0644); err != nil {
			return nil, fmt.Errorf("sandbox: 写入文件 %s: %w", f.Path, err)
		}
	}

	var args []string
	if lang.Ext != "" {
		codeFile := filepath.Join(workDir, "main"+lang.Ext)
		if err := os.WriteFile(codeFile, []byte(req.Code), 0644); err != nil {
			return nil, fmt.Errorf("sandbox: 写入代码文件: %w", err)
		}
		args = make([]string, len(lang.Args))
		copy(args, lang.Args)
		args = append(args, codeFile)
	} else {
		args = make([]string, len(lang.Args))
		copy(args, lang.Args)
		args = append(args, req.Code)
	}

	cmd := exec.CommandContext(ctx, lang.Command, args...)
	cmd.Dir = workDir

	// 安全环境变量：根据 EnvMode 过滤
	cmd.Env = s.buildSafeEnv(req.Env)

	// ====== 跨平台进程管理 ======
	setupProcess(cmd)

	stdout := &cappedBuffer{max: s.config.MaxOutputBytes}
	stderr := &cappedBuffer{max: s.config.MaxOutputBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	result := &ExecResult{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Duration:  duration,
		Truncated: stdout.Truncated() || stderr.Truncated(),
	}

	switch {
	case ctx.Err() == context.DeadlineExceeded:
		result.TimedOut = true
		result.ExitCode = -1
	case runErr == nil:
		result.ExitCode = 0
	default:
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
			result.Stderr += "\nsandbox error: " + runErr.Error()
		}
	}

	return result, nil
}

// CheckLang 检查语言是否可用
func (s *ProcessSandbox) CheckLang(lang string) error {
	cfg, ok := s.config.AllowedLangs[lang]
	if !ok {
		return fmt.Errorf("不支持的语言: %s", lang)
	}
	if _, err := exec.LookPath(cfg.Command); err != nil {
		return fmt.Errorf("找不到命令 %q: %w", cfg.Command, err)
	}
	return nil
}

// ListLangs 列出已配置的语言
func (s *ProcessSandbox) ListLangs() []string {
	langs := make([]string, 0, len(s.config.AllowedLangs))
	for name := range s.config.AllowedLangs {
		langs = append(langs, name)
	}
	sort.Strings(langs)
	return langs
}

// AddLang 动态添加语言支持
func (s *ProcessSandbox) AddLang(name string, config LangConfig) {
	s.config.AllowedLangs[name] = config
}

// Close 关闭沙箱
func (s *ProcessSandbox) Close() error {
	return nil
}

// ============================================================================
// 环境变量安全处理
// ============================================================================

// defaultBlockedPrefixes 默认黑名单：已知敏感环境变量前缀
var defaultBlockedPrefixes = []string{
	"API_KEY", "SECRET", "TOKEN", "PASSWORD", "CREDENTIAL",
	"AWS_SECRET", "AWS_ACCESS_KEY", "AZURE_", "GOOGLE_",
	"DATABASE_URL", "DB_", "REDIS_", "MONGO_",
	"PRIVATE_KEY", "SSH_", "GPG_",
}

// buildSafeEnv 根据 EnvMode 构建环境变量列表
// PreloadEnv 和 request Env 始终注入（不受 mode 影响）
func (s *ProcessSandbox) buildSafeEnv(request map[string]string) []string {
	preload := s.config.PreloadEnv

	switch s.config.EnvMode {
	case EnvModePassthrough:
		// 透传模式：继承宿主全部环境变量
		env := os.Environ()
		for k, v := range preload {
			env = append(env, k+"="+v)
		}
		for k, v := range request {
			env = append(env, k+"="+v)
		}
		return env

	case EnvModeWhitelist:
		// 白名单模式：只保留 EnvAllowList 中的变量
		allowed := make(map[string]bool, len(s.config.EnvAllowList))
		for _, k := range s.config.EnvAllowList {
			allowed[strings.ToUpper(k)] = true
		}

		env := make([]string, 0, len(allowed)+len(preload)+len(request))
		for _, entry := range os.Environ() {
			idx := strings.IndexByte(entry, '=')
			if idx < 0 {
				continue
			}
			key := strings.ToUpper(entry[:idx])
			if allowed[key] {
				env = append(env, entry)
			}
		}
		for k, v := range preload {
			env = append(env, k+"="+v)
		}
		for k, v := range request {
			env = append(env, k+"="+v)
		}
		return env

	default:
		// 黑名单模式（默认）：过滤敏感变量，其余放行
		// 合并默认黑名单 + 用户自定义黑名单
		blocked := append([]string{}, defaultBlockedPrefixes...)
		blocked = append(blocked, s.config.EnvBlockList...)

		env := make([]string, 0, 32)
		for _, entry := range os.Environ() {
			idx := strings.IndexByte(entry, '=')
			if idx < 0 {
				continue
			}
			key := entry[:idx]
			upperKey := strings.ToUpper(key)

			isBlocked := false
			for _, prefix := range blocked {
				if strings.HasPrefix(upperKey, strings.ToUpper(prefix)) {
					isBlocked = true
					break
				}
			}
			if !isBlocked {
				env = append(env, entry)
			}
		}
		for k, v := range preload {
			env = append(env, k+"="+v)
		}
		for k, v := range request {
			env = append(env, k+"="+v)
		}
		return env
	}
}

// ============================================================================
// 内部工具
// ============================================================================

// cappedBuffer 有容量上限的输出缓冲区
type cappedBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	origLen := len(p)
	if b.buf.Len() >= b.max {
		return origLen, nil // 缓冲区已满，静默丢弃
	}
	remaining := b.max - b.buf.Len()
	if len(p) > remaining {
		p = p[:remaining]
	}
	_, err := b.buf.Write(p)
	if err != nil {
		return 0, err
	}
	return origLen, nil // 始终报告完整长度，静默截断溢出部分
}

func (b *cappedBuffer) String() string {
	return b.buf.String()
}

func (b *cappedBuffer) Truncated() bool {
	return b.buf.Len() >= b.max
}
