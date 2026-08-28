package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// SDKClient 把官方 go-sdk 的 ClientSession 适配为本包 [Client]。
//
// 构造：
//   - [ConnectSDK]：任意 Transport（含 InMemory，供测试）
//   - [ConnectCommand]：stdio CommandTransport，对接外部 MCP Server 进程
type SDKClient struct {
	mu      sync.Mutex
	session *sdkmcp.ClientSession
	closed  bool
}

// ConnectSDK 用官方 Client + Transport 建立会话并返回适配器。
// 调用方负责保证 transport 对端已 Connect（InMemory 时需先起 Server）。
func ConnectSDK(ctx context.Context, transport sdkmcp.Transport) (*SDKClient, error) {
	if transport == nil {
		return nil, fmt.Errorf("toolset/mcp: nil transport")
	}
	c := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "pulse-toolset", Version: "v2"}, nil)
	session, err := c.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("toolset/mcp: connect: %w", err)
	}
	return &SDKClient{session: session}, nil
}

// ConnectCommand 通过 stdio 拉起 command 并建立 MCP 会话。
func ConnectCommand(ctx context.Context, command *exec.Cmd) (*SDKClient, error) {
	if command == nil {
		return nil, fmt.Errorf("toolset/mcp: nil command")
	}
	return ConnectSDK(ctx, &sdkmcp.CommandTransport{Command: command})
}

// ListTools 实现 [Client]：分页拉全量工具并映射为上游 Tool。
func (c *SDKClient) ListTools(ctx context.Context) ([]Tool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	session, err := c.sessionOrErr()
	if err != nil {
		return nil, err
	}

	var out []Tool
	var cursor string
	for {
		params := &sdkmcp.ListToolsParams{}
		if cursor != "" {
			params.Cursor = cursor
		}
		res, err := session.ListTools(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("toolset/mcp: list tools: %w", err)
		}
		for _, t := range res.Tools {
			if t == nil || t.Name == "" {
				continue
			}
			out = append(out, Tool{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schemaToRaw(t.InputSchema),
			})
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return out, nil
}

// CallTool 实现 [Client]。
//
// 传输/协议错误（未知工具、会话已关等）返回 err。
// MCP 工具业务失败（CallToolResult.IsError=true）按协议应让模型看见
// Content：本适配器返回拼接文本且 err=nil，由 loop 以普通工具结果回传；
// 文本前缀 "tool error: " 便于区分成功输出。
func (c *SDKClient) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	session, err := c.sessionOrErr()
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("toolset/mcp: empty tool name")
	}

	var arguments any
	if len(args) == 0 || string(args) == "null" {
		arguments = map[string]any{}
	} else {
		arguments = args // RawMessage，SDK 再 marshal 进 JSON-RPC
	}

	res, err := session.CallTool(ctx, &sdkmcp.CallToolParams{
		Name:      name,
		Arguments: arguments,
	})
	if err != nil {
		return "", fmt.Errorf("toolset/mcp: call %q: %w", name, err)
	}
	text := contentText(res.Content)
	if res.IsError {
		if text == "" {
			text = "tool error"
		}
		return "tool error: " + text, nil
	}
	return text, nil
}

// Close 关闭会话。幂等。
func (c *SDKClient) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.session == nil {
		return nil
	}
	err := c.session.Close()
	c.session = nil
	return err
}

func (c *SDKClient) sessionOrErr() (*sdkmcp.ClientSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c == nil || c.closed || c.session == nil {
		return nil, fmt.Errorf("toolset/mcp: client closed")
	}
	return c.session, nil
}

func schemaToRaw(schema any) json.RawMessage {
	if schema == nil {
		return nil
	}
	b, err := json.Marshal(schema)
	if err != nil || len(b) == 0 || string(b) == "null" {
		return nil
	}
	return b
}

func contentText(contents []sdkmcp.Content) string {
	if len(contents) == 0 {
		return ""
	}
	var parts []string
	for _, c := range contents {
		switch v := c.(type) {
		case *sdkmcp.TextContent:
			if v != nil && v.Text != "" {
				parts = append(parts, v.Text)
			}
		default:
			if c == nil {
				continue
			}
			b, err := json.Marshal(c)
			if err == nil && len(b) > 0 && string(b) != "null" {
				parts = append(parts, string(b))
			}
		}
	}
	return strings.Join(parts, "\n")
}

var _ Client = (*SDKClient)(nil)
