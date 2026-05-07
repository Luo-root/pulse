package openai

import (
	"github.com/Luo-root/pulse/components/schema"
)

type ChatModelResponse struct {
	ID      string       `json:"id"`      // 对话ID，随便看看
	Object  string       `json:"object"`  // 固定是 chat.completion
	Created int64        `json:"created"` // 时间戳
	Model   string       `json:"model"`   // 模型名
	Choices []Choice     `json:"choices"` // ✅【最重要】AI 的回答
	Usage   schema.Usage `json:"usage"`   // token 消耗
}

type Choice struct {
	Index        int            `json:"index"`         // 一般是 0
	Message      schema.Message `json:"message"`       // 完整回答
	FinishReason string         `json:"finish_reason"` // 结束原因 stop / length
}

// StreamResponse 流式响应最外层
type StreamResponse struct {
	Choices []StreamChoice `json:"choices"`
}

type StreamChoice struct {
	Index        int           `json:"index"`
	Delta        Delta         `json:"delta"`
	FinishReason string        `json:"finish_reason"`
	Usage        *schema.Usage `json:"usage,omitempty"`
}

type Delta struct {
	Role             string            `json:"role,omitempty"`
	Content          string            `json:"content"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []schema.ToolCall `json:"tool_calls,omitempty"`
}
