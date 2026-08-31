// 06-memory-agent：长期记忆全链路。
//
// 运行：go run ./examples/06-memory-agent
// 一次运行走完记忆生命周期（全部离线，embedding 用确定性词表假实现）：
//
//	store（宿主权威写入）→ index 向量召回 → assemble 注入（引用模板）
//	→ candidate 提炼（Pending 不可见）→ 宿主审批晋升 → 下一轮 assemble
//	召回新记忆 → 指标面快照。
//
// 你会亲眼看到：未审批的候选对检索天然不可见；批准 = Supersede 晋升 +
// manual 审批标记，但 taint 不变（审批是晋升闸，taint 是数据属性）。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/assemble"
	"github.com/Luo-root/pulse/memory/candidate"
	"github.com/Luo-root/pulse/memory/index"
	"github.com/Luo-root/pulse/memory/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "06-memory-agent: %v\n", err)
		os.Exit(1)
	}
}

// demoProvider 是确定性词表 embedding：文本命中词表词 → 对应维度置 1，
// 首维恒为 0.1（避免全零向量，余弦才有定义）。真实项目换
// memory/index/openai 或任何 EmbeddingProvider 实现，调用侧零改动。
type demoProvider struct{ vocab []string }

func (p demoProvider) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, len(p.vocab)+1)
		v[0] = 0.1
		lt := strings.ToLower(t)
		for j, w := range p.vocab {
			if strings.Contains(lt, w) {
				v[j+1] = 1
			}
		}
		out[i] = v
	}
	return out, nil
}

// demoExtractor 是 candidate 的假提炼 seam（真实项目由宿主 LLM 提取：
// 提取协议归宿主 prompt，v1 不提供默认实现）。
type demoExtractor struct{ items []store.MemoryItem }

func (e *demoExtractor) Extract(_ context.Context, _ []*llm.Message) ([]store.MemoryItem, error) {
	return e.items, nil
}

var (
	scope   = store.MemoryScope{TenantID: "acme", UserID: "u1"}
	ns      = scope.Namespace()
	vocab   = []string{"postgres", "audit", "yaml", "kernel", "flow"}
	seedRef = store.SourceRef{Type: store.SourceManual, Ref: "seed (host-authoritative)"}
)

func run() error {
	ctx := context.Background()
	memStore := store.NewMemoryStore()

	// ---- ① store：宿主权威写入 Active 记忆 ----
	fmt.Println("== ① store：宿主权威写入两条 Active 记忆 ==")
	for _, it := range []store.MemoryItem{
		{ID: "seed-postgres", Namespace: ns, Kind: store.KindDecision, Content: "Use PostgreSQL for audit logs", Status: store.StatusActive,
			Confidence: 1.0, Taint: store.TaintTrusted, SourceRefs: []store.SourceRef{seedRef}},
		{ID: "seed-yaml", Namespace: ns, Kind: store.KindProfile, Content: "Pulse flows are declared in YAML", Status: store.StatusActive,
			Confidence: 1.0, Taint: store.TaintTrusted, SourceRefs: []store.SourceRef{seedRef}},
	} {
		if _, err := memStore.Put(ctx, it, store.PutMemoryOptions{}); err != nil {
			return err
		}
		fmt.Printf("  put: [%s] %s\n", it.Kind, it.Content)
	}

	// ---- ② index：派生向量召回（先过滤再召回，余弦 top-k）----
	fmt.Println()
	fmt.Println("== ② index：向量召回（namespace 先过滤，余弦 top-k）==")
	idx, err := index.NewMemIndex(memStore, demoProvider{vocab: vocab})
	if err != nil {
		return err
	}
	hits, err := memStore.Search(ctx, store.MemoryQuery{Namespace: ns})
	if err != nil {
		return err
	}
	for _, h := range hits {
		if err := idx.Upsert(ctx, h.Item); err != nil { // 写入方负责：store 写后同步索引
			return err
		}
	}
	vecHits, err := idx.Search(ctx, ns, "数据库选型 audit", 2)
	if err != nil {
		return err
	}
	for _, h := range vecHits {
		fmt.Printf("  hit: score=%.3f [%s] %s\n", h.Score, h.Item.Kind, h.Item.Content)
	}

	// ---- ③ assemble：稳定前缀 + 混合召回 + 引用模板 ----
	fmt.Println()
	fmt.Println("== ③ assemble：把记忆装进请求（keyword ∪ semantic 融合）==")
	assembler := assemble.NewDefaultAssembler(memStore, nil, assemble.Budget{
		StableMemoryTokens: 200, RetrievedTokens: 200, MaxSurfaceTail: 40,
	})
	// 生产路径不 import index（§17 决议 4）：向量路经函数 seam 由装配层接线。
	assembler.Semantic = func(ctx context.Context, ns []string, q string, k int) ([]store.MemoryItem, []float64, error) {
		hits, err := idx.Search(ctx, ns, q, k)
		if err != nil {
			return nil, nil, err
		}
		items, scores := make([]store.MemoryItem, len(hits)), make([]float64, len(hits))
		for i, h := range hits {
			items[i], scores[i] = h.Item, h.Score
		}
		return items, scores, nil
	}
	query := "我们审计日志用什么数据库？"
	ac, err := assembler.Assemble(ctx, assemble.AssembleInput{
		Namespace: ns,
		Surface:   []*llm.Message{llm.UserText(query)},
		Query:     query,
	})
	if err != nil {
		return err
	}
	fmt.Printf("  组装 %d 条消息（稳定前缀 %d 条）；诊断：%v\n", len(ac.Messages), ac.StablePrefixLen, diagnostics(ac))
	for _, m := range ac.Messages {
		if strings.Contains(m.Text(), "[memory:") {
			fmt.Printf("  注入: %s\n", preview(m.Text(), 110))
		}
	}

	// ---- ④ candidate：自动提炼只写 Pending（对检索天然不可见）----
	fmt.Println()
	fmt.Println("== ④ candidate：自动提炼 → Pending（未审批不可见）==")
	cand, err := candidate.New(candidate.Options{
		Store:     memStore,
		Extractor: &demoExtractor{items: []store.MemoryItem{{Kind: store.KindLesson, Content: "prefer structured YAML for CI config"}}},
		Namespace: ns,
		OriginFn:  func() store.SourceRef { return store.SourceRef{Type: store.SourceSession, SessionID: "demo-session", Seq: 7} },
	})
	if err != nil {
		return err
	}
	simulated := []*llm.Message{llm.UserText("以后 CI 配置我们都用结构化的 YAML 写。")}
	stored, report, err := cand.Extract(ctx, simulated)
	if err != nil {
		return err
	}
	fmt.Printf("  Extract report: %+v（去重 0——新内容）\n", report)
	visible, err := memStore.Search(ctx, store.MemoryQuery{Namespace: ns})
	if err != nil {
		return err
	}
	fmt.Printf("  Pending %d 条；默认 Search 可见 %d 条 Active（候选不可见 ✓）\n", len(stored), len(visible))
	pending, err := cand.Pending(ctx)
	if err != nil {
		return err
	}

	// ---- ⑤ 审批：Supersede 晋升 + 审批标记；taint 不变 ----
	fmt.Println()
	fmt.Println("== ⑤ 宿主审批：approve = Supersede（人盖章）==")
	for _, p := range pending {
		active, err := cand.Approve(ctx, p.ID)
		if err != nil {
			return err
		}
		fmt.Printf("  批准版 %s: confidence=%.1f status=%s\n", shortID(active.ID), active.Confidence, active.Status)
		fmt.Printf("  taint=%s（批准不改 taint——审批是晋升闸，taint 是数据属性）\n", active.Taint)
		marked := false
		for _, r := range active.SourceRefs {
			if r.Type == store.SourceManual && strings.Contains(r.Ref, "approved via candidate.Pipeline") {
				marked = true
			}
		}
		fmt.Printf("  provenance 审批标记：%v；旧候选已 Superseded 留痕\n", marked)
	}

	// ---- ⑥ 闭环：批准后的记忆下一轮 assemble 可召回 + 指标面 ----
	fmt.Println()
	fmt.Println("== ⑥ 闭环 + 指标：新记忆进入下一轮组装 ==")
	if err := idx.Upsert(ctx, mustFirst(memStore, ctx, "YAML for CI config")); err != nil {
		return err
	}
	ac2, err := assembler.Assemble(ctx, assemble.AssembleInput{
		Namespace: ns,
		Surface:   []*llm.Message{llm.UserText("CI 配置规范是什么？")},
		Query:     "CI 配置规范 YAML",
	})
	if err != nil {
		return err
	}
	found := false
	for _, m := range ac2.Messages {
		if strings.Contains(m.Text(), "YAML for CI config") {
			found = true
			fmt.Printf("  召回到刚批准的记忆：%s\n", preview(m.Text(), 110))
		}
	}
	if !found {
		return fmt.Errorf("approved memory did not reach assembly")
	}
	cm := cand.Metrics()
	fmt.Printf("  candidate.Metrics = %+v（提炼率=Stored/Extracted，批准率=Approved/(Approved+Rejected)）\n", cm)

	fmt.Println()
	fmt.Println("课程要点：自动记忆只写候选、审批人盖章；scope 是存储层边界（本课全部写入钉死在")
	fmt.Printf("%v）；taint 随数据走，批准不洗白。", ns)
	return nil
}

func mustFirst(s store.MemoryStore, ctx context.Context, contains string) store.MemoryItem {
	hits, err := s.Search(ctx, store.MemoryQuery{Namespace: ns})
	if err != nil {
		panic(err)
	}
	for _, h := range hits {
		if strings.Contains(h.Item.Content, contains) {
			return h.Item
		}
	}
	panic("demo: seed memory not found: " + contains)
}

func diagnostics(ac assemble.AssembledContext) string {
	parts := make([]string, 0, len(ac.Diagnostics))
	for _, d := range ac.Diagnostics {
		parts = append(parts, fmt.Sprintf("%s:%s", d.Region, d.Reason))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

func preview(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
