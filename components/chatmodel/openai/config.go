package openai

import (
	"net/http"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// ResponseFormatType 响应格式类型
type ResponseFormatType string

const (
	Text       ResponseFormatType = "text"
	JsonObject ResponseFormatType = "json_object"
)

// ThinkingType 思考能力开关
type ThinkingType string

const (
	Enabled  ThinkingType = "enabled"
	Disabled ThinkingType = "disabled"
)

// ThinkingKeepEnum 历史思考内容保留策略
type ThinkingKeepEnum string

const (
	Null ThinkingKeepEnum = "null"
	All  ThinkingKeepEnum = "all"
)

// Thinking 思考能力配置
type Thinking struct {
	Type ThinkingType     `json:"type,omitempty"`
	Keep ThinkingKeepEnum `json:"keep,omitempty"`
}

// StreamOptions 流式响应选项
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ToolType 工具类型
type ToolType string

const (
	Function ToolType = "function"
)

// Tool API 请求中的工具定义
type Tool struct {
	Type     ToolType    `json:"type"`
	Function schema.Tool `json:"function"`
}

// WrapTools 将 schema.Tool 列表包装为 API 请求格式
func WrapTools(tools []schema.Tool) []Tool {
	if len(tools) == 0 {
		return nil
	}
	result := make([]Tool, len(tools))
	for i, t := range tools {
		result[i] = Tool{
			Type:     Function,
			Function: t,
		}
	}
	return result
}

// ChatModelConfig OpenAI 兼容 API 配置
type ChatModelConfig struct {
	BaseURL string
	APIKey  string
	Model   string

	MaxCompletionTokens uint64             `json:"max_completion_tokens,omitempty"`
	ResponseFormat      ResponseFormatType `json:"response_format,omitempty"`
	Stop                string             `json:"stop,omitempty"`
	Temperature         float64            `json:"temperature,omitempty"`
	TopP                float64            `json:"top_p,omitempty"`
	N                   uint8              `json:"n,omitempty"`
	PresencePenalty     float64            `json:"presence_penalty,omitempty"`
	FrequencyPenalty    float64            `json:"frequency_penalty,omitempty"`

	Tools    []schema.Tool `json:"-"`
	Thinking Thinking      `json:"thinking,omitempty"`

	PromptCacheKey   string `json:"prompt_cache_key,omitempty"`
	SafetyIdentifier string `json:"safety_identifier,omitempty"`

	TimeOut    time.Duration
	HTTPClient *http.Client `json:"-"`
}
