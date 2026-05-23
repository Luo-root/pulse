package memory

import (
	"fmt"
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
	messages    []*schema.Message
	lastTurnIdx int // 上一轮对话的起始索引（用于图片清理）
}

// NewSimpleWindowMemory 创建纯滑动窗口短期记忆
func NewSimpleWindowMemory(wm *WindowManager) *SimpleWindowMemory {
	return &SimpleWindowMemory{
		sessions:  make(map[string]*simpleSession),
		windowMgr: wm,
	}
}

// GetRecent 返回截断后的最新消息
func (sm *SimpleWindowMemory) GetRecent(sessionID string) []*schema.Message {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	sess, ok := sm.sessions[sessionID]
	if !ok {
		return nil
	}
	return sm.windowMgr.Truncate(sess.messages)
}

// GetContextMessages 返回构建上下文所需的消息
func (sm *SimpleWindowMemory) GetContextMessages(sessionID string) []*schema.Message {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sess, ok := sm.sessions[sessionID]
	if !ok {
		return nil
	}

	// 清理非最近一轮的图片
	sm.stripOldImages(sess)

	// Truncate 后写回，控制内存增长
	truncated := sm.windowMgr.Truncate(sess.messages)
	sess.messages = truncated
	return truncated
}

// AddTurn 添加一轮对话
func (sm *SimpleWindowMemory) AddTurn(sessionID string, msgs []*schema.Message) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sess, ok := sm.sessions[sessionID]
	if !ok {
		sess = &simpleSession{lastTurnIdx: 0}
		sm.sessions[sessionID] = sess
	}

	// 先清理上一轮的图片（现在已成为历史）
	sm.stripOldImages(sess)

	// 记录新轮次起始位置
	sess.lastTurnIdx = len(sess.messages)

	// 添加新消息（保留完整多模态内容）
	for _, msg := range msgs {
		cloned := msg.Clone()
		sess.messages = append(sess.messages, &cloned)
	}
}

// Clear 清空会话
func (sm *SimpleWindowMemory) Clear(sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sessions, sessionID)
}

// stripOldImages 将非最近一轮的多模态消息中的图片替换为文本描述
// 调用方需持有写锁
func (sm *SimpleWindowMemory) stripOldImages(sess *simpleSession) {
	for i := 0; i < sess.lastTurnIdx && i < len(sess.messages); i++ {
		msg := sess.messages[i]
		if msg.IsMultimodal() {
			sess.messages[i] = stripImages(msg)
		}
	}
}

// stripImages 将多模态消息的图片替换为文本描述
func stripImages(msg *schema.Message) *schema.Message {
	cloned := msg.Clone()

	var textParts []string
	var imgCount int

	for _, part := range cloned.ContentParts {
		switch part.Type {
		case "text":
			textParts = append(textParts, part.Text)
		case "image_url":
			imgCount++
		}
	}

	// 重建为纯文本消息
	text := ""
	if len(textParts) > 0 {
		text = joinText(textParts)
	}
	if imgCount > 0 {
		suffix := fmt.Sprintf("[此消息包含 %d 张图片，已从历史中移除]", imgCount)
		if text != "" {
			text = text + "\n" + suffix
		} else {
			text = suffix
		}
	}

	cloned.Content = text
	cloned.ContentParts = nil // 清除图片
	return &cloned
}

func joinText(parts []string) string {
	result := ""
	for i, p := range parts {
		if i > 0 {
			result += "\n"
		}
		result += p
	}
	return result
}
