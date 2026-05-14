package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
)

// Client MCP 客户端
// 实现 JSON-RPC 2.0 over MCP 协议
type Client struct {
	transport Transport
	info      ClientInfo

	// 请求 ID 原子递增
	nextID atomic.Int32

	// 待响应的请求（ID → channel）
	pendingMu sync.Mutex
	pending   map[int]chan *Response

	// 服务器信息（初始化后填充）
	serverInfo      *ServerInfo
	serverCaps      *ServerCapabilities
	protocolVersion string

	// 接收循环
	recvDone  chan struct{}
	recvErr   error
	closeOnce sync.Once
}

// ClientConfig 客户端配置
type ClientConfig struct {
	Name    string // 客户端名称
	Version string // 客户端版本
}

// NewClient 创建 MCP 客户端
func NewClient(transport Transport, config ClientConfig) *Client {
	if config.Name == "" {
		config.Name = "pulse-agent"
	}
	if config.Version == "" {
		config.Version = "1.0.0"
	}

	return &Client{
		transport: transport,
		info: ClientInfo{
			Name:    config.Name,
			Version: config.Version,
		},
		pending:  make(map[int]chan *Response),
		recvDone: make(chan struct{}),
	}
}

// Connect 建立连接并初始化
func (c *Client) Connect(ctx context.Context) error {
	// 1. 建立传输层连接
	if err := c.transport.Connect(ctx); err != nil {
		return fmt.Errorf("mcp: connect: %w", err)
	}

	// 2. 启动接收循环
	go c.recvLoop()

	// 3. 发送 initialize 请求
	initParams := InitializeParams{
		ProtocolVersion: "2024-11-05",
		Capabilities: ClientCapabilities{
			Roots: &RootsCapability{ListChanged: false},
		},
		ClientInfo: c.info,
	}

	var initResult InitializeResult
	if err := c.call(ctx, "initialize", initParams, &initResult); err != nil {
		c.Close()
		return fmt.Errorf("mcp: initialize: %w", err)
	}

	c.serverInfo = &initResult.ServerInfo
	c.serverCaps = &initResult.Capabilities
	c.protocolVersion = initResult.ProtocolVersion

	// 4. 发送 initialized 通知
	if err := c.notify("notifications/initialized", nil); err != nil {
		c.Close()
		return fmt.Errorf("mcp: initialized notification: %w", err)
	}

	return nil
}

// ListTools 获取服务器提供的工具列表
func (c *Client) ListTools(ctx context.Context) ([]MCPTool, error) {
	var result ListToolsResult
	if err := c.call(ctx, "tools/list", nil, &result); err != nil {
		return nil, fmt.Errorf("mcp: tools/list: %w", err)
	}

	// 处理分页（如果有 nextCursor）
	allTools := result.Tools
	for result.NextCursor != "" {
		params := map[string]string{"cursor": result.NextCursor}
		result = ListToolsResult{}
		if err := c.call(ctx, "tools/list", params, &result); err != nil {
			break // 分页失败时返回已获取的工具
		}
		allTools = append(allTools, result.Tools...)
	}

	return allTools, nil
}

// CallTool 调用工具
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*CallToolResult, error) {
	params := CallToolParams{
		Name:      name,
		Arguments: args,
	}

	var result CallToolResult
	if err := c.call(ctx, "tools/call", params, &result); err != nil {
		return nil, fmt.Errorf("mcp: tools/call %s: %w", name, err)
	}

	return &result, nil
}

// ServerInfo 获取服务器信息
func (c *Client) ServerInfo() *ServerInfo {
	return c.serverInfo
}

// ServerCapabilities 获取服务器能力
func (c *Client) ServerCapabilities() *ServerCapabilities {
	return c.serverCaps
}

// Close 关闭客户端
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.recvDone)
	})
	if c.transport != nil {
		return c.transport.Close()
	}
	return nil
}

// ============================================================================
// 内部方法
// ============================================================================

// call 发送 JSON-RPC 请求并等待响应
func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	id := int(c.nextID.Add(1))

	req := Request{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	// 创建等待 channel
	ch := make(chan *Response, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	// 发送请求
	if err := c.transport.Send(data); err != nil {
		return err
	}

	// 等待响应（带超时）
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && resp.Result != nil {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("unmarshal result: %w", err)
			}
		}
		return nil

	case <-ctx.Done():
		return ctx.Err()

	case <-c.recvDone:
		return fmt.Errorf("mcp: connection closed")
	}
}

// notify 发送 JSON-RPC 通知（无 ID，不等待响应）
func (c *Client) notify(method string, params any) error {
	req := Request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}

	return c.transport.Send(data)
}

// recvLoop 持续从传输层读取响应并分发
func (c *Client) recvLoop() {
	defer c.closeOnce.Do(func() {
		close(c.recvDone)
	})

	for {
		data, err := c.transport.Recv()
		if err != nil {
			return
		}

		var resp Response
		if err := json.Unmarshal(data, &resp); err != nil {
			continue // 忽略无法解析的消息
		}

		// 通知（无 ID）：暂不处理
		if resp.ID == 0 {
			continue
		}

		// 响应：分发给等待者
		c.pendingMu.Lock()
		ch, ok := c.pending[resp.ID]
		c.pendingMu.Unlock()

		if ok {
			select {
			case ch <- &resp:
			default:
				// channel 已满或已关闭，丢弃
			}
		}
	}
}
