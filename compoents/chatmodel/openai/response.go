package openai

import "github.com/Luo-root/pulse/compoents/schema"

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
