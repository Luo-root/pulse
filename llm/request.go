package llm

import (
	"context"
	"encoding/json"
)

// ToolDef 是暴露给模型的工具声明（JSON Schema 描述参数）。
type ToolDef struct {
	Name        string
	Description string
	// Parameters 是工具参数的 JSON Schema。nil 表示无参工具。
	Parameters json.RawMessage
}

// ToolChoiceMode 决定模型如何选择工具。
type ToolChoiceMode string

const (
	ToolAuto     ToolChoiceMode = "auto"     // 模型自行决定
	ToolNone     ToolChoiceMode = "none"     // 禁用工具
	ToolAny      ToolChoiceMode = "any"      // 必须调用某个工具（OpenAI: required）
	ToolSpecific ToolChoiceMode = "specific" // 必须调用指定工具
)

// ToolChoice 是工具选择策略；nil 表示 provider 默认（通常为 auto）。
type ToolChoice struct {
	Mode ToolChoiceMode
	Name string // Mode == ToolSpecific 时生效
}

// ResponseFormatType 结构化输出类型。
type ResponseFormatType string

const (
	FormatText       ResponseFormatType = "text"
	FormatJSONObject ResponseFormatType = "json_object" // 保证输出合法 JSON
	FormatJSONSchema ResponseFormatType = "json_schema" // 按 Schema 严格约束
)

// ResponseFormat 要求模型按指定格式输出；nil 表示纯文本。
type ResponseFormat struct {
	Type   ResponseFormatType
	Name   string          // schema 名称（可选）
	Schema json.RawMessage // FormatJSONSchema 时的 JSON Schema
}

// GenerateRequest 是一次对话补全请求的完整描述。
// 零值字段一律表示"交给 provider 默认"，不设魔法默认值——
// 默认策略属于装配层，不属于词汇表。
type GenerateRequest struct {
	Messages []*Message
	Tools    []ToolDef
	ToolChoice *ToolChoice

	Temperature *float64
	TopP        *float64
	MaxTokens   *int

	StopSequences  []string
	ResponseFormat *ResponseFormat

	// Metadata 供上层审计/追踪透传，provider 不理解则忽略；
	// 拦截事件（before_generate）可读取它做路由与计量归因。
	Metadata map[string]any
}

// NewRequest 构造仅含消息列表的请求。
func NewRequest(msgs ...*Message) *GenerateRequest {
	return &GenerateRequest{Messages: msgs}
}

// Clone 深拷贝请求中「拦截改写会触碰」的字段：标量指针、
// ToolChoice、ResponseFormat、Metadata 各自独立副本——waterfall
// 监听器对 Clone 的改写不会污染调用方的原请求。
//
// Messages 与 Tools/StopSequences 为切片级复制、元素共享：消息与
// 工具声明按不可变约定对待（需要变更内容时由调用方自行复制元素）。
func (r *GenerateRequest) Clone() *GenerateRequest {
	cp := *r
	cp.Messages = append([]*Message{}, r.Messages...)
	cp.Tools = append([]ToolDef{}, r.Tools...)
	cp.StopSequences = append([]string{}, r.StopSequences...)
	if r.Temperature != nil {
		v := *r.Temperature
		cp.Temperature = &v
	}
	if r.TopP != nil {
		v := *r.TopP
		cp.TopP = &v
	}
	if r.MaxTokens != nil {
		v := *r.MaxTokens
		cp.MaxTokens = &v
	}
	if r.ToolChoice != nil {
		tc := *r.ToolChoice
		cp.ToolChoice = &tc
	}
	if r.ResponseFormat != nil {
		rf := *r.ResponseFormat
		cp.ResponseFormat = &rf
	}
	if r.Metadata != nil {
		md := make(map[string]any, len(r.Metadata))
		for k, v := range r.Metadata {
			md[k] = v
		}
		cp.Metadata = md
	}
	return &cp
}

// FinishReason 解释生成结束的原因。
type FinishReason string

const (
	FinishStop          FinishReason = "stop"           // 自然结束
	FinishToolCalls     FinishReason = "tool_calls"     // 模型请求调用工具
	FinishLength        FinishReason = "length"         // 达到 token 上限
	FinishContentFilter FinishReason = "content_filter" // 被安全策略截断
	FinishError         FinishReason = "error"
)

// TokenUsage 是一次调用的 token 计量。
type TokenUsage struct {
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int // 命中 prompt cache 的输入部分；0 表示未知
}

// Total 返回已知输入 + 输出的总量。
func (u TokenUsage) Total() int { return u.InputTokens + u.OutputTokens }

// Response 是非流式调用的完整结果。
type Response struct {
	Message      *Message // 助手回复（text / tool_call / reasoning 块）
	FinishReason FinishReason
	Usage        TokenUsage
}

// ---- 流式 ----

// StreamEventKind 标记流事件的类型。
type StreamEventKind string

const (
	EventTextDelta      StreamEventKind = "text_delta"       // Text 增量
	EventReasoningDelta StreamEventKind = "reasoning_delta"  // 思维链增量
	EventToolCallBegin  StreamEventKind = "tool_call_begin"  // Index 号工具调用开始（CallID/Name 就绪）
	EventToolCallDelta  StreamEventKind = "tool_call_delta"  // Index 号工具调用参数增量（Text 为 JSON 片段）
	EventError          StreamEventKind = "error"            // 流中失败（Err 非 nil），此后关闭
	EventDone           StreamEventKind = "done"             // 正常结束（Response 为聚合结果），此后关闭
)

// StreamEvent 是流式响应的最小事件单元。
//
// 消费契约：channel 在 EventDone 或 EventError 后必然关闭；
// ctx 取消时以 EventError 收尾并关闭，消费方 range 即可。
type StreamEvent struct {
	Kind  StreamEventKind
	Index int // 内容块序号：并发工具调用靠它区分

	Text     string // text/reasoning/tool_call 参数 的增量片段
	CallID   string // tool_call_begin 时有效
	ToolName string // tool_call_begin 时有效

	Response *Response // EventDone 时携带聚合结果（含 Usage/FinishReason）
	Err      error     // EventError 时非 nil
}

// ChatModel 是 provider 中立的对话模型接口——消费方（agent-loop、
// 批处理、评测）只依赖这两个方法。
//
// 实现契约：
//   - Generate 与 Stream 语义一致，Stream 只是增量的 Generate；
//   - 两个方法都必须可被 ctx 取消；
//   - 返回的错误应为本包 [Error]（带分类），便于统一 failover。
type ChatModel interface {
	Generate(ctx context.Context, req *GenerateRequest) (*Response, error)
	Stream(ctx context.Context, req *GenerateRequest) (<-chan StreamEvent, error)
}
