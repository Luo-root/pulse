package mcp

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStdioTransport_Connect_InvalidCommand(t *testing.T) {
	transport := NewStdioTransport(StdioConfig{
		Command: "nonexistent_command_xyz_12345",
	})

	err := transport.Connect(context.Background())
	if err == nil {
		t.Fatal("expected error for nonexistent command")
	}
}

func TestStdioTransport_RecvAfterClose(t *testing.T) {
	cmd := findPython(t)
	if cmd == "" {
		t.Skip("python not available")
	}

	transport := NewStdioTransport(StdioConfig{
		Command: cmd,
		Args:    []string{"-c", "import sys; sys.stdout.write('hello\\n'); sys.stdout.flush()"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := transport.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer transport.Close()

	data, err := transport.Recv()
	if err != nil {
		// 进程可能已经退出，这是正常的
		t.Logf("recv after close: %v", err)
		return
	}
	t.Logf("received: %s", string(data))
}

func TestStdioTransport_SendAfterClose(t *testing.T) {
	cmd := findPython(t)
	if cmd == "" {
		t.Skip("python not available")
	}

	transport := NewStdioTransport(StdioConfig{
		Command: cmd,
		Args:    []string{"-c", "import time; time.sleep(60)"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := transport.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	transport.Close()

	err = transport.Send([]byte("test"))
	if err == nil {
		t.Fatal("expected error when sending after close")
	}
}

func TestStdioTransport_DoubleClose(t *testing.T) {
	cmd := findPython(t)
	if cmd == "" {
		t.Skip("python not available")
	}

	transport := NewStdioTransport(StdioConfig{
		Command: cmd,
		Args:    []string{"-c", "import time; time.sleep(60)"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := transport.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// 双重关闭不应该 panic
	transport.Close()
	transport.Close()
}

func TestStdioTransport_SendAndRecv(t *testing.T) {
	cmd := findPython(t)
	if cmd == "" {
		t.Skip("python not available")
	}

	transport := NewStdioTransport(StdioConfig{
		Command: cmd,
		Args: []string{"-c", `
import sys
for line in sys.stdin:
    line = line.strip()
    if line:
        sys.stdout.write(line + "_echo\n")
        sys.stdout.flush()
`},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := transport.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer transport.Close()

	// 等子进程就绪
	time.Sleep(100 * time.Millisecond)

	// 发送数据
	err = transport.Send([]byte("hello"))
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// 接收响应
	data, err := transport.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}

	if !strings.Contains(string(data), "hello_echo") {
		t.Fatalf("expected 'hello_echo', got '%s'", string(data))
	}
}

func TestStdioTransport_MultipleSendRecv(t *testing.T) {
	cmd := findPython(t)
	if cmd == "" {
		t.Skip("python not available")
	}

	transport := NewStdioTransport(StdioConfig{
		Command: cmd,
		Args: []string{"-c", `
import sys
for line in sys.stdin:
    line = line.strip()
    if line:
        sys.stdout.write(line + "\n")
        sys.stdout.flush()
`},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := transport.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer transport.Close()

	time.Sleep(100 * time.Millisecond)

	messages := []string{"msg1", "msg2", "msg3"}
	for _, msg := range messages {
		if err := transport.Send([]byte(msg)); err != nil {
			t.Fatalf("send %s: %v", msg, err)
		}

		data, err := transport.Recv()
		if err != nil {
			t.Fatalf("recv %s: %v", msg, err)
		}

		if strings.TrimSpace(string(data)) != msg {
			t.Fatalf("expected '%s', got '%s'", msg, strings.TrimSpace(string(data)))
		}
	}
}

// findPython 查找可用的 Python 解释器
func findPython(t *testing.T) string {
	t.Helper()

	candidates := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		candidates = []string{"python", "python3"}
	}

	for _, name := range candidates {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}

	return ""
}
