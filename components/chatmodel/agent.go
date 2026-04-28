package chatmodel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	tools "github.com/Luo-root/pulse/components/tool"
)

// AgentInterface 统一接口
type AgentInterface interface {
	// Send 非流式
	Send(ctx context.Context, userContent string) (*schema.Message, error)

	// SendStream 真正流式：实时回调，Agent 内部处理工具调用循环
	// 回调返回 bool：true=继续，false=中断（用户取消）
	SendStream(ctx context.Context, userContent string, onChunk func(msg *schema.Message, isToolCall bool) bool) (*schema.Message, error)
}

// AgentOption Agent 配置选项
type AgentOption func(*Agent)

// WithWindow 设置对话窗口管理器，用于限制工作记忆长度
func WithWindow(wm *WindowManager) AgentOption {
	return func(a *Agent) {
		a.window = wm
	}
}

// WithUsageTracker 设置 Usage 追踪器
func WithUsageTracker(tracker *UsageTracker) AgentOption {
	return func(a *Agent) {
		a.usageTracker = tracker
	}
}

// Agent  封装多轮对话（支持 Generate 和 Stream）
type Agent struct {
	model        BaseModel
	registry     *schema.ToolRegistry
	msgs         []*schema.Message
	window       *WindowManager // 对话窗口管理器（可选）
	usageTracker *UsageTracker  // Usage 追踪器（可选）
}

func NewAgent(model BaseModel, registry *schema.ToolRegistry, opts ...AgentOption) *Agent {
	ag := &Agent{
		model:    model,
		registry: registry,
		msgs:     make([]*schema.Message, 0),
	}
	for _, opt := range opts {
		opt(ag)
	}

	// 注入当前目录
	workDir := tools.GetWorkDir()
	ag.msgs = append(ag.msgs,
		schema.SystemMessage(fmt.Sprintf(`
# 核心身份
你是专业的自动化执行助手，严格遵守指令，绝不臆测、绝不编造信息。

# 【铁律一：工作目录约束（绝对禁止违反）】
当前固定工作目录：%s
1. 所有文件/文件夹操作 必须基于此目录执行，禁止使用绝对路径、禁止跳出目录、禁止擅自修改路径
2. 每次执行工具前，必须再次确认工作目录正确
3. 若涉及路径拼接，必须以当前工作目录为根路径

# 【铁律二：工具调用规则（强制高频调用）】
1. 【不确定 → 必须调用工具】：信息不明确、数据未验证、路径/内容存疑 → 立即调用工具查询确认
2. 【主动确认】：用户需求模糊、参数缺失、结果需要校验 → 主动调用工具获取真实信息
3. 【多轮验证】：工具返回结果后，若仍不完整/不准确 → 继续调用工具补充查询
4. 【禁止凭空回答】：无工具验证的信息，绝对不输出给用户
5. 【优先工具】：工具能完成的操作，绝不依赖记忆，绝不使用常识猜测

# 行为约束
1. 严格执行工具调用循环，直到信息完整、确认无误
2. 输出内容必须基于工具返回的真实数据
3. 路径、文件名、内容等关键信息必须经过工具验证
`, workDir,
		), ""))

	return ag
}

// applyWindow 在发送给模型前应用窗口截断，防止工作记忆无限增长
func (ag *Agent) applyWindow() {
	if ag.window != nil {
		ag.msgs = ag.window.Truncate(ag.msgs)
	}
}

// Send 非流式
// 返回：最终 assistant 消息（无工具调用时的回答）
func (ag *Agent) Send(ctx context.Context, userContent string) (*schema.Message, error) {
	if userContent != "" {
		ag.msgs = append(ag.msgs, schema.UserMessage(userContent))
	}

	for {
		// 每次调用模型前截断窗口
		ag.applyWindow()

		// 记录调用开始时间
		startTime := time.Now()

		resp, err := ag.model.Generate(ctx, ag.msgs)
		if err != nil {
			return nil, err
		}

		// 记录 Usage
		if ag.usageTracker != nil && resp.Usage != nil {
			duration := time.Since(startTime)
			ag.usageTracker.Record(*resp.Usage, ag.getModelName(), duration)
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
		// 每次调用模型前截断窗口
		ag.applyWindow()

		// 记录调用开始时间
		startTime := time.Now()

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
			fullMsg.Role = msg.Role

			if msg.Content != "" {
				fullMsg.Content += msg.Content
			}

			if msg.ReasoningContent != "" {
				fullMsg.ReasoningContent += msg.ReasoningContent
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

		// 记录 Usage（从 StreamReader 获取）
		if ag.usageTracker != nil {
			duration := time.Since(startTime)
			usage := schema.Usage{
				PromptTokens: reader.Usage.PromptTokens,
				Completion:   reader.Usage.Completion,
				TotalTokens:  reader.Usage.TotalTokens,
			}
			ag.usageTracker.Record(usage, ag.getModelName(), duration)
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

// getModelName 获取模型名称（用于 Usage 记录）
func (ag *Agent) getModelName() string {
	// 尝试从模型获取名称
	if modelWithName, ok := ag.model.(interface{ GetModelName() string }); ok {
		return modelWithName.GetModelName()
	}
	return "unknown"
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
func (ag *Agent) AddSystemMessage(content, reasoningContent string) {
	ag.msgs = append(ag.msgs, schema.SystemMessage(content, reasoningContent))
}

// ClearAgentHistory 清空历史（保留 system）
func (ag *Agent) ClearAgentHistory() {
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

// GetRawMessages 获取内部完整消息副本（含 ToolCalls/ToolResults）
func (ag *Agent) GetRawMessages() []*schema.Message {
	result := make([]*schema.Message, len(ag.msgs))
	for i, m := range ag.msgs {
		cloned := m.Clone()
		result[i] = &cloned
	}
	return result
}

// GetUsageTracker 获取 UsageTracker
func (ag *Agent) GetUsageTracker() *UsageTracker {
	return ag.usageTracker
}

// handleToolCalls 处理工具调用：执行 + 追加历史
func (ag *Agent) handleToolCalls(ctx context.Context, assistantMsg *schema.Message) error {
	// 执行工具
	results := ag.registry.ExecuteBatch(ctx, assistantMsg.ToolCalls)

	// 构造 assistant 消息（保留 tool_calls）
	assistantWithTools := &schema.Message{
		Role:             schema.AssistantRole,
		Content:          assistantMsg.Content,
		ReasoningContent: assistantMsg.ReasoningContent,
		ToolCalls:        assistantMsg.ToolCalls,
	}

	// 追加到历史
	ag.msgs = append(ag.msgs, assistantWithTools)
	ag.msgs = append(ag.msgs, ag.registry.ToToolMessages(results)...)

	return nil
}
