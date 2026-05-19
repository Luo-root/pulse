package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/memory"
	"github.com/Luo-root/pulse/components/schema"
	"github.com/Luo-root/pulse/components/tools"
)

// DefaultMaxToolRounds 默认最大工具调用轮次
const DefaultMaxToolRounds int = 20

// AgentInterface 统一接口
type AgentInterface interface {
	Send(ctx context.Context, userContent string) (*schema.Message, error)
	SendStream(ctx context.Context, userContent string, onChunk func(msg *schema.Message, isToolCall bool) bool) (*schema.Message, error)
}

// AgentOption Agent 配置选项
type AgentOption func(*Agent)

func WithMemoryController(mc *memory.Controller) AgentOption {
	return func(a *Agent) { a.memoryController = mc }
}

func WithUsageTracker(tracker *UsageTracker) AgentOption {
	return func(a *Agent) { a.usageTracker = tracker }
}

func WithSessionID(id string) AgentOption {
	return func(a *Agent) { a.sessionID = id }
}

func WithMaxToolRounds(n int) AgentOption {
	return func(a *Agent) {
		if n > 0 {
			a.maxToolRounds = n
		}
	}
}

// Agent 封装多轮对话（支持 Generate 和 Stream）
type Agent struct {
	model            chatmodel.BaseModel
	registry         *tools.ToolRegistry
	memoryController *memory.Controller
	sessionID        string
	usageTracker     *UsageTracker
	maxToolRounds    int
	mu               sync.Mutex // 保护 send 循环的并发安全
}

func NewAgent(model chatmodel.BaseModel, registry *tools.ToolRegistry, opts ...AgentOption) *Agent {
	ag := &Agent{
		model:         model,
		registry:      registry,
		maxToolRounds: DefaultMaxToolRounds,
	}
	for _, opt := range opts {
		opt(ag)
	}

	if ag.sessionID == "" {
		ag.sessionID = "default"
	}

	if ag.memoryController == nil {
		ag.memoryController = defaultMemoryController(model)
	}

	return ag
}

// Send 非流式
func (ag *Agent) Send(ctx context.Context, userContent string) (*schema.Message, error) {
	ag.mu.Lock()
	defer ag.mu.Unlock()

	err := ag.memoryController.SaveTurn(ctx, ag.sessionID, []*schema.Message{schema.UserMessage(userContent)})
	if err != nil {
		return nil, err
	}

	query := userContent

	for round := 0; round < ag.maxToolRounds; round++ {
		startTime := time.Now()

		msgs, err := ag.memoryController.BuildContext(ctx, ag.sessionID, query)
		if err != nil {
			return nil, err
		}

		resp, err := ag.model.Generate(ctx, msgs)
		if err != nil {
			return nil, err
		}

		ag.recordUsage(resp.Usage, startTime)

		// 无工具调用 → 最终回答
		if len(resp.ToolCalls) == 0 {
			if err := ag.memoryController.SaveTurn(ctx, ag.sessionID, []*schema.Message{resp}); err != nil {
				return nil, err
			}
			return resp, nil
		}

		// 有工具调用 → 执行工具，继续循环
		if err := ag.handleToolCalls(ctx, resp); err != nil {
			return nil, err
		}
		query = "" // 后续轮次不再用原始 query 做记忆召回
	}

	return nil, fmt.Errorf("tool call loop exceeded maximum rounds (%d)", ag.maxToolRounds)
}

// SendStream 流式
func (ag *Agent) SendStream(ctx context.Context, userContent string, onChunk func(msg *schema.Message, isToolCall bool) bool) (*schema.Message, error) {
	ag.mu.Lock()
	defer ag.mu.Unlock()

	err := ag.memoryController.SaveTurn(ctx, ag.sessionID, []*schema.Message{schema.UserMessage(userContent)})
	if err != nil {
		return nil, err
	}

	for round := 0; round < ag.maxToolRounds; round++ {
		startTime := time.Now()

		msgs, err := ag.memoryController.BuildContext(ctx, ag.sessionID, userContent)
		if err != nil {
			return nil, err
		}

		reader, err := ag.model.Stream(ctx, msgs)
		if err != nil {
			return nil, err
		}

		fullMsg, err := ag.readStream(reader, onChunk)
		if err != nil {
			return nil, err
		}

		ag.recordUsageFromReader(reader, startTime)

		// 无工具调用 → 最终回答
		if len(fullMsg.ToolCalls) == 0 {
			if err := ag.memoryController.SaveTurn(ctx, ag.sessionID, []*schema.Message{fullMsg}); err != nil {
				return nil, err
			}
			return fullMsg, nil
		}

		// 有工具调用 → 执行工具，继续循环
		if err := ag.handleToolCalls(ctx, fullMsg); err != nil {
			return nil, err
		}
	}

	return nil, fmt.Errorf("tool call loop exceeded maximum rounds (%d)", ag.maxToolRounds)
}

// readStream 从 StreamReader 读取所有 chunk，组装完整消息
func (ag *Agent) readStream(reader *schema.StreamReader, onChunk func(msg *schema.Message, isToolCall bool) bool) (*schema.Message, error) {
	fullMsg := &schema.Message{Role: schema.AssistantRole}

	for {
		msg, err := reader.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		fullMsg.Role = msg.Role
		if msg.Content != "" {
			fullMsg.Content += msg.Content
		}
		if msg.ReasoningContent != "" {
			fullMsg.ReasoningContent += msg.ReasoningContent
		}
		if len(msg.ToolCalls) > 0 {
			fullMsg.ToolCalls = msg.ToolCalls
		}

		// 当前 chunk 是否包含工具调用
		isToolCall := len(msg.ToolCalls) > 0
		if !onChunk(msg, isToolCall) {
			return fullMsg, errors.New("user cancelled stream")
		}
	}

	if reader.Usage.TotalTokens > 0 {
		fullMsg.Usage = &schema.Usage{
			PromptTokens:        reader.Usage.PromptTokens,
			CompletionTokens:    reader.Usage.CompletionTokens,
			TotalTokens:         reader.Usage.TotalTokens,
			CachedTokens:        reader.Usage.CachedTokens,
			PromptTokensDetails: reader.Usage.PromptTokensDetails,
		}
	}

	return fullMsg, nil
}

// handleToolCalls 执行工具调用，将结果保存到记忆
func (ag *Agent) handleToolCalls(ctx context.Context, assistantMsg *schema.Message) error {
	results := ag.registry.ExecuteBatch(ctx, assistantMsg.ToolCalls)

	msgs := []*schema.Message{
		{
			Role:             schema.AssistantRole,
			Content:          assistantMsg.Content,
			ReasoningContent: assistantMsg.ReasoningContent,
			ToolCalls:        assistantMsg.ToolCalls,
		},
	}
	msgs = append(msgs, schema.ToolResultsMessage(results)...)

	return ag.memoryController.SaveTurn(ctx, ag.sessionID, msgs)
}

// recordUsage 记录 Usage（非流式）
func (ag *Agent) recordUsage(usage *schema.Usage, startTime time.Time) {
	if ag.usageTracker == nil || usage == nil {
		return
	}
	ag.usageTracker.Record(*usage, ag.getModelName(), time.Since(startTime))
}

// recordUsageFromReader 记录 Usage（流式，从 StreamReader 获取）
func (ag *Agent) recordUsageFromReader(reader *schema.StreamReader, startTime time.Time) {
	if ag.usageTracker == nil {
		return
	}
	usage := schema.Usage{
		PromptTokens:        reader.Usage.PromptTokens,
		CompletionTokens:    reader.Usage.CompletionTokens,
		TotalTokens:         reader.Usage.TotalTokens,
		CachedTokens:        reader.Usage.CachedTokens,
		PromptTokensDetails: reader.Usage.PromptTokensDetails,
	}
	ag.usageTracker.Record(usage, ag.getModelName(), time.Since(startTime))
}

// ============================================================================
// 消息管理
// ============================================================================

func (ag *Agent) SetMessages(ctx context.Context, msgs []*schema.Message) error {
	if err := ag.memoryController.Clear(ctx, ag.sessionID); err != nil {
		return err
	}
	return ag.memoryController.SaveTurn(ctx, ag.sessionID, msgs)
}

func (ag *Agent) AddMessages(ctx context.Context, msgs []*schema.Message) error {
	return ag.memoryController.SaveTurn(ctx, ag.sessionID, msgs)
}

func (ag *Agent) AddSystemMessage(content string) {
	ag.memoryController.SystemPrompt = append(ag.memoryController.SystemPrompt, schema.SystemMessage(content))
}

func (ag *Agent) ClearAgentHistory(ctx context.Context) error {
	return ag.memoryController.Clear(ctx, ag.sessionID)
}

func (ag *Agent) GetHistory(ctx context.Context) ([]*schema.Message, error) {
	return ag.memoryController.GetHistory(ctx, ag.sessionID)
}

func (ag *Agent) GetRawMessages() []*schema.Message {
	msgs := ag.memoryController.ShortMemory.GetContextMessages(ag.sessionID)
	result := make([]*schema.Message, len(msgs))
	for i, m := range msgs {
		cloned := m.Clone()
		result[i] = &cloned
	}
	return result
}

func (ag *Agent) GetUsageTracker() *UsageTracker {
	return ag.usageTracker
}

func (ag *Agent) ChangeModel(model chatmodel.BaseModel) {
	ag.model = model
}

// getModelName 获取模型名称
func (ag *Agent) getModelName() string {
	if m, ok := ag.model.(interface{ GetModelName() string }); ok {
		return m.GetModelName()
	}
	return "unknown"
}

// ============================================================================
// 默认配置
// ============================================================================

const defaultSystemPromptTemplate = `# 核心身份
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
`

func defaultSystemPrompt(workDir string) []*schema.Message {
	return []*schema.Message{
		schema.SystemMessage(fmt.Sprintf(defaultSystemPromptTemplate, workDir)),
	}
}

func defaultMemoryController(model chatmodel.BaseModel) *memory.Controller {
	workDir := tools.GetWorkDir()
	return memory.NewController(
		defaultSystemPrompt(workDir),
		memory.NewSimpleWindowMemory(
			memory.NewWindowManager(memory.WindowConfig{
				MaxHistoryMessages: 200,
				ReserveTokens:      8000,
			}, model, nil),
		),
		nil,
	)
}
