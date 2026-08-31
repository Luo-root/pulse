package candidate

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
)

// PipelineKey 是 memory/candidate 的 kernel 服务键（memory/* 先例）。
var PipelineKey = kernel.NewServiceKey[*Pipeline]("memory.candidate")

// 审批标记（追加在批准版 SourceRefs 尾部——store audit 的 generic
// supersede 之外，审批动作在 provenance 里显式可辨）。
const approvalRef = "approved via candidate.Pipeline"

// 包内哨兵错误。调用方用 errors.Is 判别。
var (
	// ErrNotPending：目标 item 不是 Pending 状态（Active/Superseded/
	// Revoked 不可再审批——fail closed）。
	ErrNotPending = errors.New("candidate: item is not pending")
	// ErrOutsideScope：目标 item 的 namespace 与 Pipeline 作用域不完全
	// 相等——审批/列表与 selfedit 写权限同口径（票 #90 复审修订），
	// 父 scope Pipeline 不得下钻操作子 scope 候选。
	ErrOutsideScope = errors.New("candidate: item namespace is outside the pipeline scope")
)

// Extractor 是候选提炼 seam（宿主注入——LLM 提取协议归宿主 prompt，
// v1 不提供默认实现，compaction.LLMSummarizer seam 同理）。返回的
// item 只取 Kind/Content/Structured 三字段——namespace/status/taint/
// source/ID 由 Pipeline 钉死（模型参数最小化，selfedit 哲学）。
type Extractor interface {
	Extract(ctx context.Context, surface []*llm.Message) ([]store.MemoryItem, error)
}

// Options 是管线装配项（票 #90 冻结）。
type Options struct {
	// Store 是记忆来源（必填）。
	Store store.MemoryStore
	// Extractor 是候选提炼 seam（必填）。
	Extractor Extractor
	// Namespace 是候选入库作用域（必填——scope 防污染同 selfedit：
	// 提炼/列表/审批全部钉死在此边界，模型与调用方都越不了界）。
	Namespace []string
	// OriginFn 返回当前会话回链（必填——SourceRefs 强制：自动提炼的
	// 记忆必须能回链到 surface 来源）。
	OriginFn func() store.SourceRef
	// Taint 是候选信任级（空 = TaintUntrustedExt——ASI06：自动提炼自
	// 工具/外部内容，未过审批不得视为可信；批准晋升不改 taint）。
	Taint store.TaintLevel
	// NewID 生成候选/批准版 ID（空 = crypto/rand 16B hex）。
	NewID func() string
}

// withDefaults 补默认值并校验必填项（fail closed）。
func (o Options) withDefaults() (Options, error) {
	if o.Store == nil {
		return o, fmt.Errorf("candidate: store is required")
	}
	if o.Extractor == nil {
		return o, fmt.Errorf("candidate: extractor is required")
	}
	if len(o.Namespace) == 0 {
		return o, fmt.Errorf("candidate: namespace is required")
	}
	for _, ns := range o.Namespace {
		if strings.TrimSpace(ns) == "" {
			return o, fmt.Errorf("candidate: namespace element is empty")
		}
	}
	if o.OriginFn == nil {
		return o, fmt.Errorf("candidate: origin fn (session source ref) is required: auto-extracted memories must be attributable")
	}
	if o.Taint == "" {
		o.Taint = store.TaintUntrustedExt
	}
	if o.NewID == nil {
		o.NewID = randomID
	}
	return o, nil
}

// randomID 默认 ID 生成器：crypto/rand 16B hex。熵源不可用返回空串——
// 写入路径以「empty id」拒绝（fail closed，不做弱回退）。
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// Report 是一次 Extract 的可解释计数（禁止静默丢——D4 指标票的雏形）。
type Report struct {
	// Extracted 是 extractor 返回的候选数。
	Extracted int
	// Stored 是入库 Pending 的候选数。
	Stored int
	// Duplicates 是去重丢弃数（归一后已有 item 的 Content 包含候选）。
	Duplicates int
	// Invalid 是形状丢弃数（空 Content / Structured 非法 JSON）。
	Invalid int
}

// Pipeline 是候选管线：提炼 → 去重 → Pending 入库 → 审批晋升/否决。
// 作用域钉死在 Options.Namespace（scope 防污染同 selfedit）。
type Pipeline struct {
	opt Options
	m   metrics // D4 指标面：累计动作计数（atomic，metrics.go）
}

// New 创建候选管线（显式装配；本包默认关——无后台循环，调用时机归宿主）。
func New(opt Options) (*Pipeline, error) {
	opt, err := opt.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Pipeline{opt: opt}, nil
}

// Extract 提炼候选并入库（StatusPending）：全量扫作用域做双归一去重
// （O(n)，候选低量——票 #90 声明），逐条形状校验（空 Content /
// Structured 非法 JSON 计 Invalid，不中断批次）。提取错误透传（调用
// 方决定重试/放弃——管线不静默）；去重查询失败中断批次（store 故障宁
// 可失败让宿主重试，不做半批静默）。
func (p *Pipeline) Extract(ctx context.Context, surface []*llm.Message) ([]store.MemoryItem, Report, error) {
	proposals, err := p.opt.Extractor.Extract(ctx, surface)
	if err != nil {
		return nil, Report{}, fmt.Errorf("candidate: extract: %w", err)
	}
	rep := Report{Extracted: len(proposals)}
	// 去重（v1 保守口径，双归一——复审修订）：归一（小写 + 空白收紧，
	// 存量/候选两侧同口径）后已有 item 的 Content 包含候选 → 丢弃（子
	// 串冗余即重复，超集不拦——超集归 Supersede 修订语义）。全量扫而
	// 不用 store 子串查询作粗筛：store 的 asciiFold 不收紧空白，存量侧
	// 脏数据（双空格等）会被漏拦——双归一必须在内存做。判定集 = 存量
	// 归一快照 + 本轮已入库候选的归一（随入库追加）——快照不含本轮
	// 写入，缺后者批次内重复会漏拦。
	existing, err := p.opt.Store.Search(ctx, store.MemoryQuery{
		Namespace:       p.opt.Namespace,
		IncludeInactive: true,
	})
	if err != nil {
		return nil, rep, fmt.Errorf("candidate: dedup search: %w", err)
	}
	norms := make([]string, 0, len(existing)+len(proposals))
	for _, h := range existing {
		norms = append(norms, normalize(h.Item.Content))
	}
	accepted := make([]store.MemoryItem, 0, len(proposals))
	for _, prop := range proposals {
		content := strings.TrimSpace(prop.Content)
		if content == "" {
			rep.Invalid++
			continue
		}
		if len(prop.Structured) > 0 && !json.Valid(prop.Structured) {
			rep.Invalid++
			continue
		}
		norm := normalize(content)
		dup := false
		for _, n := range norms {
			if strings.Contains(n, norm) {
				dup = true
				break
			}
		}
		if dup {
			rep.Duplicates++
			continue
		}
		id, err := p.newID()
		if err != nil {
			return accepted, rep, err
		}
		it := store.MemoryItem{
			ID:         id,
			Namespace:  p.opt.Namespace,
			Kind:       prop.Kind,
			Content:    content,
			Structured: prop.Structured,
			Status:     store.StatusPending,
			SourceRefs: []store.SourceRef{p.opt.OriginFn()},
			Taint:      p.opt.Taint,
		}
		saved, err := p.opt.Store.Put(ctx, it, store.PutMemoryOptions{})
		if err != nil {
			if errors.Is(err, store.ErrItemExists) {
				rep.Duplicates++ // 随机 ID 理论不撞——防御计数
				continue
			}
			return accepted, rep, fmt.Errorf("candidate: put: %w", err)
		}
		accepted = append(accepted, saved)
		norms = append(norms, norm) // 批次内去重：本轮入库进判定集
		rep.Stored++
	}
	// D4 指标面：整轮成功才累计（错误中断的批次不计——宿主重试成功后
	// 完整计一轮；rep 已是本轮完整计数）。
	p.m.extracted.Add(uint64(rep.Extracted))
	p.m.stored.Add(uint64(rep.Stored))
	p.m.duplicates.Add(uint64(rep.Duplicates))
	p.m.invalid.Add(uint64(rep.Invalid))
	return accepted, rep, nil
}

// Pending 列出作用域内的全部 Pending 候选。store 无 status 过滤器——
// IncludeInactive 后内存过滤（v1 口径）；namespace **完全相等**过滤
// （selfedit 写权限同口径——父 scope Pipeline 不列出子 scope 候选）。
func (p *Pipeline) Pending(ctx context.Context) ([]store.MemoryItem, error) {
	hits, err := p.opt.Store.Search(ctx, store.MemoryQuery{
		Namespace:       p.opt.Namespace,
		IncludeInactive: true,
	})
	if err != nil {
		return nil, fmt.Errorf("candidate: list: %w", err)
	}
	out := make([]store.MemoryItem, 0, len(hits))
	for _, h := range hits {
		if h.Item.Status != store.StatusPending {
			continue
		}
		if !slices.Equal(h.Item.Namespace, p.opt.Namespace) {
			continue
		}
		out = append(out, h.Item)
	}
	return out, nil
}

// Approve 把 Pending 候选晋升为 Active——既有状态机：Supersede（旧候
// 选 Superseded 留痕、批准版新 ID Active）。批准版 Confidence=1.0（批
// 准即宿主背书，Active 必须显式置信度）；Content/Taint 继承；SourceRefs
// = 继承 + manual 审批标记（provenance 里显式可辨审批动作），taint 不
// 因批准改变（审批是晋升闸，taint 是数据属性）。namespace 与 Pipeline
// 作用域不完全相等 → ErrOutsideScope（selfedit 写权限同口径）；非
// Pending → ErrNotPending。
func (p *Pipeline) Approve(ctx context.Context, id string) (store.MemoryItem, error) {
	old, err := p.opt.Store.Get(ctx, p.opt.Namespace, id)
	if err != nil {
		return store.MemoryItem{}, fmt.Errorf("candidate: approve: %w", err)
	}
	if !slices.Equal(old.Namespace, p.opt.Namespace) {
		return store.MemoryItem{}, fmt.Errorf("candidate: %s: %w", id, ErrOutsideScope)
	}
	if old.Status != store.StatusPending {
		return store.MemoryItem{}, fmt.Errorf("candidate: %s: %w", id, ErrNotPending)
	}
	nextID, err := p.newID()
	if err != nil {
		return store.MemoryItem{}, err
	}
	saved, err := p.opt.Store.Supersede(ctx, id, store.MemoryItem{
		ID:         nextID,
		Namespace:  old.Namespace,
		Kind:       old.Kind,
		Content:    old.Content,
		Structured: old.Structured,
		Status:     store.StatusActive,
		Confidence: 1.0,
		SourceRefs: append(old.SourceRefs, store.SourceRef{Type: store.SourceManual, Ref: approvalRef}),
		Taint:      old.Taint,
	})
	if err != nil {
		return store.MemoryItem{}, fmt.Errorf("candidate: approve: %w", err)
	}
	p.m.approved.Add(1)
	return saved, nil
}

// Reject 显式否决候选（store.Revoke——reason 落审计）。namespace 与
// Pipeline 作用域不完全相等 → ErrOutsideScope；非 Pending →
// ErrNotPending（已 Superseded 的候选不可 revoke——操作对象错了）。
func (p *Pipeline) Reject(ctx context.Context, id, reason string) error {
	old, err := p.opt.Store.Get(ctx, p.opt.Namespace, id)
	if err != nil {
		return fmt.Errorf("candidate: reject: %w", err)
	}
	if !slices.Equal(old.Namespace, p.opt.Namespace) {
		return fmt.Errorf("candidate: %s: %w", id, ErrOutsideScope)
	}
	if old.Status != store.StatusPending {
		return fmt.Errorf("candidate: %s: %w", id, ErrNotPending)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("candidate: reject: reason is required (audit trail)")
	}
	if err := p.opt.Store.Revoke(ctx, id, reason); err != nil {
		return fmt.Errorf("candidate: reject: %w", err)
	}
	p.m.rejected.Add(1)
	if old.Taint == store.TaintUntrustedExt {
		p.m.rejectedUntrusted.Add(1) // ASI06 污染闸实证（仅 untrusted-external 档）
	}
	return nil
}

// newID 生成 item ID（空 ID 拒绝——randomID 熵源失败的 fail-closed 路）。
func (p *Pipeline) newID() (string, error) {
	id := p.opt.NewID()
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("candidate: id generator returned empty id")
	}
	return id, nil
}

// normalize 归一文本用于去重比较：小写（strings.ToLower，含 Unicode）
// + 空白收紧。存量/候选**两侧同口径**——去重判定在内存双归一完成，不
// 依赖 store 的 ASCII 折叠子串匹配（其不收紧空白，会漏拦存量侧脏数据）。
func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
