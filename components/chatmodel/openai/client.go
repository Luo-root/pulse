package openai

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

// APIMessage 发送给 OpenAI 的消息格式
type APIMessage struct {
	Role             string            `json:"role"`
	Content          any               `json:"content"` // string 或 []schema.ContentPart
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	Name             string            `json:"name,omitempty"`
	ToolCalls        []schema.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string            `json:"tool_call_id,omitempty"`
}

// RequestBody OpenAI API 请求体
type RequestBody struct {
	Model               string             `json:"model"`
	Messages            []APIMessage       `json:"messages"`
	MaxCompletionTokens uint64             `json:"max_completion_tokens,omitempty"`
	ResponseFormat      ResponseFormatType `json:"response_format,omitempty"`
	Stream              bool               `json:"stream,omitempty"`
	StreamOptions       *StreamOptions     `json:"stream_options,omitempty"`
	Tools               []Tool             `json:"tools,omitempty"`
	PromptCacheKey      string             `json:"prompt_cache_key,omitempty"`
	SafetyIdentifier    string             `json:"safety_identifier,omitempty"`
	Thinking            Thinking           `json:"thinking,omitempty"`
	Temperature         float64            `json:"temperature,omitempty"`
	TopP                float64            `json:"top_p,omitempty"`
	N                   uint8              `json:"n,omitempty"`
	PresencePenalty     float64            `json:"presence_penalty,omitempty"`
	FrequencyPenalty    float64            `json:"frequency_penalty,omitempty"`
}

// Client OpenAI 兼容 HTTP 客户端
type Client struct {
	cli         *http.Client
	baseURL     string
	apiKey      string
	requestBody *RequestBody
}

// NewClient 创建 OpenAI 兼容客户端
func NewClient(config *ChatModelConfig) *Client {
	baseURL := strings.TrimRight(config.BaseURL, "/") + "/chat/completions"

	cli := config.HTTPClient
	if cli == nil {
		cli = &http.Client{Timeout: config.TimeOut}
	}

	return &Client{
		cli:     cli,
		baseURL: baseURL,
		apiKey:  config.APIKey,
		requestBody: &RequestBody{
			Model:               config.Model,
			MaxCompletionTokens: config.MaxCompletionTokens,
			ResponseFormat:      config.ResponseFormat,
			Tools:               WrapTools(config.Tools),
			PromptCacheKey:      config.PromptCacheKey,
			SafetyIdentifier:    config.SafetyIdentifier,
			Thinking:            config.Thinking,
			Temperature:         config.Temperature,
			TopP:                config.TopP,
			N:                   config.N,
			PresencePenalty:     config.PresencePenalty,
			FrequencyPenalty:    config.FrequencyPenalty,
		},
	}
}

// genRequest 生成 HTTP 请求
func (c *Client) genRequest() (*http.Request, error) {
	jsonData, err := json.Marshal(c.requestBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", c.baseURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	return req, nil
}

// Generate 非流式生成
func (c *Client) Generate(ctx context.Context, in []*schema.Message) (*schema.Message, error) {
	c.requestBody.Messages = toAPIMessages(in)
	c.requestBody.Stream = false

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

	var modelResp ChatModelResponse
	if err := json.Unmarshal(body, &modelResp); err != nil {
		return nil, err
	}

	if len(modelResp.Choices) == 0 {
		return nil, fmt.Errorf("empty choices in response, body: %s", string(body))
	}

	msg := modelResp.Choices[0].Message
	msg.Usage = &modelResp.Usage
	return &msg, nil
}

// Stream 流式生成
func (c *Client) Stream(ctx context.Context, in []*schema.Message) (*schema.StreamReader, error) {
	c.requestBody.Messages = toAPIMessages(in)
	c.requestBody.Stream = true

	if c.requestBody.StreamOptions == nil {
		c.requestBody.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

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
		resp.Body.Close()
		return nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(body))
	}

	return streamResponse(resp), nil
}

// toAPIMessages 将 schema.Message 列表转换为 OpenAI API 格式
func toAPIMessages(messages []*schema.Message) []APIMessage {
	result := make([]APIMessage, len(messages))
	for i, m := range messages {
		am := APIMessage{
			Role:             string(m.Role),
			ReasoningContent: m.ReasoningContent,
			Name:             m.Name,
		}

		// 多模态内容优先：ContentParts 非空时转为 content 数组
		if len(m.ContentParts) > 0 {
			am.Content = convertContentParts(m.ContentParts)
		} else {
			am.Content = m.Content
		}

		if m.Role == schema.AssistantRole && len(m.ToolCalls) > 0 {
			am.ToolCalls = m.ToolCalls
		}

		if m.Role == schema.ToolRole {
			am.ToolCallID = m.ToolCallID
		}

		result[i] = am
	}
	return result
}

// convertContentParts 将 schema.ContentPart 列表转换为 OpenAI content 数组格式
func convertContentParts(parts []schema.ContentPart) []map[string]any {
	result := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case schema.ContentTypeText:
			result = append(result, map[string]any{
				"type": "text",
				"text": p.Text,
			})

		case schema.ContentTypeImageURL:
			if p.ImageURL != nil {
				result = append(result, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": p.ImageURL.URL, "detail": p.ImageURL.Detail},
				})
			}

		case schema.ContentTypeInputAudio:
			if p.InputAudio != nil {
				result = append(result, map[string]any{
					"type":        "input_audio",
					"input_audio": map[string]any{"data": p.InputAudio.Data, "format": p.InputAudio.Format},
				})
			}

		case schema.ContentTypeVideoURL:
			if p.VideoURL != nil {
				result = append(result, map[string]any{
					"type":      "video_url",
					"video_url": map[string]any{"url": p.VideoURL.URL},
				})
			}

		case schema.ContentTypeFileURL:
			if p.FileURL != nil {
				result = append(result, map[string]any{
					"type":     "file_url",
					"file_url": map[string]any{"url": p.FileURL.URL},
				})
			}

		case schema.ContentTypeInlineData:
			if p.InlineData != nil {
				result = append(result, map[string]any{
					"type":        "input_audio",
					"input_audio": map[string]any{"data": p.InlineData.Data, "format": p.InlineData.MediaType},
				})
			}
		}
	}
	return result
}
