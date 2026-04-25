package openai

import (
	"context"
	"fmt"

	"net/http"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

type ResponseFormatType string

const (
	Text       ResponseFormatType = "text"
	JsonObject ResponseFormatType = "json_object"
)

type ChatModelConfig struct {
	BaseUrl  string
	APIKey   string
	Model    string            `json:"model"`
	Messages []*schema.Message `json:"messages"`
	// 聊天补全生成的最大 Token 数量。如果不给的话，默认给一个不错的整数比如 1024。
	//如果结果达到最大 Token 数而未结束，finish reason 将为 "length"；否则为 "stop"。
	//此值为期望返回的 Token 长度，而非输入加输出的总长度。如果输入加 max_completion_tokens 超出模型上下文窗口，将返回 invalid_request_error。
	MaxCompletionTokens uint64 `json:"max_completion_tokens,omitempty"`
	// 设置为 {"type": "json_object"} 可启用 JSON 模式，确保生成的内容为有效 JSON。设置后，需在 prompt 中明确引导模型输出 JSON 格式并指定具体格式，否则可能产生意外结果。默认值为 {"type": "text"}。
	ResponseFormat ResponseFormatType `json:"response_format,omitempty"`
	// 停用词，完全匹配时将停止输出。匹配到的词本身不会被输出。最多允许 5 个字符串，每个不超过 32 字节
	Stop string `json:"stop,omitempty"`
	// 是否以流式方式返回响应，默认 false
	Stream bool `json:"stream,omitempty"`
	// 模型可调用的工具列表, 最大长度 128
	Tools []schema.Tool `json:"-"`
	// 用于缓存相似请求的响应以优化缓存命中率。给长系统提示词 / 长记忆做缓存,让速度变快、省钱
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	// 用于检测可能违反使用政策的用户的稳定标识符。应为唯一标识每个用户的字符串。建议对用户名或邮箱进行哈希处理以避免发送可识别信息
	SafetyIdentifier string `json:"safety_identifier,omitempty"`
	// 控制 kimi-k2.6 模型是否启用思考能力, 以及是否完整保留多轮对话中的 reasoning_content
	Thinking Thinking `json:"thinking,omitempty"`
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

	TimeOut    time.Duration
	HTTPClient *http.Client `json:"http_client"`
}

type ToolType string

const (
	Function ToolType = "function"
)

type Tool struct {
	Type     ToolType     `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	// 函数名称。必须符合正则表达式：^[a-zA-Z_][a-zA-Z0-9-_]{2,63}$
	Name string `json:"name"`
	// 函数参数，JSON Schema 格式
	Parameters any `json:"parameters"`
	// 函数功能描述
	Description string `json:"description"`
}

type ThinkingType string

const (
	Enabled  ThinkingType = "enabled"
	Disabled ThinkingType = "disabled"
)

type ThinkingKeepEnum string

const (
	Null ThinkingType = "null"
	All  ThinkingType = "all"
)

type Thinking struct {
	// 启用或禁用思考能力
	Type ThinkingType `json:"type,omitempty"`
	// 控制是否保留历史对话轮次（previous turns）的 reasoning_content
	// 默认为 null: 服务端会忽略历史 turns 的 reasoning_content。
	// "all"：保留历史 turns 的 reasoning_content 并随上下文一同提供给模型，启用 Preserved Thinking。使用时需把每一轮历史 assistant 消息中的 reasoning_content 原样保留在 messages 中。
	Keep ThinkingKeepEnum `json:"keep,omitempty"`
}

func SchemaToOpenAI(tools []schema.Tool) []Tool {
	result := make([]Tool, len(tools))
	for i, t := range tools {
		result[i] = Tool{
			Type: Function, // "function"
			Function: ToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		}
	}
	return result
}

type ChatModel struct {
	client *Client
}

func NewChatModel(ctx context.Context, config *ChatModelConfig) (*ChatModel, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	var httpClient *http.Client
	if config.HTTPClient != nil {
		// 用户传了自定义 client → 用用户的
		httpClient = config.HTTPClient
	} else {
		// 没有 → 创建默认 client，设置超时
		httpClient = &http.Client{
			Timeout: config.TimeOut,
		}
	}

	config.HTTPClient = httpClient

	cli := NewClient(ctx, config)
	return &ChatModel{
		client: cli,
	}, nil
}

func (cm *ChatModel) Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	return cm.client.Generate(ctx, input)
}

func (cm *ChatModel) Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error) {
	return cm.client.Stream(ctx, input)
}
