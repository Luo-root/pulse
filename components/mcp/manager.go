package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Luo-root/pulse/components/tools"
)

// ============================================================================
// 服务器配置
// ============================================================================

// ServerConfig MCP 服务器配置
type ServerConfig struct {
	Name      string          `json:"name"`      // 服务器名称（用于日志和工具名前缀）
	Transport TransportConfig `json:"transport"` // 传输配置
	Enabled   bool            `json:"enabled"`   // 是否启用（默认 true）
	Prefix    string          `json:"prefix"`    // 工具名前缀（默认用 Name）
	Tags      []string        `json:"tags"`      // 额外标签
}

// TransportConfig 传输层配置
type TransportConfig struct {
	Type    string   `json:"type"`    // "stdio" 或 "sse"
	Command string   `json:"command"` // stdio: 命令路径
	Args    []string `json:"args"`    // stdio: 命令参数
	Env     []string `json:"env"`     // stdio: 环境变量
	WorkDir string   `json:"work_dir"`

	// SSE 传输配置
	URL     string            `json:"url"`     // sse: SSE 端点地址
	Headers map[string]string `json:"headers"` // sse: 自定义请求头（如认证）
}

// ============================================================================
// Manager
// ============================================================================

// Manager 管理多个 MCP 服务器连接
type Manager struct {
	registry *tools.ToolRegistry
	clients  map[string]*serverEntry
	mu       sync.RWMutex
}

type serverEntry struct {
	config ServerConfig
	client *Client
	tools  []MCPTool
}

// NewManager 创建 MCP 管理器
func NewManager(registry *tools.ToolRegistry) *Manager {
	return &Manager{
		registry: registry,
		clients:  make(map[string]*serverEntry),
	}
}

// Connect 连接到 MCP 服务器并注册其工具
func (m *Manager) Connect(ctx context.Context, config ServerConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.clients[config.Name]; exists {
		return fmt.Errorf("mcp server %s already connected", config.Name)
	}

	if config.Prefix == "" {
		config.Prefix = config.Name
	}

	// 创建传输层
	var transport Transport
	switch config.Transport.Type {
	case "stdio", "":
		if config.Transport.Command == "" {
			return fmt.Errorf("mcp server %s: transport.command is required for stdio", config.Name)
		}
		transport = NewStdioTransport(StdioConfig{
			Command: config.Transport.Command,
			Args:    config.Transport.Args,
			Env:     config.Transport.Env,
			WorkDir: config.Transport.WorkDir,
		})
	case "sse":
		if config.Transport.URL == "" {
			return fmt.Errorf("mcp server %s: transport.url is required for sse", config.Name)
		}
		transport = NewSSETransport(SSEConfig{
			URL:     config.Transport.URL,
			Headers: config.Transport.Headers,
		})
	default:
		return fmt.Errorf("mcp server %s: unsupported transport type %q", config.Name, config.Transport.Type)
	}

	// 创建客户端
	client := NewClient(transport, ClientConfig{
		Name: "pulse-agent",
	})

	// 连接 + 初始化
	if err := client.Connect(ctx); err != nil {
		return fmt.Errorf("mcp server %s: connect: %w", config.Name, err)
	}

	// 发现工具
	toolList, err := client.ListTools(ctx)
	if err != nil {
		client.Close()
		return fmt.Errorf("mcp server %s: list tools: %w", config.Name, err)
	}

	// 注册到 ToolRegistry
	entry := &serverEntry{
		config: config,
		client: client,
		tools:  toolList,
	}

	if err := m.registerTools(entry); err != nil {
		client.Close()
		return err
	}

	m.clients[config.Name] = entry
	return nil
}

// Disconnect 断开服务器连接并注销工具
func (m *Manager) Disconnect(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.clients[name]
	if !ok {
		return fmt.Errorf("mcp server %s not found", name)
	}

	// 注销工具
	m.unregisterTools(entry)

	// 关闭客户端
	if err := entry.client.Close(); err != nil {
		return err
	}

	delete(m.clients, name)
	return nil
}

// ConnectAll 批量连接多个服务器
func (m *Manager) ConnectAll(ctx context.Context, configs []ServerConfig) []error {
	var errs []error
	for _, config := range configs {
		if !config.Enabled {
			continue
		}
		if err := m.Connect(ctx, config); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// ListServers 列出所有已连接的服务器
func (m *Manager) ListServers() map[string]ServerConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]ServerConfig)
	for name, entry := range m.clients {
		result[name] = entry.config
	}
	return result
}

// ListAllTools 列出所有 MCP 工具（按服务器分组）
func (m *Manager) ListAllTools() map[string][]MCPTool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string][]MCPTool)
	for name, entry := range m.clients {
		tools_ := make([]MCPTool, len(entry.tools))
		copy(tools_, entry.tools)
		result[name] = tools_
	}
	return result
}

// Close 关闭所有连接
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var firstErr error
	for name, entry := range m.clients {
		m.unregisterTools(entry)
		if err := entry.client.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(m.clients, name)
	}

	return firstErr
}

// ============================================================================
// 内部方法
// ============================================================================

// registerTools 将 MCP 工具注册到 ToolRegistry
func (m *Manager) registerTools(entry *serverEntry) error {
	prefix := entry.config.Prefix + "/"

	for _, t := range entry.tools {
		toolName := prefix + t.Name

		// 跳过已存在的工具（避免冲突）
		if _, exists := m.registry.Get(toolName); exists {
			continue
		}

		// 捕获变量
		client := entry.client
		toolName_ := t.Name

		handler := func(ctx context.Context, args map[string]any) (any, error) {
			return callMCPTool(ctx, client, toolName_, args)
		}

		// 合并标签
		tags := []string{"mcp", entry.config.Name}
		tags = append(tags, entry.config.Tags...)

		meta := tools.ToolMetadata{
			Name:        toolName,
			Description: t.Description,
			Parameters:  t.InputSchema,
			Category:    "mcp",
			Tags:        tags,
		}

		if err := m.registry.Register(meta, handler); err != nil {
			return fmt.Errorf("register mcp tool %s: %w", toolName, err)
		}
	}

	return nil
}

// unregisterTools 从 ToolRegistry 注销工具
func (m *Manager) unregisterTools(entry *serverEntry) {
	prefix := entry.config.Prefix + "/"
	for _, t := range entry.tools {
		m.registry.Unregister(prefix + t.Name)
	}
}

// callMCPTool 调用 MCP 工具并转换结果
func callMCPTool(ctx context.Context, client *Client, toolName string, args map[string]any) (any, error) {
	result, err := client.CallTool(ctx, toolName, args)
	if err != nil {
		return nil, err
	}

	// 如果标记为错误，包装成 error
	if result.IsError {
		text := extractText(result.Content)
		return nil, fmt.Errorf("mcp tool error: %s", text)
	}

	// 提取文本内容
	text := extractText(result.Content)
	if text != "" {
		return text, nil
	}

	// 无文本内容时返回原始结果
	return result.Content, nil
}

// extractText 从 ContentItem 列表提取文本
func extractText(items []ContentItem) string {
	if len(items) == 0 {
		return ""
	}

	var parts []string
	for _, item := range items {
		if item.Type == "text" && item.Text != "" {
			parts = append(parts, item.Text)
		}
	}

	return strings.Join(parts, "\n")
}
