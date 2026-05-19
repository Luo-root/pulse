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
	Content          string            `json:"content"`
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

	return &modelResp.Choices[0].Message, nil
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
			Content:          m.Content,
			ReasoningContent: m.ReasoningContent,
			Name:             m.Name,
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
