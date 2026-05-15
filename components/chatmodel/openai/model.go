package openai

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Luo-root/pulse/components/schema"
)

// ChatModel OpenAI 兼容聊天模型（实现 chatmodel.BaseModel）
type ChatModel struct {
	client *Client
	model  string
}

// NewChatModel 创建 OpenAI 兼容聊天模型
func NewChatModel(config *ChatModelConfig) (*ChatModel, error) {
	if config == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}

	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: config.TimeOut}
	}
	config.HTTPClient = httpClient

	cli := NewClient(config)
	return &ChatModel{
		client: cli,
		model:  config.Model,
	}, nil
}

func (cm *ChatModel) Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	return cm.client.Generate(ctx, input)
}

func (cm *ChatModel) Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error) {
	return cm.client.Stream(ctx, input)
}

func (cm *ChatModel) GetModelName() string {
	return cm.model
}
