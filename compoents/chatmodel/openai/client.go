package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"pulse/compoents/schema"

	"net/http"
)

type Client struct {
	cli         *http.Client
	Header      *Header
	RequestBody *RequestBody
}

type Header struct {
	BaseUrl string
	APIKey  string
}

type RequestBody struct {
	Model    string            `json:"model"`
	Messages []*schema.Message `json:"messages"`
	// 聊天补全生成的最大 Token 数量。如果不给的话，默认给一个不错的整数比如 1024。
	//如果结果达到最大 Token 数而未结束，finish reason 将为 "length"；否则为 "stop"。
	//此值为期望返回的 Token 长度，而非输入加输出的总长度。如果输入加 max_completion_tokens 超出模型上下文窗口，将返回 invalid_request_error。
	MaxCompletionTokens uint64 `json:"max_completion_tokens,omitempty"`
	// 设置为 {"type": "json_object"} 可启用 JSON 模式，确保生成的内容为有效 JSON。设置后，需在 prompt 中明确引导模型输出 JSON 格式并指定具体格式，否则可能产生意外结果。默认值为 {"type": "text"}。
	ResponseFormat ResponseFormatType `json:"response_format,omitempty"`
	// 是否以流式方式返回响应，默认 false
	Stream bool `json:"stream,omitempty"`
	// 模型可调用的工具列表, 最大长度 128
	Tools []Tool `json:"tools,omitempty"`
	// 用于缓存相似请求的响应以优化缓存命中率。给长系统提示词 / 长记忆做缓存,让速度变快、省钱
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	// 用于检测可能违反使用政策的用户的稳定标识符。应为唯一标识每个用户的字符串。建议对用户名或邮箱进行哈希处理以避免发送可识别信息
	SafetyIdentifier string `json:"safety_identifier,omitempty"`
	// 采样温度，范围 0 到 1。较高的值（如 0.7）使输出更随机，较低的值（如 0.2）使输出更集中和确定。默认值为 0.6。
	Temperature float64 `json:"temperature,omitempty"`
	// 另一种采样方法，模型考虑累积概率质量为 top_p 的 Token 结果。例如 0.1 表示仅考虑概率质量前 10% 的 Token。通常建议只修改此参数或 temperature 其中之一。默认值为 1.0。
	// 0 <= x <= 1
	TopP float64 `json:"top_p,omitempty"`
	// 每条输入消息生成的结果数量。默认为 1，不超过 5。当温度非常接近 0 时，只能返回 1 个结果。
	// 1 <= x <= 5
	N uint8 `json:"n,omitempty"`
	// 存在惩罚，范围 -2.0 到 2.0。正值会根据 Token 是否出现在文本中进行惩罚，增加模型讨论新话题的可能性
	// -2 <= x <= 2
	PresencePenalty float64 `json:"presence_penalty,omitempty"`
	// 频率惩罚，范围 -2.0 到 2.0。正值会根据 Token 在文本中的现有频率进行惩罚，降低模型逐字重复相同短语的可能性
	// -2 <= x <= 2
	FrequencyPenalty float64 `json:"frequency_penalty,omitempty"`
}

func NewClient(ctx context.Context, config *ChatModelConfig) *Client {
	header := &Header{
		BaseUrl: config.BaseUrl,
		APIKey:  config.APIKey,
	}

	reqBode := &RequestBody{
		Model:               config.Model,
		Messages:            config.Messages,
		MaxCompletionTokens: config.MaxCompletionTokens,
		ResponseFormat:      config.ResponseFormat,
		Stream:              config.Stream,
		Tools:               config.Tools,
		PromptCacheKey:      config.PromptCacheKey,
		SafetyIdentifier:    config.SafetyIdentifier,
		Temperature:         config.Temperature,
		TopP:                config.TopP,
		N:                   config.N,
		PresencePenalty:     config.PresencePenalty,
		FrequencyPenalty:    config.FrequencyPenalty,
	}

	cli := &Client{
		cli:         config.HTTPClient,
		Header:      header,
		RequestBody: reqBode,
	}
	return cli
}

func (c *Client) genRequest() (*http.Request, error) {
	jsonData, err := json.Marshal(c.RequestBody)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", c.Header.BaseUrl, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Header.APIKey)

	return req, nil
}

func (c *Client) Generate(ctx context.Context, in []*schema.Message) (*schema.Message, error) {
	c.RequestBody.Messages = in
	c.RequestBody.Stream = false

	var modelResp ChatModelResponse
	req, err := c.genRequest()
	if err != nil {
		return nil, err
	}

	req.WithContext(ctx)

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

	if len(modelResp.Choices) == 0 {
		return nil, fmt.Errorf("empty choices in response, body: %s", string(body))
	}

	return &modelResp.Choices[0].Message, nil
}

func (c *Client) Stream(ctx context.Context, in []*schema.Message) (*schema.StreamReader, error) {
	c.RequestBody.Messages = in
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

	reader, err := schema.StreamReception(resp)
	if err != nil {
		return nil, err
	}

	return reader, nil
}
