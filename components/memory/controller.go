package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/Luo-root/pulse/components/schema"
)

// Controller 记忆控制器，管理三类记忆
type Controller struct {
	// 系统提示词 —— 永久存在于上下文底层
	SystemPrompt []*schema.Message

	// 短期记忆管理器 —— 滑动窗口 / 摘要
	ShortMemory ShortMemoryManager

	// 长期记忆存储器 —— 外部持久化 + 向量检索
	LongStore LongTermStore

	// 召回数量
	TopK int
}

// NewController SystemPrompt 系统提示词, ShortMemory 短期记忆管理器, LongStore 长期记忆存储器
func NewController(systemPrompt []*schema.Message, shortMemory ShortMemoryManager, longStore LongTermStore) *Controller {
	topK := 3 // 默认召回 3 条
	return &Controller{
		SystemPrompt: systemPrompt,
		ShortMemory:  shortMemory,
		LongStore:    longStore,
		TopK:         topK,
	}
}

// SaveTurn 保存一轮对话（user + assistant）
func (c *Controller) SaveTurn(ctx context.Context, sessionID string, msgs []*schema.Message) error {
	// 更新短期记忆（内部自动做窗口维护/摘要）
	if c.ShortMemory != nil {
		c.ShortMemory.AddTurn(sessionID, msgs)
	}

	// 更新长期记忆
	if c.LongStore != nil {
		if err := c.LongStore.Save(ctx, sessionID, msgs); err != nil {
			return fmt.Errorf("long-term save failed: %w", err)
		}
	}
	return nil
}

// BuildContext 构建带记忆的上文
// 返回消息顺序：系统提示词 → 长期记忆召回 → 短期记忆（窗口+摘要）
func (c *Controller) BuildContext(ctx context.Context, sessionID string, currentQuery string) ([]*schema.Message, error) {
	msgs := make([]*schema.Message, 0, 64)

	// 1. 系统提示词（永久记忆）
	if len(c.SystemPrompt) > 0 {
		msgs = append(msgs, c.SystemPrompt...)
	}

	// 2. 长期记忆注入（跨会话相关历史）
	if c.LongStore != nil && currentQuery != "" {
		recallMsg, err := c.buildRecallMessage(ctx, sessionID, currentQuery)
		if err != nil {
			// 召回失败降级，不影响主流程
			_ = err
		} else if recallMsg != nil {
			msgs = append(msgs, recallMsg)
		}
	}

	// 3. 短期记忆（当前会话的窗口 + 摘要）
	if c.ShortMemory != nil {
		shortMsgs := c.ShortMemory.GetContextMessages(sessionID)
		if len(shortMsgs) > 0 {
			msgs = append(msgs, shortMsgs...)
		}
	}

	return msgs, nil
}

// buildRecallMessage 构建长期记忆召回消息（局部变量，避免共享状态竞争）
func (c *Controller) buildRecallMessage(ctx context.Context, sessionID string, query string) (*schema.Message, error) {
	topK := c.TopK
	if topK <= 0 {
		topK = 3
	}

	mems, err := c.LongStore.Recall(ctx, sessionID, query, topK)
	if err != nil {
		return nil, err
	}
	if len(mems) == 0 {
		return nil, nil
	}

	var sb strings.Builder
	sb.WriteString("以下是与当前问题相关的历史记忆：\n")
	for i, m := range mems {
		sb.WriteString(fmt.Sprintf("%d. [%s]: %s\n", i+1, m.Role, m.Content))
	}

	return schema.SystemMessage(sb.String()), nil
}

// GetHistory 获取完整历史
func (c *Controller) GetHistory(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	if c.LongStore == nil {
		// 降级到短期记忆
		if c.ShortMemory != nil {
			return c.ShortMemory.GetRecent(sessionID), nil
		}
		return nil, nil
	}
	return c.LongStore.GetSession(ctx, sessionID)
}

// Clear 清空会话
func (c *Controller) Clear(ctx context.Context, sessionID string) error {
	// 清空短期记忆
	if c.ShortMemory != nil {
		c.ShortMemory.Clear(sessionID)
	}

	// 清空长期记忆
	if c.LongStore != nil {
		if err := c.LongStore.ClearSession(ctx, sessionID); err != nil {
			return fmt.Errorf("clear long-term store failed: %w", err)
		}
	}
	return nil
}

// Close 释放资源
func (c *Controller) Close() error {
	if c.LongStore != nil {
		return c.LongStore.Close()
	}
	return nil
}
