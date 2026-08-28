package mcp

import (
	"context"
	"encoding/json"

	"github.com/Luo-root/pulse/llm"
)

// Tool 是 Client 列出的一条上游工具（尚未定最终模型可见名）。
type Tool struct {
	// Name 是上游原名（MCP tool name）。Source 在 Register 前按
	// Config.NamePrefix 拼出 llm.ToolDef.Name，禁止冲突后改名。
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema；nil = 无参
}

// Client 是 MCP 会话的最小消费面（对标 llm.Factory 的可替换缝）。
//
// 实现可来自 mock、官方 go-sdk、mcp-go 等；Source 只依赖本接口。
type Client interface {
	// ListTools 返回当前会话可用工具。应尊重 ctx 取消。
	ListTools(ctx context.Context) ([]Tool, error)
	// CallTool 执行上游工具。args 为模型给出的 JSON；应尊重 ctx 取消。
	CallTool(ctx context.Context, name string, args json.RawMessage) (string, error)
	// Close 释放会话。幂等。
	Close() error
}

// DefFromTool 把上游 Tool 转成 llm.ToolDef（Name 已是最终模型可见名）。
func DefFromTool(name string, t Tool) llm.ToolDef {
	return llm.ToolDef{
		Name:        name,
		Description: t.Description,
		Parameters:  t.Parameters,
	}
}
