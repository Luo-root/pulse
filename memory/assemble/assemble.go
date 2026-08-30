// Package assemble 是 P2-C 的上下文装配层：把 stable memory、检索型
// 记忆与 session surface 组装成带预算边界的模型请求序列。
//
// # 定位与不变式
//
//   - 预算按类配置（§8.1）：稳定记忆小固定预算（超限省略并记诊断——
//     不静默丢）、检索记忆动态预算（超限降 top-k）、surface 尾部保留
//     完整合法尾部（超限只诊断不裁切——裁切归 compaction/prune）；
//   - 预算可解释：每次组装的省略/降级都落在 Diagnostics 里；
//   - stable snapshot policy（§8.3）：frozen profile 默认缓存（同
//     namespace 复用，保 cache），RefreshStable 显式重建；
//   - 本轮立即应用的记忆走 Injected（明确 session injected context，
//     紧贴当前消息），不修改已缓存稳定前缀；
//   - 检索排序确定性（P2-C）：keyword 命中优先 + recency + taint 降权，
//     不依赖 Confidence（w_conf 权重 0 是 P2-D 的事）；semantic 归 P2-D。
//   - 每条注入记忆带 SourceRefs 可读引用模板——避免模型把低置信候选
//     当成无条件事实（§8.2）。
//
// # 分层纪律
//
// import memory/session + memory/store + llm（§7.1 import 图 + 词汇表）；
// kernel 仅借 ServiceKey。TokenCounter 由宿主注入（不 import compaction，
// meter 复用走装配层接线票）。
//
// 设计全貌见 docs/design/memory-layer-research-and-v2-design.md §8；
// 实现票 #80（C3）。
package assemble

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/store"
)

// ContextAssemblerKey 是 memory/assemble 的 kernel 服务键。
var ContextAssemblerKey = kernel.NewServiceKey[ContextAssembler]("memory.assemble")

// TokenCounter 计量一段消息的 token 量（宿主注入精确计数或估算；nil 时
// DefaultAssembler 用字符/4 估算）。不 import compaction——meter 复用
// 由装配层接线票决定。
type TokenCounter func(msgs []*llm.Message) int

// Budget 是按类配置的预算（§8.1：不是只给一个 max messages）。零值字段
// 表示该类不设限。
type Budget struct {
	// StableMemoryTokens 是稳定前缀里 frozen memory 的 token 上限；
	// 超限按最新 revision 优先保留，其余省略并记诊断。
	StableMemoryTokens int
	// RetrievedTokens 是检索型记忆的动态预算；超限降 top-k 并记诊断。
	RetrievedTokens int
	// MaxSurfaceTail 是 surface 尾部节点数检查：超限**只诊断不裁切**
	// （裁切归 compaction §9.1 / prune §9.2，本包不破坏合法尾部）。
	MaxSurfaceTail int
}

// AssembleInput 是一次组装的输入。
type AssembleInput struct {
	// Namespace 是记忆可见域（store 的前缀匹配口径）。
	Namespace []string
	// Surface 是最近 session surface（宿主从 session.Session.Surface()
	// 取得；组装器原样放在消息序列中，不裁切）。
	Surface []*llm.Message
	// Injected 是本轮立即应用的记忆（§8.3：明确 injected context，
	// 紧贴当前消息追加，不改已缓存稳定前缀）。
	Injected []store.MemoryItem
	// RefreshStable 显式重建稳定前缀缓存（§8.3：默认缓存到下个
	// session/显式 refresh）。
	RefreshStable bool
	// Query 是检索召回信号（本轮用户消息文本等）；空 = 只取稳定记忆。
	Query string
}

// Diagnostic 记录一次组装中的省略/裁切/缓存事件——预算可解释的载体。
type Diagnostic struct {
	Region  string // "stable-memory" / "retrieved" / "surface-tail" / "stable-snapshot"
	Dropped int    // 省略/降级的条数或节点数
	Reason  string // 人读原因（"budget exhausted" / "cache hit" 等）
}

// AssembledContext 是组装产物：完整消息序列 + 稳定前缀边界 + 诊断。
type AssembledContext struct {
	// Messages 是组装完的序列：稳定前缀（frozen memory）→ surface 尾部 →
	// 检索记忆 → injected。
	Messages []*llm.Message
	// StablePrefixLen 是稳定前缀的消息数（Messages 的前缀边界——§8.3
	// cache 语义：宿主对该前缀做 provider 侧缓存）。
	StablePrefixLen int
	// Diagnostics 是本次组装的诊断记录。
	Diagnostics []Diagnostic
}

// ContextAssembler 把记忆与 surface 组装成带预算边界的请求序列（§7.1）。
type ContextAssembler interface {
	Assemble(ctx context.Context, in AssembleInput) (AssembledContext, error)
}

// DefaultAssembler 是 ContextAssembler 的默认实现：store.Search 召回 +
// 确定性排序 + 预算边界 + stable snapshot 缓存。
type DefaultAssembler struct {
	// Store 是记忆来源（必填）。
	Store store.MemoryStore
	// Meter 计量 token（nil 用字符/4 估算）。
	Meter TokenCounter
	// Budget 是按类预算（零值字段 = 该类不设限）。
	Budget Budget

	// stable snapshot 缓存（§8.3）：per-namespace，RefreshStable 重建。
	snapMu    sync.RWMutex
	snapshots map[string]*stableSnapshotCache
}

// NewDefaultAssembler 创建默认装配器。
func NewDefaultAssembler(store store.MemoryStore, meter TokenCounter, budget Budget) *DefaultAssembler {
	return &DefaultAssembler{
		Store:     store,
		Meter:     meter,
		Budget:    budget,
		snapshots: make(map[string]*stableSnapshotCache),
	}
}

// compile-time 断言：默认实现满足接口。
var _ ContextAssembler = (*DefaultAssembler)(nil)

// tokens 估算消息序列（Meter 注入优先）。
func (a *DefaultAssembler) tokens(msgs []*llm.Message) int {
	if a.Meter != nil {
		return a.Meter(msgs)
	}
	total := 0
	for _, m := range msgs {
		if m == nil {
			continue
		}
		for _, p := range m.Parts {
			total += len([]rune(p.Text))
		}
	}
	return total / 4
}

// Assemble 实现 ContextAssembler。
func (a *DefaultAssembler) Assemble(ctx context.Context, in AssembleInput) (AssembledContext, error) {
	if a.Store == nil {
		return AssembledContext{}, fmt.Errorf("assemble: store is required")
	}
	diags := []Diagnostic{}

	// 1. 稳定前缀：frozen memory（缓存策略见 snapshot.go）。
	prefixMsgs, prefixDiag := a.stablePrefix(ctx, in)
	diags = append(diags, prefixDiag...)

	// 2. surface 尾部：原样保留（合法性宿主保证）；超限只诊断不裁切。
	surfaceTail := in.Surface
	if a.Budget.MaxSurfaceTail > 0 && len(in.Surface) > a.Budget.MaxSurfaceTail {
		diags = append(diags, Diagnostic{
			Region:  "surface-tail",
			Dropped: len(in.Surface) - a.Budget.MaxSurfaceTail,
			Reason:  "surface exceeds MaxSurfaceTail; kept intact (compaction/prune owns trimming)",
		})
	}
	_ = surfaceTail

	// 3. 检索记忆：Query 非空才召回（预算内 top-k）。
	retrievedMsgs, retrievedDiag := a.retrieved(ctx, in)
	diags = append(diags, retrievedDiag...)

	// 4. injected：本轮立即应用（无预算约束——用户明确要求）。
	injectedMsgs := make([]*llm.Message, 0, len(in.Injected))
	for _, it := range in.Injected {
		injectedMsgs = append(injectedMsgs, memoryMessage(it))
	}

	msgs := make([]*llm.Message, 0, len(prefixMsgs)+len(surfaceTail)+len(retrievedMsgs)+len(injectedMsgs))
	msgs = append(msgs, prefixMsgs...)
	msgs = append(msgs, surfaceTail...)
	msgs = append(msgs, retrievedMsgs...)
	msgs = append(msgs, injectedMsgs...)
	return AssembledContext{
		Messages:        msgs,
		StablePrefixLen: len(prefixMsgs),
		Diagnostics:     diags,
	}, nil
}

// stablePrefix 产出稳定前缀消息（frozen memory 引用模板），缓存策略见
// stableSnapshot。
func (a *DefaultAssembler) stablePrefix(ctx context.Context, in AssembleInput) ([]*llm.Message, []Diagnostic) {
	items, diag := a.stableSnapshot(ctx, in)
	budget := a.Budget.StableMemoryTokens
	kept := make([]*llm.Message, 0, len(items))
	for _, it := range items {
		candidate := append(kept, memoryMessage(it))
		if budget > 0 && a.tokens(candidate) > budget {
			diag := append(diag, Diagnostic{
				Region:  "stable-memory",
				Dropped: len(items) - len(kept),
				Reason:  "stable memory budget exhausted; kept newest revisions first",
			})
			return kept, diag
		}
		kept = candidate
	}
	return kept, diag
}

// retrieved 按 Query 召回检索型记忆（预算内 top-k，确定性排序）。
func (a *DefaultAssembler) retrieved(ctx context.Context, in AssembleInput) ([]*llm.Message, []Diagnostic) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, nil
	}
	hits, err := a.Store.Search(ctx, store.MemoryQuery{
		Namespace: in.Namespace,
		Query:     in.Query,
	})
	if err != nil {
		// 召回失败不静默也不中断组装：记诊断，surface 照常。
		return nil, []Diagnostic{{Region: "retrieved", Reason: fmt.Sprintf("search failed: %v", err)}}
	}
	ranked := rankHits(hits)
	budget := a.Budget.RetrievedTokens
	msgs := make([]*llm.Message, 0, len(ranked))
	kept := 0
	tokens := 0
	for _, it := range ranked {
		m := memoryMessage(it)
		cost := a.tokens([]*llm.Message{m})
		if budget > 0 && tokens+cost > budget {
			return msgs, []Diagnostic{{
				Region:  "retrieved",
				Dropped: len(ranked) - kept,
				Reason:  "retrieved budget exhausted; reduced top-k",
			}}
		}
		msgs = append(msgs, m)
		tokens += cost
		kept++
	}
	return msgs, nil
}

// rankHits 确定性排序（P2-C 口径）：untrusted-external 降权 > recency
// 降序 > ID 升序。不读 Confidence（排序不得依赖没人写的值）。
func rankHits(hits []store.MemoryHit) []store.MemoryItem {
	out := make([]store.MemoryItem, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		si, sj := 0, 0
		if out[i].Taint == store.TaintUntrustedExt {
			si -= 4
		}
		if out[j].Taint == store.TaintUntrustedExt {
			sj -= 4
		}
		if si != sj {
			return si > sj
		}
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// memoryMessage 把一条记忆转成带可读引用的注入消息（RoleUser：记忆是
// 历史陈述，不是模型发言）。
func memoryMessage(it store.MemoryItem) *llm.Message {
	refs := make([]string, 0, len(it.SourceRefs))
	for _, r := range it.SourceRefs {
		switch r.Type {
		case store.SourceSession:
			refs = append(refs, fmt.Sprintf("session %s#%d", r.SessionID, r.Seq))
		default:
			refs = append(refs, r.Ref)
		}
	}
	header := fmt.Sprintf("[memory:%s %s (source: %s)]", it.Kind, it.ID, strings.Join(refs, "; "))
	return llm.UserText(header + " " + it.Content)
}
