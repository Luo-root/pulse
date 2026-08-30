// Package store 是 P2-C 的长期记忆 canonical store：MemoryItem 的
// Put/Get/Search/Supersede/Revoke。
//
// # 定位与不变式
//
//   - Namespace 是 canonical 键：权限、隔离、检索都走 Namespace；
//     MemoryScope 只是构造 helper（展开成 Namespace 层级），不与
//     Namespace 并存两套真相。可见性 = Namespace 前缀匹配：父 scope
//     读得到子 scope 的 item，兄弟 scope 绝不互见。
//   - 禁止物理 DELETE（§17.7-1）：接口无 Delete；同一事实的更新走
//     Supersede（旧 item Status=Superseded，新 item 入库），撤销走
//     Revoke（Status=Revoked + 审计）——两者都可追溯。
//   - SourceRefs 是防幻觉的根：每条 active 记忆必须回链 session/event
//     或人工输入；无来源的 Put 被拒（§10.2）。
//   - 并发写用 revision CAS（§13.1：MemoryItem CAS 是 P2-C 验收；
//     session 单写者锁是 P2-A 的事，两者不相干）。
//   - Confidence 检索排序不依赖：P2-C 无 scoring 产出方，写入方必须
//     显式提供（scoring 属 P2-D）。
//
// # 分层纪律
//
// 独立包：不 import llm / session / kernel（§7.1 import 图）；service
// key（MemoryStoreKey）归本包，kernel 不 import memory。
//
// 设计全貌见 docs/design/memory-layer-research-and-v2-design.md §6.5/
// §10/§13.1；实现票 #76（C1）、C2（SQLite+FTS）、C3（Assembler）。
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Luo-root/pulse/kernel"
)

// MemoryStoreKey 是 memory/store 的 kernel 服务键（对齐 toolset.ServiceKey
// 先例：service key 归 memory/* 各包）。
var MemoryStoreKey = kernel.NewServiceKey[MemoryStore]("memory.store")

// MemoryKind 是记忆的领域类别：open string + 已知常量（宿主可定义自己的
// 类别，框架不闭枚举）。
type MemoryKind string

// 已知 MemoryKind（§6.5）。
const (
	KindProfile     MemoryKind = "profile"
	KindDecision    MemoryKind = "decision"
	KindEnvironment MemoryKind = "environment"
	KindEpisode     MemoryKind = "episode"
	KindLesson      MemoryKind = "lesson"
)

// Valid 报告 k 是否本包已知类别（宿主自定义类别返回 false——调用方自行
// 决定是否放行，本包不做白名单拦截）。
func (k MemoryKind) Valid() bool {
	switch k {
	case KindProfile, KindDecision, KindEnvironment, KindEpisode, KindLesson:
		return true
	}
	return false
}

// MemoryStatus 是 item 状态机：Active（生效）/ Superseded（被新版本替代）/
// Revoked（显式撤销）/ Pending（P2-D candidate 的占位状态——本票写入路径
// 不产生）。禁止物理 DELETE：Superseded/Revoked 的 item 永久可查。
type MemoryStatus string

// 已知 MemoryStatus。
const (
	StatusActive     MemoryStatus = "active"
	StatusSuperseded MemoryStatus = "superseded"
	StatusRevoked    MemoryStatus = "revoked"
	StatusPending    MemoryStatus = "pending"
)

// Valid 报告 s 是否已知状态（未知状态在 Put/校验时拒绝——fail closed）。
func (s MemoryStatus) Valid() bool {
	switch s {
	case StatusActive, StatusSuperseded, StatusRevoked, StatusPending:
		return true
	}
	return false
}

// TaintLevel 是记忆的信任分级：open string + 已知常量。taint gate 的
// 执行方是 policy（P2-D 抽取链），本包只承载与校验。
type TaintLevel string

// 已知 TaintLevel（§6.5）。
const (
	TaintTrusted      TaintLevel = "trusted"
	TaintUserSupplied TaintLevel = "user-supplied"
	TaintUntrustedExt TaintLevel = "untrusted-external"
)

// Valid 报告 t 是否已知分级。
func (t TaintLevel) Valid() bool {
	switch t {
	case TaintTrusted, TaintUserSupplied, TaintUntrustedExt:
		return true
	}
	return false
}

// SourceType 是来源类别：session 回链（防幻觉根）、人工输入、外部导入。
type SourceType string

// 已知 SourceType。
const (
	SourceSession  SourceType = "session"
	SourceManual   SourceType = "manual"
	SourceExternal SourceType = "external"
)

// Valid 报告 t 是否已知来源类别（未知来源拒绝——来源是审计根，不能开放）。
func (t SourceType) Valid() bool {
	switch t {
	case SourceSession, SourceManual, SourceExternal:
		return true
	}
	return false
}

// SourceRef 是一条记忆的来源锚点：session 来源回链到 SessionID+Seq（可
// 定位到 canonical event）；manual/external 用 Ref 承载自由描述。
type SourceRef struct {
	Type      SourceType `json:"type"`
	SessionID string     `json:"sessionID,omitempty"` // Type == session
	Seq       uint64     `json:"seq,omitempty"`       // Type == session
	Ref       string     `json:"ref,omitempty"`       // manual / external
}

// Validate 校验来源锚点的形状：类型已知；session 来源必须带 SessionID
// 与正 Seq；manual/external 必须带非空 Ref。
func (r SourceRef) Validate() error {
	if !r.Type.Valid() {
		return fmt.Errorf("%w: unknown source type %q", ErrInvalidItem, r.Type)
	}
	switch r.Type {
	case SourceSession:
		if r.SessionID == "" || r.Seq == 0 {
			return fmt.Errorf("%w: session source requires sessionID and seq", ErrInvalidItem)
		}
	default:
		if strings.TrimSpace(r.Ref) == "" {
			return fmt.Errorf("%w: %s source requires ref", ErrInvalidItem, r.Type)
		}
	}
	return nil
}

// MemoryScope 是 namespace 的构造 helper：按固定顺序展开成自描述键值
// 层级（空字段跳过）。Namespace 才是 canonical——scope 不参与存储与检索
// 的判定，只负责人类可读地构造层级。
type MemoryScope struct {
	TenantID    string
	UserID      string
	ProjectID   string
	WorkspaceID string
	AgentID     string
}

// Namespace 按固定顺序展开 scope：tenant → user → project → workspace →
// agent，形如 ["tenant:acme", "user:u1", "project:p1"]。新维度（如
// TeamID）追加层级即可，不改结构体。
func (s MemoryScope) Namespace() []string {
	parts := make([]string, 0, 5)
	add := func(prefix, id string) {
		if id != "" {
			parts = append(parts, prefix+":"+id)
		}
	}
	add("tenant", s.TenantID)
	add("user", s.UserID)
	add("project", s.ProjectID)
	add("workspace", s.WorkspaceID)
	add("agent", s.AgentID)
	return parts
}

// MemoryItem 是长期记忆的 canonical record（§6.5）。字段语义见设计文；
// 本包不改字段集（Revoke 的 reason 走 store 审计，不进结构体）。
type MemoryItem struct {
	ID         string
	Namespace  []string
	Kind       MemoryKind
	Content    string
	Structured json.RawMessage
	Status     MemoryStatus
	Confidence float32
	SourceRefs []SourceRef
	ValidFrom  time.Time
	ValidUntil *time.Time
	KnownAt    time.Time // 摄取时间（bi-temporal 最小占位，§17.7-2）
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Revision   uint64
	Taint      TaintLevel
}

// validate 是 Put/Supersede 的入库前校验（fail closed）。
func (it MemoryItem) validate() error {
	if strings.TrimSpace(it.ID) == "" {
		return fmt.Errorf("%w: empty id", ErrInvalidItem)
	}
	if len(it.Namespace) == 0 {
		return fmt.Errorf("%w: empty namespace", ErrInvalidItem)
	}
	for _, ns := range it.Namespace {
		if strings.TrimSpace(ns) == "" {
			return fmt.Errorf("%w: empty namespace element", ErrInvalidItem)
		}
	}
	if !it.Kind.Valid() && strings.TrimSpace(string(it.Kind)) == "" {
		return fmt.Errorf("%w: empty kind", ErrInvalidItem)
	}
	if strings.TrimSpace(it.Content) == "" {
		return fmt.Errorf("%w: empty content", ErrInvalidItem)
	}
	if !it.Status.Valid() {
		return fmt.Errorf("%w: unknown status %q", ErrInvalidItem, it.Status)
	}
	if !it.Taint.Valid() && strings.TrimSpace(string(it.Taint)) == "" {
		return fmt.Errorf("%w: empty taint", ErrInvalidItem)
	}
	if len(it.SourceRefs) == 0 {
		return fmt.Errorf("%w: at least one source ref is required", ErrInvalidItem)
	}
	for i, r := range it.SourceRefs {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("sourceRefs[%d]: %w", i, err)
		}
	}
	if it.Status == StatusActive && it.Confidence <= 0 {
		// P2-C 无 scoring 产出方：active 记忆必须显式给置信度（建议 1.0）；
		// 排序不得依赖没人写的值（评审定案）。
		return fmt.Errorf("%w: active item requires explicit confidence", ErrInvalidItem)
	}
	if len(it.Structured) > 0 && !json.Valid(it.Structured) {
		return fmt.Errorf("%w: structured is not valid JSON", ErrInvalidItem)
	}
	if it.ValidUntil != nil && it.ValidUntil.Before(it.ValidFrom) {
		return fmt.Errorf("%w: validUntil before validFrom", ErrInvalidItem)
	}
	return nil
}

// MemoryQuery 是 Search 的过滤条件：Namespace 前缀匹配 + Kind 过滤 +
// 关键词（C1 内存版子串匹配；FTS 是 C2）+ 状态开关。零值 = 全 namespace
// 全 Kind，仅 Active。
type MemoryQuery struct {
	// Namespace 是前缀过滤：item.Namespace 以它为前缀即可见；空 = 不过滤。
	Namespace []string
	// Kinds 非空时只返回这些类别。
	Kinds []MemoryKind
	// Query 关键词：内存版对 Content 做大小写不敏感子串匹配；空 = 不过滤
	// （FTS 全文检索在 C2）。
	Query string
	// IncludeInactive 打开后 Superseded/Revoked/Pending 也返回（默认只
	// Active——同一事实永远只有一条生效版本）。
	IncludeInactive bool
	// Limit 上限（<=0 不限）；超限截断到 Limit——Search 不是游标分页面
	// （列表语义与 session.List 不同），Limit 语义在票面声明为硬上限。
	Limit int
}

// MemoryHit 是 Search 命中：item 本体（检索排序与 scoring 属 P2-D，C1
// 不返回分数——未命中不伪造，命中不编造相关性）。
type MemoryHit struct {
	Item MemoryItem
}

// PutMemoryOptions 是 Put 的写入选项：ExpectedRevision 即 CAS。
type PutMemoryOptions struct {
	// ExpectedRevision 为 0 表示新建（ID 不得已存在）；>0 表示更新，必须
	// 与当前 Revision 一致，否则 ErrRevisionConflict。
	ExpectedRevision uint64
}

// MemoryStore 是长期记忆 canonical store（§7.1）。禁止物理 DELETE。
type MemoryStore interface {
	// Put 新建或更新（CAS）。item.Status 由写入方给（一般 Active）；
	// Revision/KnownAt/CreatedAt/UpdatedAt 由 store 分配，调用方不填。
	Put(ctx context.Context, item MemoryItem, opts PutMemoryOptions) (MemoryItem, error)
	// Get 按 ID 取 item；ns 必须是 item.Namespace 的前缀（层级可见），
	// 否则按不存在处理（跨 namespace 不互见）。
	Get(ctx context.Context, ns []string, id string) (MemoryItem, error)
	// Search 按 namespace 前缀 / Kind / 关键词 / 状态过滤。未命中返回
	// 空切片，不伪造。
	Search(ctx context.Context, q MemoryQuery) ([]MemoryHit, error)
	// Supersede 用 next 替代 oldID：旧 item Status→Superseded，next 入库
	// （新 Revision=1）。next.ID 必须不同于 oldID；oldID 必须存在且非
	// Revoked（Revoked 是终态，不可再被替代）。
	Supersede(ctx context.Context, oldID string, next MemoryItem) (MemoryItem, error)
	// Revoke 显式撤销：Status→Revoked + 审计记录（reason 走 store 审计，
	// 不进 MemoryItem）。已是 Revoked → 幂等成功；已 Superseded → 拒绝
	// （先撤销生效版本没有意义，宿主应明确操作对象）。
	Revoke(ctx context.Context, id string, reason string) error
}

// 包内哨兵错误。调用方用 errors.Is 判别。
var (
	// ErrItemNotFound：Get/Supersede/Revoke 的目标不存在（或 namespace
	// 不互见）。
	ErrItemNotFound = errors.New("store: item not found")
	// ErrItemExists：Put 新建（ExpectedRevision=0）但 ID 已存在。
	ErrItemExists = errors.New("store: item already exists")
	// ErrRevisionConflict：CAS 失败——ExpectedRevision 与当前 Revision
	// 不符；数据不变。
	ErrRevisionConflict = errors.New("store: revision conflict")
	// ErrInvalidItem：item 校验失败（形状、来源、状态、置信度等）。
	ErrInvalidItem = errors.New("store: memory item invalid")
	// ErrInvalidQuery：Search 条件非法。
	ErrInvalidQuery = errors.New("store: memory query invalid")
	// ErrSupersedeRevoked：对 Revoked item 做 Supersede（Revoked 是终态）。
	ErrSupersedeRevoked = errors.New("store: cannot supersede a revoked item")
	// ErrSupersedeSelf：Supersede 的 next.ID 与 oldID 相同。
	ErrSupersedeSelf = errors.New("store: supersede target id equals source id")
)
