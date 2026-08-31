package selfedit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/store"
	"github.com/Luo-root/pulse/toolset"
)

// defaultSource 是 Registration.Source 的默认元数据值。
const defaultSource = "memory.selfedit"

// excerptLimit 是 Preview 卡片里人读摘要的截断长度（rune）。
const excerptLimit = 120

// ErrOutsideScope 是写权限哨兵：目标 item 的 namespace 与 env 绑定作用域
// 不完全相等。store 的前缀可见性是**读**口径（向下可见）；写入钉死在
// env.ns——父 scope 工具不得下钻改写子 scope 的 item（票 #82 复审定案）。
var ErrOutsideScope = errors.New("selfedit: item namespace is outside the write scope")

// Options 是 self-edit 工具组的装配项（票 #82 冻结）。模型侧参数刻意最小
// 化：namespace / 来源 / 信任级 / 置信度 / 状态 / revision 全部由本结构钉
// 死——scope 是存储层边界，不是提示词约定（§17.1 Letta 失效模式对位）。
type Options struct {
	// Store 是记忆来源（必填）。
	Store store.MemoryStore
	// Namespace 是工具唯一作用域：写入与 supersede/revoke 的可见性都被钉
	// 在这个前缀边界内（必填；模型参数里没有 namespace）。
	Namespace []string
	// OriginFn 返回当前来源锚点（session 回链；store 校验 SessionID+Seq>0）。
	// 必填：每条模型写的记忆都要能定位「哪轮会话写的」，缺省 Register 直接
	// 失败，不静默降级为无来源。
	OriginFn func() store.SourceRef
	// Taint 是写入信任级（空 = TaintUntrustedExt——ASI06 对位，§17.7：
	// self-edit 是模型把工具输出/外部内容复述进长期记忆的通道，默认不得
	// 与宿主权威写入同级；before_tool_call 审批是晋升闸，taint 保持诚实的
	// 数据属性。可信写手场景宿主显式覆盖为 TaintTrusted）。
	Taint store.TaintLevel
	// NewID 生成 item ID（空 = crypto/rand 16B hex）。
	NewID func() string
	// Source 是 Registration.Source 元数据（空 = "memory.selfedit"）。
	Source string
}

// withDefaults 补默认值并校验必填项（fail closed）。
func (o Options) withDefaults() (Options, error) {
	if o.Store == nil {
		return o, fmt.Errorf("selfedit: store is required")
	}
	if len(o.Namespace) == 0 {
		return o, fmt.Errorf("selfedit: namespace is required")
	}
	for _, ns := range o.Namespace {
		if strings.TrimSpace(ns) == "" {
			return o, fmt.Errorf("selfedit: namespace element is empty")
		}
	}
	if o.OriginFn == nil {
		return o, fmt.Errorf("selfedit: origin fn (session source ref) is required: model-written memories must be attributable")
	}
	if o.Taint == "" {
		o.Taint = store.TaintUntrustedExt
	}
	if o.NewID == nil {
		o.NewID = randomID
	}
	if o.Source == "" {
		o.Source = defaultSource
	}
	return o, nil
}

// randomID 默认 ID 生成器：crypto/rand 16B hex（plan9/js 可用）。熵源不可
// 用时返回空串——写入路径以「empty id」拒绝（fail closed，不做弱回退）。
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// env 是工具组共享的运行时。
type env struct {
	opt Options
}

// Register 将 self-edit 工具组登记到 reg（显式 opt-in：不进任何默认装配，
// 是否放给模型由宿主决定）。返回统一 dispose（撤销本批登记，逆序回滚）。
//
// 登记工具：memory_put / memory_supersede / memory_revoke（RiskReadWrite，
// PreviewFn 挂 #56 W2 面）。审批归宿主 HITL（before_tool_call）——本包不做
// 自动批准白名单。
func Register(scope *kernel.Context, reg *toolset.Registry, opt Options) (dispose func(), err error) {
	if scope == nil {
		return nil, fmt.Errorf("selfedit: nil scope")
	}
	if reg == nil {
		return nil, fmt.Errorf("selfedit: nil registry")
	}
	opt, err = opt.withDefaults()
	if err != nil {
		return nil, err
	}
	e := &env{opt: opt}
	disposers := make([]func(), 0, 3)
	rollback := func() {
		for i := len(disposers) - 1; i >= 0; i-- {
			disposers[i]()
		}
	}
	for _, r := range []toolset.Registration{e.regPut(), e.regSupersede(), e.regRevoke()} {
		r.Source = opt.Source
		d, err := reg.Register(scope, r)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("selfedit: register %s: %w", r.Def.Name, err)
		}
		disposers = append(disposers, d)
	}
	return func() {
		for i := len(disposers) - 1; i >= 0; i-- {
			disposers[i]()
		}
	}, nil
}

// ---- memory_put ----

// putArgs 是 memory_put 的模型参数全集（namespace/来源/信任级不在其中）。
type putArgs struct {
	Kind       string `json:"kind"`
	Content    string `json:"content"`
	Structured string `json:"structured,omitempty"`
}

func (e *env) regPut() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name: "memory_put",
			Description: "Persist a long-term memory item for future sessions. " +
				"Namespace, source attribution and trust level are fixed by the host; you only choose the kind and content. " +
				"Write durable facts, decisions, lessons or episodes — not transient chit-chat. " +
				"To update an existing memory, use memory_supersede instead.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "kind":{"type":"string","description":"Memory category: profile | decision | environment | episode | lesson (host-defined kinds may also be accepted)"},
    "content":{"type":"string","description":"The memory content as one self-contained statement"},
    "structured":{"type":"string","description":"Optional domain-structured payload serialized as JSON"}
  },
  "required":["kind","content"]
}`),
		},
		Fn:        e.put,
		Risk:      toolset.RiskReadWrite,
		PreviewFn: e.previewPut,
	}
}

func (e *env) put(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p putArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("selfedit/memory_put: invalid args: %w", err)
	}
	if strings.TrimSpace(p.Content) == "" {
		return "", fmt.Errorf("selfedit/memory_put: content is required")
	}
	it, err := e.newItem(e.opt.Namespace, p.Kind, p.Content, p.Structured)
	if err != nil {
		return "", fmt.Errorf("selfedit/memory_put: %w", err)
	}
	saved, err := e.opt.Store.Put(ctx, it, store.PutMemoryOptions{ExpectedRevision: 0})
	if err != nil {
		return "", fmt.Errorf("selfedit/memory_put: %w", err)
	}
	return fmt.Sprintf("stored memory %s (kind=%s, revision=%d)", saved.ID, saved.Kind, saved.Revision), nil
}

func (e *env) previewPut(_ context.Context, args json.RawMessage) (toolset.Preview, error) {
	var p putArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return toolset.Preview{}, err
	}
	kind := strings.TrimSpace(p.Kind)
	if kind == "" {
		kind = "<unset>"
	}
	return toolset.Preview{
		Kind:    toolset.KindOpaque,
		Action:  toolset.ActionWrite,
		Subject: "memory: " + e.subject() + "/" + kind,
		Opaque: &toolset.OpaqueChange{
			Summary:     "memory_put " + kind + ": " + excerpt(p.Content),
			ArgsExcerpt: string(args),
		},
	}, nil
}

// ---- memory_supersede ----

// supersedeArgs 是 memory_supersede 的模型参数全集。
type supersedeArgs struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Kind    string `json:"kind,omitempty"`
}

func (e *env) regSupersede() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name: "memory_supersede",
			Description: "Replace an existing memory with a corrected or newer version. " +
				"The old item is kept as superseded for audit (no physical delete). " +
				"Kind defaults to the old item's kind when omitted.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "id":{"type":"string","description":"ID of the memory to replace (from a previous memory_put / memory_supersede result)"},
    "content":{"type":"string","description":"The replacement content"},
    "kind":{"type":"string","description":"Optional new category; defaults to the old item's kind"}
  },
  "required":["id","content"]
}`),
		},
		Fn:        e.supersede,
		Risk:      toolset.RiskReadWrite,
		PreviewFn: e.previewSupersede,
	}
}

func (e *env) supersede(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p supersedeArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("selfedit/memory_supersede: invalid args: %w", err)
	}
	if strings.TrimSpace(p.ID) == "" {
		return "", fmt.Errorf("selfedit/memory_supersede: id is required")
	}
	if strings.TrimSpace(p.Content) == "" {
		return "", fmt.Errorf("selfedit/memory_supersede: content is required")
	}
	// 边界先行：ns 不可见 = 不存在（跨 namespace 不互见——票 #82 验收）。
	old, err := e.opt.Store.Get(ctx, e.opt.Namespace, p.ID)
	if err != nil {
		return "", fmt.Errorf("selfedit/memory_supersede: %w", err)
	}
	// 写权限口径（复审定案）：env.ns 是读可见前缀，但写入要求 namespace
	// 完全相等——父 scope 工具不得下钻改写子 scope item。
	if !slices.Equal(old.Namespace, e.opt.Namespace) {
		return "", fmt.Errorf("selfedit/memory_supersede: item %s: %w", p.ID, ErrOutsideScope)
	}
	kind := old.Kind // 缺省沿用旧类别
	if k := strings.TrimSpace(p.Kind); k != "" {
		kind = store.MemoryKind(k)
	}
	// next 保持原 item 的 namespace（写权限口径下 old.ns 恒等于 env.ns）；
	// structured 不继承——修正语义下旧领域载荷未必还成立。
	next, err := e.newItem(old.Namespace, string(kind), p.Content, "")
	if err != nil {
		return "", fmt.Errorf("selfedit/memory_supersede: %w", err)
	}
	saved, err := e.opt.Store.Supersede(ctx, p.ID, next)
	if err != nil {
		return "", fmt.Errorf("selfedit/memory_supersede: %w", err)
	}
	return fmt.Sprintf("superseded %s -> %s (kind=%s, revision=%d); old item kept as superseded", p.ID, saved.ID, saved.Kind, saved.Revision), nil
}

func (e *env) previewSupersede(_ context.Context, args json.RawMessage) (toolset.Preview, error) {
	var p supersedeArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return toolset.Preview{}, err
	}
	return toolset.Preview{
		Kind:    toolset.KindOpaque,
		Action:  toolset.ActionWrite,
		Subject: "memory: " + e.subject() + "/" + strings.TrimSpace(p.ID),
		Opaque: &toolset.OpaqueChange{
			Summary:     "memory_supersede " + strings.TrimSpace(p.ID) + ": " + excerpt(p.Content),
			ArgsExcerpt: string(args),
		},
	}, nil
}

// ---- memory_revoke ----

// revokeArgs 是 memory_revoke 的模型参数全集。
type revokeArgs struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

func (e *env) regRevoke() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name: "memory_revoke",
			Description: "Revoke (invalidate) a memory by id. The record is kept for audit (no physical delete). " +
				"Revoking an already-revoked memory succeeds idempotently; " +
				"revoking a superseded one fails — supersede its active version instead.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "id":{"type":"string","description":"ID of the memory to revoke"},
    "reason":{"type":"string","description":"Why this memory is invalidated (goes to the audit trail)"}
  },
  "required":["id","reason"]
}`),
		},
		Fn:        e.revoke,
		Risk:      toolset.RiskReadWrite,
		PreviewFn: e.previewRevoke,
	}
}

func (e *env) revoke(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p revokeArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("selfedit/memory_revoke: invalid args: %w", err)
	}
	if strings.TrimSpace(p.ID) == "" {
		return "", fmt.Errorf("selfedit/memory_revoke: id is required")
	}
	if strings.TrimSpace(p.Reason) == "" {
		return "", fmt.Errorf("selfedit/memory_revoke: reason is required (audit trail)")
	}
	// 边界先行 + 写权限口径：同 supersede（不可见即不存在；namespace 不
	// 完全相等拒绝——不下钻改写）。
	old, err := e.opt.Store.Get(ctx, e.opt.Namespace, p.ID)
	if err != nil {
		return "", fmt.Errorf("selfedit/memory_revoke: %w", err)
	}
	if !slices.Equal(old.Namespace, e.opt.Namespace) {
		return "", fmt.Errorf("selfedit/memory_revoke: item %s: %w", p.ID, ErrOutsideScope)
	}
	if err := e.opt.Store.Revoke(ctx, p.ID, p.Reason); err != nil {
		return "", fmt.Errorf("selfedit/memory_revoke: %w", err)
	}
	return fmt.Sprintf("revoked memory %s", p.ID), nil
}

func (e *env) previewRevoke(_ context.Context, args json.RawMessage) (toolset.Preview, error) {
	var p revokeArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return toolset.Preview{}, err
	}
	return toolset.Preview{
		Kind:    toolset.KindOpaque,
		Action:  toolset.ActionWrite,
		Subject: "memory: " + e.subject() + "/" + strings.TrimSpace(p.ID),
		Opaque: &toolset.OpaqueChange{
			Summary:     "memory_revoke " + strings.TrimSpace(p.ID) + ": " + excerpt(p.Reason),
			ArgsExcerpt: string(args),
		},
	}, nil
}

// ---- 共用 helpers ----

// newItem 按 env 装配项构造 MemoryItem：namespace/来源/信任级/置信度全部
// 宿主钉死，模型只给 kind/content/structured（票 #82 设计理念 1/2）。
func (e *env) newItem(ns []string, kind, content, structured string) (store.MemoryItem, error) {
	id, err := e.newID()
	if err != nil {
		return store.MemoryItem{}, err
	}
	it := store.MemoryItem{
		ID:         id,
		Namespace:  ns,
		Kind:       store.MemoryKind(strings.TrimSpace(kind)),
		Content:    strings.TrimSpace(content),
		Status:     store.StatusActive,
		Confidence: 1.0, // store 评审定案：active 必须显式置信度；本包无 scoring 产出方
		SourceRefs: []store.SourceRef{e.opt.OriginFn()},
		Taint:      e.opt.Taint,
	}
	if s := strings.TrimSpace(structured); s != "" {
		if !json.Valid([]byte(s)) {
			return store.MemoryItem{}, fmt.Errorf("structured is not valid JSON")
		}
		it.Structured = json.RawMessage(s)
	}
	return it, nil
}

// newID 生成 item ID（空 ID 拒绝——randomID 熵源失败的 fail-closed 路）。
func (e *env) newID() (string, error) {
	id := e.opt.NewID()
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("id generator returned empty id")
	}
	return id, nil
}

// subject 是审批键的显示形态（ns-key/kind 或 ns-key/id）。
func (e *env) subject() string {
	return strings.Join(e.opt.Namespace, "/")
}

// excerpt 截断人读摘要（超限显式标注，不静默砍——对齐仓库截断口径）。
func excerpt(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= excerptLimit {
		return s
	}
	return string(r[:excerptLimit]) + fmt.Sprintf("…(truncated, %d runes total)", len(r))
}
