package gemini

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Luo-root/pulse/components/schema"
)

// TODO: 适配gemini

type RequestBody struct {
	Contents []Content    `json:"contents"`
	Tools    []GeminiTool `json:"tools,omitempty"`
}

type Content struct {
	Role  string `json:"role"`
	Parts []Part `json:"parts"`
}

type Part struct {
	Text         string        `json:"text,omitempty"`
	FunctionCall *FunctionCall `json:"functionCall,omitempty"` // ← 注意大小写
}

type FunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"` // 直接是对象
}

type GeminiTool struct {
	FunctionDeclarations []schema.Tool `json:"functionDeclarations"`
}

// 响应
type GeminiResponse struct {
	Candidates []struct {
		Content Content `json:"content"`
	} `json:"candidates"`
}

type Client struct {
	apiKey  string
	model   string
	baseURL string
}

func (c *Client) Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	// ... 发送请求 ...

	var resp GeminiResponse
	// decode ...

	return c.toSchemaMessage(resp), nil
}

// ====== 核心适配：Gemini → schema.Message ======

func (c *Client) toSchemaMessage(resp GeminiResponse) *schema.Message {
	if len(resp.Candidates) == 0 {
		return nil
	}

	content := resp.Candidates[0].Content
	msg := &schema.Message{
		Role: schema.AssistantRole,
	}

	var toolCalls []schema.ToolCall
	var texts []string

	for _, part := range content.Parts {
		if part.Text != "" {
			texts = append(texts, part.Text)
		}
		if part.FunctionCall != nil {
			argsJSON, _ := json.Marshal(part.FunctionCall.Args)
			// Gemini 没有 call ID，需要自己生成
			toolCalls = append(toolCalls, schema.ToolCall{
				ID:   fmt.Sprintf("gemini-%s-%d", part.FunctionCall.Name, len(toolCalls)),
				Type: "function",
				Function: schema.FunctionCall{
					Name:      part.FunctionCall.Name,
					Arguments: string(argsJSON),
				},
			})
		}
	}

	// msg.Content = joinStrings(texts)
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	return msg
}
