package anthropic

import (
	"net/http"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// ChatModelConfig Anthropic 模型配置
type ChatModelConfig struct {
	BaseURL string
	APIKey  string
	Model   string

	// 模型参数
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"top_p,omitempty"`
	TopK        int     `json:"top_k,omitempty"`

	// 工具
	Tools []schema.Tool `json:"-"`

	// Thinking（扩展思考）
	Thinking *Thinking `json:"thinking,omitempty"`

	// 流式
	Stream bool `json:"stream,omitempty"`

	// HTTP
	TimeOut    time.Duration
	HTTPClient *http.Client `json:"-"`
}

// ThinkingType 思考能力开关
type ThinkingType string

const (
	Enabled  ThinkingType = "enabled"
	Disabled ThinkingType = "disabled"
)

// Thinking Anthropic 扩展思考配置
type Thinking struct {
	Type         ThinkingType `json:"type"`                    // "enabled"
	BudgetTokens int          `json:"budget_tokens,omitempty"` // 思考预算
}

// APIRequest Anthropic API 请求结构
type APIRequest struct {
	Model       string       `json:"model"`
	Messages    []APIMessage `json:"messages"`
	MaxTokens   int          `json:"max_tokens"`
	System      string       `json:"system,omitempty"` // 顶层系统提示
	Temperature float64      `json:"temperature,omitempty"`
	TopP        float64      `json:"top_p,omitempty"`
	TopK        int          `json:"top_k,omitempty"`
	Stream      bool         `json:"stream,omitempty"`
	Tools       []APITool    `json:"tools,omitempty"`
	Thinking    *Thinking    `json:"thinking,omitempty"`
}

// APIMessage Anthropic API 消息格式
type APIMessage struct {
	Role    string         `json:"role"`    // "user" 或 "assistant"
	Content []ContentBlock `json:"content"` // 内容块数组
}

// ContentBlock 内容块（多态结构）
type ContentBlock struct {
	Type string `json:"type"` // "text", "tool_use", "tool_result", "thinking"

	// type=text
	Text string `json:"text,omitempty"`

	// type=tool_use
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`

	// type=tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"` // tool_result 的内容
	IsError   bool   `json:"is_error,omitempty"`

	// type=thinking
	Thinking string `json:"thinking,omitempty"`
}

// APITool Anthropic 工具定义
type APITool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// APIResponse Anthropic API 非流式响应
type APIResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

// Usage Token 使用情况
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}
