package assemble

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/index"
	"github.com/Luo-root/pulse/memory/store"
)

// countingStore 包装内存 store：记录 Search 调用次数（缓存语义断言）。
type countingStore struct {
	inner  store.MemoryStore
	mu     sync.Mutex
	search int
}

func (c *countingStore) Put(ctx context.Context, it store.MemoryItem, o store.PutMemoryOptions) (store.MemoryItem, error) {
	return c.inner.Put(ctx, it, o)
}
func (c *countingStore) Get(ctx context.Context, ns []string, id string) (store.MemoryItem, error) {
	return c.inner.Get(ctx, ns, id)
}
func (c *countingStore) Search(ctx context.Context, q store.MemoryQuery) ([]store.MemoryHit, error) {
	c.mu.Lock()
	c.search++
	c.mu.Unlock()
	return c.inner.Search(ctx, q)
}
func (c *countingStore) Supersede(ctx context.Context, oldID string, next store.MemoryItem) (store.MemoryItem, error) {
	return c.inner.Supersede(ctx, oldID, next)
}
func (c *countingStore) Revoke(ctx context.Context, id, reason string) error {
	return c.inner.Revoke(ctx, id, reason)
}
func (c *countingStore) searches() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.search
}

// failingStore 的 Search 永远失败（召回失败不中断组装的断言）。
type failingStore struct{ store.MemoryStore }

func (f failingStore) Search(ctx context.Context, q store.MemoryQuery) ([]store.MemoryHit, error) {
	return nil, errors.New("store offline")
}

func item(id string, kind store.MemoryKind, content string) store.MemoryItem {
	return store.MemoryItem{
		ID:         id,
		Namespace:  []string{"tenant:a"},
		Kind:       kind,
		Content:    content,
		Status:     store.StatusActive,
		Confidence: 1.0,
		Taint:      store.TaintTrusted,
		SourceRefs: []store.SourceRef{{Type: store.SourceSession, SessionID: "s9", Seq: 12}},
	}
}

// seedStable 落两条稳定记忆（profile + decision）。
func seedStable(t *testing.T, ctx context.Context, s store.MemoryStore) {
	t.Helper()
	for _, it := range []store.MemoryItem{
		item("p1", store.KindProfile, "User prefers TOML config"),
		item("d1", store.KindDecision, "Use yaml for CI"),
	} {
		if _, err := s.Put(ctx, it, store.PutMemoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAssembleOrderAndCitations：组装顺序（稳定前缀 → surface → 检索 →
// injected）、StablePrefixLen、引用模板带 SourceRefs。
func TestAssembleOrderAndCitations(t *testing.T) {
	cs := &countingStore{inner: store.NewMemoryStore()}
	ctx := t.Context()
	seedStable(t, ctx, cs)
	if _, err := cs.Put(ctx, item("e1", store.KindEpisode, "deployed via kubectl yesterday"), store.PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	a := NewDefaultAssembler(cs, nil, Budget{})
	surface := []*llm.Message{llm.UserText("how do I deploy?")}
	ac, err := a.Assemble(ctx, AssembleInput{
		Namespace: []string{"tenant:a"},
		Surface:   surface,
		Query:     "deploy",
		Injected:  []store.MemoryItem{item("i1", store.KindLesson, "always dry-run first")},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 顺序：2 条稳定 → 1 条 surface → 1 条检索（episode 命中 deploy）→ 1 条 injected。
	if len(ac.Messages) != 5 {
		t.Fatalf("messages = %d (%+v), want 5", len(ac.Messages), ac.Messages)
	}
	// 稳定前缀按最新 revision 优先（UpdatedAt 降序）：decision d1 最新在前。
	if ac.StablePrefixLen != 2 {
		t.Fatalf("StablePrefixLen = %d, want 2", ac.StablePrefixLen)
	}
	if !strings.Contains(ac.Messages[0].Text(), "Use yaml for CI") {
		t.Fatalf("prefix[0] = %q（最新在前）", ac.Messages[0].Text())
	}
	if !strings.Contains(ac.Messages[1].Text(), "User prefers TOML config") {
		t.Fatalf("prefix[1] = %q", ac.Messages[1].Text())
	}
	if ac.Messages[2] != surface[0] {
		t.Fatal("surface must be kept by reference (原样不裁切)")
	}
	if !strings.Contains(ac.Messages[3].Text(), "[memory:episode e1 (source: session s9#12)]") {
		t.Fatalf("citation template missing: %q", ac.Messages[3].Text())
	}
	if !strings.Contains(ac.Messages[4].Text(), "[memory:lesson i1") {
		t.Fatalf("injected missing: %q", ac.Messages[4].Text())
	}
}

// TestStableSnapshotCache：同 namespace 二次组装命中缓存（不重查 store）；
// RefreshStable 重建；不同 namespace 缓存互不污染（§8.3）。
func TestStableSnapshotCache(t *testing.T) {
	cs := &countingStore{inner: store.NewMemoryStore()}
	ctx := t.Context()
	seedStable(t, ctx, cs)
	a := NewDefaultAssembler(cs, nil, Budget{})
	nsA := []string{"tenant:a"}
	nsB := []string{"tenant:b"}
	for i := 0; i < 2; i++ {
		if _, err := a.Assemble(ctx, AssembleInput{Namespace: nsA}); err != nil {
			t.Fatal(err)
		}
	}
	if got := cs.searches(); got != 1 {
		t.Fatalf("searches = %d, want 1（第二次组装必须命中缓存）", got)
	}
	// RefreshStable 重建。
	if _, err := a.Assemble(ctx, AssembleInput{Namespace: nsA, RefreshStable: true}); err != nil {
		t.Fatal(err)
	}
	if got := cs.searches(); got != 2 {
		t.Fatalf("searches after refresh = %d, want 2", got)
	}
	// 不同 namespace 隔离：B 的组装不命中 A 的缓存，也看不到 A 的 item。
	hits, err := a.Assemble(ctx, AssembleInput{Namespace: nsB})
	if err != nil {
		t.Fatal(err)
	}
	if hits.StablePrefixLen != 0 {
		t.Fatalf("nsB prefix = %d, want 0（namespace 互不污染）", hits.StablePrefixLen)
	}
	if got := cs.searches(); got != 3 {
		t.Fatalf("searches for nsB = %d, want 3", got)
	}
}

// TestBudgetDiagnostics：三类预算超限都有诊断（预算可解释），且行为正确
// ——稳定记忆按预算省略、检索降 top-k、surface 只诊断不裁切。
func TestBudgetDiagnostics(t *testing.T) {
	cs := &countingStore{inner: store.NewMemoryStore()}
	ctx := t.Context()
	seedStable(t, ctx, cs)
	for _, it := range []store.MemoryItem{
		item("e1", store.KindEpisode, "episode one about deploy"),
		item("e2", store.KindEpisode, "episode two about deploy rollback"),
		item("e3", store.KindEpisode, "episode three about deploy alerts"),
	} {
		if _, err := cs.Put(ctx, it, store.PutMemoryOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	a := NewDefaultAssembler(cs, nil, Budget{
		StableMemoryTokens: 20, // 只装得下最新一条稳定记忆（估算 ~16/条）
		RetrievedTokens:    25, // 检索降 top-k：约一条 episode 的成本
		MaxSurfaceTail:     2,
	})
	surface := []*llm.Message{llm.UserText("q1"), llm.UserText("q2"), llm.UserText("q3")}
	ac, err := a.Assemble(ctx, AssembleInput{
		Namespace: []string{"tenant:a"},
		Surface:   surface,
		Query:     "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	var stableDiag, retrievedDiag, surfaceDiag bool
	for _, d := range ac.Diagnostics {
		switch d.Region {
		case "stable-memory":
			stableDiag = d.Dropped > 0
		case "retrieved":
			retrievedDiag = d.Dropped > 0
		case "surface-tail":
			surfaceDiag = d.Dropped == 1 // 只诊断
		}
	}
	if !stableDiag || !retrievedDiag || !surfaceDiag {
		t.Fatalf("diagnostics incomplete: %+v", ac.Diagnostics)
	}
	// surface 原样保留（不裁切）。
	for i, m := range surface {
		if ac.Messages[ac.StablePrefixLen+i] != m {
			t.Fatal("surface tail must be intact（裁切归 compaction/prune）")
		}
	}
	// 检索 top-k 收缩：预算内至少一条。
	retrieved := ac.Messages[ac.StablePrefixLen+len(surface):]
	if len(retrieved) == 0 || len(retrieved) >= 3 {
		t.Fatalf("retrieved = %d, want within (0, 3)", len(retrieved))
	}
}

// TestFusionOrder：§8.2 融合排序（D2 默认权重）——双路命中 > semantic
// 单路 > keyword 单路；同 item 双路命中只出现一次（去重，4 输入 3 块）。
func TestFusionOrder(t *testing.T) {
	a := &DefaultAssembler{
		Semantic: func(ctx context.Context, ns []string, q string, k int) ([]store.MemoryItem, []float64, error) {
			return []store.MemoryItem{
					item("both", store.KindEpisode, "b"),
					item("sem", store.KindEpisode, "s"),
				},
				[]float64{0.9, 0.9}, nil
		},
	}
	kwHits := []store.MemoryHit{
		{Item: item("kw", store.KindEpisode, "k")},
		{Item: item("both", store.KindEpisode, "b")},
	}
	var diag []Diagnostic
	ranked := a.fuseAndRank(t.Context(), AssembleInput{Namespace: []string{"tenant:a"}, Query: "deploy"}, kwHits, DefaultRankingWeights, &diag)
	// both = 0.3 + 0.5*0.9 = 0.75；sem = 0.45；kw = 0.3。
	want := []string{"both", "sem", "kw"}
	if len(ranked) != len(want) {
		t.Fatalf("ranked = %d, want %d（4 输入去重为 3）", len(ranked), len(want))
	}
	for i, id := range want {
		if ranked[i].ID != id {
			t.Fatalf("ranked[%d] = %s, want %s", i, ranked[i].ID, id)
		}
	}
}

// TestFusionConfidenceAndTaint：D2 启用 w_conf（同路命中 conf 高者前）；
// P2-C 的 taint 固定 −4 由 w_taint*taint_pen 取代。
func TestFusionConfidenceAndTaint(t *testing.T) {
	a := &DefaultAssembler{}
	var diag []Diagnostic
	in := AssembleInput{Namespace: []string{"tenant:a"}, Query: "deploy"}

	hiConf := item("hi", store.KindLesson, "h")
	hiConf.Confidence = 0.9
	loConf := item("lo", store.KindLesson, "l")
	loConf.Confidence = 0.5
	ranked := a.fuseAndRank(t.Context(), in, []store.MemoryHit{{Item: loConf}, {Item: hiConf}}, DefaultRankingWeights, &diag)
	// keyword-only：hi = 0.3+0.2*0.9 = 0.48 > lo = 0.3+0.2*0.5 = 0.4。
	if ranked[0].ID != "hi" {
		t.Fatalf("ranked[0] = %s, want higher confidence first（w_conf D2 启用）", ranked[0].ID)
	}

	untrusted := item("u", store.KindLesson, "u")
	untrusted.Taint = store.TaintUntrustedExt
	trusted := item("t", store.KindLesson, "t")
	ranked = a.fuseAndRank(t.Context(), in, []store.MemoryHit{{Item: untrusted}, {Item: trusted}}, DefaultRankingWeights, &diag)
	// t = 0.3+0.2 = 0.5；u = 0.5 − 0.3 = 0.2。
	if ranked[0].ID != "t" {
		t.Fatalf("ranked[0] = %s, want trusted first（w_taint 取代固定 −4）", ranked[0].ID)
	}
}

// TestFusionConfidenceClamp：w_conf 输入归一（clamp01）——store 只强制
// Active conf > 0、无上限，conf=5 未归一时以 1.3 碾碎双路命中 0.65；
// clamp 后与 conf=1 同分（审稿必修项的回归锁）。
func TestFusionConfidenceClamp(t *testing.T) {
	a := &DefaultAssembler{
		Semantic: func(ctx context.Context, ns []string, q string, k int) ([]store.MemoryItem, []float64, error) {
			return []store.MemoryItem{item("b", store.KindEpisode, "b")}, []float64{0.9}, nil
		},
	}
	var diag []Diagnostic
	in := AssembleInput{Namespace: []string{"tenant:a"}, Query: "deploy"}
	inflated := item("a", store.KindEpisode, "a")
	inflated.Confidence = 5 // store 无上限校验；排序侧必须 clamp
	plain := item("k", store.KindEpisode, "k")
	ranked := a.fuseAndRank(t.Context(), in, []store.MemoryHit{{Item: inflated}, {Item: plain}}, DefaultRankingWeights, &diag)
	// clamp 后：b = 0.45+0.2 = 0.65；a = k = 0.3+0.2 = 0.5（tie → ID asc）。
	if ranked[0].ID != "b" {
		t.Fatalf("ranked[0] = %s, want double-path hit b（conf=5 未归一时会以 1.3 碾碎 0.65）", ranked[0].ID)
	}
	if ranked[1].ID != "a" || ranked[2].ID != "k" {
		t.Fatalf("ranked = %v, want [b a k]（clamp 后 a=k=0.5，ID asc）", []string{ranked[0].ID, ranked[1].ID, ranked[2].ID})
	}
}

// TestSemanticFiltersInactive：semantic 路非 Active 过滤——seam 是任意
// 宿主函数，契约之外的实现可能不复核状态，融合层兜底（审稿建议 2）。
func TestSemanticFiltersInactive(t *testing.T) {
	a := &DefaultAssembler{
		Semantic: func(ctx context.Context, ns []string, q string, k int) ([]store.MemoryItem, []float64, error) {
			active := item("v1", store.KindEpisode, "v1")
			superseded := item("v2", store.KindEpisode, "v2")
			superseded.Status = store.StatusSuperseded
			revoked := item("v3", store.KindEpisode, "v3")
			revoked.Status = store.StatusRevoked
			return []store.MemoryItem{active, superseded, revoked}, []float64{0.9, 0.9, 0.9}, nil
		},
	}
	var diag []Diagnostic
	ranked := a.fuseAndRank(t.Context(), AssembleInput{Namespace: []string{"tenant:a"}, Query: "deploy"}, nil, DefaultRankingWeights, &diag)
	if len(ranked) != 1 || ranked[0].ID != "v1" {
		t.Fatalf("ranked = %+v, want only active v1（非 Active 不进融合）", ranked)
	}
}

// TestFusionTiebreak：同分 → UpdatedAt 降序 → ID 升序（确定性）。
func TestFusionTiebreak(t *testing.T) {
	a := &DefaultAssembler{}
	var diag []Diagnostic
	in := AssembleInput{Namespace: []string{"tenant:a"}, Query: "deploy"}
	old := item("old", store.KindEpisode, "same content")
	newer := item("new", store.KindEpisode, "same content")
	old.UpdatedAt = old.UpdatedAt.Add(-time.Hour)
	ranked := a.fuseAndRank(t.Context(), in, []store.MemoryHit{{Item: old}, {Item: newer}}, DefaultRankingWeights, &diag)
	if ranked[0].ID != "new" || ranked[1].ID != "old" {
		t.Fatalf("ranked = %v, want newest first on tie", []string{ranked[0].ID, ranked[1].ID})
	}
	sameA := item("a", store.KindEpisode, "x")
	sameB := item("b", store.KindEpisode, "x")
	ranked = a.fuseAndRank(t.Context(), in, []store.MemoryHit{{Item: sameB}, {Item: sameA}}, DefaultRankingWeights, &diag)
	if ranked[0].ID != "a" || ranked[1].ID != "b" {
		t.Fatalf("ranked = %v, want ID asc on full tie", []string{ranked[0].ID, ranked[1].ID})
	}
}

// TestSemanticFailureDiag：Semantic 失败 → 诊断 + keyword 结果照常
//（fail safe，与 FTS 失败同口径，组装不中断）。
func TestSemanticFailureDiag(t *testing.T) {
	cs := &countingStore{inner: store.NewMemoryStore()}
	ctx := t.Context()
	seedStable(t, ctx, cs)
	if _, err := cs.Put(ctx, item("e1", store.KindEpisode, "deploy via kubectl"), store.PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	a := NewDefaultAssembler(cs, nil, Budget{})
	a.Semantic = func(ctx context.Context, ns []string, q string, k int) ([]store.MemoryItem, []float64, error) {
		return nil, nil, errors.New("vector offline")
	}
	ac, err := a.Assemble(ctx, AssembleInput{Namespace: []string{"tenant:a"}, Query: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range ac.Diagnostics {
		if strings.Contains(d.Reason, "semantic search failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %+v, want semantic-failure entry", ac.Diagnostics)
	}
	// keyword 结果照常：稳定 2 条 + 检索 e1。
	if len(ac.Messages) != 3 {
		t.Fatalf("messages = %d, want 3（稳定 2 + 检索 1）", len(ac.Messages))
	}
}

// TestSemanticShapeMismatchDiag：items/scores 长度不符 → 丢弃语义路
//（诊断），keyword 照常。
func TestSemanticShapeMismatchDiag(t *testing.T) {
	cs := &countingStore{inner: store.NewMemoryStore()}
	ctx := t.Context()
	if _, err := cs.Put(ctx, item("e1", store.KindEpisode, "deploy via kubectl"), store.PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	a := NewDefaultAssembler(cs, nil, Budget{})
	a.Semantic = func(ctx context.Context, ns []string, q string, k int) ([]store.MemoryItem, []float64, error) {
		return []store.MemoryItem{item("v1", store.KindEpisode, "vec hit")}, []float64{0.9, 0.8}, nil
	}
	ac, err := a.Assemble(ctx, AssembleInput{Namespace: []string{"tenant:a"}, Query: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range ac.Diagnostics {
		if strings.Contains(d.Reason, "shape mismatch") {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %+v, want shape-mismatch entry", ac.Diagnostics)
	}
	// 语义路被丢弃：只剩 keyword 命中 e1（稳定前缀为空）。
	if len(ac.Messages) != 1 {
		t.Fatalf("messages = %d, want 1（语义路丢弃，keyword 照常）", len(ac.Messages))
	}
}

// TestRankingOverride：Ranking 覆盖——Keyword=0/Semantic=1 时 semantic
// 独占排序（kw-only 得 0 分垫底）；指针 nil = 默认、显式 0 = 关闭。
func TestRankingOverride(t *testing.T) {
	a := &DefaultAssembler{
		Semantic: func(ctx context.Context, ns []string, q string, k int) ([]store.MemoryItem, []float64, error) {
			return []store.MemoryItem{item("v", store.KindEpisode, "v")}, []float64{0.8}, nil
		},
		Ranking: &RankingWeights{Semantic: 1, Keyword: 0, Confidence: 0, Taint: 0},
	}
	var diag []Diagnostic
	ranked := a.fuseAndRank(t.Context(), AssembleInput{Namespace: []string{"tenant:a"}, Query: "deploy"},
		[]store.MemoryHit{{Item: item("k", store.KindEpisode, "k")}}, *a.Ranking, &diag)
	// v = 0.8；k = 0。
	if ranked[0].ID != "v" || ranked[1].ID != "k" {
		t.Fatalf("ranked = %v, want semantic-dominant order", []string{ranked[0].ID, ranked[1].ID})
	}
}

// e2eProvider 是与 index 包测试同构的确定性假 embedding（首词 → 预置
// 向量，未登记词给零向量）。
type e2eProvider struct {
	dims int
	vecs map[string][]float32
}

func (f *e2eProvider) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = make([]float32, f.dims)
		if fs := strings.Fields(t); len(fs) > 0 {
			if v, ok := f.vecs[fs[0]]; ok {
				copy(out[i], v)
			}
		}
	}
	return out, nil
}

// TestHybridE2EWithMemIndex：MemIndex 包成 Semantic seam 接进
// Assemble——keyword（子串）+ semantic（余弦）双路命中同一 item，融合
// 去重后注入一次；诊断含 semantic seam 标记。
func TestHybridE2EWithMemIndex(t *testing.T) {
	s := store.NewMemoryStore()
	ctx := t.Context()
	seedStable(t, ctx, s)
	saved, err := s.Put(ctx, item("e1", store.KindEpisode, "deploy via kubectl"), store.PutMemoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	memIdx, err := index.NewMemIndex(s, &e2eProvider{dims: 4, vecs: map[string][]float32{"deploy": {1, 0, 0, 0}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := memIdx.Upsert(ctx, saved); err != nil {
		t.Fatal(err)
	}
	a := NewDefaultAssembler(s, nil, Budget{})
	a.Semantic = func(ctx context.Context, ns []string, q string, k int) ([]store.MemoryItem, []float64, error) {
		hits, err := memIdx.Search(ctx, ns, q, k)
		if err != nil {
			return nil, nil, err
		}
		items := make([]store.MemoryItem, len(hits))
		scores := make([]float64, len(hits))
		for i, h := range hits {
			items[i], scores[i] = h.Item, h.Score
		}
		return items, scores, nil
	}
	ac, err := a.Assemble(ctx, AssembleInput{Namespace: []string{"tenant:a"}, Query: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	// 稳定前缀 2 条 + e1 恰好一次（双路命中去重）。
	if len(ac.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(ac.Messages))
	}
	count := 0
	for _, m := range ac.Messages {
		if strings.Contains(m.Text(), "e1") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("e1 appears %d times, want 1（双路去重）", count)
	}
	viaSemantic := false
	for _, d := range ac.Diagnostics {
		if strings.Contains(d.Reason, "recall via semantic seam") {
			viaSemantic = true
		}
	}
	if !viaSemantic {
		t.Fatalf("diagnostics = %+v, want semantic seam entry", ac.Diagnostics)
	}
}
// TestSearchFailureDiag：store 召回失败 → 诊断记录、组装不中断、surface
// 照常。
func TestSearchFailureDiag(t *testing.T) {
	a := NewDefaultAssembler(failingStore{store.NewMemoryStore()}, nil, Budget{})
	ac, err := a.Assemble(t.Context(), AssembleInput{
		Namespace: []string{"tenant:a"},
		Surface:   []*llm.Message{llm.UserText("q")},
		Query:     "anything",
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range ac.Diagnostics {
		if d.Region == "retrieved" && strings.Contains(d.Reason, "store offline") {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics = %+v, want search-failure entry（召回失败不静默）", ac.Diagnostics)
	}
	if len(ac.Messages) != 1 {
		t.Fatalf("messages = %d, want 1（surface 照常）", len(ac.Messages))
	}
}

// TestInjectedWithoutBudget：injected 不受预算约束（用户明确要求）。
func TestInjectedWithoutBudget(t *testing.T) {
	a := NewDefaultAssembler(store.NewMemoryStore(), nil, Budget{RetrievedTokens: 0, StableMemoryTokens: 0})
	ac, err := a.Assemble(t.Context(), AssembleInput{
		Namespace: []string{"tenant:a"},
		Injected:  []store.MemoryItem{item("i1", store.KindLesson, "dry-run first")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ac.StablePrefixLen != 0 || len(ac.Messages) != 1 || !strings.Contains(ac.Messages[0].Text(), "dry-run first") {
		t.Fatalf("injected = %+v", ac.Messages)
	}
}

// ftsStore 是带 FTS 能力的假 store：SearchFTS 行为可注入（断言 FTS 优先
// 与失败回退两路召回）。
type ftsStore struct {
	store.MemoryStore
	hits []store.MemoryHit
	err  error
}

func (f *ftsStore) SearchFTS(ctx context.Context, ns []string, match string, limit int) ([]store.MemoryHit, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

// hasDiag 断言诊断列表里存在 Region 匹配且 Reason 含子串的条目。
func hasDiag(diags []Diagnostic, region, reasonSub string) bool {
	for _, d := range diags {
		if d.Region == region && strings.Contains(d.Reason, reasonSub) {
			return true
		}
	}
	return false
}

// TestRecallFTSPath：实现 SearchFTS 的 store 优先走 FTS token 召回
// （viaFTS 诊断）；FTS 失败回退 Search 子串（fallback 诊断）。两路候选
// 都过 rankHits 统一排序。
func TestRecallFTSPath(t *testing.T) {
	ctx := t.Context()
	ns := []string{"tenant:a"}

	// FTS 命中：f1 只来自 SearchFTS（内存 store 里没有），证明走了 FTS 路。
	fs := &ftsStore{
		MemoryStore: store.NewMemoryStore(),
		hits:        []store.MemoryHit{{Item: item("f1", store.KindEpisode, "fts token prefix hit")}},
	}
	ac, err := NewDefaultAssembler(fs, nil, Budget{}).Assemble(ctx, AssembleInput{Namespace: ns, Query: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ac.Messages) != 1 || !strings.Contains(ac.Messages[0].Text(), "fts token prefix hit") {
		t.Fatalf("messages = %+v, want FTS hit f1", ac.Messages)
	}
	if !hasDiag(ac.Diagnostics, "retrieved", "recall via fts token prefix") {
		t.Fatalf("diagnostics = %+v, want viaFTS entry", ac.Diagnostics)
	}

	// FTS 失败：回退子串 Search（e1 只在内存 store 里），诊断注明 fallback。
	inner := store.NewMemoryStore()
	if _, err := inner.Put(ctx, item("e1", store.KindEpisode, "deployed via kubectl yesterday"), store.PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	fs2 := &ftsStore{MemoryStore: inner, err: errors.New("fts syntax error")}
	ac2, err := NewDefaultAssembler(fs2, nil, Budget{}).Assemble(ctx, AssembleInput{Namespace: ns, Query: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ac2.Messages) != 1 || !strings.Contains(ac2.Messages[0].Text(), "deployed via kubectl") {
		t.Fatalf("messages = %+v, want substring fallback hit e1", ac2.Messages)
	}
	if !hasDiag(ac2.Diagnostics, "retrieved", "fts failed, falling back to substring") {
		t.Fatalf("diagnostics = %+v, want fallback entry", ac2.Diagnostics)
	}
}

// brokenStore 包装内存 store：Search 可注入故障（snapshot 重建失败的
// 退回路径断言）。
type brokenStore struct {
	store.MemoryStore
	mu   sync.Mutex
	fail bool
}

func (b *brokenStore) Search(ctx context.Context, q store.MemoryQuery) ([]store.MemoryHit, error) {
	b.mu.Lock()
	fail := b.fail
	b.mu.Unlock()
	if fail {
		return nil, errors.New("store offline")
	}
	return b.MemoryStore.Search(ctx, q)
}

// TestSnapshotRefreshFailureFallsBack：RefreshStable 重建失败 → 有旧缓存
// 退 stale 快照（不空、不中断）+ 诊断；store 恢复后重建成功。
func TestSnapshotRefreshFailureFallsBack(t *testing.T) {
	bs := &brokenStore{MemoryStore: store.NewMemoryStore()}
	ctx := t.Context()
	seedStable(t, ctx, bs)
	a := NewDefaultAssembler(bs, nil, Budget{})
	ns := []string{"tenant:a"}

	ac1, err := a.Assemble(ctx, AssembleInput{Namespace: ns})
	if err != nil {
		t.Fatal(err)
	}
	if ac1.StablePrefixLen != 2 {
		t.Fatalf("StablePrefixLen = %d, want 2（首次重建成功）", ac1.StablePrefixLen)
	}

	// store 故障 + RefreshStable：退回旧快照 + stale 诊断，组装不中断。
	bs.mu.Lock()
	bs.fail = true
	bs.mu.Unlock()
	ac2, err := a.Assemble(ctx, AssembleInput{Namespace: ns, RefreshStable: true})
	if err != nil {
		t.Fatal(err)
	}
	if ac2.StablePrefixLen != 2 {
		t.Fatalf("stale StablePrefixLen = %d, want 2（退回旧快照）", ac2.StablePrefixLen)
	}
	if !hasDiag(ac2.Diagnostics, "stable-snapshot", "serving stale snapshot") {
		t.Fatalf("diagnostics = %+v, want stale entry", ac2.Diagnostics)
	}

	// store 恢复：重建成功。
	bs.mu.Lock()
	bs.fail = false
	bs.mu.Unlock()
	ac3, err := a.Assemble(ctx, AssembleInput{Namespace: ns, RefreshStable: true})
	if err != nil {
		t.Fatal(err)
	}
	if ac3.StablePrefixLen != 2 || !hasDiag(ac3.Diagnostics, "stable-snapshot", "rebuilt") {
		t.Fatalf("recovery prefix = %d diag = %+v, want rebuilt with 2", ac3.StablePrefixLen, ac3.Diagnostics)
	}
}
