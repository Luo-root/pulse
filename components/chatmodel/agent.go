package chatmodel

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Luo-root/pulse/components/schema"
	tools "github.com/Luo-root/pulse/components/tool"
)

// AgentInterface 统一接口
type AgentInterface interface {
	// Send 非流式
	Send(ctx context.Context, userContent string) (*schema.Message, error)

	// SendStream 真正流式：实时回调，Agent 内部处理工具调用循环
	// 回调返回 bool：true=继续，false=中断（用户取消）
	SendStream(ctx context.Context, userContent string, onChunk func(msg *schema.Message) bool) error
}

// Agent  封装多轮对话（支持 Generate 和 Stream）
type Agent struct {
	model    BaseModel
	executor *schema.ToolExecutor
	msgs     []*schema.Message
}

func NewAgent(model BaseModel, executor *schema.ToolExecutor) *Agent {
	ag := &Agent{
		model:    model,
		executor: executor,
		msgs:     make([]*schema.Message, 0),
	}

	// 注入当前目录
	workDir := tools.GetWorkDir()
	ag.msgs = append(ag.msgs, schema.SystemMessage(fmt.Sprintf(
		"你是一个有用的助手。当前工作目录是：%s。使用文件相关工具时，请基于此目录操作。",
		workDir,
	)))

	return ag
}

// Send 非流式
// 返回：最终 assistant 消息（无工具调用时的回答）
func (ag *Agent) Send(ctx context.Context, userContent string) (*schema.Message, error) {
	if userContent != "" {
		ag.msgs = append(ag.msgs, schema.UserMessage(userContent))
	}

	for {
		resp, err := ag.model.Generate(ctx, ag.msgs)
		if err != nil {
			return nil, err
		}

		if len(resp.ToolCalls) == 0 {
			ag.msgs = append(ag.msgs, resp)
			return resp, nil
		}

		if err := ag.handleToolCalls(ctx, resp); err != nil {
			return nil, err
		}
	}
}

// SendStream 流式
// 功能：自动处理流式输出、实时回调、工具调用循环、用户中断
func (ag *Agent) SendStream(ctx context.Context, userContent string, onChunk func(msg *schema.Message, isToolCall bool) bool) (*schema.Message, error) {
	// 将用户输入添加到对话历史
	if userContent != "" {
		ag.msgs = append(ag.msgs, schema.UserMessage(userContent))
	}

	for {
		// 调用模型流式接口
		reader, err := ag.model.Stream(ctx, ag.msgs)
		if err != nil {
			return nil, err
		}

		// 流式读取，实时回调
		var fullMsg schema.Message
		var isToolPhase bool

		if fullMsg.Role == "" {
			fullMsg.Role = schema.AssistantRole
		}

		// 流式读取每一个chunk
		for {
			msg, err := reader.Recv()
			// 流结束，退出读取循环
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}

			// 累加文本内容
			if msg.Content != "" {
				fullMsg.Content += msg.Content
				fullMsg.Role = msg.Role
			}

			// 覆盖工具调用（LLM流式返回的是完整累加状态）
			if len(msg.ToolCalls) > 0 {
				isToolPhase = true
				fullMsg.ToolCalls = msg.ToolCalls
			}

			// 实时回调：将chunk推送给调用方
			// 如果回调返回false，代表用户主动中断，直接退出
			if !onChunk(msg, isToolPhase) {
				return &fullMsg, errors.New("user cancelled stream")
			}
		}

		// 无工具调用 → 对话结束，退出总循环
		if len(fullMsg.ToolCalls) == 0 {
			// 将完整的助手消息加入历史
			ag.msgs = append(ag.msgs, &fullMsg)
			return &fullMsg, nil
		}

		// 有工具调用 → 复用已有方法执行工具，并追加历史
		if err := ag.handleToolCalls(ctx, &fullMsg); err != nil {
			return &fullMsg, err
		}

		// 工具执行完成，继续循环，让模型生成最终回答
	}
}

// SetMessages 直接设置完整消息列表（用于注入记忆上下文）
func (ag *Agent) SetMessages(msgs []*schema.Message) {
	ag.msgs = msgs
}

// AddMessages 追加多条消息
func (ag *Agent) AddMessages(msgs []*schema.Message) {
	ag.msgs = append(ag.msgs, msgs...)
}

// AddMessage 添加任意消息（灵活扩展）
func (ag *Agent) AddMessage(msg *schema.Message) {
	ag.msgs = append(ag.msgs, msg)
}

// AddUserMessage 添加用户消息
func (ag *Agent) AddUserMessage(content string) {
	ag.msgs = append(ag.msgs, schema.UserMessage(content))
}

// AddSystemMessage 添加系统消息
func (ag *Agent) AddSystemMessage(content string) {
	ag.msgs = append(ag.msgs, schema.SystemMessage(content))
}

// AgentHistory 清空历史（保留 system）
func (ag *Agent) AgentHistory() {
	var systemMsgs []*schema.Message
	for _, m := range ag.msgs {
		if m.Role == schema.SystemRole {
			systemMsgs = append(systemMsgs, m)
		}
	}
	ag.msgs = systemMsgs
}

// GetHistory 获取当前对话历史
func (ag *Agent) GetHistory() []*schema.Message {
	result := make([]*schema.Message, len(ag.msgs))
	for i, m := range ag.msgs {
		result[i] = &schema.Message{
			Role:    m.Role,
			Content: m.Content,
			Name:    m.Name,
			// 不拷贝 ToolCalls/ToolResults，外部只读即可
		}
	}
	return result
}

// handleToolCalls 处理工具调用：执行 + 追加历史
func (ag *Agent) handleToolCalls(ctx context.Context, assistantMsg *schema.Message) error {
	// 执行工具
	results := ag.executor.ExecuteBatch(ctx, assistantMsg.ToolCalls)

	// 构造 assistant 消息（保留 tool_calls）
	assistantWithTools := &schema.Message{
		Role:      schema.AssistantRole,
		Content:   assistantMsg.Content,
		ToolCalls: assistantMsg.ToolCalls,
	}

	// 追加到历史
	ag.msgs = append(ag.msgs, assistantWithTools)
	ag.msgs = append(ag.msgs, ag.executor.ToToolMessages(results)...)

	return nil
}
