package memory

import (
	"sync"

	"github.com/Luo-root/pulse/components/schema"
)

// SimpleWindowMemory 纯滑动窗口短期记忆（无摘要）
type SimpleWindowMemory struct {
	mu        sync.RWMutex
	sessions  map[string]*simpleSession
	windowMgr *WindowManager
}

type simpleSession struct {
	messages []*schema.Message
}

// NewSimpleWindowMemory 创建纯滑动窗口短期记忆
func NewSimpleWindowMemory(wm *WindowManager) *SimpleWindowMemory {
	return &SimpleWindowMemory{
		sessions:  make(map[string]*simpleSession),
		windowMgr: wm,
	}
}

// GetRecent 返回截断后的最新消息（不保留过期部分）
func (sm *SimpleWindowMemory) GetRecent(sessionID string) []*schema.Message {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	sess, ok := sm.sessions[sessionID]
	if !ok {
		return nil
	}
	return sm.windowMgr.Truncate(sess.messages)
}

// GetContextMessages 返回构建上下文所需的消息，等于截断后的最新消息
func (sm *SimpleWindowMemory) GetContextMessages(sessionID string) []*schema.Message {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sess, ok := sm.sessions[sessionID]
	if !ok {
		return nil
	}
	// 应用窗口截断并将结果写回存储，保持状态轻量
	sess.messages = sm.windowMgr.Truncate(sess.messages)
	return sess.messages
}

// AddTurn 添加一轮对话
func (sm *SimpleWindowMemory) AddTurn(sessionID string, msgs []*schema.Message) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sess, ok := sm.sessions[sessionID]
	if !ok {
		sess = &simpleSession{}
		sm.sessions[sessionID] = sess
	}
	sess.messages = append(sess.messages, msgs...)
}

// Clear 清空会话
func (sm *SimpleWindowMemory) Clear(sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sessions, sessionID)
}
