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

	// 取前几条
	TopK int
}

// NewController SystemPrompt 系统提示词, ShortMemory 短期记忆管理器, LongStore 长期记忆存储器
func NewController(systemPrompt []*schema.Message, shortMemory ShortMemoryManager, longStore LongTermStore) *Controller {
	return &Controller{
		SystemPrompt: systemPrompt,
		ShortMemory:  shortMemory,
		LongStore:    longStore,
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
		// 通常这里只保存 user + assistant 消息本身，后续会有提取模块生成“记忆条目”
		err := c.LongStore.Save(ctx, sessionID, msgs)
		if err != nil {
			return err
		}
	}
	return nil
}

// BuildContext 构建带记忆的上文
func (c *Controller) BuildContext(ctx context.Context, sessionID string, currentQuery string) ([]*schema.Message, error) {
	msgs := make([]*schema.Message, 0)

	// 系统提示词（永久记忆）
	if c.SystemPrompt != nil {
		msgs = append(msgs, c.SystemPrompt...)
	}

	// 长期记忆注入（跨会话相关历史）
	if c.LongStore != nil {
		mems, err := c.LongStore.Recall(ctx, sessionID, currentQuery, c.TopK)
		if err != nil {
			// 可以降级，不影响主流程
		} else if len(mems) > 0 {
			// 将检索到的长期记忆组装成一条系统消息，置于系统提示词之后
			var sb strings.Builder
			sb.WriteString("以下是与当前问题相关的历史记忆：\n")
			for i, m := range mems {
				sb.WriteString(fmt.Sprintf("%d. [%s]: %s\n", i+1, m.Role, m.Content))
			}
			msgs = append(msgs, schema.SystemMessage(sb.String()))
		}
	}

	// 短期记忆（当前会话的窗口 + 摘要）
	if c.ShortMemory != nil {
		shortMsgs := c.ShortMemory.GetContextMessages(sessionID)
		msgs = append(msgs, shortMsgs...)
	}

	// 当前用户输入
	msgs = append(msgs, &schema.Message{Role: schema.UserRole, Content: currentQuery})

	return msgs, nil
}

// GetHistory 获取完整历史
func (c *Controller) GetHistory(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	return c.LongStore.GetSession(ctx, sessionID)
}

// Clear 清空会话
func (c *Controller) Clear(ctx context.Context, sessionID string) error {
	return c.LongStore.ClearSession(ctx, sessionID)
}
