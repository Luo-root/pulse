package sandbox

import (
	"context"
	"time"
)

// Sandbox 代码执行沙箱接口
type Sandbox interface {
	// Execute 执行代码/命令
	Execute(ctx context.Context, req ExecRequest) (*ExecResult, error)
	// CheckLang 检查语言是否可用
	CheckLang(lang string) error
	// ListLangs 列出可用语言
	ListLangs() []string
	// Close 关闭沙箱
	Close() error
}

// ExecRequest 执行请求
type ExecRequest struct {
	Language string            `json:"language"`           // python, node, go, shell, ruby, etc.
	Code     string            `json:"code"`               // 代码或命令
	Timeout  time.Duration     `json:"timeout,omitempty"`  // 超时，0 用默认值
	Env      map[string]string `json:"env,omitempty"`      // 额外环境变量
	Files    []InputFile       `json:"files,omitempty"`    // 执行前创建的文件
	WorkDir  string            `json:"work_dir,omitempty"` // 指定工作目录（空 = 使用临时目录）
}

// InputFile 执行前注入的文件
type InputFile struct {
	Path    string `json:"path"`    // 相对于工作目录的路径
	Content string `json:"content"` // 文件内容
}

// ExecResult 执行结果
type ExecResult struct {
	Stdout    string        `json:"stdout"`              // 标准输出
	Stderr    string        `json:"stderr"`              // 标准错误
	ExitCode  int           `json:"exit_code"`           // 退出码
	Duration  time.Duration `json:"duration"`            // 执行耗时
	TimedOut  bool          `json:"timed_out,omitempty"` // 是否超时
	Truncated bool          `json:"truncated,omitempty"` // 输出是否被截断
}

// LangConfig 语言配置
type LangConfig struct {
	Command   string            `json:"command"`              // 可执行命令（如 python3、node）
	Args      []string          `json:"args,omitempty"`       // 前置参数（如 ["run"] for go run）
	Ext       string            `json:"ext,omitempty"`        // 代码文件扩展名（空 = shell 模式）
	InitFiles map[string]string `json:"init_files,omitempty"` // 预创建文件（如 go.mod）
}

// ProcessConfig 进程沙箱配置
type ProcessConfig struct {
	DefaultTimeout time.Duration         // 默认超时（默认 30s）
	MaxOutputBytes int                   // 最大输出字节数（默认 1MB）
	AllowedLangs   map[string]LangConfig // 语言配置（nil = 使用默认值）
	PreloadEnv     map[string]string     // 每次执行都注入的环境变量（不受 EnvMode 影响）
	EnvMode        EnvMode               // 环境变量过滤模式（默认 EnvModeBlacklist）
	EnvAllowList   []string              // 白名单模式下的允许变量名列表
	EnvBlockList   []string              // 黑名单模式下的额外拦截变量名前缀（追加到默认列表）
}

// EnvMode 沙箱环境变量过滤模式
type EnvMode string

const (
	// EnvModeBlacklist 黑名单模式（默认）：过滤已知敏感变量，其余放行
	EnvModeBlacklist EnvMode = "blacklist"
	// EnvModeWhitelist 白名单模式：只保留指定变量，其余全部过滤
	EnvModeWhitelist EnvMode = "whitelist"
	// EnvModePassthrough 透传模式：继承宿主全部环境变量（不推荐生产环境）
	EnvModePassthrough EnvMode = "passthrough"
)
