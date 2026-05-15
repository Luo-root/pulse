package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/schema"
)

// SummaryFunc 摘要生成函数类型
type SummaryFunc func(messages []*schema.Message, model chatmodel.BaseModel) string

// WindowShortMemory 结合滑动窗口与摘要的短期记忆实现
type WindowShortMemory struct {
	mu       sync.RWMutex
	sessions map[string]*sessionBuffer

	windowMgr  *WindowManager
	model      chatmodel.BaseModel
	summarizer SummaryFunc
}

type sessionBuffer struct {
	messages []*schema.Message
	summary  string
}

func NewWindowShortMemory(wm *WindowManager, model chatmodel.BaseModel, summarizer SummaryFunc) *WindowShortMemory {
	return &WindowShortMemory{
		sessions:   make(map[string]*sessionBuffer),
		windowMgr:  wm,
		model:      model,
		summarizer: summarizer,
	}
}

func (ws *WindowShortMemory) GetRecent(sessionID string) []*schema.Message {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	sess, ok := ws.sessions[sessionID]
	if !ok {
		return nil
	}
	return ws.windowMgr.Truncate(sess.messages)
}

// GetContextMessages 返回构建上下文所需的短期记忆
// 修复：RLock → Lock（因为会修改 sess.messages 和 sess.summary）
func (ws *WindowShortMemory) GetContextMessages(sessionID string) []*schema.Message {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	sess, ok := ws.sessions[sessionID]
	if !ok {
		return nil
	}

	// 1. 计算窗口保留区
	preserved := ws.windowMgr.Truncate(sess.messages)

	// 2. 计算溢出区
	var result []*schema.Message
	overflowCount := len(sess.messages) - len(preserved)

	if ws.summarizer != nil && overflowCount > 0 {
		discarded := sess.messages[:overflowCount]

		// 生成摘要
		newSummary := ws.summarizer(discarded, ws.model)
		sess.summary = mergeSummaries(sess.summary, newSummary)

		// 摘要作为系统消息置于最前
		if sess.summary != "" {
			result = append(result, schema.SystemMessage(
				fmt.Sprintf("[对话历史摘要] %s", sess.summary),
			))
		}
	}

	// 3. 写回截断后的消息
	sess.messages = preserved
	result = append(result, preserved...)

	return result
}

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

func (ws *WindowShortMemory) Clear(sessionID string) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	delete(ws.sessions, sessionID)
}

func mergeSummaries(old, new string) string {
	if old == "" {
		return new
	}
	return old + "\n" + new
}

// DefaultSummaryFunc 默认摘要函数
func DefaultSummaryFunc() SummaryFunc {
	return func(messages []*schema.Message, model chatmodel.BaseModel) string {
		if len(messages) == 0 {
			return ""
		}

		prompt := buildSummaryPrompt(messages)

		ctx := context.Background()
		msgs := []*schema.Message{
			schema.UserMessage(prompt),
		}

		resp, err := model.Generate(ctx, msgs)
		if err != nil {
			return fallbackSummary(messages)
		}

		return strings.TrimSpace(resp.Content)
	}
}

func buildSummaryPrompt(messages []*schema.Message) string {
	var sb strings.Builder
	sb.WriteString("请将以下对话历史总结为一段简洁的摘要，保留关键信息、决策和行动：\n\n")

	for _, m := range messages {
		switch m.Role {
		case schema.UserRole:
			sb.WriteString(fmt.Sprintf("用户: %s\n", m.Content))
		case schema.AssistantRole:
			content := m.Content
			if m.ReasoningContent != "" {
				content = m.ReasoningContent + " " + content
			}
			sb.WriteString(fmt.Sprintf("助手: %s\n", content))
		case schema.ToolRole:
			summary := summarizeToolResult(m)
			sb.WriteString(fmt.Sprintf("工具结果: %s\n", summary))
		}
	}

	sb.WriteString("\n摘要：")
	return sb.String()
}

func summarizeToolResult(msg *schema.Message) string {
	content := msg.Content
	if content == "" {
		return "(空)"
	}
	if strings.HasPrefix(content, "[Error] ") {
		rest := content[len("[Error] "):]
		if len([]rune(rest)) > 200 {
			rest = string([]rune(rest)[:200]) + "..."
		}
		return fmt.Sprintf("错误: %s", rest)
	}
	if len([]rune(content)) > 200 {
		content = string([]rune(content)[:200]) + "..."
	}
	return content
}

func fallbackSummary(messages []*schema.Message) string {
	var parts []string
	for _, m := range messages {
		if m.Role == schema.UserRole || m.Role == schema.AssistantRole {
			text := m.Content
			if len([]rune(text)) > 100 {
				text = string([]rune(text)[:100]) + "..."
			}
			parts = append(parts, text)
		}
		if len(parts) >= 5 {
			break
		}
	}
	return strings.Join(parts, " | ")
}
