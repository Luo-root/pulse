package candidate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/memory/store"
)

// fakeExtractor 是确定性假提炼 seam。
type fakeExtractor struct {
	items []store.MemoryItem
	err   error
}

func (f *fakeExtractor) Extract(_ context.Context, _ []*llm.Message) ([]store.MemoryItem, error) {
	return f.items, f.err
}

// failingSearchStore 的 Search 永远失败（去重查询中断批次的断言）。
type failingSearchStore struct {
	store.MemoryStore
}

func (f failingSearchStore) Search(_ context.Context, _ store.MemoryQuery) ([]store.MemoryHit, error) {
	return nil, errors.New("store offline")
}

// origin 是测试用固定会话回链。
func origin() store.SourceRef {
	return store.SourceRef{Type: store.SourceSession, SessionID: "s1", Seq: 7}
}

func newPipeline(t *testing.T, mutate func(*Options)) (*Pipeline, Options) {
	t.Helper()
	opt := Options{
		Store:     store.NewMemoryStore(),
		Extractor: &fakeExtractor{},
		Namespace: []string{"tenant:a"},
		OriginFn:  origin,
	}
	if mutate != nil {
		mutate(&opt)
	}
	p, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	return p, opt
}

// prop 是 extractor 视角的候选（只贡献 Kind/Content/Structured）。
func prop(kind, content string) store.MemoryItem {
	return store.MemoryItem{Kind: store.MemoryKind(kind), Content: content}
}

var surface = []*llm.Message{llm.UserText("today we decided things")}

// TestExtractStoresPending：候选以 Pending 入库，taint/source 默认值
// 就位；Pending 列表可见；默认 Search（只 Active）**不可见**。
func TestExtractStoresPending(t *testing.T) {
	p, opt := newPipeline(t, func(o *Options) {
		o.Extractor = &fakeExtractor{items: []store.MemoryItem{
			prop("lesson", "always dry-run first"),
			prop("decision", "use yaml for CI"),
		}}
	})
	stored, rep, err := p.Extract(t.Context(), surface)
	if err != nil {
		t.Fatal(err)
	}
	if rep != (Report{Extracted: 2, Stored: 2}) {
		t.Fatalf("report = %+v, want {2,2,0,0}", rep)
	}
	if len(stored) != 2 {
		t.Fatalf("stored = %d, want 2", len(stored))
	}
	for _, it := range stored {
		if it.Status != store.StatusPending {
			t.Fatalf("%s status = %s, want pending", it.ID, it.Status)
		}
		if it.Taint != store.TaintUntrustedExt {
			t.Fatalf("%s taint = %s, want default untrusted-external（ASI06）", it.ID, it.Taint)
		}
		if len(it.SourceRefs) != 1 || it.SourceRefs[0] != origin() {
			t.Fatalf("%s sourceRefs = %+v, want OriginFn backlink", it.ID, it.SourceRefs)
		}
		if it.Namespace[0] != "tenant:a" {
			t.Fatalf("%s namespace = %v", it.ID, it.Namespace)
		}
	}
	pending, err := p.Pending(t.Context())
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %d/%v, want 2", len(pending), err)
	}
	// 未批准不进上下文：默认 Search 只 Active，Pending 不可见。
	hits, err := opt.Store.Search(t.Context(), store.MemoryQuery{Namespace: []string{"tenant:a"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("default search hits = %d, want 0（未批准不可见）", len(hits))
	}
}

// TestExtractDedupContainment：归一后已有 item 的 Content 包含候选 →
// 丢弃（相等/子串）；超集不拦（超集信息归 Supersede 修订语义）。
func TestExtractDedupContainment(t *testing.T) {
	_, opt := newPipeline(t, nil)
	existing := store.MemoryItem{
		ID: "existing", Namespace: []string{"tenant:a"}, Kind: store.KindProfile,
		Content: "User prefers TOML config", Status: store.StatusActive,
		Confidence: 1.0, Taint: store.TaintTrusted,
		SourceRefs: []store.SourceRef{origin()},
	}
	if _, err := opt.Store.Put(t.Context(), existing, store.PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}
	p2, err := New(Options{
		Store: opt.Store,
		Extractor: &fakeExtractor{items: []store.MemoryItem{
			prop("profile", "user prefers toml  config"),               // 归一后相等 → 重复
			prop("profile", "TOML"),                                    // 子串 → 重复
			prop("profile", "User prefers TOML config and fish shell"), // 超集 → 不拦
		}},
		Namespace: []string{"tenant:a"},
		OriginFn:  origin,
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, rep, err := p2.Extract(t.Context(), surface)
	if err != nil {
		t.Fatal(err)
	}
	if rep != (Report{Extracted: 3, Stored: 1, Duplicates: 2}) {
		t.Fatalf("report = %+v, want {3,1,2,0}", rep)
	}
	if len(stored) != 1 || !strings.Contains(stored[0].Content, "fish shell") {
		t.Fatalf("stored = %+v, want superset only", stored)
	}
}

// TestExtractSkipsInvalid：空 Content / Structured 非法 JSON 计 Invalid，
// 不中断批次。
func TestExtractSkipsInvalid(t *testing.T) {
	p, _ := newPipeline(t, func(o *Options) {
		o.Extractor = &fakeExtractor{items: []store.MemoryItem{
			prop("lesson", ""),
			prop("lesson", "   "),
			{Kind: store.KindLesson, Content: "valid one", Structured: json.RawMessage("{not json")},
			prop("lesson", "valid two"),
		}}
	})
	stored, rep, err := p.Extract(t.Context(), surface)
	if err != nil {
		t.Fatal(err)
	}
	if rep != (Report{Extracted: 4, Stored: 1, Invalid: 3}) {
		t.Fatalf("report = %+v, want {4,1,0,3}", rep)
	}
	if len(stored) != 1 || stored[0].Content != "valid two" {
		t.Fatalf("stored = %+v", stored)
	}
}

// TestExtractErrorPassthrough：提取错误透传（管线不静默）。
func TestExtractErrorPassthrough(t *testing.T) {
	p, _ := newPipeline(t, func(o *Options) {
		o.Extractor = &fakeExtractor{err: errors.New("llm offline")}
	})
	if _, _, err := p.Extract(t.Context(), surface); err == nil || !strings.Contains(err.Error(), "llm offline") {
		t.Fatalf("err = %v, want extractor error passthrough", err)
	}
}

// TestDedupSearchFailureInterrupts：去重查询失败中断批次（store 故障
// 宁可失败让宿主重试，不做半批静默）。
func TestDedupSearchFailureInterrupts(t *testing.T) {
	p, err := New(Options{
		Store:     failingSearchStore{store.NewMemoryStore()},
		Extractor: &fakeExtractor{items: []store.MemoryItem{prop("lesson", "x")}},
		Namespace: []string{"tenant:a"},
		OriginFn:  origin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Extract(t.Context(), surface); err == nil || !strings.Contains(err.Error(), "dedup search") {
		t.Fatalf("err = %v, want dedup search failure", err)
	}
}

// TestApprovePromotesViaSupersede：approve = Supersede——批准版新 ID
// Active（Confidence=1.0、Content/Taint/SourceRefs 继承、taint 不变），
// 旧候选 Superseded 留痕；批准后默认 Search 可见；Pending 清空。
func TestApprovePromotesViaSupersede(t *testing.T) {
	p, opt := newPipeline(t, func(o *Options) {
		o.Extractor = &fakeExtractor{items: []store.MemoryItem{prop("lesson", "always dry-run first")}}
	})
	stored, _, err := p.Extract(t.Context(), surface)
	if err != nil {
		t.Fatal(err)
	}
	candidate := stored[0]
	active, err := p.Approve(t.Context(), candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.Status != store.StatusActive || active.ID == candidate.ID {
		t.Fatalf("active = %+v, want new-id active item", active)
	}
	if active.Confidence != 1.0 {
		t.Fatalf("confidence = %v, want 1.0（批准即宿主背书）", active.Confidence)
	}
	if active.Content != candidate.Content || active.Taint != candidate.Taint {
		t.Fatalf("active = %+v, want content/taint inherited", active)
	}
	if len(active.SourceRefs) != 1 || active.SourceRefs[0] != origin() {
		t.Fatalf("sourceRefs = %+v, want inherited", active.SourceRefs)
	}
	old, err := opt.Store.Get(t.Context(), []string{"tenant:a"}, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != store.StatusSuperseded {
		t.Fatalf("old status = %s, want superseded（留痕）", old.Status)
	}
	hits, err := opt.Store.Search(t.Context(), store.MemoryQuery{Namespace: []string{"tenant:a"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Item.ID != active.ID {
		t.Fatalf("default search = %+v, want approved item visible", hits)
	}
	pending, err := p.Pending(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %d/%v, want 0", len(pending), err)
	}
}

// TestApproveRejectsNonPending：对非 Pending approve/reject 一律
// ErrNotPending（fail closed）；未知 ID 透传 store 哨兵。
func TestApproveRejectsNonPending(t *testing.T) {
	p, _ := newPipeline(t, func(o *Options) {
		o.Extractor = &fakeExtractor{items: []store.MemoryItem{prop("lesson", "x")}}
	})
	stored, _, err := p.Extract(t.Context(), surface)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Approve(t.Context(), stored[0].ID); err != nil {
		t.Fatal(err)
	}
	// 已批准（Superseded）再 approve。
	if _, err := p.Approve(t.Context(), stored[0].ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("err = %v, want ErrNotPending", err)
	}
	// 已批准的候选不能再 reject。
	if err := p.Reject(t.Context(), stored[0].ID, "late"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("err = %v, want ErrNotPending", err)
	}
	// 未知 ID：store 哨兵透传。
	if _, err := p.Approve(t.Context(), "missing"); !errors.Is(err, store.ErrItemNotFound) {
		t.Fatalf("err = %v, want ErrItemNotFound", err)
	}
}

// TestRejectRevokes：reject = Revoke（reason 落审计）；空 reason 拒绝。
func TestRejectRevokes(t *testing.T) {
	p, opt := newPipeline(t, func(o *Options) {
		o.Extractor = &fakeExtractor{items: []store.MemoryItem{prop("lesson", "noisy note")}}
	})
	stored, _, err := p.Extract(t.Context(), surface)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Reject(t.Context(), stored[0].ID, "  "); err == nil {
		t.Fatal("empty reason must fail（审计必填）")
	}
	if err := p.Reject(t.Context(), stored[0].ID, "noisy"); err != nil {
		t.Fatal(err)
	}
	pending, err := p.Pending(t.Context())
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %d/%v, want 0", len(pending), err)
	}
	old, err := opt.Store.Get(t.Context(), []string{"tenant:a"}, stored[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != store.StatusRevoked {
		t.Fatalf("status = %s, want revoked", old.Status)
	}
}

// TestTaintOverrideAndApproveKeepsTaint：Taint 可覆盖；批准晋升不改
// taint（审批是晋升闸，taint 是数据属性）。
func TestTaintOverrideAndApproveKeepsTaint(t *testing.T) {
	p, _ := newPipeline(t, func(o *Options) {
		o.Extractor = &fakeExtractor{items: []store.MemoryItem{prop("lesson", "user said so")}}
		o.Taint = store.TaintUserSupplied
	})
	stored, _, err := p.Extract(t.Context(), surface)
	if err != nil {
		t.Fatal(err)
	}
	if stored[0].Taint != store.TaintUserSupplied {
		t.Fatalf("taint = %s, want overridden user-supplied", stored[0].Taint)
	}
	active, err := p.Approve(t.Context(), stored[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if active.Taint != store.TaintUserSupplied {
		t.Fatalf("approved taint = %s, want unchanged（批准不改 taint）", active.Taint)
	}
}

// TestNewValidation：必填缺失 fail closed。
func TestNewValidation(t *testing.T) {
	base := Options{
		Store:     store.NewMemoryStore(),
		Extractor: &fakeExtractor{},
		Namespace: []string{"tenant:a"},
		OriginFn:  origin,
	}
	if _, err := New(Options{Extractor: base.Extractor, Namespace: base.Namespace, OriginFn: origin}); err == nil {
		t.Fatal("missing store must fail")
	}
	if _, err := New(Options{Store: base.Store, Namespace: base.Namespace, OriginFn: origin}); err == nil {
		t.Fatal("missing extractor must fail")
	}
	if _, err := New(Options{Store: base.Store, Extractor: base.Extractor, OriginFn: origin}); err == nil {
		t.Fatal("missing namespace must fail")
	}
	if _, err := New(Options{Store: base.Store, Extractor: base.Extractor, Namespace: base.Namespace}); err == nil {
		t.Fatal("missing origin fn must fail")
	}
	if _, err := New(Options{Store: base.Store, Extractor: base.Extractor, Namespace: []string{"tenant:a", " "}, OriginFn: origin}); err == nil {
		t.Fatal("empty namespace element must fail")
	}
}
