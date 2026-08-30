package session

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/Luo-root/pulse/llm"
)

// Codec 校验一个事件类型的 payload。校验在 Append 入库前执行——事件数据
// 仅允许可无损 JSON 的值，避免存储时才发现不可编码（设计 §6.2）。
// Data 为 nil 时 codec 也会被调用，由各校验器决定「无载荷」是否合法。
type Codec func(data json.RawMessage) error

// codecEntry 是 Registry 中一个事件类型的绑定：分级 + 是否允许 surface + codec。
type codecEntry struct {
	class      Classification
	hasSurface bool
	codec      Codec
}

// Registry 把 EventType 绑定到 payload codec、分级与 surface 许可（设计
// §6.2：事件类型不散落为无约束字符串，经注册表统一裁决）。
//
// NewRegistry 自带最小核心事件族；插件扩展调用 Register 注册自己的类型
// （通常为 Ignorable）。重复注册同一类型会被拒绝，内置类型不可覆盖。
type Registry struct {
	mu      sync.RWMutex
	entries map[EventType]codecEntry
}

// NewRegistry 创建带最小核心事件族（设计 §6.3 分级表）的注册表。
func NewRegistry() *Registry {
	r := &Registry{entries: make(map[EventType]codecEntry, 16)}
	// Required：生命周期、消息、工具。缺了 fold 出的 surface 与配对就不完整。
	r.mustRegister(EventTurnStarted, ClassRequired, false, validateLifecycle)
	r.mustRegister(EventTurnEnded, ClassRequired, false, validateLifecycle)
	r.mustRegister(EventStepStarted, ClassRequired, false, validateLifecycle)
	r.mustRegister(EventStepEnded, ClassRequired, false, validateLifecycle)
	r.mustRegister(EventMessageUser, ClassRequired, true, validateMessage)
	r.mustRegister(EventMessageAssistant, ClassRequired, true, validateMessage)
	r.mustRegister(EventToolCalled, ClassRequired, false, validateToolCalled)
	r.mustRegister(EventToolResult, ClassRequired, true, validateToolResult)
	// Ignorable：流碎片与调用环境。Ignorable ≠ 可以不记——request.header
	// 仍必须由写入方发出（system + ToolDef + model 三样）。
	r.mustRegister(EventAssistantChunk, ClassIgnorable, false, validateChunk)
	r.mustRegister(EventRequestHeader, ClassIgnorable, false, validateRequestHeader)
	r.mustRegister(EventRequestRoute, ClassIgnorable, false, validateRequestRoute)
	r.mustRegister(EventRequestUsage, ClassIgnorable, false, validateRequestUsage)
	// P2-B 压缩族：started/summarized/ended 是 log-only 审计锁（Required——
	// 崩溃恢复要靠 started 识别「未闭合 compaction 视作失败尝试」）；
	// checkpoint 是唯一的 surface Replace 事件（fold 成稳定前缀消息，
	// Role 由 codec 钉死 RoleUser，不得伪装 message.user）。
	r.mustRegister(EventCompactionStarted, ClassRequired, false, validateCompactionStatus)
	r.mustRegister(EventCompactionSummarized, ClassRequired, false, validateCompactionStatus)
	r.mustRegister(EventCompactionEnded, ClassRequired, false, validateCompactionStatus)
	r.mustRegister(EventCompactionCheckpoint, ClassRequired, true, validateCompactionCheckpoint)
	return r
}

func (r *Registry) mustRegister(t EventType, class Classification, surface bool, codec Codec) {
	if err := r.Register(t, class, surface, codec); err != nil {
		panic(fmt.Sprintf("session: register built-in event %q: %v", t, err))
	}
}

// Register 注册一个事件类型。同一类型重复注册返回 ErrEventRegistered；
// 内置类型不可覆盖。
func (r *Registry) Register(t EventType, class Classification, allowSurface bool, codec Codec) error {
	if t == "" {
		return fmt.Errorf("%w: empty event type", ErrPayloadInvalid)
	}
	if class != ClassRequired && class != ClassIgnorable {
		return fmt.Errorf("%w: unknown classification %q for %q", ErrPayloadInvalid, class, t)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.entries[t]; dup {
		return fmt.Errorf("%w: %q", ErrEventRegistered, t)
	}
	r.entries[t] = codecEntry{class: class, hasSurface: allowSurface, codec: codec}
	return nil
}

// lookup 返回类型的绑定；known == false 表示未注册。
func (r *Registry) lookup(t EventType) (entry codecEntry, known bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[t]
	return e, ok
}

// ---- 最小核心事件族的 payload 类型 ----
//
// payload 只存模型可见形态与具名审计字段；禁止把整包 GenerateRequest.Metadata
// （map）或 API key 塞进日志——类型层面就没有这些字段。

// MessagePayload 是 message.user / message.assistant 的载荷：消息内容块
// 原样序列化（含 PartToolCall / PartReasoning；llm.Part 各字段均可无损
// JSON roundtrip，ImageData 走 base64）。
type MessagePayload struct {
	Parts []llm.Part `json:"parts"`
}

// ToolCalledPayload 是 tool.called 的载荷。该事件 log-only，仅供 HITL/
// 时序/崩溃检测当「调用已发生」的审计锚点，不进 surface。
type ToolCalledPayload struct {
	ToolCallID string          `json:"toolCallID"`
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
}

// ToolResultPayload 是 tool.result 的载荷：只存模型可见形态 IsError + 文本
// （对齐 loop 回传模型的 toolResultMsg 契约）。不存 Go error 接口——无法
// 无损 JSON。
type ToolResultPayload struct {
	ToolCallID string `json:"toolCallID"`
	Text       string `json:"text"`
	IsError    bool   `json:"isError"`
}

// ChunkPayload 是 assistant.chunk 的载荷：流碎片，UI 重放与诊断用。
// 完整消息（message.assistant）是权威；chunk 永不进 surface。
type ChunkPayload struct {
	Text string `json:"text"`
}

// RequestHeaderPayload 是 request.header 的载荷：无损记录重建调用环境的
// 三样——system 文本（或显式无）、本回合工具声明快照、model/route 标识。
// 缺 header 的会话可以 fold 出消息，但重放/续跑是降级（没工具集、没 system）。
type RequestHeaderPayload struct {
	// System 为 nil 表示「显式无 system」；有值即 system 文本。
	System   *string       `json:"system,omitempty"`
	ToolDefs []llm.ToolDef `json:"toolDefs,omitempty"`
	Model    string        `json:"model"`
}

// RequestRoutePayload 是 request.route 的载荷：本回合路由标识。P2-B 的
// token usage / meter 是另立事件族，不塞进这里。
type RequestRoutePayload struct {
	Model string `json:"model"`
}

// LifecyclePayload 是 turn/step 生命周期事件的载荷。started 可只带 ID；
// ended 带 Reason（interrupted / completed 等，开放字符串）。
type LifecyclePayload struct {
	ID     string `json:"id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// CompactionStatusPayload 是 compaction.started / summarized / ended 的
// 载荷（log-only 审计）：started 带 ID 锁；summarized 记录摘要模型与
// usage、来源 refs；ended 收口。恢复路径不补 ended——未闭合 compaction
// 在日志里保持可见（视作失败尝试，不假装完成，§9.1）。
type CompactionStatusPayload struct {
	ID           string   `json:"id"`
	Reason       string   `json:"reason,omitempty"`
	Model        string   `json:"model,omitempty"`
	InputTokens  int      `json:"inputTokens,omitempty"`
	OutputTokens int      `json:"outputTokens,omitempty"`
	SourceRefs   []uint64 `json:"sourceRefs,omitempty"`
}

// CompactionCheckpointPayload 是 compaction.checkpoint 的载荷：Replace
// 窗口替换后的新 surface 节点集（每条 Role 必须是 user/assistant/tool，
// 压缩摘要场景为单条 RoleUser 稳定前缀消息——不得伪装 message.user 事件
// 类型，也不得携带 RoleSystem，system 归宿主/Assembler）+ 被替代窗口的
// canonical event seq 全集（审计锚点，重放可追溯）。
type CompactionCheckpointPayload struct {
	Messages []llm.Message `json:"messages"`
	Replaced []uint64      `json:"replaced"`
}

// RequestUsagePayload 是 request.usage 的载荷（Ignorable，log-only 审计）：
// 具名字段对齐 llm.TokenUsage，禁整包 Metadata（map）与 API key。
type RequestUsagePayload struct {
	Model             string `json:"model"`
	InputTokens       int    `json:"inputTokens,omitempty"`
	OutputTokens      int    `json:"outputTokens,omitempty"`
	CachedInputTokens int    `json:"cachedInputTokens,omitempty"`
}

// ---- 校验器 ----

func decode(data json.RawMessage, out any) error {
	if len(data) == 0 {
		return fmt.Errorf("%w: payload required", ErrPayloadInvalid)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: %v", ErrPayloadInvalid, err)
	}
	return nil
}

func validateMessage(data json.RawMessage) error {
	var p MessagePayload
	return decode(data, &p)
}

func validateToolCalled(data json.RawMessage) error {
	var p ToolCalledPayload
	if err := decode(data, &p); err != nil {
		return err
	}
	if p.ToolCallID == "" || p.Name == "" {
		return fmt.Errorf("%w: tool.called requires toolCallID and name", ErrPayloadInvalid)
	}
	if len(p.Arguments) > 0 && !json.Valid(p.Arguments) {
		return fmt.Errorf("%w: tool.called.arguments not valid JSON", ErrPayloadInvalid)
	}
	return nil
}

func validateToolResult(data json.RawMessage) error {
	var p ToolResultPayload
	if err := decode(data, &p); err != nil {
		return err
	}
	if p.ToolCallID == "" {
		return fmt.Errorf("%w: tool.result requires toolCallID", ErrPayloadInvalid)
	}
	return nil
}

func validateChunk(data json.RawMessage) error {
	var p ChunkPayload
	return decode(data, &p)
}

func validateRequestHeader(data json.RawMessage) error {
	var p RequestHeaderPayload
	if err := decode(data, &p); err != nil {
		return err
	}
	if p.Model == "" {
		return fmt.Errorf("%w: request.header requires model", ErrPayloadInvalid)
	}
	return nil
}

func validateRequestRoute(data json.RawMessage) error {
	var p RequestRoutePayload
	if err := decode(data, &p); err != nil {
		return err
	}
	if p.Model == "" {
		return fmt.Errorf("%w: request.route requires model", ErrPayloadInvalid)
	}
	return nil
}

// validateLifecycle 允许无载荷（len 0）与任意开放 reason；只校验形状。
func validateLifecycle(data json.RawMessage) error {
	if len(data) == 0 {
		return nil
	}
	var p LifecyclePayload
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("%w: %v", ErrPayloadInvalid, err)
	}
	return nil
}

func validateCompactionStatus(data json.RawMessage) error {
	var p CompactionStatusPayload
	if err := decode(data, &p); err != nil {
		return err
	}
	if p.ID == "" {
		return fmt.Errorf("%w: compaction status requires id", ErrPayloadInvalid)
	}
	return nil
}

// validateCompactionCheckpoint：至少一条新节点；Role 限 user/assistant/
// tool（压缩摘要是 RoleUser 稳定前缀消息，见 fold 映射表）；Replaced
// 非空——没有替代任何节点的 Replace 没有意义。
func validateCompactionCheckpoint(data json.RawMessage) error {
	var p CompactionCheckpointPayload
	if err := decode(data, &p); err != nil {
		return err
	}
	if len(p.Messages) == 0 {
		return fmt.Errorf("%w: checkpoint requires at least one message", ErrPayloadInvalid)
	}
	for i := range p.Messages {
		switch p.Messages[i].Role {
		case llm.RoleUser, llm.RoleAssistant, llm.RoleTool:
		default:
			return fmt.Errorf("%w: checkpoint message[%d] role %q not allowed", ErrPayloadInvalid, i, p.Messages[i].Role)
		}
	}
	if len(p.Replaced) == 0 {
		return fmt.Errorf("%w: checkpoint requires non-empty Replaced", ErrPayloadInvalid)
	}
	return nil
}

func validateRequestUsage(data json.RawMessage) error {
	var p RequestUsagePayload
	if err := decode(data, &p); err != nil {
		return err
	}
	if p.Model == "" {
		return fmt.Errorf("%w: request.usage requires model", ErrPayloadInvalid)
	}
	return nil
}
