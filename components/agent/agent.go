package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/memory"
	"github.com/Luo-root/pulse/components/memory/window"
	"github.com/Luo-root/pulse/components/schema"
	"github.com/Luo-root/pulse/components/tools"
)

const DefaultMaxToolRounds int = 20

type Interface interface {
	SendMessage(ctx context.Context, msg *schema.Message) (*schema.Message, error)
	SendMessageStream(ctx context.Context, msg *schema.Message, onChunk func(msg *schema.Message, isToolCall bool) bool) (*schema.Message, error)
}

type Option func(*Agent)

func WithMemoryController(mc *memory.Controller) Option {
	return func(a *Agent) { a.memoryController = mc }
}

func WithUsageTracker(tracker *UsageTracker) Option {
	return func(a *Agent) { a.usageTracker = tracker }
}

func WithSessionID(id string) Option {
	return func(a *Agent) { a.sessionID = id }
}

func WithMaxToolRounds(n int) Option {
	return func(a *Agent) {
		if n > 0 {
			a.maxToolRounds = n
		}
	}
}

type Agent struct {
	model            chatmodel.BaseModel
	registry         *tools.ToolRegistry
	memoryController *memory.Controller
	sessionID        string
	usageTracker     *UsageTracker
	maxToolRounds    int
	mu               sync.Mutex
}

func NewAgent(model chatmodel.BaseModel, registry *tools.ToolRegistry, opts ...Option) *Agent {
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

// ============================================================================
// 公共 API
// ============================================================================

func (ag *Agent) Send(ctx context.Context, userContent string) (*schema.Message, error) {
	return ag.SendMessage(ctx, schema.UserMessage(userContent))
}

func (ag *Agent) SendStream(ctx context.Context, userContent string, onChunk func(msg *schema.Message, isToolCall bool) bool) (*schema.Message, error) {
	return ag.SendMessageStream(ctx, schema.UserMessage(userContent), onChunk)
}

func (ag *Agent) SendMessage(ctx context.Context, msg *schema.Message) (*schema.Message, error) {
	ag.mu.Lock()
	defer ag.mu.Unlock()

	if err := ag.memoryController.SaveTurn(ctx, ag.sessionID, []*schema.Message{msg}); err != nil {
		return nil, err
	}

	query := msg.TextContent()
	for round := 0; round < ag.maxToolRounds; round++ {
		msgs, err := ag.memoryController.BuildContext(ctx, ag.sessionID, query)
		if err != nil {
			return nil, err
		}

		startTime := time.Now()
		resp, err := ag.generateWithRetry(ctx, msgs)
		if err != nil {
			return nil, err
		}
		ag.recordUsage(resp.Usage, startTime)

		if len(resp.ToolCalls) == 0 {
			if err := ag.memoryController.SaveTurn(ctx, ag.sessionID, []*schema.Message{resp}); err != nil {
				return nil, err
			}
			return resp, nil
		}

		if err := ag.handleToolCalls(ctx, resp); err != nil {
			return nil, err
		}
		query = ""
	}
	return nil, fmt.Errorf("tool call loop exceeded maximum rounds (%d)", ag.maxToolRounds)
}

func (ag *Agent) SendMessageStream(ctx context.Context, msg *schema.Message, onChunk func(msg *schema.Message, isToolCall bool) bool) (*schema.Message, error) {
	ag.mu.Lock()
	defer ag.mu.Unlock()

	if err := ag.memoryController.SaveTurn(ctx, ag.sessionID, []*schema.Message{msg}); err != nil {
		return nil, err
	}

	query := msg.TextContent()
	for round := 0; round < ag.maxToolRounds; round++ {
		msgs, err := ag.memoryController.BuildContext(ctx, ag.sessionID, query)
		if err != nil {
			return nil, err
		}

		startTime := time.Now()
		reader, err := ag.streamWithRetry(ctx, msgs)
		if err != nil {
			return nil, err
		}

		fullMsg, err := ag.readStream(reader, onChunk)
		if err != nil {
			return nil, err
		}
		ag.recordUsageFromReader(reader, startTime)

		if len(fullMsg.ToolCalls) == 0 {
			if err := ag.memoryController.SaveTurn(ctx, ag.sessionID, []*schema.Message{fullMsg}); err != nil {
				return nil, err
			}
			return fullMsg, nil
		}

		if err := ag.handleToolCalls(ctx, fullMsg); err != nil {
			return nil, err
		}
		query = ""
	}
	return nil, fmt.Errorf("tool call loop exceeded maximum rounds (%d)", ag.maxToolRounds)
}

// ============================================================================
// 内部实现
// ============================================================================

// retryableError 判断是否为可重试的瞬态错误
func retryableError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "429") ||
		strings.Contains(errStr, "500") ||
		strings.Contains(errStr, "502") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "deadline exceeded")
}

// generateWithRetry 带指数退避重试的 Generate
func (ag *Agent) generateWithRetry(ctx context.Context, msgs []*schema.Message) (*schema.Message, error) {
	const maxRetries = 3
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		resp, err := ag.model.Generate(ctx, msgs)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		if !retryableError(err) || attempt == maxRetries {
			return nil, err
		}

		backoff := time.Duration(math.Pow(2, float64(attempt))) * time.Second
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, lastErr
}

// streamWithRetry 带指数退避重试的 Stream
func (ag *Agent) streamWithRetry(ctx context.Context, msgs []*schema.Message) (*schema.StreamReader, error) {
	const maxRetries = 3
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		reader, err := ag.model.Stream(ctx, msgs)
		if err == nil {
			return reader, nil
		}
		lastErr = err

		if !retryableError(err) || attempt == maxRetries {
			return nil, err
		}

		backoff := time.Duration(math.Pow(2, float64(attempt))) * time.Second
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, lastErr
}

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
		if len(msg.ContentParts) > 0 {
			fullMsg.ContentParts = append(fullMsg.ContentParts, msg.ContentParts...)
		}
		if len(msg.OutputImages) > 0 {
			fullMsg.OutputImages = append(fullMsg.OutputImages, msg.OutputImages...)
		}
		if msg.OutputAudio != nil {
			fullMsg.OutputAudio = msg.OutputAudio
		}

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

func (ag *Agent) handleToolCalls(ctx context.Context, assistantMsg *schema.Message) error {
	results := ag.registry.ExecuteBatch(ctx, assistantMsg.ToolCalls)

	msgs := []*schema.Message{
		{
			Role:             schema.AssistantRole,
			Content:          assistantMsg.Content,
			ReasoningContent: assistantMsg.ReasoningContent,
			ToolCalls:        assistantMsg.ToolCalls,
			ContentParts:     assistantMsg.ContentParts,
			OutputImages:     assistantMsg.OutputImages,
			OutputAudio:      assistantMsg.OutputAudio,
		},
	}
	msgs = append(msgs, schema.ToolResultsMessage(results)...)

	return ag.memoryController.SaveTurn(ctx, ag.sessionID, msgs)
}

func (ag *Agent) recordUsage(usage *schema.Usage, startTime time.Time) {
	if ag.usageTracker == nil || usage == nil {
		return
	}
	ag.usageTracker.Record(*usage, ag.getModelName(), time.Since(startTime))
}

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

func (ag *Agent) getModelName() string {
	if m, ok := ag.model.(interface{ GetModelName() string }); ok {
		return m.GetModelName()
	}
	return "unknown"
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
	ag.mu.Lock()
	defer ag.mu.Unlock()
	ag.memoryController.AddSystemPrompt(schema.SystemMessage(content))
}

func (ag *Agent) ClearAgentHistory(ctx context.Context) error {
	return ag.memoryController.Clear(ctx, ag.sessionID)
}

func (ag *Agent) GetHistory(ctx context.Context) ([]*schema.Message, error) {
	return ag.memoryController.GetHistory(ctx, ag.sessionID)
}

func (ag *Agent) GetRawMessages() []*schema.Message {
	ag.mu.Lock()
	defer ag.mu.Unlock()
	msgs := ag.memoryController.GetShortMemory().GetContextMessages(ag.sessionID)
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
	ag.mu.Lock()
	defer ag.mu.Unlock()
	ag.model = model
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
		window.NewSimpleWindowMemory(
			window.NewManager(window.Config{
				MaxHistoryMessages: 200,
				ReserveTokens:      8000,
			}, model, nil),
		),
	)
}
