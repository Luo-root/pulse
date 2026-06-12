package tools

import (
	"context"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

func TestToolRegistryBasic(t *testing.T) {
	registry := NewToolRegistry()

	// 注册工具
	meta := ToolMetadata{
		Name:        "test_tool",
		Description: "A test tool",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{"type": "string"},
			},
		},
		Permission: PermReadOnly,
		Category:   "test",
		Tags:       []string{"test", "demo"},
	}

	handler := func(ctx context.Context, args map[string]any) (any, error) {
		return map[string]string{"result": "ok"}, nil
	}

	if err := registry.Register(meta, handler); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	// 获取工具
	tool, ok := registry.Get("test_tool")
	if !ok {
		t.Fatal("tool not found")
	}
	if tool.Metadata.Name != "test_tool" {
		t.Errorf("expected test_tool, got %s", tool.Metadata.Name)
	}

	// 获取启用的工具
	enabled := registry.GetEnabledTools()
	if len(enabled) != 1 {
		t.Errorf("expected 1 enabled tool, got %d", len(enabled))
	}
}

func TestToolRegistryDuplicate(t *testing.T) {
	registry := NewToolRegistry()

	meta := ToolMetadata{
		Name:        "dup_tool",
		Description: "Duplicate test",
		Parameters:  map[string]any{},
	}

	handler := func(ctx context.Context, args map[string]any) (any, error) {
		return nil, nil
	}

	registry.Register(meta, handler)

	// 重复注册应该失败
	if err := registry.Register(meta, handler); err == nil {
		t.Error("expected error for duplicate registration")
	}
}

func TestToolRegistryUnregister(t *testing.T) {
	registry := NewToolRegistry()

	meta := ToolMetadata{
		Name:        "temp_tool",
		Description: "Temporary tool",
		Parameters:  map[string]any{},
	}

	registry.Register(meta, func(ctx context.Context, args map[string]any) (any, error) {
		return nil, nil
	})

	// 注销
	if err := registry.Unregister("temp_tool"); err != nil {
		t.Fatalf("unregister failed: %v", err)
	}

	// 验证已注销
	if _, ok := registry.Get("temp_tool"); ok {
		t.Error("tool should be unregistered")
	}
}

func TestToolRegistryEnableDisable(t *testing.T) {
	registry := NewToolRegistry()

	meta := ToolMetadata{
		Name:        "toggle_tool",
		Description: "Toggle test",
		Parameters:  map[string]any{},
	}

	registry.Register(meta, func(ctx context.Context, args map[string]any) (any, error) {
		return nil, nil
	})

	// 禁用
	if err := registry.Disable("toggle_tool"); err != nil {
		t.Fatalf("disable failed: %v", err)
	}

	enabled := registry.GetEnabledTools()
	if len(enabled) != 0 {
		t.Errorf("expected 0 enabled tools, got %d", len(enabled))
	}

	// 启用
	if err := registry.Enable("toggle_tool"); err != nil {
		t.Fatalf("enable failed: %v", err)
	}

	enabled = registry.GetEnabledTools()
	if len(enabled) != 1 {
		t.Errorf("expected 1 enabled tool, got %d", len(enabled))
	}
}

func TestToolRegistryExecute(t *testing.T) {
	registry := NewToolRegistry()

	meta := ToolMetadata{
		Name:        "echo_tool",
		Description: "Echo tool",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{"type": "string"},
			},
		},
		Permission: PermReadOnly,
		Timeout:    5 * time.Second,
	}

	registry.Register(meta, func(ctx context.Context, args map[string]any) (any, error) {
		msg, _ := args["message"].(string)
		return map[string]string{"echo": msg}, nil
	})

	// 执行工具
	call := schema.ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "echo_tool",
			Arguments: `{"message": "hello"}`,
		},
	}

	result := registry.Execute(context.Background(), call)
	if result.IsError {
		t.Fatalf("execute failed: %s", result.Content)
	}
	if result.Content == "" {
		t.Error("expected non-empty result")
	}
}

func TestToolRegistryExecuteDisabled(t *testing.T) {
	registry := NewToolRegistry()

	meta := ToolMetadata{
		Name:        "disabled_tool",
		Description: "Disabled tool",
		Parameters:  map[string]any{},
	}

	registry.Register(meta, func(ctx context.Context, args map[string]any) (any, error) {
		return nil, nil
	})
	registry.Disable("disabled_tool")

	call := schema.ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "disabled_tool",
			Arguments: "{}",
		},
	}

	result := registry.Execute(context.Background(), call)
	if !result.IsError {
		t.Error("expected error for disabled tool")
	}
}

func TestToolRegistryHooks(t *testing.T) {
	registry := NewToolRegistry()

	var beforeCalled, afterCalled bool

	registry.AddBeforeExecuteHook(func(ctx context.Context, toolName string, args map[string]any) error {
		beforeCalled = true
		return nil
	})

	registry.AddAfterExecuteHook(func(ctx context.Context, toolName string, result schema.ToolResult, duration time.Duration) {
		afterCalled = true
	})

	meta := ToolMetadata{
		Name:        "hook_tool",
		Description: "Hook test",
		Parameters:  map[string]any{},
		Timeout:     5 * time.Second,
	}

	registry.Register(meta, func(ctx context.Context, args map[string]any) (any, error) {
		return "ok", nil
	})

	call := schema.ToolCall{
		ID:   "call_1",
		Type: "function",
		Function: schema.FunctionCall{
			Name:      "hook_tool",
			Arguments: "{}",
		},
	}

	registry.Execute(context.Background(), call)

	if !beforeCalled {
		t.Error("before execute hook not called")
	}
	if !afterCalled {
		t.Error("after execute hook not called")
	}
}

func TestToolRegistrySearch(t *testing.T) {
	registry := NewToolRegistry()

	// 注册多个工具
	tools := []struct {
		name     string
		category string
		perm     ToolPermission
		tags     []string
	}{
		{"file_read", "file", PermReadOnly, []string{"file", "read"}},
		{"file_write", "file", PermReadWrite, []string{"file", "write"}},
		{"command_exec", "system", PermDangerous, []string{"system", "dangerous"}},
		{"env_read", "env", PermReadOnly, []string{"env", "read"}},
	}

	for _, tt := range tools {
		meta := ToolMetadata{
			Name:        tt.name,
			Description: tt.name,
			Parameters:  map[string]any{},
			Permission:  tt.perm,
			Category:    tt.category,
			Tags:        tt.tags,
		}
		registry.Register(meta, func(ctx context.Context, args map[string]any) (any, error) {
			return nil, nil
		})
	}

	// 按分类搜索
	fileTools := registry.GetByCategory("file")
	if len(fileTools) != 2 {
		t.Errorf("expected 2 file tools, got %d", len(fileTools))
	}

	// 按权限搜索
	readonlyTools := registry.GetByPermission(PermReadOnly)
	if len(readonlyTools) != 2 {
		t.Errorf("expected 2 readonly tools, got %d", len(readonlyTools))
	}

	// 按标签搜索
	readTools := registry.GetByTag("read")
	if len(readTools) != 2 {
		t.Errorf("expected 2 tools with 'read' tag, got %d", len(readTools))
	}
}

func TestToolRegistryBatchExecute(t *testing.T) {
	registry := NewToolRegistry()

	meta := ToolMetadata{
		Name:        "batch_tool",
		Description: "Batch test",
		Parameters:  map[string]any{},
		Timeout:     5 * time.Second,
	}

	registry.Register(meta, func(ctx context.Context, args map[string]any) (any, error) {
		return "result", nil
	})

	calls := []schema.ToolCall{
		{ID: "1", Type: "function", Function: schema.FunctionCall{Name: "batch_tool", Arguments: "{}"}},
		{ID: "2", Type: "function", Function: schema.FunctionCall{Name: "batch_tool", Arguments: "{}"}},
		{ID: "3", Type: "function", Function: schema.FunctionCall{Name: "batch_tool", Arguments: "{}"}},
	}

	results := registry.ExecuteBatch(context.Background(), calls)
	if len(results) != 3 {
		t.Errorf("expected 3 results, got %d", len(results))
	}

	for i, r := range results {
		if r.IsError {
			t.Errorf("result %d failed: %s", i, r.Content)
		}
	}
}
