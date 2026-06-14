package memory

import (
	"context"
	"time"

	"github.com/Luo-root/pulse/components/schema"
)

// ============================================================================
// 核心接口：Store（存储）+ Retriever（检索）
// ============================================================================

// Store 纯消息存储接口
// 负责消息的持久化，不关心检索逻辑
type Store interface {
	// Save 保存消息
	Save(ctx context.Context, sessionID string, msgs []*schema.Message) error

	// GetSession 获取完整会话历史
	GetSession(ctx context.Context, sessionID string) ([]*schema.Message, error)

	// ClearSession 清空会话
	ClearSession(ctx context.Context, sessionID string) error

	// Close 关闭存储
	Close() error
}

// Retriever 消息检索接口
// 负责根据查询召回相关消息，不关心存储细节
type Retriever interface {
	// Recall 根据查询召回相关消息
	Recall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error)

	// Close 关闭检索器
	Close() error
}

// Indexer 向量索引管理接口
// 允许外部通知检索器更新索引（新增/删除向量、重建索引）
type Indexer interface {
	// AddToIndex 将消息的嵌入向量添加到索引
	AddToIndex(ctx context.Context, sessionID string, msgs []*schema.Message) error

	// RemoveFromIndex 从索引中移除指定会话的所有向量
	RemoveFromIndex(sessionID string) error

	// RebuildIndex 从存储中重建完整索引
	RebuildIndex()

	// IndexReady 返回索引是否就绪
	IndexReady() bool
}

// StoreEventType 存储事件类型
type StoreEventType int

const (
	StoreEventSave  StoreEventType = iota // 新消息保存
	StoreEventClear                       // 会话清空
)

// StoreEvent 存储事件
type StoreEvent struct {
	Type      StoreEventType
	SessionID string
	Messages  []*schema.Message // Save 事件时有值
}

// StoreHook 存储事件回调
type StoreHook func(event StoreEvent)

// ============================================================================
// 组合接口：LongTermStore（保持向后兼容）
// ============================================================================

// LongTermStore 记忆存储接口（组合 Store + Retriever）
type LongTermStore interface {
	Store
	Retriever
}

// ============================================================================
// 组合实现：CompositeLongTermStore
// ============================================================================

// CompositeLongTermStore 组合 Store + Retriever 实现 LongTermStore
type CompositeLongTermStore struct {
	store     Store
	retriever Retriever
	indexer   Indexer // 可选：向量索引管理
	hooks     []StoreHook
}

// NewCompositeLongTermStore 创建组合存储
func NewCompositeLongTermStore(store Store, retriever Retriever) *CompositeLongTermStore {
	return &CompositeLongTermStore{
		store:     store,
		retriever: retriever,
	}
}

// AddHook 添加存储事件钩子
// 当 Store 发生 Save/Clear 操作时，通知 Retriever 更新索引
func (c *CompositeLongTermStore) AddHook(hook StoreHook) {
	c.hooks = append(c.hooks, hook)
}

// notify 通知所有钩子
func (c *CompositeLongTermStore) notify(event StoreEvent) {
	for _, hook := range c.hooks {
		hook(event)
	}
}

func (c *CompositeLongTermStore) Save(ctx context.Context, sessionID string, msgs []*schema.Message) error {
	if err := c.store.Save(ctx, sessionID, msgs); err != nil {
		return err
	}
	c.notify(StoreEvent{Type: StoreEventSave, SessionID: sessionID, Messages: msgs})
	if c.indexer != nil {
		_ = c.indexer.AddToIndex(ctx, sessionID, msgs)
	}
	return nil
}

func (c *CompositeLongTermStore) Recall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error) {
	return c.retriever.Recall(ctx, sessionID, query, topK)
}

func (c *CompositeLongTermStore) GetSession(ctx context.Context, sessionID string) ([]*schema.Message, error) {
	return c.store.GetSession(ctx, sessionID)
}

func (c *CompositeLongTermStore) ClearSession(ctx context.Context, sessionID string) error {
	if err := c.store.ClearSession(ctx, sessionID); err != nil {
		return err
	}
	c.notify(StoreEvent{Type: StoreEventClear, SessionID: sessionID})
	if c.indexer != nil {
		_ = c.indexer.RemoveFromIndex(sessionID)
	}
	return nil
}

func (c *CompositeLongTermStore) Close() error {
	err1 := c.retriever.Close()
	err2 := c.store.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// GetStore 获取底层 Store
func (c *CompositeLongTermStore) GetStore() Store {
	return c.store
}

// GetRetriever 获取底层 Retriever
func (c *CompositeLongTermStore) GetRetriever() Retriever {
	return c.retriever
}

// AttachIndexer 关联向量索引管理器
func (c *CompositeLongTermStore) AttachIndexer(indexer Indexer) {
	c.indexer = indexer
}

// IndexReady 返回向量索引是否就绪（无 indexer 时始终返回 true）
func (c *CompositeLongTermStore) IndexReady() bool {
	if c.indexer == nil {
		return true
	}
	return c.indexer.IndexReady()
}

// ============================================================================
// 辅助类型
// ============================================================================

// MessageRecord 存储的记录结构
type MessageRecord struct {
	ID        string            `json:"id"`
	SessionID string            `json:"session_id"`
	Role      string            `json:"role"`
	Content   string            `json:"content"`
	Embedding []float32         `json:"embedding,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// EmbeddingFunc 文本转向量函数
type EmbeddingFunc func(ctx context.Context, text string) ([]float32, error)
