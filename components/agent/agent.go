package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/memory"
	"github.com/Luo-root/pulse/components/schema"
	"github.com/Luo-root/pulse/components/tools"
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

// WithMemoryController 记忆控制器
func WithMemoryController(mc *memory.Controller) AgentOption {
	return func(a *Agent) {
		a.memoryController = mc
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
	model            chatmodel.BaseModel
	registry         *schema.ToolRegistry
	memoryController *memory.Controller // 记忆控制器（可选）
	sessionID        string             // 会话 ID（可选）
	usageTracker     *UsageTracker      // Usage 追踪器（可选）
}

func NewAgent(model chatmodel.BaseModel, registry *schema.ToolRegistry, opts ...AgentOption) *Agent {
	ag := &Agent{
		model:    model,
		registry: registry,
	}
	for _, opt := range opts {
		opt(ag)
	}

	if ag.memoryController == nil {
		workDir := tools.GetWorkDir()
		systemPrompt := []*schema.Message{
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
`, workDir)),
		}

		ag.memoryController = memory.NewController(systemPrompt, memory.NewSimpleWindowMemory(
			memory.NewWindowManager(memory.WindowConfig{
				MaxHistoryMessages: 200,
				ReserveTokens:      8000,
			},
				ag.model,
				nil,
			)),
			nil)
	}

	return ag
}

// Send 非流式
// 返回：最终 assistant 消息（无工具调用时的回答）
func (ag *Agent) Send(ctx context.Context, userContent string) (*schema.Message, error) {
	err := ag.memoryController.SaveTurn(ctx, ag.sessionID, []*schema.Message{schema.UserMessage(userContent)})
	if err != nil {
		return nil, err
	}

	query := userContent

	for {
		// 记录调用开始时间
		startTime := time.Now()

		msgs, err := ag.memoryController.BuildContext(ctx, ag.sessionID, query)
		if err != nil {
			return nil, err
		}

		resp, err := ag.model.Generate(ctx, msgs)
		if err != nil {
			return nil, err
		}

		// 记录 Usage
		if ag.usageTracker != nil && resp.Usage != nil {
			duration := time.Since(startTime)
			ag.usageTracker.Record(*resp.Usage, ag.getModelName(), duration)
		}

		if len(resp.ToolCalls) != 0 {
			if shouldContinue, err := ag.handleToolCalls(ctx, resp); err != nil {
				return nil, err
			} else if shouldContinue {
				query = ""
				continue // 再次进入循环，获取模型最终回答
			}
		}
		err = ag.memoryController.SaveTurn(ctx, ag.sessionID, []*schema.Message{resp})
		if err != nil {
			return nil, err
		}
		return resp, nil
	}
}

// SendStream 流式
// 功能：自动处理流式输出、实时回调、工具调用循环、用户中断
func (ag *Agent) SendStream(ctx context.Context, userContent string, onChunk func(msg *schema.Message, isToolCall bool) bool) (*schema.Message, error) {
	// 将用户输入添加到对话历史
	err := ag.memoryController.SaveTurn(ctx, ag.sessionID, []*schema.Message{schema.UserMessage(userContent)})
	if err != nil {
		return nil, err
	}

	for {
		// 记录调用开始时间
		startTime := time.Now()

		msgs, err := ag.memoryController.BuildContext(ctx, ag.sessionID, userContent)
		if err != nil {
			return nil, err
		}
		// 调用模型流式接口
		reader, err := ag.model.Stream(ctx, msgs)
		if err != nil {
			return nil, err
		}

		// 流式读取，实时回调
		fullMsg := schema.Message{
			Role: schema.AssistantRole,
		}
		var isToolPhase bool

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

		if len(fullMsg.ToolCalls) != 0 {
			if shouldContinue, err := ag.handleToolCalls(ctx, &fullMsg); err != nil {
				return nil, err
			} else if shouldContinue {
				continue // 再次进入循环，获取模型最终回答
			}
		}
		err = ag.memoryController.SaveTurn(ctx, ag.sessionID, []*schema.Message{&fullMsg})
		if err != nil {
			return nil, err
		}
		return &fullMsg, nil
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
func (ag *Agent) SetMessages(ctx context.Context, msgs []*schema.Message) error {
	err := ag.memoryController.Clear(ctx, ag.sessionID)
	if err != nil {
		return err
	}

	err = ag.memoryController.SaveTurn(ctx, ag.sessionID, msgs)
	if err != nil {
		return err
	}
	return nil
}

// AddMessages 追加多条消息
func (ag *Agent) AddMessages(ctx context.Context, msgs []*schema.Message) error {
	err := ag.memoryController.SaveTurn(ctx, ag.sessionID, msgs)
	if err != nil {
		return err
	}
	return nil
}

// AddSystemMessage 添加系统消息
func (ag *Agent) AddSystemMessage(content string) {
	ag.memoryController.SystemPrompt = append(ag.memoryController.SystemPrompt, schema.SystemMessage(content))
}

// ClearAgentHistory 清空历史（保留 system）
func (ag *Agent) ClearAgentHistory(ctx context.Context) error {
	err := ag.memoryController.Clear(ctx, ag.sessionID)
	if err != nil {
		return err
	}
	return nil
}

// GetHistory 获取当前对话历史
func (ag *Agent) GetHistory(ctx context.Context) ([]*schema.Message, error) {
	history, err := ag.memoryController.GetHistory(ctx, ag.sessionID)
	if err != nil {
		return nil, err
	}
	return history, nil
}

// GetRawMessages 获取内部完整消息副本（含 ToolCalls/ToolResults）
func (ag *Agent) GetRawMessages() []*schema.Message {
	msgs := ag.memoryController.ShortMemory.GetContextMessages(ag.sessionID)
	result := make([]*schema.Message, len(ag.memoryController.ShortMemory.GetContextMessages(ag.sessionID)))
	for i, m := range msgs {
		cloned := m.Clone()
		result[i] = &cloned
	}
	return result
}

// GetUsageTracker 获取 UsageTracker
func (ag *Agent) GetUsageTracker() *UsageTracker {
	return ag.usageTracker
}

func (ag *Agent) ChangeModel(model chatmodel.BaseModel) {
	ag.model = model
}

// handleToolCalls 处理工具调用：执行 + 追加历史 + 返回是否需要继续循环
func (ag *Agent) handleToolCalls(ctx context.Context, assistantMsg *schema.Message) (bool, error) {
	// 执行工具
	results := ag.registry.ExecuteBatch(ctx, assistantMsg.ToolCalls)

	var msgs []*schema.Message

	// 构造 assistant 消息（保留 tool_calls）
	msgs = append(msgs, &schema.Message{
		Role:             schema.AssistantRole,
		Content:          assistantMsg.Content,
		ReasoningContent: assistantMsg.ReasoningContent,
		ToolCalls:        assistantMsg.ToolCalls,
	})

	// 追加到历史
	msgs = append(msgs, schema.ToolResultsMessage(results)...)

	err := ag.memoryController.SaveTurn(ctx, ag.sessionID, msgs)
	if err != nil {
		return false, err
	}

	// 返回 true 表示已保存历史，主循环应继续请求模型
	return true, nil
}
