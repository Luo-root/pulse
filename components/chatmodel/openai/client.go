package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

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
	cli     *http.Client
	baseURL string
	apiKey  string
	config  *ChatModelConfig
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
		config:  config,
	}
}

// buildRequestBody 根据配置和消息构建请求体（每次调用独立，无竞争）
func (c *Client) buildRequestBody(messages []APIMessage, stream bool) *RequestBody {
	return &RequestBody{
		Model:               c.config.Model,
		Messages:            messages,
		MaxCompletionTokens: c.config.MaxCompletionTokens,
		ResponseFormat:      c.config.ResponseFormat,
		Stream:              stream,
		StreamOptions:       &StreamOptions{IncludeUsage: true},
		Tools:               WrapTools(c.config.Tools),
		PromptCacheKey:      c.config.PromptCacheKey,
		SafetyIdentifier:    c.config.SafetyIdentifier,
		Thinking:            c.config.Thinking,
		Temperature:         c.config.Temperature,
		TopP:                c.config.TopP,
		N:                   c.config.N,
		PresencePenalty:     c.config.PresencePenalty,
		FrequencyPenalty:    c.config.FrequencyPenalty,
	}
}

// genRequest 生成 HTTP 请求
func (c *Client) genRequest(body *RequestBody) (*http.Request, error) {
	if c.config.Debug {
		c.debugLogRequest(body)
	}

	jsonData, err := json.Marshal(body)
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

// debugLogRequest 输出请求摘要到日志文件
func (c *Client) debugLogRequest(body *RequestBody) {
	logFile, err := os.OpenFile("pulse-debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer logFile.Close()

	fmt.Fprintf(logFile, "\n[%s] OpenAI Request ===\n", time.Now().Format("2006-01-02 15:04:05"))
	fmt.Fprintf(logFile, "  URL:   %s\n", c.baseURL)
	fmt.Fprintf(logFile, "  Model: %s\n", body.Model)
	fmt.Fprintf(logFile, "  Stream: %v\n", body.Stream)
	fmt.Fprintf(logFile, "  Messages: %d\n", len(body.Messages))
	for i, msg := range body.Messages {
		switch content := msg.Content.(type) {
		case string:
			preview := content
			if len(preview) > 120 {
				preview = preview[:120] + "..."
			}
			fmt.Fprintf(logFile, "    [%d] %s: %s\n", i, msg.Role, preview)
		case []map[string]any:
			fmt.Fprintf(logFile, "    [%d] %s: %d content parts\n", i, msg.Role, len(content))
			for j, part := range content {
				typ, _ := part["type"].(string)
				switch typ {
				case "text":
					text, _ := part["text"].(string)
					if len(text) > 100 {
						text = text[:100] + "..."
					}
					fmt.Fprintf(logFile, "      part[%d] text: %s\n", j, text)
				case "image_url":
					url, _ := part["image_url"].(map[string]any)
					urlStr, _ := url["url"].(string)
					if len(urlStr) > 80 {
						urlStr = urlStr[:80] + "..."
					}
					detail, _ := url["detail"].(string)
					fmt.Fprintf(logFile, "      part[%d] image_url: %s (detail=%q)\n", j, urlStr, detail)
				case "input_audio":
					audio, _ := part["input_audio"].(map[string]any)
					format, _ := audio["format"].(string)
					data, _ := audio["data"].(string)
					fmt.Fprintf(logFile, "      part[%d] input_audio: format=%s, data=%d bytes\n", j, format, len(data))
				default:
					fmt.Fprintf(logFile, "      part[%d] %s\n", j, typ)
				}
			}
		}
	}
	if len(body.Tools) > 0 {
		fmt.Fprintf(logFile, "  Tools: %d\n", len(body.Tools))
	}
	fmt.Fprintf(logFile, "=== End Request\n\n")
}

// Generate 非流式生成
func (c *Client) Generate(ctx context.Context, in []*schema.Message) (*schema.Message, error) {
	body := c.buildRequestBody(toAPIMessages(in), false)

	req, err := c.genRequest(body)
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

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var modelResp ChatModelResponse
	if err := json.Unmarshal(respBody, &modelResp); err != nil {
		return nil, err
	}

	if len(modelResp.Choices) == 0 {
		return nil, fmt.Errorf("empty choices in response, body: %s", string(respBody))
	}

	msg := modelResp.Choices[0].Message
	msg.Usage = &modelResp.Usage
	return &msg, nil
}

// Stream 流式生成
func (c *Client) Stream(ctx context.Context, in []*schema.Message) (*schema.StreamReader, error) {
	body := c.buildRequestBody(toAPIMessages(in), true)

	req, err := c.genRequest(body)
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
				imgObj := map[string]any{"url": p.ImageURL.URL}
				if p.ImageURL.Detail != "" {
					imgObj["detail"] = p.ImageURL.Detail
				}
				result = append(result, map[string]any{
					"type":      "image_url",
					"image_url": imgObj,
				})
			}

		case schema.ContentTypeInputAudio:
			if p.InputAudio != nil {
				result = append(result, map[string]any{
					"type":        "input_audio",
					"input_audio": map[string]any{"data": p.InputAudio.Data, "format": p.InputAudio.Format},
				})
			}

		case schema.ContentTypeInlineData:
			if p.InlineData != nil {
				// 根据 MIME 类型映射到 OpenAI 支持的格式
				switch {
				case strings.HasPrefix(p.InlineData.MediaType, "image/"):
					// 图片 → image_url with data URI
					dataURI := "data:" + p.InlineData.MediaType + ";base64," + p.InlineData.Data
					result = append(result, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": dataURI},
					})
				case strings.HasPrefix(p.InlineData.MediaType, "audio/"):
					// 音频 → input_audio
					format := "mp3"
					if strings.Contains(p.InlineData.MediaType, "wav") {
						format = "wav"
					} else if strings.Contains(p.InlineData.MediaType, "ogg") {
						format = "ogg"
					}
					result = append(result, map[string]any{
						"type":        "input_audio",
						"input_audio": map[string]any{"data": p.InlineData.Data, "format": format},
					})
				default:
					// 其他类型 → 降级为文本描述
					result = append(result, map[string]any{
						"type": "text",
						"text": fmt.Sprintf("[附件: %s (%s)]", p.InlineData.MediaType, formatBytes(len(p.InlineData.Data))),
					})
				}
			}

		// video_url 和 file_url 不是 OpenAI 标准类型，降级为文本描述
		case schema.ContentTypeVideoURL:
			if p.VideoURL != nil {
				result = append(result, map[string]any{
					"type": "text",
					"text": fmt.Sprintf("[视频: %s]", p.VideoURL.URL),
				})
			}

		case schema.ContentTypeFileURL:
			if p.FileURL != nil {
				result = append(result, map[string]any{
					"type": "text",
					"text": fmt.Sprintf("[文件: %s]", p.FileURL.URL),
				})
			}
		}
	}
	return result
}

// formatBytes 格式化字节数
func formatBytes(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
