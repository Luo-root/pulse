package memory

import "github.com/Luo-root/pulse/components/schema"

// ShortMemoryManager 短期记忆管理器
type ShortMemoryManager interface {
	// GetRecent 返回当前窗口内的完整消息（不进行压缩）
	GetRecent(sessionID string) []*schema.Message

	// GetContextMessages 返回构建上下文所需的短期记忆消息
	// 内部可能整合：窗口内的完整消息 + 超出部分的摘要
	GetContextMessages(sessionID string) []*schema.Message

	// AddTurn 添加一轮对话，触发窗口维护、摘要生成等
	AddTurn(sessionID string, msgs []*schema.Message)

	// Clear 清空会话短期记忆
	Clear(sessionID string)
}
