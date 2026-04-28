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

func NewMemoryAgent(model BaseModel, registry *schema.ToolRegistry, store memory.Store, sessionID string, opts ...AgentOption) *MemoryAgent {
	return &MemoryAgent{
		agent:     NewAgent(model, registry, opts...),
		manager:   memory.NewManager(store),
		sessionID: sessionID,
	}
}

// buildContext 构建带记忆的上下文并注入 Agent
// 返回用户消息（用于保存到记忆）
func (ma *MemoryAgent) buildContext(ctx context.Context, userContent string) (*schema.Message, error) {
	// 1. 获取历史记忆
	history, _ := ma.manager.GetHistory(ctx, ma.sessionID)

	// 2. 构建带记忆的上下文（包含当前用户消息）
	userMsg := schema.UserMessage(userContent)
	history = append(history, userMsg)
	contextMsgs, _ := ma.manager.BuildContext(ctx, ma.sessionID, userContent, history)

	// 3. 将上下文注入 Agent（包含用户消息，Agent 不再重复添加）
	ma.agent.AddMessages(contextMsgs)

	return userMsg, nil
}

// saveTurn 保存本轮对话到记忆
func (ma *MemoryAgent) saveTurn(ctx context.Context, userMsg *schema.Message, resp *schema.Message) error {
	return ma.manager.SaveTurn(ctx, ma.sessionID, userMsg, resp)
}

// Send 非流式
func (ma *MemoryAgent) Send(ctx context.Context, userContent string) (*schema.Message, error) {
	userMsg, err := ma.buildContext(ctx, userContent)
	if err != nil {
		return nil, err
	}

	// 调用 Agent.Send，传入空字符串避免重复添加用户消息
	resp, err := ma.agent.Send(ctx, "")
	if err != nil {
		return nil, err
	}

	// 保存本轮对话到记忆
	if err := ma.saveTurn(ctx, userMsg, resp); err != nil {
		return nil, err
	}

	return resp, nil
}

// SendStream 流式
func (ma *MemoryAgent) SendStream(ctx context.Context, userContent string, onChunk func(msg *schema.Message, isToolPhase bool) bool) (*schema.Message, error) {
	userMsg, err := ma.buildContext(ctx, userContent)
	if err != nil {
		return nil, err
	}

	// 调用 Agent.SendStream，传入空字符串避免重复添加用户消息
	lastResp, err := ma.agent.SendStream(ctx, "", onChunk)
	if err != nil {
		return lastResp, err
	}

	// 保存本轮对话到记忆
	if lastResp != nil {
		if err := ma.saveTurn(ctx, userMsg, lastResp); err != nil {
			return lastResp, err
		}
	}

	return lastResp, nil
}

// Clear 清空会话
func (ma *MemoryAgent) Clear(ctx context.Context) error {
	return ma.manager.Clear(ctx, ma.sessionID)
}

// GetHistory 获取当前对话历史
func (ma *MemoryAgent) GetHistory() []*schema.Message {
	return ma.agent.GetHistory()
}
