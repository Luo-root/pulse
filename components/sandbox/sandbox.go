package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Luo-root/pulse/components/bufutil"
)

// Sandbox 子进程沙箱接口
type Sandbox interface {
	Execute(ctx context.Context, req ExecRequest) (*ExecResult, error)
	Close() error
}

// ExecRequest 执行请求
type ExecRequest struct {
	Command string            `json:"command"`            // 要执行的命令
	Args    []string          `json:"args,omitempty"`     // 命令参数
	Timeout time.Duration     `json:"timeout,omitempty"`  // 超时，0 用默认值
	Env     map[string]string `json:"env,omitempty"`      // 额外环境变量
	Files   []InputFile       `json:"files,omitempty"`    // 执行前在工作目录创建的文件
	WorkDir string            `json:"work_dir,omitempty"` // 指定工作目录（空 = 临时目录）
}

// InputFile 执行前注入的文件
type InputFile struct {
	Path    string `json:"path"`    // 相对于工作目录的路径
	Content string `json:"content"` // 文件内容
}

// ExecResult 执行结果
type ExecResult struct {
	Stdout    string        `json:"stdout"`
	Stderr    string        `json:"stderr"`
	ExitCode  int           `json:"exit_code"`
	Duration  time.Duration `json:"duration"`
	TimedOut  bool          `json:"timed_out,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
}

// ProcessConfig 沙箱配置
type ProcessConfig struct {
	DefaultTimeout time.Duration     // 默认超时（默认 30s）
	MaxOutputBytes int               // 最大输出字节数（默认 1MB）
	PreloadEnv     map[string]string // 每次执行都注入的环境变量
	EnvMode        EnvMode           // 环境变量过滤模式（默认 EnvModeBlacklist）
	EnvAllowList   []string          // 白名单模式下的允许变量名列表
	EnvBlockList   []string          // 黑名单模式下的额外拦截变量名前缀
}

type EnvMode string

const (
	EnvModeBlacklist   EnvMode = "blacklist"
	EnvModeWhitelist   EnvMode = "whitelist"
	EnvModePassthrough EnvMode = "passthrough"
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
	if config.EnvMode == "" {
		config.EnvMode = EnvModeBlacklist
	}
	return &ProcessSandbox{config: config}
}

func (s *ProcessSandbox) Execute(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	if req.Command == "" {
		return nil, fmt.Errorf("sandbox: command 不能为空")
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = s.config.DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	workDir, cleanup, err := s.prepareWorkDir(req.WorkDir)
	if err != nil {
		return nil, err
	}
	if cleanup {
		defer os.RemoveAll(workDir)
	}

	if err := s.writeFiles(workDir, req.Files); err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, req.Command, req.Args...)
	cmd.Dir = workDir
	cmd.Env = s.buildSafeEnv(req.Env)
	setupProcess(cmd)

	stdout := bufutil.NewCappedBuffer(s.config.MaxOutputBytes)
	stderr := bufutil.NewCappedBuffer(s.config.MaxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	start := time.Now()
	runErr := cmd.Run()

	result := &ExecResult{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Duration:  time.Since(start),
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

func (s *ProcessSandbox) Close() error { return nil }

func (s *ProcessSandbox) prepareWorkDir(specified string) (string, bool, error) {
	if specified != "" {
		absDir, err := filepath.Abs(specified)
		if err != nil {
			return "", false, err
		}
		if err := os.MkdirAll(absDir, 0755); err != nil {
			return "", false, err
		}
		return absDir, false, nil
	}
	dir, err := os.MkdirTemp("", "pulse-sandbox-*")
	return dir, true, err
}

func (s *ProcessSandbox) writeFiles(workDir string, files []InputFile) error {
	absWorkDir, _ := filepath.Abs(workDir)
	for _, f := range files {
		fpath := filepath.Join(workDir, filepath.FromSlash(f.Path))
		absPath, _ := filepath.Abs(fpath)
		if !strings.HasPrefix(absPath, absWorkDir+string(os.PathSeparator)) && absPath != absWorkDir {
			return fmt.Errorf("sandbox: 文件路径 %q 超出工作目录范围", f.Path)
		}
		if err := os.MkdirAll(filepath.Dir(fpath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(fpath, []byte(f.Content), 0644); err != nil {
			return err
		}
	}
	return nil
}

// ============================================================================
// 环境变量安全处理
// ============================================================================

var defaultBlockedPrefixes = []string{
	"API_KEY", "SECRET", "TOKEN", "PASSWORD", "CREDENTIAL",
	"AWS_SECRET", "AWS_ACCESS_KEY", "AZURE_", "GOOGLE_",
	"DATABASE_URL", "DB_", "REDIS_", "MONGO_",
	"PRIVATE_KEY", "SSH_", "GPG_",
}

func (s *ProcessSandbox) buildSafeEnv(request map[string]string) []string {
	preload := s.config.PreloadEnv

	switch s.config.EnvMode {
	case EnvModePassthrough:
		env := os.Environ()
		for k, v := range preload {
			env = append(env, k+"="+v)
		}
		for k, v := range request {
			env = append(env, k+"="+v)
		}
		return env

	case EnvModeWhitelist:
		allowed := make(map[string]bool, len(s.config.EnvAllowList))
		for _, k := range s.config.EnvAllowList {
			allowed[strings.ToUpper(k)] = true
		}
		env := make([]string, 0, len(allowed)+len(preload)+len(request))
		for _, entry := range os.Environ() {
			idx := strings.IndexByte(entry, '=')
			if idx >= 0 && allowed[strings.ToUpper(entry[:idx])] {
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
		blocked := append([]string{}, defaultBlockedPrefixes...)
		blocked = append(blocked, s.config.EnvBlockList...)

		env := make([]string, 0, 32)
		for _, entry := range os.Environ() {
			idx := strings.IndexByte(entry, '=')
			if idx < 0 {
				continue
			}
			upperKey := strings.ToUpper(entry[:idx])
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
