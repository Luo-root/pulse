package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/schema"
	"github.com/Luo-root/pulse/components/tools"
)

// AgentLoop ReAct 循环引擎
// 执行模式：思考 → 行动 → 观察 → 循环，直到 Agent 给出最终回答
type AgentLoop struct {
	model        chatmodel.BaseModel
	registry     *tools.ToolRegistry
	maxRounds    int
	maxMessages  int // 消息窗口大小，0 = 不限制
	systemPrompt string
	onStep       func(step *Step)
}

// Step ReAct 单步记录
type Step struct {
	Round       int
	Thinking    string              // Agent 的思考内容
	ToolCalls   []schema.ToolCall   // 本轮工具调用
	ToolResults []schema.ToolResult // 工具执行结果
	Response    string              // 最终回答（最后一轮）
	Duration    time.Duration
	IsFinal     bool // 是否是最终回答
}

// AgentResult ReAct 循环执行结果
type AgentResult struct {
	Answer string
	Steps  []*Step
	Rounds int
	Usage  schema.Usage
}

// Option AgentLoop 配置选项
type Option func(*AgentLoop)

// WithMaxRounds 设置最大循环次数（0 = 不限制）
func WithMaxRounds(n int) Option {
	return func(al *AgentLoop) {
		al.maxRounds = n
	}
}

// WithSystemPrompt 设置系统提示词
func WithSystemPrompt(prompt string) Option {
	return func(al *AgentLoop) {
		al.systemPrompt = prompt
	}
}

// WithStepCallback 设置每步回调
func WithStepCallback(fn func(step *Step)) Option {
	return func(al *AgentLoop) {
		al.onStep = fn
	}
}

// WithMaxMessages 设置消息窗口大小（防止长任务内存膨胀，0=不限制）
// 保留系统消息 + 最近 N 条消息
func WithMaxMessages(n int) Option {
	return func(al *AgentLoop) {
		al.maxMessages = n
	}
}

// NewAgentLoop 创建 ReAct 循环引擎
func NewAgentLoop(model chatmodel.BaseModel, registry *tools.ToolRegistry, opts ...Option) *AgentLoop {
	al := &AgentLoop{
		model:     model,
		registry:  registry,
		maxRounds: 20,
	}
	for _, opt := range opts {
		opt(al)
	}
	return al
}

// Run 执行 ReAct 循环
func (al *AgentLoop) Run(ctx context.Context, userMessage string) (*AgentResult, error) {
	messages := al.buildInitialMessages(userMessage)
	result := &AgentResult{}

	for round := 0; al.maxRounds == 0 || round < al.maxRounds; round++ {
		step := &Step{Round: round + 1}
		startTime := time.Now()

		// 1. 调用模型
		resp, err := al.model.Generate(ctx, messages)
		if err != nil {
			return nil, fmt.Errorf("round %d model call failed: %w", round+1, err)
		}

		// 累计 usage
		if resp.Usage != nil {
			result.Usage.PromptTokens += resp.Usage.PromptTokens
			result.Usage.CompletionTokens += resp.Usage.CompletionTokens
			result.Usage.TotalTokens += resp.Usage.TotalTokens
		}

		step.Thinking = resp.ReasoningContent
		step.Duration = time.Since(startTime)

		// 2. 没有工具调用 → 最终回答
		if len(resp.ToolCalls) == 0 {
			step.Response = resp.TextContent()
			step.IsFinal = true
			result.Steps = append(result.Steps, step)
			result.Answer = step.Response
			result.Rounds = round + 1

			if al.onStep != nil {
				al.onStep(step)
			}
			return result, nil
		}

		// 3. 有工具调用 → 执行工具
		step.ToolCalls = resp.ToolCalls

		// 把助手消息加入上下文
		messages = append(messages, resp)

		// 执行工具
		toolResults := al.registry.ExecuteBatch(ctx, resp.ToolCalls)
		step.ToolResults = toolResults

		// 把工具结果加入上下文
		toolMsgs := schema.ToolResultsMessage(toolResults)
		messages = append(messages, toolMsgs...)

		// 消息窗口裁剪：保留系统消息 + 最近消息
		if al.maxMessages > 0 && len(messages) > al.maxMessages {
			messages = al.trimMessages(messages)
		}

		result.Steps = append(result.Steps, step)

		if al.onStep != nil {
			al.onStep(step)
		}
	}

	return nil, fmt.Errorf("exceeded max rounds (%d)", al.maxRounds)
}

// RunStream 流式执行 ReAct 循环
func (al *AgentLoop) RunStream(ctx context.Context, userMessage string, onChunk func(msg *schema.Message, isToolCall bool) bool) (*AgentResult, error) {
	messages := al.buildInitialMessages(userMessage)
	result := &AgentResult{}

	for round := 0; al.maxRounds == 0 || round < al.maxRounds; round++ {
		step := &Step{Round: round + 1}
		startTime := time.Now()

		// 1. 流式调用模型
		reader, err := al.model.Stream(ctx, messages)
		if err != nil {
			return nil, fmt.Errorf("round %d stream failed: %w", round+1, err)
		}

		// 读取流式输出
		fullMsg := &schema.Message{Role: schema.AssistantRole}
		for {
			chunk, recvErr := reader.Recv()
			if recvErr != nil {
				break
			}

			if chunk.Content != "" {
				fullMsg.Content += chunk.Content
			}
			if chunk.ReasoningContent != "" {
				fullMsg.ReasoningContent += chunk.ReasoningContent
			}
			if len(chunk.ToolCalls) > 0 {
				fullMsg.ToolCalls = chunk.ToolCalls
			}
			if len(chunk.ContentParts) > 0 {
				fullMsg.ContentParts = append(fullMsg.ContentParts, chunk.ContentParts...)
			}
			if len(chunk.OutputImages) > 0 {
				fullMsg.OutputImages = append(fullMsg.OutputImages, chunk.OutputImages...)
			}
			if chunk.OutputAudio != nil {
				fullMsg.OutputAudio = chunk.OutputAudio
			}

			isToolCall := len(chunk.ToolCalls) > 0
			if !onChunk(chunk, isToolCall) {
				return nil, fmt.Errorf("user cancelled stream")
			}
		}

		// 累计 usage
		if reader.Usage.TotalTokens > 0 {
			result.Usage.PromptTokens += reader.Usage.PromptTokens
			result.Usage.CompletionTokens += reader.Usage.CompletionTokens
			result.Usage.TotalTokens += reader.Usage.TotalTokens
		}

		step.Thinking = fullMsg.ReasoningContent
		step.Duration = time.Since(startTime)

		// 2. 没有工具调用 → 最终回答
		if len(fullMsg.ToolCalls) == 0 {
			step.Response = fullMsg.TextContent()
			step.IsFinal = true
			result.Steps = append(result.Steps, step)
			result.Answer = step.Response
			result.Rounds = round + 1

			if al.onStep != nil {
				al.onStep(step)
			}
			return result, nil
		}

		// 3. 有工具调用 → 执行工具
		step.ToolCalls = fullMsg.ToolCalls
		messages = append(messages, fullMsg)

		toolResults := al.registry.ExecuteBatch(ctx, fullMsg.ToolCalls)
		step.ToolResults = toolResults

		toolMsgs := schema.ToolResultsMessage(toolResults)
		messages = append(messages, toolMsgs...)

		if al.maxMessages > 0 && len(messages) > al.maxMessages {
			messages = al.trimMessages(messages)
		}

		result.Steps = append(result.Steps, step)

		if al.onStep != nil {
			al.onStep(step)
		}
	}

	return nil, fmt.Errorf("exceeded max rounds (%d)", al.maxRounds)
}

// buildInitialMessages 构建初始消息列表
func (al *AgentLoop) buildInitialMessages(userMessage string) []*schema.Message {
	var messages []*schema.Message
	if al.systemPrompt != "" {
		messages = append(messages, schema.SystemMessage(al.systemPrompt))
	}
	messages = append(messages, schema.UserMessage(userMessage))
	return messages
}

// trimMessages 裁剪消息列表到 maxMessages 大小
// 保留系统消息（第一条）+ 最近的消息
func (al *AgentLoop) trimMessages(messages []*schema.Message) []*schema.Message {
	if al.maxMessages <= 0 || len(messages) <= al.maxMessages {
		return messages
	}

	// 保留系统消息
	var systemMsg *schema.Message
	startIdx := 0
	if len(messages) > 0 && messages[0].Role == schema.SystemRole {
		systemMsg = messages[0]
		startIdx = 1
	}

	// 保留最后 maxMessages-1 条（留 1 个位置给系统消息）
	keep := al.maxMessages - 1
	if systemMsg == nil {
		keep = al.maxMessages
	}
	if keep < 1 {
		keep = 1
	}

	tail := messages[startIdx:]
	if len(tail) > keep {
		tail = tail[len(tail)-keep:]
	}

	if systemMsg != nil {
		result := make([]*schema.Message, 0, 1+len(tail))
		result = append(result, systemMsg)
		result = append(result, tail...)
		return result
	}
	return tail
}

// FormatResult 格式化执行结果为可读文本
func FormatResult(r *AgentResult) string {
	data, _ := json.MarshalIndent(map[string]any{
		"answer":      r.Answer,
		"rounds":      r.Rounds,
		"total_steps": len(r.Steps),
		"usage": map[string]any{
			"prompt_tokens":     r.Usage.PromptTokens,
			"completion_tokens": r.Usage.CompletionTokens,
			"total_tokens":      r.Usage.TotalTokens,
		},
	}, "", "  ")
	return string(data)
}
