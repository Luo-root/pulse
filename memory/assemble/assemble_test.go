package assemble

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/llm"
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

// TestRankDeterministic：untrusted-external 降权 + recency 降序；
// Confidence 不参与排序（两个不同 Confidence 的同 taint/recency item 按
// ID 稳定序）。
func TestRankDeterministic(t *testing.T) {
	trusted := item("t1", store.KindLesson, "trusted lesson")
	untrusted := item("u1", store.KindLesson, "untrusted lesson")
	untrusted.Taint = store.TaintUntrustedExt
	ranked := rankHits([]store.MemoryHit{{Item: untrusted}, {Item: trusted}})
	if ranked[0].ID != "t1" {
		t.Fatalf("ranked[0] = %s, want trusted first（taint 降权）", ranked[0].ID)
	}
	// recency。
	old := item("old", store.KindEpisode, "old")
	new := item("new", store.KindEpisode, "new")
	old.UpdatedAt = old.UpdatedAt.Add(-time.Hour)
	ranked = rankHits([]store.MemoryHit{{Item: old}, {Item: new}})
	if ranked[0].ID != "new" {
		t.Fatalf("ranked[0] = %s, want newest first", ranked[0].ID)
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
