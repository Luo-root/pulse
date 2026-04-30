package claude

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// ============================================================================
// ChatModelConfig — Anthropic Claude 模型配置（对应 openai.ChatModelConfig）
// ============================================================================

type ChatModelConfig struct {
	BaseURL  string // 如 https://api.deepseek.com/anthropic
	APIKey   string
	Model    string            `json:"model"`
	Messages []*schema.Message `json:"messages"`

	// ★ Anthropic 特有：system 作为独立字段
	System string `json:"system,omitempty"`

	// 聊天补全生成的最大 Token 数量
	MaxTokens uint64 `json:"max_tokens"`

	// 停用词（Anthropic 支持数组，但配置层接受逗号分隔字符串，内部转换）
	Stop string `json:"stop,omitempty"`

	// 是否以流式方式返回响应，默认 false
	Stream bool `json:"stream,omitempty"`

	// 采样温度，Anthropic 范围 [0.0 ~ 2.0]，DeepSeek 兼容
	Temperature float64 `json:"temperature,omitempty"`

	// Top-P 采样
	TopP float64 `json:"top_p,omitempty"`

	// Top-K 采样（DeepSeek 忽略此字段）
	TopK int `json:"top_k,omitempty"`

	// 工具列表（Claude 格式，内部转换）
	Tools []ClaudeTool `json:"-"`

	// 工具选择策略
	ToolChoice *ClaudeToolChoice `json:"-"`

	// 思考配置
	Thinking *ClaudeThinkingConfig `json:"thinking,omitempty"`

	// 输出配置（仅 effort 被 DeepSeek 支持）
	OutputConfig *ClaudeOutputConfig `json:"output_config,omitempty"`

	// HTTP 相关
	TimeOut    time.Duration
	HTTPClient *http.Client `json:"http_client"`
}

// ============================================================================
// SchemaToClaudeTools 转换：schema.Tool → ClaudeTool
// ============================================================================

func SchemaToClaudeTools(tools []schema.Tool) []ClaudeTool {
	result := make([]ClaudeTool, len(tools))
	for i, t := range tools {
		// 解析 Parameters (JSON Schema)
		var properties map[string]interface{}
		var required []string

		// t.Parameters 是 any，预期为 map[string]interface{}
		if paramMap, ok := t.Parameters.(map[string]interface{}); ok {
			if props, ok := paramMap["properties"].(map[string]interface{}); ok {
				properties = props
			}
			if req, ok := paramMap["required"].([]interface{}); ok {
				for _, r := range req {
					if rStr, ok := r.(string); ok {
						required = append(required, rStr)
					}
				}
			}
		}

		result[i] = ClaudeTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: InputSchema{
				Type:       "object",
				Properties: properties,
				Required:   required,
			},
		}
	}
	return result
}

// ============================================================================
// ChatModel — 对外门面（实现 BaseModel 接口）
// ============================================================================

type ChatModel struct {
	client *Client
	model  string
}

// NewChatModel 创建 Claude 模型客户端
func NewChatModel(ctx context.Context, config *ChatModelConfig) (*ChatModel, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	var httpClient *http.Client
	if config.HTTPClient != nil {
		httpClient = config.HTTPClient
	} else {
		httpClient = &http.Client{
			Timeout: config.TimeOut,
		}
	}

	config.HTTPClient = httpClient

	cli := NewClient(ctx, config)
	return &ChatModel{
		client: cli,
		model:  config.Model,
	}, nil
}

// Generate 非流式生成
func (cm *ChatModel) Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	return cm.client.Generate(ctx, input)
}

// Stream 流式生成
func (cm *ChatModel) Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error) {
	return cm.client.Stream(ctx, input)
}

// GetModelName 返回模型名称（用于 UsageTracker 记录）
func (cm *ChatModel) GetModelName() string {
	return cm.model
}
