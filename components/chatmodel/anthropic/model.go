package anthropic

import (
	"context"
	"fmt"

	"github.com/Luo-root/pulse/components/schema"
)

// ChatModel Anthropic 聊天模型（实现 chatmodel.BaseModel）
type ChatModel struct {
	client *Client
	model  string
}

// NewChatModel 创建 Anthropic 聊天模型
func NewChatModel(config *ChatModelConfig) (*ChatModel, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if config.BaseURL == "" {
		config.BaseURL = "https://api.anthropic.com"
	}
	if config.MaxTokens <= 0 {
		config.MaxTokens = 4096
	}

	client := NewClient(config)

	return &ChatModel{
		client: client,
		model:  config.Model,
	}, nil
}

// Generate 非流式生成
func (m *ChatModel) Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	return m.client.Generate(ctx, input)
}

// Stream 流式生成
func (m *ChatModel) Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error) {
	return m.client.Stream(ctx, input)
}

// GetModelName 返回模型名称
func (m *ChatModel) GetModelName() string {
	return m.model
}
