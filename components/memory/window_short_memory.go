package memory

import (
	"fmt"
	"sync"

	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/schema"
)

// WindowShortMemory 结合滑动窗口与摘要的短期记忆实现
type WindowShortMemory struct {
	mu       sync.RWMutex
	sessions map[string]*sessionBuffer

	windowMgr  *WindowManager
	model      chatmodel.BaseModel
	summarizer SummaryFunc // 可选的摘要函数
}

// sessionBuffer 保存每个会话的原始消息（未截断）
type sessionBuffer struct {
	messages []*schema.Message
	summary  string // 早期消息的累积摘要（超出窗口部分）
}

// SummaryFunc 摘要生成函数类型
// 传入早期消息，返回摘要文本
type SummaryFunc func(messages []*schema.Message, model chatmodel.BaseModel) string

// NewWindowShortMemory 创建一个基于窗口管理的短期记忆
func NewWindowShortMemory(wm *WindowManager, model chatmodel.BaseModel, summarizer SummaryFunc) *WindowShortMemory {
	return &WindowShortMemory{
		sessions:   make(map[string]*sessionBuffer),
		windowMgr:  wm,
		model:      model,
		summarizer: summarizer,
	}
}

// GetRecent 返回窗口内的完整消息（不进行压缩，直接返回截断后的）
func (ws *WindowShortMemory) GetRecent(sessionID string) []*schema.Message {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	sess, ok := ws.sessions[sessionID]
	if !ok {
		return nil
	}
	// 直接对全量消息应用窗口截断
	return ws.windowMgr.Truncate(sess.messages)
}

// GetContextMessages 返回构建上下文所需的短期记忆
// 整合：窗口内的完整消息 + 超出部分的摘要
func (ws *WindowShortMemory) GetContextMessages(sessionID string) []*schema.Message {
	ws.mu.RLock()
	sess, ok := ws.sessions[sessionID]
	if !ok {
		return nil
	}

	// 1. 计算窗口边界：在全量消息上应用 Truncate，得到“保留区”和“丢弃区”
	allMsgs := sess.messages
	preserved := ws.windowMgr.Truncate(allMsgs)

	// 2. 生成溢出区的摘要（如果存在摘要器）
	var result []*schema.Message
	if ws.summarizer != nil && len(preserved) < len(allMsgs) {
		discarded := allMsgs[:len(allMsgs)-len(preserved)]
		// 增量更新会话摘要
		newSummary := ws.summarizer(discarded, ws.model)
		sess.summary = mergeSummaries(sess.summary, newSummary)

		// 将会话摘要放到前面
		if sess.summary != "" {
			result = append(result, schema.SystemMessage(
				fmt.Sprintf("[对话历史摘要] %s", sess.summary),
			))
		}
		// 并将摘要后的消息从原始存储中移除（保持状态轻量）
		sess.messages = preserved
	} else {
		// 如果没有摘要器，直接保留最新的消息（窗口已经截断）
		sess.messages = preserved
	}

	result = append(result, preserved...)
	ws.mu.RUnlock()
	return result
}

// AddTurn 添加一轮对话
func (ws *WindowShortMemory) AddTurn(sessionID string, msgs []*schema.Message) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	sess := ws.sessions[sessionID]
	if sess == nil {
		sess = &sessionBuffer{}
		ws.sessions[sessionID] = sess
	}
	sess.messages = append(sess.messages, msgs...)
}

// Clear 清空会话
func (ws *WindowShortMemory) Clear(sessionID string) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	delete(ws.sessions, sessionID)
}

// 辅助：合并摘要
func mergeSummaries(old, new string) string {
	if old == "" {
		return new
	}
	return old + "\n" + new // 简单拼接，也可由摘要器处理
}
