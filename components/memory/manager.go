package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/Luo-root/pulse/components/schema"
)

type Manager struct {
	store Store
}

func NewManager(store Store) *Manager {
	return &Manager{store: store}
}

// SaveTurn 保存一轮对话（user + assistant）
func (m *Manager) SaveTurn(ctx context.Context, sessionID string, userMsg, assistantMsg *schema.Message) error {
	return m.store.Save(ctx, sessionID, []*schema.Message{userMsg, assistantMsg})
}

// BuildContext 构建带记忆的上文
// 把召回的记忆注入到 system prompt 或作为历史消息
func (m *Manager) BuildContext(ctx context.Context, sessionID string, currentQuery string, history []*schema.Message) ([]*schema.Message, error) {
	// 1. 召回相关记忆
	memories, err := m.store.Recall(ctx, sessionID, currentQuery, 3)
	if err != nil {
		return history, err // 召回失败不影响主流程
	}

	if len(memories) == 0 {
		return history, nil
	}

	// 2. 构造记忆提示
	var memoryTexts []string
	for _, m := range memories {
		memoryTexts = append(memoryTexts, fmt.Sprintf("[%s]: %s", m.Role, m.Content))
	}

	memoryPrompt := fmt.Sprintf(
		"以下是与当前问题相关的历史记忆：\n%s\n请结合这些记忆回答。",
		strings.Join(memoryTexts, "\n"),
	)

	// 3. 插入到开头（system 之后）
	result := make([]*schema.Message, 0, len(history)+1)

	// 找到 system 消息位置
	var hasSystem bool
	for i, msg := range history {
		if msg.Role == schema.SystemRole {
			result = append(result, msg)
			// 在 system 后插入记忆
			result = append(result, schema.SystemMessage(memoryPrompt))
			result = append(result, history[i+1:]...)
			hasSystem = true
			break
		}
	}

	if !hasSystem {
		// 没有 system，插入到最前面
		result = append([]*schema.Message{schema.SystemMessage(memoryPrompt)}, history...)
	}

	return result, nil
}

// GetHistory 获取完整历史
func (m *Manager) GetHistory(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	return m.store.GetSession(ctx, sessionID)
}

// Clear 清空会话
func (m *Manager) Clear(ctx context.Context, sessionID string) error {
	return m.store.ClearSession(ctx, sessionID)
}
