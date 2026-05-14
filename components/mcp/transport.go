package mcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// Transport MCP 传输层接口
type Transport interface {
	// Connect 建立连接
	Connect(ctx context.Context) error
	// Send 发送数据（一帧）
	Send(data []byte) error
	// Recv 接收数据（一帧）
	Recv() ([]byte, error)
	// Close 关闭连接
	Close() error
}

// ============================================================================
// StdioTransport 基于子进程 stdin/stdout 的传输层
// ============================================================================

// StdioConfig Stdio 传输配置
type StdioConfig struct {
	Command string   // 可执行文件路径
	Args    []string // 命令参数
	Env     []string // 额外环境变量（格式 KEY=VALUE）
	WorkDir string   // 工作目录
}

// StdioTransport 基于子进程 stdio 的传输层
// 协议：每行一个 JSON 对象，以换行符分隔
type StdioTransport struct {
	config StdioConfig

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner

	mu     sync.Mutex
	closed bool
}

func NewStdioTransport(config StdioConfig) *StdioTransport {
	return &StdioTransport{config: config}
}

func (t *StdioTransport) Connect(ctx context.Context) error {
	t.cmd = exec.CommandContext(ctx, t.config.Command, t.config.Args...)
	if len(t.config.Env) > 0 {
		t.cmd.Env = t.config.Env
	}
	if t.config.WorkDir != "" {
		t.cmd.Dir = t.config.WorkDir
	}

	var err error
	t.stdin, err = t.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("mcp: stdin pipe: %w", err)
	}

	stdout, err := t.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("mcp: stdout pipe: %w", err)
	}

	// MCP 协议用换行分隔消息，Scanner 默认按行分割
	t.stdout = bufio.NewScanner(stdout)
	// 允许较大的消息（MCP 工具描述可能很长）
	t.stdout.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	if err := t.cmd.Start(); err != nil {
		return fmt.Errorf("mcp: start process %s: %w", t.config.Command, err)
	}

	return nil
}

func (t *StdioTransport) Send(data []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return fmt.Errorf("mcp: transport closed")
	}

	// 写入数据 + 换行符
	if _, err := t.stdin.Write(data); err != nil {
		return fmt.Errorf("mcp: write: %w", err)
	}
	if _, err := t.stdin.Write([]byte("\n")); err != nil {
		return fmt.Errorf("mcp: write newline: %w", err)
	}

	return nil
}

func (t *StdioTransport) Recv() ([]byte, error) {
	if t.stdout.Scan() {
		return t.stdout.Bytes(), nil
	}
	if err := t.stdout.Err(); err != nil {
		return nil, fmt.Errorf("mcp: read: %w", err)
	}
	return nil, fmt.Errorf("mcp: connection closed")
}

func (t *StdioTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true

	if t.stdin != nil {
		t.stdin.Close()
	}
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		_ = t.cmd.Wait()
	}

	return nil
}
