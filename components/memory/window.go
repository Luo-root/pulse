package memory

import (
	"github.com/Luo-root/pulse/components/chatmodel"
	"github.com/Luo-root/pulse/components/schema"
)

// WindowConfig 对话窗口配置
// 提供手动限制和自动计算两种模式，两者可同时启用，取更严格的限制
type WindowConfig struct {
	// MaxHistoryMessages 最大保留的历史消息数（不包括 System 消息）
	// 例如设置为 20，则只保留最近的 20 条 user/assistant/tool 消息
	// 0 表示不限制（默认，兼容旧行为）
	MaxHistoryMessages int

	// MaxHistoryTokens 历史消息的最大估算 Token 数（不包括 System 消息）
	// 优先级与 MaxHistoryMessages 同级，两者同时设置时取交集（更严格的）
	// 0 表示不限制（默认）
	MaxHistoryTokens int

	// ReserveTokens 自动模式下为模型输出预留的 Token 数
	// 如果设置了此值且 model 实现了 ModelContextWindow 接口，
	// WindowManager 会自动计算 MaxHistoryTokens = ContextWindow - ReserveTokens
	// 例如：模型 128k 上下文，预留 8k 给输出+缓冲，则历史限制为 120k
	ReserveTokens int
}

// ModelContextWindow 模型上下文窗口信息接口
// 各模型实现（如 OpenAI/Gemini/Claude）可实现此接口暴露上下文长度
// WindowManager 会通过类型断言自动识别
type ModelContextWindow interface {
	ContextWindow() int
}

// TokenEstimator Token 估算器
// 如需精确控制，可接入 tiktoken 等第三方库实现此接口
type TokenEstimator interface {
	Estimate(msg *schema.Message) int
}

// defaultEstimator 默认估算器
// 混合文本保守估算：平均 1 token ≈ 1.8 个 rune（中英文混合偏保守）
// defaultEstimator 默认估算器
type defaultEstimator struct{}

func (e *defaultEstimator) Estimate(msg *schema.Message) int {
	// ---- 文本 Token ----
	textLen := 0
	textLen += len([]rune(msg.Content))
	textLen += len([]rune(msg.ReasoningContent))
	for _, tc := range msg.ToolCalls {
		textLen += len([]rune(tc.Function.Name))
		textLen += len([]rune(tc.Function.Arguments))
	}
	if msg.ToolCallID != "" {
		textLen += len([]rune(msg.ToolCallID))
	}

	// ContentParts 中的文本也要计入
	for _, part := range msg.ContentParts {
		if part.Type == "text" {
			textLen += len([]rune(part.Text))
		}
	}

	textTokens := int(float64(textLen) / 1.8)

	// ---- 图片 Token ----
	// OpenAI 图片 Token 估算：
	// - low detail: ~85 tokens/图
	// - high detail: ~765+ tokens/图
	// - base64 数据越大 → 越可能是高分辨率 → 越多 token
	// 保守按 base64 长度 / 100 估算，最低 85
	imageTokens := 0
	for _, part := range msg.ContentParts {
		if part.Type == "image_url" && part.ImageURL != nil {
			perImage := len(part.ImageURL.URL) / 100
			if perImage < 85 {
				perImage = 85
			}
			imageTokens += perImage
		}
	}

	total := textTokens + imageTokens
	if total < 1 && (textLen > 0 || imageTokens > 0) {
		return 1
	}
	return total
}

// WindowManager 对话窗口管理器
// 在每次模型调用前截断消息列表，防止工作记忆无限增长导致 Token 爆炸
type WindowManager struct {
	config    WindowConfig
	estimator TokenEstimator
}

// NewWindowManager 创建窗口管理器
// model 用于自动获取上下文长度（可选，实现 ModelContextWindow 接口即可）
// estimator 可自定义 Token 计算方式，传 nil 使用默认估算
func NewWindowManager(config WindowConfig, model chatmodel.BaseModel, estimator TokenEstimator) *WindowManager {
	if estimator == nil {
		estimator = &defaultEstimator{}
	}

	// 自动计算：如果未手动设置 Token 限制，但设置了预留且模型支持
	if config.MaxHistoryTokens == 0 && config.ReserveTokens > 0 && model != nil {
		if mi, ok := model.(ModelContextWindow); ok {
			cw := mi.ContextWindow()
			if cw > config.ReserveTokens {
				config.MaxHistoryTokens = cw - config.ReserveTokens
			}
		}
	}

	return &WindowManager{
		config:    config,
		estimator: estimator,
	}
}

// Truncate 截断消息列表，保留 System 消息，丢弃过旧的对话历史
// 规则：
//  1. 始终保留所有 System 消息（置于开头）
//  2. 保留最近的历史消息，从旧消息开始丢弃
//  3. 如果截断导致 ToolResult 失去对应的 ToolCall，丢弃该孤立的 ToolResult
func (wm *WindowManager) Truncate(msgs []*schema.Message) []*schema.Message {
	if wm == nil || len(msgs) == 0 {
		return msgs
	}

	// 分离 System 和对话消息
	var systemMsgs []*schema.Message
	var convMsgs []*schema.Message
	for _, m := range msgs {
		if m.Role == schema.SystemRole {
			systemMsgs = append(systemMsgs, m)
		} else {
			convMsgs = append(convMsgs, m)
		}
	}

	if len(convMsgs) == 0 {
		return systemMsgs
	}

	// 应用数量限制
	if wm.config.MaxHistoryMessages > 0 && len(convMsgs) > wm.config.MaxHistoryMessages {
		convMsgs = convMsgs[len(convMsgs)-wm.config.MaxHistoryMessages:]
	}

	// 应用 Token 限制
	if wm.config.MaxHistoryTokens > 0 {
		convMsgs = wm.truncateByTokens(convMsgs)
	}

	// 修复工具链完整性：如果第一条是 ToolRole，丢弃它（对应 assistant tool_call 已被截断）
	for len(convMsgs) > 0 && convMsgs[0].Role == schema.ToolRole {
		convMsgs = convMsgs[1:]
	}

	// 修复工具链完整性：如果末尾是 assistant + tool_calls，但对应的 tool result 已被截断
	// 不需要处理这种情况——Agent 循环会处理未完成的工具调用

	// 合并
	result := make([]*schema.Message, 0, len(systemMsgs)+len(convMsgs))
	result = append(result, systemMsgs...)
	result = append(result, convMsgs...)
	return result
}

// truncateByTokens 按 Token 数截断，保留尾部
// 修复：从尾部向前累加，精确找到能放入的起始位置
func (wm *WindowManager) truncateByTokens(msgs []*schema.Message) []*schema.Message {
	maxTokens := wm.config.MaxHistoryTokens
	if maxTokens <= 0 {
		return msgs
	}

	// 从尾部向前累加 token，找到能装下的最早消息
	total := 0
	start := len(msgs)

	for i := len(msgs) - 1; i >= 0; i-- {
		tokens := wm.estimator.Estimate(msgs[i])
		if total+tokens > maxTokens {
			start = i + 1
			break
		}
		total += tokens
		// 如果已经遍历到第一条，说明全部能放下
		if i == 0 {
			start = 0
		}
	}

	if start >= len(msgs) {
		// 极端情况：连一条都放不下，保留最后一条
		return msgs[len(msgs)-1:]
	}
	return msgs[start:]
}

// GetConfig 返回当前生效的配置（便于调试）
func (wm *WindowManager) GetConfig() WindowConfig {
	return wm.config
}
