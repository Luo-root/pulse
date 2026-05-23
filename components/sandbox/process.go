package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
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
	return &ProcessSandbox{config: config}
}

func DefaultLangs() map[string]LangConfig {
	if runtime.GOOS == "windows" {
		return map[string]LangConfig{
			"python": {Command: "python", Ext: ".py"},
			"node":   {Command: "node", Ext: ".js"},
			"go": {
				Command: "go", Args: []string{"run"}, Ext: ".go",
				InitFiles: map[string]string{"go.mod": "module sandbox\ngo 1.21\n"},
			},
			"shell": {Command: "cmd", Args: []string{"/C"}},
		}
	}
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

	workDir, err := os.MkdirTemp("", "pulse-sandbox-*")
	if err != nil {
		return nil, fmt.Errorf("sandbox: 创建工作目录: %w", err)
	}
	defer os.RemoveAll(workDir)

	for name, content := range lang.InitFiles {
		if err := os.WriteFile(filepath.Join(workDir, name), []byte(content), 0644); err != nil {
			return nil, fmt.Errorf("sandbox: 写入初始化文件 %s: %w", name, err)
		}
	}

	for _, f := range req.Files {
		fpath := filepath.Join(workDir, filepath.FromSlash(f.Path))
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

	// ====== 关键修复: Windows 进程组管理 ======
	if runtime.GOOS == "windows" {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			// 创建独立的 Job 对象, 使得子进程能被一并清理
			CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
		}
		// 自定义 Cancel: 杀掉整棵进程树, 而非仅 go.exe
		cmd.Cancel = func() error {
			killProcessTree(cmd.Process)
			return cmd.Process.Kill()
		}
		// 安全兜底: 如果 Cancel 后 I/O 仍未结束, 最多再等 3 秒就放弃
		cmd.WaitDelay = 3 * time.Second
	}

	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

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

// killProcessTree 在 Windows 上用 taskkill 杀掉整个进程树
func killProcessTree(p *os.Process) {
	if p == nil {
		return
	}
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprintf("%d", p.Pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Run() // 忽略错误, 进程可能已退出
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
