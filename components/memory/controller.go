package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/Luo-root/pulse/components/schema"
)

// Controller 记忆控制器，管理三类记忆
type Controller struct {
	systemPrompt []*schema.Message
	shortMemory  ShortMemoryManager
	longStore    LongTermStore
	topK         int
}

// Option Controller 配置选项
type Option func(*Controller)

// WithTopK 设置长期记忆召回数量
func WithTopK(k int) Option {
	return func(c *Controller) {
		if k > 0 {
			c.topK = k
		}
	}
}

// WithLongStore 设置长期记忆存储器
func WithLongStore(store LongTermStore) Option {
	return func(c *Controller) {
		c.longStore = store
	}
}

// NewController 创建记忆控制器（保持向后兼容）
func NewController(systemPrompt []*schema.Message, shortMemory ShortMemoryManager, opts ...Option) *Controller {
	c := &Controller{
		systemPrompt: systemPrompt,
		shortMemory:  shortMemory,
		topK:         3,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// AddSystemPrompt 追加系统提示词
func (c *Controller) AddSystemPrompt(msgs ...*schema.Message) {
	c.systemPrompt = append(c.systemPrompt, msgs...)
}

// GetShortMemory 获取短期记忆管理器
func (c *Controller) GetShortMemory() ShortMemoryManager {
	return c.shortMemory
}

// SaveTurn 保存一轮对话（user + assistant）
func (c *Controller) SaveTurn(ctx context.Context, sessionID string, msgs []*schema.Message) error {
	if c.shortMemory != nil {
		c.shortMemory.AddTurn(sessionID, msgs)
	}

	if c.longStore != nil {
		if err := c.longStore.Save(ctx, sessionID, msgs); err != nil {
			return fmt.Errorf("long-term save failed: %w", err)
		}
	}
	return nil
}

// BuildContext 构建带记忆的上文
func (c *Controller) BuildContext(ctx context.Context, sessionID string, currentQuery string) ([]*schema.Message, error) {
	msgs := make([]*schema.Message, 0, 64)

	if len(c.systemPrompt) > 0 {
		msgs = append(msgs, c.systemPrompt...)
	}

	if c.longStore != nil && currentQuery != "" {
		recallMsg, err := c.buildRecallMessage(ctx, sessionID, currentQuery)
		if err != nil {
			_ = err
		} else if recallMsg != nil {
			msgs = append(msgs, recallMsg)
		}
	}

	if c.shortMemory != nil {
		shortMsgs := c.shortMemory.GetContextMessages(sessionID)
		if len(shortMsgs) > 0 {
			msgs = append(msgs, shortMsgs...)
		}
	}

	return msgs, nil
}

func (c *Controller) buildRecallMessage(ctx context.Context, sessionID string, query string) (*schema.Message, error) {
	topK := c.topK
	if topK <= 0 {
		topK = 3
	}

	mems, err := c.longStore.Recall(ctx, sessionID, query, topK)
	if err != nil {
		return nil, err
	}
	if len(mems) == 0 {
		return nil, nil
	}

	var sb strings.Builder
	sb.WriteString("以下是与当前问题相关的历史记忆：\n")
	for i, m := range mems {
		sb.WriteString(fmt.Sprintf("%d. [%s]: %s\n", i+1, m.Role, m.TextContent()))
	}

	return schema.SystemMessage(sb.String()), nil
}

// GetHistory 获取完整历史
func (c *Controller) GetHistory(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	if c.longStore == nil {
		if c.shortMemory != nil {
			return c.shortMemory.GetRecent(sessionID), nil
		}
		return nil, nil
	}
	return c.longStore.GetSession(ctx, sessionID)
}

// Clear 清空会话
func (c *Controller) Clear(ctx context.Context, sessionID string) error {
	if c.shortMemory != nil {
		c.shortMemory.Clear(sessionID)
	}

	if c.longStore != nil {
		if err := c.longStore.ClearSession(ctx, sessionID); err != nil {
			return fmt.Errorf("clear long-term store failed: %w", err)
		}
	}
	return nil
}

// Close 释放资源
func (c *Controller) Close() error {
	if c.longStore != nil {
		return c.longStore.Close()
	}
	return nil
}
