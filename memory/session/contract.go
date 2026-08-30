package session

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
)

// FormatVersion 是当前事件日志格式版本。header 版本不兼容时拒绝加载，
// 不做「猜测迁移」。
const FormatVersion uint32 = 1

// EventType 标识一类事件。类型不散落为裸字符串：每个进入日志的类型都在
// Registry 注册 codec 与分级；插件私有扩展用 "plugin/<name>/<event>" 命名
// 并自行注册为 Ignorable。
type EventType string

// 最小核心事件族（设计 §6.3）。
const (
	EventTurnStarted      EventType = "turn.started"
	EventTurnEnded        EventType = "turn.ended"
	EventStepStarted      EventType = "step.started"
	EventStepEnded        EventType = "step.ended"
	EventMessageUser      EventType = "message.user"
	EventMessageAssistant EventType = "message.assistant"
	EventToolCalled       EventType = "tool.called"
	EventToolResult       EventType = "tool.result"
	EventAssistantChunk   EventType = "assistant.chunk"
	EventRequestHeader    EventType = "request.header"
	EventRequestRoute     EventType = "request.route"
)

// Classification 是事件分级（§6.3 评审定案表）。分级由 Registry 注册时
// 钉死；信封上的 Ignorable flag 只对未知扩展类型有意义。
type Classification string

const (
	// ClassRequired：缺了 fold 出的 surface 与配对就不完整，永不跳过。
	ClassRequired Classification = "required"
	// ClassIgnorable：流碎片与调用环境，丢了不影响 surface 正确性。
	// 注意 Ignorable ≠ 可以不记（写入方仍必须发 request.header 三样）。
	ClassIgnorable Classification = "ignorable"
)

// SurfaceOpKind 区分 surface 变更方式。P2-A 只有 Append；Replace 由
// P2-B 的 compaction.checkpoint 引入（专用事件类型，不得伪装 message.user）。
type SurfaceOpKind string

const (
	SurfaceAppend  SurfaceOpKind = "append"
	SurfaceReplace SurfaceOpKind = "replace"
)

// SurfaceIntent 描述一条事件对模型 surface 的变更意图。
//
// Replace 的 Start/End 是当前 fold 后 surface 的 0-based 消息下标（含端点），
// 不复用 event Seq——两者是不同数轴，数值序不可互相推断。P2-A 阶段没有
// 注册任何 Replace 事件类型，携带 Replace 意图的 Append 会被拒绝。
type SurfaceIntent struct {
	Op SurfaceOpKind `json:"op"` // Append / Replace
	// Start/End 仅 Replace 使用：当前 surface 的 0-based 消息下标（含端点）；
	// Append 忽略。合法范围 Start ≤ End，越界或反向由 fold/Append 拒绝。
	Start int `json:"start,omitempty"`
	End   int `json:"end,omitempty"`
	// Sources 是生成或替代的源事件 Seq（审计锚点）。
	Sources []uint64 `json:"sources,omitempty"`
}

// EventEnvelope 是日志中的一条事件。Seq / Time 由 Store 分配，调用方不填；
// 信封一旦写入视为不可变。
type EventEnvelope struct {
	Seq       uint64          `json:"seq"` // session 内严格连续，从 1 起
	Time      time.Time       `json:"time"`
	Type      EventType       `json:"type"`
	Data      json.RawMessage `json:"data,omitempty"`      // codec 校验后的 payload；允许 nil（无载荷）
	Ignorable bool            `json:"ignorable,omitempty"` // 未知类型被跳过的唯一凭据；已知类型以注册表分级为准
	Surface   *SurfaceIntent  `json:"surface,omitempty"`   // 仅注册为 surface 的类型允许非 nil
}

// EventDraft 是 Append 的写入入口；Seq / Time 由 Store 分配，调用方不填。
type EventDraft struct {
	Type EventType
	Data json.RawMessage // codec 编码后的 payload
	// Surface 仅允许注册为 surface 的事件类型非 nil。
	Surface *SurfaceIntent
	// Ignorable 只对未知扩展类型有意义（§6.3 裁决表）；已知类型忽略此
	// flag，以注册表分级为准。
	Ignorable bool
}

// SessionHeader 是日志外的存储元数据，不混进模型 history。
type SessionHeader struct {
	FormatVersion   uint32    `json:"formatVersion"`
	SessionID       string    `json:"sessionID"`
	CreatedAt       time.Time `json:"createdAt"`
	Workspace       string    `json:"workspace,omitempty"`
	ParentSessionID string    `json:"parentSessionID,omitempty"` // fork 时存在
	SeedLength      uint64    `json:"seedLength,omitempty"`      // fork 继承的事件边界（父会话 Seq）
	AgentID         string    `json:"agentID,omitempty"`
	AgentPreset     string    `json:"agentPreset,omitempty"`
	DelegationDepth uint32    `json:"delegationDepth,omitempty"`
}

// SessionFilter 是 List 的最小过滤：零值 = 全部；After 是上一页末尾的
// SessionID 游标。列表按 CreatedAt 降序 + SessionID tiebreak 稳定排序 +
// 游标分页，不静默截断。
type SessionFilter struct {
	After string
}

// SessionStore 是会话存储。它是存储，不是插件树：接口吃 context.Context
// 做取消，不把 *kernel.Context 焊进来；Provide 它的 Plugin 才碰 kernel。
type SessionStore interface {
	// Create 以给定 header 建会话。SessionID 为空时由 store 生成；
	// CreatedAt 零值取当前时间；FormatVersion 零值取本包版本，非零且
	// 不兼容时拒绝。同 ID 已存在 → ErrSessionExists（单写者：拒绝第二写者）。
	Create(ctx context.Context, header SessionHeader) (Session, error)
	// Open 打开既有会话。对内存实现即冷恢复路径：发现未闭合 turn/step
	// 或 unpaired ToolCall 时合成闭合事件真实写回日志，再返回；已闭合
	// 则原样返回。恢复临界区互斥，第二写者 → ErrWriterBusy。
	Open(ctx context.Context, id string) (Session, error)
	// List 会话列表：CreatedAt 降序 + SessionID tiebreak，游标分页。
	List(ctx context.Context, filter SessionFilter) (page []SessionHeader, next string, err error)
	// Delete 丢弃整个会话（JSONL 文件 + blobs 的 A2 语义）。会话不是
	// MemoryItem，不适用 Supersede/Revoke；数据不可恢复由宿主负责。
	Delete(ctx context.Context, id string) error
}

// Session 是一个可追加的事件日志会话。同一 session 同一时刻一个 writer
// （进程内锁；文件锁兜底在 P2-A2），Append 非幂等。
type Session interface {
	// Header 返回会话头（快照语义，调用方可持有）。
	Header() SessionHeader
	// Append 追加一条事件，返回分配了 Seq/Time 的信封。非幂等：宿主
	// Flush 失败后不要原样重放同一批事件（重新 Append 会产生双份）。
	Append(ctx context.Context, draft EventDraft) (EventEnvelope, error)
	// Events 返回 Seq >= fromSeq 的事件拷贝（fromSeq == 0 表示全部）。
	Events(ctx context.Context, fromSeq uint64) ([]EventEnvelope, error)
	// Surface 把日志折成模型消息投影：[]*llm.Message，不含 system
	// （归宿主/Assembler），assistant.chunk 永不进 surface，Parts 原样。
	Surface(ctx context.Context) ([]*llm.Message, error)
	// Fork 以 atSeq 为切点派生子会话（拷贝 Seq 1..atSeq 的日志为 seed）。
	// 切点落在 tool 组中间（assistant 的 ToolCall 无对应 result）→ 拒绝，
	// 不拷出非法 surface。
	Fork(ctx context.Context, atSeq uint64) (Session, error)
	// Flush 把已写入事件固化到持久层。内存实现是成功空操作（语义占位）；
	// JSONL/SQLite 实现里 Flush 才 fsync，崩溃只保证 Flush 点之前。
	Flush(ctx context.Context) error
}

// SessionStoreKey 是 memory/session 的 kernel 服务键（对齐 toolset.ServiceKey
// 先例：service key 归 memory/* 各包，kernel 不 import memory）。
var SessionStoreKey = kernel.NewServiceKey[SessionStore]("memory.session.store")

// 恢复合成事件使用的固定 reason / 文案（§9.3：固定文案，如 interrupted）。
const (
	ReasonInterrupted = "interrupted"
	ReasonCompleted   = "completed"
	// interruptedResultText 是 unpaired ToolCall 补齐的固定结果文案。
	interruptedResultText = "interrupted"
)

// 包内哨兵错误。调用方用 errors.Is 判别；包装时保留语义。
var (
	// ErrSessionExists：Create 同 ID 已存在（拒绝第二写者）。
	ErrSessionExists = errors.New("session: session already exists")
	// ErrSessionNotFound：Open/Delete 不存在的会话。
	ErrSessionNotFound = errors.New("session: session not found")
	// ErrWriterBusy：恢复临界区被占（单写者保护）。
	ErrWriterBusy = errors.New("session: another writer holds this session")
	// ErrDeleted：会话已 Delete，拒绝继续写入。
	ErrDeleted = errors.New("session: session deleted")
	// ErrUnknownEvent：Append 未注册类型且未标 Ignorable（fail closed）。
	ErrUnknownEvent = errors.New("session: unknown event type without Ignorable")
	// ErrUnknownRequired：恢复/折叠遇到未注册且不可跳过的事件——拒绝 Open。
	ErrUnknownRequired = errors.New("session: unknown required event type")
	// ErrSurfaceNotAllowed：非 surface 类型携带 SurfaceIntent。
	ErrSurfaceNotAllowed = errors.New("session: surface intent on non-surface event type")
	// ErrReplaceNotSupported：本阶段没有注册任何 Replace 事件类型
	// （compaction.checkpoint 在 P2-B 引入）。
	ErrReplaceNotSupported = errors.New("session: surface replace has no registered event type")
	// ErrReplaceRange：Replace 范围反向（Start > End）或越界。
	ErrReplaceRange = errors.New("session: replace range invalid")
	// ErrPayloadInvalid：payload 缺失、形状错误或缺必填字段。
	ErrPayloadInvalid = errors.New("session: event payload invalid")
	// ErrForkSplitToolGroup：Fork 切点落在 tool 组中间。
	ErrForkSplitToolGroup = errors.New("session: fork boundary splits a tool call/result group")
	// ErrForkBadAt：Fork 切点越界（0 或超出当前最大 Seq）。
	ErrForkBadAt = errors.New("session: fork boundary out of range")
	// ErrFormatVersion：header 格式版本不兼容，拒绝加载。
	ErrFormatVersion = errors.New("session: format version incompatible")
	// ErrEventRegistered：重复注册已存在的事件类型。
	ErrEventRegistered = errors.New("session: event type already registered")
	// ErrCursorStale：List 游标指向的会话已不存在（列表已变化）。调用方
	// 应重置分页（After 清空从第一页开始）；不静默从头——那会重复返回。
	ErrCursorStale = errors.New("session: list cursor stale")
	// ErrCorruptLog：持久日志损坏（中部坏行、seq 断链、checksum 不符），
	// 拒绝加载（fail closed），不做「猜测修复」。
	ErrCorruptLog = errors.New("session: event log corrupt")
	// ErrSessionClosed：文件实现 Close 之后的写入/Flush——显式哨兵，
	// 不依赖 os.File 的 nil-safe 巧合。
	ErrSessionClosed = errors.New("session: session closed")
	// ErrInvalidSessionID：会话 ID 非法（须匹配 [A-Za-z0-9_-]{1,128}）——
	// 参数错误，不是「会话不存在」。
	ErrInvalidSessionID = errors.New("session: invalid session id")
)
