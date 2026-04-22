package chatmodel

import (
	"context"

	"github.com/Luo-root/pulse/components/memory"
	"github.com/Luo-root/pulse/components/schema"
)

// MemoryAgent 带记忆的智能体
type MemoryAgent struct {
	agent     *Agent
	manager   *memory.Manager
	sessionID string
}

func NewMemoryAgent(model BaseModel, executor *schema.ToolExecutor, store memory.Store, sessionID string) *MemoryAgent {
	return &MemoryAgent{
		agent:     NewAgent(model, executor),
		manager:   memory.NewManager(store),
		sessionID: sessionID,
	}
}

// Send 非流式
func (ma *MemoryAgent) Send(ctx context.Context, userContent string) (*schema.Message, error) {
	history, _ := ma.manager.GetHistory(ctx, ma.sessionID)
	history = append(history, schema.UserMessage(userContent))
	contextMsgs, _ := ma.manager.BuildContext(ctx, ma.sessionID, userContent, history)

	ma.agent.SetMessages(contextMsgs)
	resp, err := ma.agent.Send(ctx, userContent) // 复用 Agent 的 Send
	if err != nil {
		return nil, err
	}

	ma.manager.SaveTurn(ctx, ma.sessionID, schema.UserMessage(userContent), resp)
	return resp, nil
}

// SendStream 流式
func (ma *MemoryAgent) SendStream(ctx context.Context, userContent string, onChunk func(msg *schema.Message) bool) error {
	history, _ := ma.manager.GetHistory(ctx, ma.sessionID)
	history = append(history, schema.UserMessage(userContent))
	contextMsgs, _ := ma.manager.BuildContext(ctx, ma.sessionID, userContent, history)

	ma.agent.SetMessages(contextMsgs)

	// 包装回调，保存结果
	var lastResp *schema.Message
	wrappedOnChunk := func(msg *schema.Message) bool {
		if !onChunk(msg) {
			return false
		}
		// 收集完整响应用于保存
		if msg.Role == schema.AssistantRole && len(msg.ToolCalls) == 0 {
			lastResp = msg
		}
		return true
	}

	err := ma.agent.SendStream(ctx, userContent, wrappedOnChunk)
	if err != nil {
		return err
	}

	if lastResp != nil {
		ma.manager.SaveTurn(ctx, ma.sessionID, schema.UserMessage(userContent), lastResp)
	}
	return nil
}

// Clear 清空会话
func (ma *MemoryAgent) Clear(ctx context.Context) error {
	return ma.manager.Clear(ctx, ma.sessionID)
}
