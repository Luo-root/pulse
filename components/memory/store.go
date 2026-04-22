package memory

import (
	"context"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// Store 记忆存储接口
type Store interface {
	// Save 保存消息到记忆
	Save(ctx context.Context, sessionID string, msgs []*schema.Message) error

	// Recall 根据查询召回相关记忆
	// query: 查询文本（如用户最新问题）
	// topK: 召回数量
	Recall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error)

	// GetSession 获取完整会话历史
	GetSession(ctx context.Context, sessionID string) ([]*schema.Message, error)

	// ClearSession 清空会话
	ClearSession(ctx context.Context, sessionID string) error

	// Close 关闭存储
	Close() error
}

// MessageRecord 存储的记录结构
type MessageRecord struct {
	ID        string            `json:"id"`
	SessionID string            `json:"session_id"`
	Role      string            `json:"role"`
	Content   string            `json:"content"`
	Embedding []float32         `json:"embedding,omitempty"` // 向量（可选）
	Timestamp time.Time         `json:"timestamp"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}
