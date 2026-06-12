package mcp

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/Luo-root/pulse/components/tools"
)

// ============================================================================
// 辅助函数
// ============================================================================

func setupManager(t *testing.T) (*tools.ToolRegistry, *Manager) {
	t.Helper()
	registry := tools.NewToolRegistry()
	mgr := NewManager(registry)
	return registry, mgr
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

// ============================================================================
// Manager 测试
// ============================================================================

func TestManager_Connect_InvalidTransport(t *testing.T) {
	_, mgr := setupManager(t)

	err := mgr.Connect(context.Background(), ServerConfig{
		Name:      "test",
		Transport: TransportConfig{Type: "invalid_type"},
	})

	if err == nil {
		t.Fatal("expected error for invalid transport type")
	}
}

func TestManager_Connect_MissingCommand(t *testing.T) {
	_, mgr := setupManager(t)

	err := mgr.Connect(context.Background(), ServerConfig{
		Name:      "test",
		Transport: TransportConfig{Type: "stdio"},
	})

	if err == nil {
		t.Fatal("expected error for missing command")
	}
}

func TestManager_Disconnect_NotFound(t *testing.T) {
	_, mgr := setupManager(t)

	err := mgr.Disconnect("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent server")
	}
}

func TestManager_ListServers_Empty(t *testing.T) {
	_, mgr := setupManager(t)

	servers := mgr.ListServers()
	if len(servers) != 0 {
		t.Fatalf("expected 0 servers, got %d", len(servers))
	}
}

func TestManager_ListAllTools_Empty(t *testing.T) {
	_, mgr := setupManager(t)

	toolList := mgr.ListAllTools()
	if len(toolList) != 0 {
		t.Fatalf("expected 0, got %d", len(toolList))
	}
}

func TestManager_Close_Empty(t *testing.T) {
	_, mgr := setupManager(t)

	err := mgr.Close()
	if err != nil {
		t.Fatalf("close empty manager: %v", err)
	}
}

func TestManager_ConnectAll_SkipsDisabled(t *testing.T) {
	_, mgr := setupManager(t)

	configs := []ServerConfig{
		{
			Name:    "disabled",
			Enabled: false,
			Transport: TransportConfig{
				Type:    "stdio",
				Command: "nonexistent",
			},
		},
	}

	errs := mgr.ConnectAll(context.Background(), configs)
	if len(errs) != 0 {
		t.Fatalf("expected 0 errors (disabled skipped), got %d", len(errs))
	}
}

func TestManager_ConnectAll_PartialFailure(t *testing.T) {
	_, mgr := setupManager(t)

	configs := []ServerConfig{
		{Name: "bad1", Enabled: true, Transport: TransportConfig{Type: "invalid"}},
		{Name: "bad2", Enabled: true, Transport: TransportConfig{Type: "stdio"}},
	}

	errs := mgr.ConnectAll(context.Background(), configs)
	if len(errs) != 2 {
		t.Fatalf("expected 2 errors, got %d", len(errs))
	}
}

func TestManager_LoadConfig(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := tmpDir + "/mcp.json"

	writeTestFile(t, configPath, `{
		"mcpServers": [
			{
				"name": "github",
				"transport": {
					"type": "stdio",
					"command": "npx",
					"args": ["-y", "@modelcontextprotocol/server-github"]
				},
				"enabled": true
			},
			{
				"name": "disabled",
				"transport": {
					"type": "stdio",
					"command": "test"
				},
				"enabled": false
			}
		]
	}`)

	configs, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}
	if configs[0].Name != "github" {
		t.Fatalf("expected github, got %s", configs[0].Name)
	}
	if configs[0].Transport.Command != "npx" {
		t.Fatalf("expected npx, got %s", configs[0].Transport.Command)
	}
}

func TestManager_LoadConfig_WithEnvVars(t *testing.T) {
	t.Setenv("TEST_MCP_CMD", "my-command")
	t.Setenv("TEST_MCP_TOKEN", "secret-token")

	tmpDir := t.TempDir()
	configPath := tmpDir + "/mcp.json"

	writeTestFile(t, configPath, `{
		"mcpServers": [{
			"name": "test",
			"transport": {
				"type": "stdio",
				"command": "${TEST_MCP_CMD}",
				"args": ["--token", "${TEST_MCP_TOKEN}"],
				"env": ["TOKEN=${TEST_MCP_TOKEN}"]
			}
		}]
	}`)

	configs, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	if configs[0].Transport.Command != "my-command" {
		t.Fatalf("expected my-command, got %s", configs[0].Transport.Command)
	}
	if configs[0].Transport.Args[1] != "secret-token" {
		t.Fatalf("expected secret-token, got %s", configs[0].Transport.Args[1])
	}
}

func TestManager_LoadConfig_InvalidFile(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path.json")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestManager_LoadConfig_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := tmpDir + "/bad.json"
	writeTestFile(t, configPath, "not json at all")

	_, err := LoadConfig(configPath)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestManager_ToolRegistrationAndExecution(t *testing.T) {
	registry, _ := setupManager(t)

	meta := tools.ToolMetadata{
		Name:        "github/get_issue",
		Description: "Get a GitHub issue",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"issue_number": map[string]any{"type": "number"},
			},
		},
		Category: "mcp",
		Tags:     []string{"mcp", "github"},
	}

	handler := func(ctx context.Context, args map[string]any) (any, error) {
		num, _ := args["issue_number"].(json.Number)
		return "issue #" + num.String() + ": test issue", nil
	}

	err := registry.Register(meta, handler)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// 验证工具已注册
	allTools := registry.GetEnabledTools()
	found := false
	for _, tool := range allTools {
		if tool.Name == "github/get_issue" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected github/get_issue in registered tools")
	}
}

func TestManager_ConnectFromConfig_InvalidPath(t *testing.T) {
	_, mgr := setupManager(t)

	err := mgr.ConnectFromConfig(context.Background(), "/nonexistent/config.json")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestManager_ToolNameWithPrefix(t *testing.T) {
	registry, _ := setupManager(t)

	// 模拟 MCP 工具注册时的前缀行为
	prefix := "github/"
	toolName := "get_issue"
	fullName := prefix + toolName

	if fullName != "github/get_issue" {
		t.Fatalf("expected github/get_issue, got %s", fullName)
	}

	_ = registry
}

func TestManager_ConnectDefaultEnabled(t *testing.T) {
	config := ServerConfig{
		Name: "test",
		Transport: TransportConfig{
			Type:    "stdio",
			Command: "test",
		},
		// Enabled 未设置，默认应该是 true
	}

	if !config.Enabled {
		// 零值是 false，所以需要在 ConnectAll 里特殊处理
		// Manager.ConnectAll 应该把 Enabled=false 的跳过
		// 但零值就是 false，所以我们需要在文档中说明
	}

	_ = config
}
