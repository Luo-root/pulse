package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Luo-root/pulse/components/schema"
)

// ============================================================================
// Client — HTTP 通信核心
// ============================================================================

// Client Anthropic Messages API 客户端
type Client struct {
	cli         *http.Client
	Header      *Header
	RequestBody *RequestBody
}

// Header 请求头配置
type Header struct {
	BaseURL string
	APIKey  string
}

// ============================================================================
// 消息格式转换：schema.Message → Anthropic ContentBlock 格式
// ============================================================================

func extractSystemMessage(messages []*schema.Message) string {
	for _, m := range messages {
		if m.Role == schema.SystemRole {
			return m.Content
		}
	}
	return ""
}

func toContentBlocks(msg *schema.Message) []ContentBlock {
	var blocks []ContentBlock

	if msg.Content != "" {
		blocks = append(blocks, ContentBlock{
			Type: "text",
			Text: msg.Content,
		})
	}

	if msg.ReasoningContent != "" {
		blocks = append(blocks, ContentBlock{
			Type:    "thinking",
			Content: msg.ReasoningContent,
		})
	}

	for _, tc := range msg.ToolCalls {
		var input map[string]interface{}
		if tc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
		}
		if input == nil {
			input = make(map[string]interface{})
		}
		blocks = append(blocks, ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
	}

	for _, tr := range msg.ToolResults {
		blocks = append(blocks, ContentBlock{
			Type:      "tool_result",
			ToolUseID: tr.CallID,
			Content:   tr.Content,
			IsError:   tr.IsError,
		})
	}

	return blocks
}

func toUserMessages(messages []*schema.Message) []ContentBlockMessage {
	var result []ContentBlockMessage
	for _, m := range messages {
		switch m.Role {
		case schema.UserRole:
			result = append(result, ContentBlockMessage{
				Role:    "user",
				Content: toContentBlocks(m),
			})
		case schema.AssistantRole:
			result = append(result, ContentBlockMessage{
				Role:    "assistant",
				Content: toContentBlocks(m),
			})
		case schema.ToolRole:
			result = append(result, ContentBlockMessage{
				Role:    "user",
				Content: toContentBlocks(m),
			})
		}
	}
	return result
}

// ============================================================================
// API 消息结构
// ============================================================================

type ContentBlockMessage struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// ============================================================================
// 请求体
// ============================================================================

type RequestBody struct {
	Model         string                `json:"model"`
	MaxTokens     uint64                `json:"max_tokens"`
	Messages      []ContentBlockMessage `json:"messages"`
	System        string                `json:"system,omitempty"`
	StopSequences []string              `json:"stop_sequences,omitempty"`
	Stream        bool                  `json:"stream,omitempty"`
	Temperature   float64               `json:"temperature,omitempty"`
	TopP          float64               `json:"top_p,omitempty"`
	TopK          int                   `json:"top_k,omitempty"`
	Tools         []ClaudeTool          `json:"tools,omitempty"`
	ToolChoice    *ClaudeToolChoice     `json:"tool_choice,omitempty"`
	Thinking      *ClaudeThinkingConfig `json:"thinking,omitempty"`
	OutputConfig  *ClaudeOutputConfig   `json:"output_config,omitempty"`
}

// ============================================================================
// NewClient 创建客户端
// ============================================================================

func NewClient(ctx context.Context, config *ChatModelConfig) *Client {
	baseURL := strings.TrimRight(config.BaseURL, "/") + "/v1/messages"

	header := &Header{
		BaseURL: baseURL,
		APIKey:  config.APIKey,
	}

	var stopSeqs []string
	if config.Stop != "" {
		stopSeqs = []string{config.Stop}
	}

	reqBody := &RequestBody{
		Model:         config.Model,
		MaxTokens:     config.MaxTokens,
		Messages:      toUserMessages(config.Messages),
		System:        config.System,
		StopSequences: stopSeqs,
		Stream:        config.Stream,
		Temperature:   config.Temperature,
		TopP:          config.TopP,
		TopK:          config.TopK,
		Tools:         config.Tools,
		ToolChoice:    config.ToolChoice,
		Thinking:      config.Thinking,
		OutputConfig:  config.OutputConfig,
	}

	return &Client{
		cli:         config.HTTPClient,
		Header:      header,
		RequestBody: reqBody,
	}
}

// ============================================================================
// genRequest 构建 HTTP 请求
// ============================================================================

func (c *Client) genRequest() (*http.Request, error) {
	jsonData, err := json.Marshal(c.RequestBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", c.Header.BaseURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.Header.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	return req, nil
}

// ============================================================================
// Generate 非流式生成
// ============================================================================

func contentBlocksToMessage(resp *ClaudeMessageResponse) *schema.Message {
	msg := &schema.Message{
		Role: schema.AssistantRole,
	}

	var textParts []string
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "thinking":
			if thinkingStr, ok := block.Content.(string); ok {
				msg.ReasoningContent = thinkingStr
			}
		case "tool_use":
			inputJSON := ""
			if block.Input != nil {
				b, _ := json.Marshal(block.Input)
				inputJSON = string(b)
			}
			msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: schema.FunctionCall{
					Name:      block.Name,
					Arguments: inputJSON,
				},
			})
		}
	}
	msg.Content = strings.Join(textParts, "")

	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		msg.Usage = &schema.Usage{
			PromptTokens: resp.Usage.InputTokens,
			Completion:   resp.Usage.OutputTokens,
			TotalTokens:  resp.Usage.InputTokens + resp.Usage.OutputTokens,
		}
	}

	return msg
}

func (c *Client) Generate(ctx context.Context, in []*schema.Message) (*schema.Message, error) {
	c.RequestBody.Messages = toUserMessages(in)
	c.RequestBody.System = extractSystemMessage(in)
	c.RequestBody.Stream = false

	var modelResp ClaudeMessageResponse
	req, err := c.genRequest()
	if err != nil {
		return nil, err
	}

	req = req.WithContext(ctx)

	resp, err := c.cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	err = json.Unmarshal(body, &modelResp)
	if err != nil {
		return nil, err
	}

	return contentBlocksToMessage(&modelResp), nil
}

// ============================================================================
// Stream 流式生成 — Anthropic SSE 协议
// ============================================================================

type streamEvent struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Role  string `json:"role"`
	} `json:"message,omitempty"`
	Index        *int          `json:"index,omitempty"`
	ContentBlock *ContentBlock `json:"content_block,omitempty"`
	Delta        *ContentDelta `json:"delta,omitempty"`
	Usage        *ClaudeUsage  `json:"usage,omitempty"`
}

type ContentDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

func (c *Client) Stream(ctx context.Context, in []*schema.Message) (*schema.StreamReader, error) {
	c.RequestBody.Messages = toUserMessages(in)
	c.RequestBody.System = extractSystemMessage(in)
	c.RequestBody.Stream = true

	req, err := c.genRequest()
	if err != nil {
		return nil, err
	}

	req = req.WithContext(ctx)

	resp, err := c.cli.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(body))
	}

	reader := anthropicStreamReception(resp)
	return reader, nil
}

// ============================================================================
// anthropicStreamReception Anthropic SSE 流式解析器
// ============================================================================

func anthropicStreamReception(resp *http.Response) *schema.StreamReader {
	reader := schema.NewStreamReader()

	go func() {
		defer func() {
			_ = resp.Body.Close()
			reader.Close()
		}()

		scanner := bufio.NewScanner(resp.Body)
		const maxBufferSize = 1 << 20
		scanner.Buffer(make([]byte, maxBufferSize), maxBufferSize)

		var (
			currentEvent string
			dataBuffer   strings.Builder
			msg          schema.Message
			toolCallIdx  = -1
		)

		sendChunk := func() {
			reader.StreamChan <- msg.Clone()
		}

		processEvent := func(eventType, data string) {
			if data == "" {
				return
			}
			var evt streamEvent
			if err := json.Unmarshal([]byte(data), &evt); err != nil {
				return
			}

			switch evt.Type {
			case "message_start":
				msg.Role = schema.AssistantRole

			case "content_block_start":
				if evt.ContentBlock != nil {
					block := evt.ContentBlock
					switch block.Type {
					case "text":
						// 文本块开始，不推送空 chunk
					case "thinking":
						// 思考块开始，不推送空 chunk
					case "tool_use":
						toolCallIdx = len(msg.ToolCalls)
						msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{
							ID:   block.ID,
							Type: "function",
							Function: schema.FunctionCall{
								Name:      block.Name,
								Arguments: "", // 由 input_json_delta 从头累加
							},
						})
					}
				}

			case "content_block_delta":
				if evt.Delta != nil {
					delta := evt.Delta
					switch delta.Type {
					case "text_delta":
						msg.Content = delta.Text
						sendChunk()
					case "thinking_delta":
						msg.ReasoningContent = delta.Thinking
						sendChunk()
					case "input_json_delta":
						if toolCallIdx >= 0 && toolCallIdx < len(msg.ToolCalls) {
							msg.ToolCalls[toolCallIdx].Function.Arguments += delta.PartialJSON
							sendChunk()
						}
					}
				}

			case "content_block_stop":
				sendChunk()
				toolCallIdx = -1

			case "message_delta":
				if evt.Usage != nil {
					msg.Usage = &schema.Usage{
						PromptTokens: evt.Usage.InputTokens,
						Completion:   evt.Usage.OutputTokens,
						TotalTokens:  evt.Usage.InputTokens + evt.Usage.OutputTokens,
					}
				}

			case "message_stop":
				sendChunk()

			case "ping":
				// 心跳忽略
			}
		}

		for scanner.Scan() {
			line := scanner.Text()

			if strings.HasPrefix(line, "event: ") {
				currentEvent = strings.TrimPrefix(line, "event: ")
				continue
			}

			if strings.HasPrefix(line, "data: ") {
				data := strings.TrimPrefix(line, "data: ")
				dataBuffer.WriteString(data)
				continue
			}

			if line == "" && dataBuffer.Len() > 0 {
				processEvent(currentEvent, dataBuffer.String())
				currentEvent = ""
				dataBuffer.Reset()
			}
		}

		if dataBuffer.Len() > 0 {
			processEvent(currentEvent, dataBuffer.String())
		}
	}()

	return reader
}
