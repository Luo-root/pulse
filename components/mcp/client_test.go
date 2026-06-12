package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// ============================================================================
// MockTransport — 阻塞式，正确配对请求和响应
// ============================================================================

// MockTransport 模拟 MCP 传输层
// Recv 会阻塞直到有 Send 发生，然后用 handler 生成响应并返回
type MockTransport struct {
	mu       sync.Mutex
	cond     *sync.Cond
	pending  []Request // 已发送、待处理的请求
	handlers map[string]func(Request) json.RawMessage
	closed   bool
}

func NewMockTransport() *MockTransport {
	m := &MockTransport{
		handlers: make(map[string]func(Request) json.RawMessage),
	}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// On 注册某个 method 的响应处理器
func (m *MockTransport) On(method string, handler func(Request) json.RawMessage) *MockTransport {
	m.handlers[method] = handler
	return m
}

func (m *MockTransport) Connect(ctx context.Context) error {
	return nil
}

func (m *MockTransport) Send(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return fmt.Errorf("closed")
	}

	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		return err
	}

	m.pending = append(m.pending, req)
	m.cond.Signal() // 通知 Recv 有新请求了
	return nil
}

func (m *MockTransport) Recv() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 阻塞等待直到有请求被发送
	for len(m.pending) == 0 && !m.closed {
		m.cond.Wait()
	}

	if m.closed && len(m.pending) == 0 {
		return nil, fmt.Errorf("connection closed")
	}

	// 取出第一个待处理请求
	req := m.pending[0]
	m.pending = m.pending[1:]

	// 查找 handler
	handler, ok := m.handlers[req.Method]
	if !ok {
		return nil, fmt.Errorf("no handler for method: %s", req.Method)
	}

	// 生成响应
	result := handler(req)

	// 包装成完整的 JSON-RPC 响应（关键：带上 ID）
	resp := Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	}

	return json.Marshal(resp)
}

func (m *MockTransport) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.cond.Broadcast() // 唤醒所有等待的 Recv
	return nil
}

// ============================================================================
// 辅助：构建常见响应
// ============================================================================

func makeInitResult() json.RawMessage {
	data, _ := json.Marshal(InitializeResult{
		ProtocolVersion: "2024-11-05",
		Capabilities:    ServerCapabilities{Tools: &ToolsCapability{}},
		ServerInfo:      ServerInfo{Name: "test-server", Version: "0.1.0"},
	})
	return data
}

func makeToolsResult(tools ...MCPTool) json.RawMessage {
	data, _ := json.Marshal(ListToolsResult{Tools: tools})
	return data
}

func makeCallResult(text string) json.RawMessage {
	data, _ := json.Marshal(CallToolResult{
		Content: []ContentItem{{Type: "text", Text: text}},
	})
	return data
}

func makeCallErrorResult(text string) json.RawMessage {
	data, _ := json.Marshal(CallToolResult{
		Content: []ContentItem{{Type: "text", Text: text}},
		IsError: true,
	})
	return data
}

// ============================================================================
// 测试
// ============================================================================

func TestClient_Connect(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			return makeInitResult()
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		})

	client := NewClient(transport, ClientConfig{
		Name:    "test-client",
		Version: "1.0",
	})

	err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	info := client.ServerInfo()
	if info == nil {
		t.Fatal("expected server info")
	}
	if info.Name != "test-server" {
		t.Fatalf("expected test-server, got %s", info.Name)
	}
	if info.Version != "0.1.0" {
		t.Fatalf("expected 0.1.0, got %s", info.Version)
	}

	caps := client.ServerCapabilities()
	if caps == nil {
		t.Fatal("expected server capabilities")
	}
	if caps.Tools == nil {
		t.Fatal("expected tools capability")
	}
}

func TestClient_Connect_VersionNegotiation(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			// 服务器返回不同的版本
			data, _ := json.Marshal(InitializeResult{
				ProtocolVersion: "2024-11-05",
				Capabilities:    ServerCapabilities{Tools: &ToolsCapability{}},
				ServerInfo:      ServerInfo{Name: "server", Version: "1.0"},
			})
			return data
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		})

	client := NewClient(transport, ClientConfig{})
	err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Close()

	if client.protocolVersion != "2024-11-05" {
		t.Fatalf("expected 2024-11-05, got %s", client.protocolVersion)
	}
}

func TestClient_ListTools(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			return makeInitResult()
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		}).
		On("tools/list", func(req Request) json.RawMessage {
			return makeToolsResult(
				MCPTool{Name: "file_read", Description: "read a file"},
				MCPTool{Name: "file_list", Description: "list files"},
			)
		})

	client := NewClient(transport, ClientConfig{})
	client.Connect(context.Background())
	defer client.Close()

	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Name != "file_read" {
		t.Fatalf("expected file_read, got %s", tools[0].Name)
	}
	if tools[0].Description != "read a file" {
		t.Fatalf("expected 'read a file', got '%s'", tools[0].Description)
	}
	if tools[1].Name != "file_list" {
		t.Fatalf("expected file_list, got %s", tools[1].Name)
	}
}

func TestClient_ListTools_WithSchema(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			return makeInitResult()
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		}).
		On("tools/list", func(req Request) json.RawMessage {
			return makeToolsResult(MCPTool{
				Name:        "file_read",
				Description: "read file",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string", "description": "file path"},
					},
					"required": []string{"path"},
				},
			})
		})

	client := NewClient(transport, ClientConfig{})
	client.Connect(context.Background())
	defer client.Close()

	tools, _ := client.ListTools(context.Background())

	schema := tools[0].InputSchema
	if schema["type"] != "object" {
		t.Fatalf("expected object, got %v", schema["type"])
	}
}

func TestClient_ListTools_Empty(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			return makeInitResult()
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		}).
		On("tools/list", func(req Request) json.RawMessage {
			return makeToolsResult() // 空列表
		})

	client := NewClient(transport, ClientConfig{})
	client.Connect(context.Background())
	defer client.Close()

	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	if len(tools) != 0 {
		t.Fatalf("expected 0 tools, got %d", len(tools))
	}
}

func TestClient_CallTool(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			return makeInitResult()
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		}).
		On("tools/call", func(req Request) json.RawMessage {
			// 验证请求参数
			var params CallToolParams
			data, _ := json.Marshal(req.Params)
			json.Unmarshal(data, &params)

			if params.Name != "file_read" {
				return makeCallErrorResult("unexpected tool: " + params.Name)
			}

			path, _ := params.Arguments["path"].(string)
			return makeCallResult("contents of " + path)
		})

	client := NewClient(transport, ClientConfig{})
	client.Connect(context.Background())
	defer client.Close()

	result, err := client.CallTool(context.Background(), "file_read", map[string]any{
		"path": "/etc/passwd",
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	if result.Content[0].Text != "contents of /etc/passwd" {
		t.Fatalf("unexpected result: %s", result.Content[0].Text)
	}
	if result.IsError {
		t.Fatal("should not be error")
	}
}

func TestClient_CallTool_MultipleArgs(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			return makeInitResult()
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		}).
		On("tools/call", func(req Request) json.RawMessage {
			var params CallToolParams
			data, _ := json.Marshal(req.Params)
			json.Unmarshal(data, &params)

			// 验证多个参数
			if params.Arguments["path"] == nil || params.Arguments["encoding"] == nil {
				return makeCallErrorResult("missing args")
			}

			return makeCallResult("ok")
		})

	client := NewClient(transport, ClientConfig{})
	client.Connect(context.Background())
	defer client.Close()

	result, err := client.CallTool(context.Background(), "file_read", map[string]any{
		"path":     "/test",
		"encoding": "utf-8",
	})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	if result.Content[0].Text != "ok" {
		t.Fatalf("expected ok, got %s", result.Content[0].Text)
	}
}

func TestClient_CallTool_ErrorResult(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			return makeInitResult()
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		}).
		On("tools/call", func(req Request) json.RawMessage {
			return makeCallErrorResult("permission denied")
		})

	client := NewClient(transport, ClientConfig{})
	client.Connect(context.Background())
	defer client.Close()

	result, err := client.CallTool(context.Background(), "file_read", nil)
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	if !result.IsError {
		t.Fatal("expected IsError=true")
	}
	if result.Content[0].Text != "permission denied" {
		t.Fatalf("expected 'permission denied', got '%s'", result.Content[0].Text)
	}
}

func TestClient_CallTool_MultipleContentBlocks(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			return makeInitResult()
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		}).
		On("tools/call", func(req Request) json.RawMessage {
			data, _ := json.Marshal(CallToolResult{
				Content: []ContentItem{
					{Type: "text", Text: "line 1"},
					{Type: "text", Text: "line 2"},
				},
			})
			return data
		})

	client := NewClient(transport, ClientConfig{})
	client.Connect(context.Background())
	defer client.Close()

	result, err := client.CallTool(context.Background(), "test", nil)
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}

	if len(result.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(result.Content))
	}
}

func TestClient_DefaultConfig(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			return makeInitResult()
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		})

	// 空配置，应该用默认值
	client := NewClient(transport, ClientConfig{})

	if client.info.Name != "pulse-agent" {
		t.Fatalf("expected default name 'pulse-agent', got '%s'", client.info.Name)
	}
	if client.info.Version != "1.0.0" {
		t.Fatalf("expected default version '1.0.0', got '%s'", client.info.Version)
	}

	client.Connect(context.Background())
	client.Close()
}

func TestClient_RecvLoopHandlesInvalidJSON(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			return makeInitResult()
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		})

	client := NewClient(transport, ClientConfig{})
	client.Connect(context.Background())
	defer client.Close()

	// 正常使用不会 panic
	info := client.ServerInfo()
	if info == nil {
		t.Fatal("expected server info")
	}
}

func TestClient_Close(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			return makeInitResult()
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		})

	client := NewClient(transport, ClientConfig{})
	client.Connect(context.Background())

	err := client.Close()
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	// 关闭后再调用应该失败
	_, err = client.ListTools(context.Background())
	if err == nil {
		t.Fatal("expected error after close")
	}
}

func TestClient_ServerInfo_AfterConnect(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			return makeInitResult()
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		})

	client := NewClient(transport, ClientConfig{})

	// 连接前应该为 nil
	if client.ServerInfo() != nil {
		t.Fatal("expected nil before connect")
	}

	client.Connect(context.Background())
	defer client.Close()

	if client.ServerInfo() == nil {
		t.Fatal("expected non-nil after connect")
	}
}

func TestClient_MultipleTools(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			return makeInitResult()
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		}).
		On("tools/list", func(req Request) json.RawMessage {
			return makeToolsResult(
				MCPTool{Name: "tool_a", Description: "A"},
				MCPTool{Name: "tool_b", Description: "B"},
				MCPTool{Name: "tool_c", Description: "C"},
				MCPTool{Name: "tool_d", Description: "D"},
				MCPTool{Name: "tool_e", Description: "E"},
			)
		})

	client := NewClient(transport, ClientConfig{})
	client.Connect(context.Background())
	defer client.Close()

	tools, _ := client.ListTools(context.Background())
	if len(tools) != 5 {
		t.Fatalf("expected 5, got %d", len(tools))
	}
}

func TestClient_ConcurrentCalls(t *testing.T) {
	transport := NewMockTransport().
		On("initialize", func(req Request) json.RawMessage {
			return makeInitResult()
		}).
		On("notifications/initialized", func(req Request) json.RawMessage {
			return json.RawMessage(`{}`)
		}).
		On("tools/call", func(req Request) json.RawMessage {
			var params CallToolParams
			data, _ := json.Marshal(req.Params)
			json.Unmarshal(data, &params)
			return makeCallResult("result for " + params.Name)
		})

	client := NewClient(transport, ClientConfig{})
	client.Connect(context.Background())
	defer client.Close()

	// 并发调用工具
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			toolName := fmt.Sprintf("tool_%d", idx)
			result, err := client.CallTool(context.Background(), toolName, nil)
			if err != nil {
				t.Errorf("call %s: %v", toolName, err)
				return
			}
			expected := "result for " + toolName
			if result.Content[0].Text != expected {
				t.Errorf("expected '%s', got '%s'", expected, result.Content[0].Text)
			}
		}(i)
	}
	wg.Wait()
}

// ============================================================================
// extractText 测试
// ============================================================================

func TestExtractText(t *testing.T) {
	tests := []struct {
		name     string
		items    []ContentItem
		expected string
	}{
		{"nil", nil, ""},
		{"empty", []ContentItem{}, ""},
		{"single", []ContentItem{{Type: "text", Text: "hello"}}, "hello"},
		{"multiple", []ContentItem{
			{Type: "text", Text: "part1"},
			{Type: "text", Text: "part2"},
		}, "part1\npart2"},
		{"mixed", []ContentItem{
			{Type: "text", Text: "text"},
			{Type: "image", Data: "base64"},
		}, "text"},
		{"no_text", []ContentItem{
			{Type: "image", Data: "base64"},
		}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractText(tt.items)
			if result != tt.expected {
				t.Fatalf("expected '%s', got '%s'", tt.expected, result)
			}
		})
	}
}

func TestExtractText_SpecialChars(t *testing.T) {
	items := []ContentItem{
		{Type: "text", Text: "line1\nline2\ttab"},
	}
	result := extractText(items)
	if !strings.Contains(result, "\n") {
		t.Fatal("expected newline preserved")
	}
}

func TestExtractText_Unicode(t *testing.T) {
	items := []ContentItem{
		{Type: "text", Text: "你好世界 🌍"},
	}
	result := extractText(items)
	if result != "你好世界 🌍" {
		t.Fatalf("expected unicode preserved, got '%s'", result)
	}
}
