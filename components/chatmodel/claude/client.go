package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/Luo-root/pulse/components/schema"
)

// TODO: 适配claude

// Claude 请求体
type RequestBody struct {
	Model     string          `json:"model"`
	Messages  []ClaudeMessage `json:"messages"`
	MaxTokens uint64          `json:"max_tokens,omitempty"`
	Tools     []ClaudeTool    `json:"tools,omitempty"`
	Stream    bool            `json:"stream,omitempty"`
	// ... 其他参数
}

type ClaudeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string 或 []ContentBlock
}

type ContentBlock struct {
	Type  string `json:"type"` // "text" | "tool_use" | "tool_result"
	Text  string `json:"text,omitempty"`
	ID    string `json:"id,omitempty"`    // tool_use 时有
	Name  string `json:"name,omitempty"`  // tool_use 时有
	Input any    `json:"input,omitempty"` // tool_use 时有，直接是对象
}

type ClaudeTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"input_schema"`
}

// Claude 响应
type ClaudeResponse struct {
	Content []ContentBlock `json:"content"`
	Usage   schema.Usage   `json:"usage"`
}

type Client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	model      string
	tools      []schema.Tool
}

func NewClient(apiKey, model string, tools []schema.Tool) *Client {
	// 转换 tool 格式
	claudeTools := make([]ClaudeTool, len(tools))
	for i, t := range tools {
		claudeTools[i] = ClaudeTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		}
	}
	return &Client{
		apiKey:  apiKey,
		baseURL: "https://api.anthropic.com/v1/messages",
		model:   model,
		tools:   tools,
	}
}

func (c *Client) Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	// 1. 转换 schema.Message → ClaudeMessage
	claudeMsgs := make([]ClaudeMessage, len(input))
	for i, m := range input {
		claudeMsgs[i] = c.toClaudeMessage(m)
	}

	reqBody := RequestBody{
		Model:     c.model,
		Messages:  claudeMsgs,
		MaxTokens: 4096,
		Tools:     c.claudeTools(),
		Stream:    false,
	}

	jsonData, _ := json.Marshal(reqBody)
	req, _ := http.NewRequestWithContext(ctx, "POST", c.baseURL, bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("claude API error: %d %s", resp.StatusCode, body)
	}

	var claudeResp ClaudeResponse
	if err := json.NewDecoder(resp.Body).Decode(&claudeResp); err != nil {
		return nil, err
	}

	// 2. 转换 ClaudeResponse → schema.Message
	return c.toSchemaMessage(claudeResp), nil
}

// ====== 核心适配：Claude → schema.Message ======

func (c *Client) toSchemaMessage(resp ClaudeResponse) *schema.Message {
	msg := &schema.Message{
		Role: schema.AssistantRole,
	}

	var toolCalls []schema.ToolCall
	var textContents []string

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			textContents = append(textContents, block.Text)
		case "tool_use":
			// Claude 的 input 是对象，需要 marshal 成字符串
			argsJSON, _ := json.Marshal(block.Input)
			toolCalls = append(toolCalls, schema.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: schema.FunctionCall{
					Name:      block.Name,
					Arguments: string(argsJSON),
				},
			})
		}
	}

	msg.Content = joinStrings(textContents)
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	return msg
}

// schema.Message → ClaudeMessage（回传工具结果时用）
func (c *Client) toClaudeMessage(m *schema.Message) ClaudeMessage {
	// tool 角色的消息要转成 content block
	if m.Role == schema.ToolRole {
		return ClaudeMessage{
			Role: "user",
			Content: []ContentBlock{{
				Type: "tool_result",
				ID:   m.Name, // 用 Name 存 call_id
				Text: m.Content,
			}},
		}
	}

	// assistant 有 tool_calls 的情况
	if len(m.ToolCalls) > 0 {
		blocks := make([]ContentBlock, 0, len(m.ToolCalls)+1)
		if m.Content != "" {
			blocks = append(blocks, ContentBlock{Type: "text", Text: m.Content})
		}
		for _, tc := range m.ToolCalls {
			var input map[string]any
			json.Unmarshal([]byte(tc.Function.Arguments), &input)
			blocks = append(blocks, ContentBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: input,
			})
		}
		return ClaudeMessage{Role: string(m.Role), Content: blocks}
	}

	return ClaudeMessage{Role: string(m.Role), Content: m.Content}
}

func (c *Client) claudeTools() []ClaudeTool {
	// 转换 schema.Tool → ClaudeTool
	// ...
	return nil
}

func joinStrings(strs []string) string {
	// strings.Join
	return ""
}
