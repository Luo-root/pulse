package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	mcpsrc "github.com/Luo-root/pulse/toolset/mcp"
)

// mockClient 是可控制的 Client：支持动态改工具表、模拟断开与取消。
type mockClient struct {
	mu      sync.Mutex
	tools   []mcpsrc.Tool
	closed  bool
	calls   []string
	failList error
	failCall error
}

func (m *mockClient) ListTools(ctx context.Context) ([]mcpsrc.Tool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("mock: closed")
	}
	if m.failList != nil {
		return nil, m.failList
	}
	out := make([]mcpsrc.Tool, len(m.tools))
	copy(out, m.tools)
	return out, nil
}

func (m *mockClient) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", fmt.Errorf("mock: closed")
	}
	if m.failCall != nil {
		return "", m.failCall
	}
	m.calls = append(m.calls, name)
	return fmt.Sprintf(`{"tool":%q,"args":%s}`, name, string(args)), nil
}

func (m *mockClient) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockClient) setTools(tools ...mcpsrc.Tool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools = append([]mcpsrc.Tool(nil), tools...)
}

var _ mcpsrc.Client = (*mockClient)(nil)
