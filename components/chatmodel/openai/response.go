package openai

import "github.com/Luo-root/pulse/components/schema"

// ChatModelResponse OpenAI 非流式响应
type ChatModelResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []Choice     `json:"choices"`
	Usage   schema.Usage `json:"usage"`
}

// Choice 响应选项
type Choice struct {
	Index        int            `json:"index"`
	Message      schema.Message `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

// StreamResponse 流式响应
type StreamResponse struct {
	Choices []StreamChoice `json:"choices"`
}

// StreamChoice 流式选项
type StreamChoice struct {
	Index        int           `json:"index"`
	Delta        Delta         `json:"delta"`
	FinishReason string        `json:"finish_reason"`
	Usage        *schema.Usage `json:"usage,omitempty"`
}

// Delta 流式增量
type Delta struct {
	Role             string            `json:"role,omitempty"`
	Content          string            `json:"content"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ToolCalls        []schema.ToolCall `json:"tool_calls,omitempty"`
}
