package candidate

import (
	"testing"

	"github.com/Luo-root/pulse/memory/store"
)

// TestReportDuplicateHitsExplainCollisions：被去重拦下的候选要能解释
// 「撞了哪条、什么状态」——尤其撞的是 Revoked 存量时（v1 保守口径仍然
// 拦下候选，但宿主得知道原因，不必去翻全库）。
func TestReportDuplicateHitsExplainCollisions(t *testing.T) {
	ctx := t.Context()
	const content = "User prefers TOML config"
	p, opt := newPipeline(t, func(o *Options) {
		o.Extractor = &fakeExtractor{items: []store.MemoryItem{prop(string(store.KindProfile), content)}}
	})

	existing := store.MemoryItem{
		ID:         "keep",
		Namespace:  opt.Namespace,
		Kind:       store.KindProfile,
		Content:    content,
		Status:     store.StatusActive,
		Confidence: 1.0,
		Taint:      store.TaintTrusted,
		SourceRefs: []store.SourceRef{origin()},
	}
	if _, err := opt.Store.Put(ctx, existing, store.PutMemoryOptions{}); err != nil {
		t.Fatal(err)
	}

	_, rep, err := p.Extract(ctx, surface)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Duplicates != 1 || len(rep.DuplicateHits) != 1 {
		t.Fatalf("report = %+v, want exactly one explained duplicate", rep)
	}
	if got := rep.DuplicateHits[0]; got.ID != "keep" || got.Status != store.StatusActive {
		t.Fatalf("duplicate hit = %+v, want keep/active", got)
	}

	// 撤销之后同一文本再被观察到：仍然拦下（不去打扰审批面），但状态可解释。
	if err := opt.Store.Revoke(ctx, "keep", "user revoked"); err != nil {
		t.Fatal(err)
	}
	_, rep2, err := p.Extract(ctx, surface)
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Stored != 0 {
		t.Fatalf("revoked content must stay blocked, stored = %d", rep2.Stored)
	}
	if rep2.Duplicates != 1 || len(rep2.DuplicateHits) != 1 || rep2.DuplicateHits[0].Status != store.StatusRevoked {
		t.Fatalf("after revoke: report = %+v, want a revoked-status duplicate hit", rep2)
	}
}
