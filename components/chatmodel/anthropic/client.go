package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Luo-root/pulse/components/schema"
)

// Client Anthropic API 客户端
type Client struct {
	cli     *http.Client
	baseURL string
	apiKey  string
	model   string
	config  *ChatModelConfig
}

// NewClient 创建 Anthropic 客户端
func NewClient(config *ChatModelConfig) *Client {
	cli := config.HTTPClient
	if cli == nil {
		cli = &http.Client{Timeout: config.TimeOut}
	}

	return &Client{
		cli:     cli,
		baseURL: strings.TrimRight(config.BaseURL, "/") + "/v1/messages",
		apiKey:  config.APIKey,
		model:   config.Model,
		config:  config,
	}
}

// Generate 非流式生成
func (c *Client) Generate(ctx context.Context, messages []*schema.Message) (*schema.Message, error) {
	// 转换消息格式
	systemPrompt, apiMessages := toAPIMessages(messages)

	// 构建请求
	reqBody := APIRequest{
		Model:       c.model,
		Messages:    apiMessages,
		MaxTokens:   c.config.MaxTokens,
		System:      systemPrompt,
		Temperature: c.config.Temperature,
		TopP:        c.config.TopP,
		TopK:        c.config.TopK,
		Stream:      false,
		Tools:       toAPITools(c.config.Tools),
		Thinking:    c.config.Thinking,
	}

	// 发送请求
	resp, err := c.doRequest(ctx, reqBody)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	// 解析响应
	var apiResp APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// 转换为 schema.Message
	return toSchemaMessage(&apiResp), nil
}

// Stream 流式生成
func (c *Client) Stream(ctx context.Context, messages []*schema.Message) (*schema.StreamReader, error) {
	// 转换消息格式
	systemPrompt, apiMessages := toAPIMessages(messages)

	// 构建请求
	reqBody := APIRequest{
		Model:       c.model,
		Messages:    apiMessages,
		MaxTokens:   c.config.MaxTokens,
		System:      systemPrompt,
		Temperature: c.config.Temperature,
		TopP:        c.config.TopP,
		TopK:        c.config.TopK,
		Stream:      true,
		Tools:       toAPITools(c.config.Tools),
		Thinking:    c.config.Thinking,
	}

	// 发送请求
	resp, err := c.doRequest(ctx, reqBody)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	// 启动流式读取
	return streamResponse(resp), nil
}

// doRequest 发送 HTTP 请求
func (c *Client) doRequest(ctx context.Context, body APIRequest) (*http.Response, error) {
	jsonData, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL, bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	// Anthropic 认证头
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	return c.cli.Do(req)
}

// ============================================================================
// 消息转换：schema.Message ↔ Anthropic API 格式
// ============================================================================

// toAPIMessages 将 schema.Message 列表转换为 Anthropic 格式
// 返回：(systemPrompt, apiMessages)
func toAPIMessages(messages []*schema.Message) (string, []APIMessage) {
	var systemPrompt string
	var apiMessages []APIMessage

	for _, msg := range messages {
		switch msg.Role {
		case schema.SystemRole:
			// System 消息提取为顶层参数
			if systemPrompt == "" {
				systemPrompt = msg.Content
			} else {
				systemPrompt += "\n\n" + msg.Content
			}

		case schema.UserRole:
			apiMessages = append(apiMessages, APIMessage{
				Role:    "user",
				Content: []ContentBlock{{Type: "text", Text: msg.Content}},
			})

		case schema.AssistantRole:
			var blocks []ContentBlock

			// 思考内容
			if msg.ReasoningContent != "" {
				blocks = append(blocks, ContentBlock{
					Type:     "thinking",
					Thinking: msg.ReasoningContent,
				})
			}

			// 文本内容
			if msg.Content != "" {
				blocks = append(blocks, ContentBlock{
					Type: "text",
					Text: msg.Content,
				})
			}

			// 工具调用
			for _, tc := range msg.ToolCalls {
				input := parseJSON(tc.Function.Arguments)
				blocks = append(blocks, ContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: input,
				})
			}

			if len(blocks) > 0 {
				apiMessages = append(apiMessages, APIMessage{
					Role:    "assistant",
					Content: blocks,
				})
			}

		case schema.ToolRole:
			// Anthropic 的 tool_result 要放在 user 消息里
			apiMessages = append(apiMessages, APIMessage{
				Role: "user",
				Content: []ContentBlock{
					{
						Type:      "tool_result",
						ToolUseID: msg.ToolCallID,
						Content:   msg.Content,
					},
				},
			})
		}
	}

	return systemPrompt, apiMessages
}

// toSchemaMessage 将 Anthropic 响应转换为 schema.Message
func toSchemaMessage(resp *APIResponse) *schema.Message {
	msg := &schema.Message{
		Role: schema.AssistantRole,
	}

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			msg.Content += block.Text

		case "thinking":
			msg.ReasoningContent += block.Thinking

		case "tool_use":
			argsJSON, _ := json.Marshal(block.Input)
			msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: schema.FunctionCall{
					Name:      block.Name,
					Arguments: string(argsJSON),
				},
			})
		}
	}

	// 转换 Usage
	msg.Usage = &schema.Usage{
		PromptTokens: uint64(resp.Usage.InputTokens),
		Completion:   uint64(resp.Usage.OutputTokens),
		TotalTokens:  uint64(resp.Usage.InputTokens + resp.Usage.OutputTokens),
	}

	return msg
}

// toAPITools 将 schema.Tool 转换为 Anthropic 工具格式
func toAPITools(tools []schema.Tool) []APITool {
	if len(tools) == 0 {
		return nil
	}

	result := make([]APITool, len(tools))
	for i, t := range tools {
		params, ok := t.Parameters.(map[string]any)
		if !ok {
			params = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}

		result[i] = APITool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: params,
		}
	}

	return result
}

// parseJSON 安全解析 JSON 字符串
func parseJSON(s string) map[string]any {
	if s == "" {
		return nil
	}
	var result map[string]any
	json.Unmarshal([]byte(s), &result)
	return result
}
