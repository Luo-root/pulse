package index

import (
	"testing"

	"github.com/Luo-root/pulse/memory/store"
)

// TestCountedNilInner：nil inner 拒绝（fail closed）。
func TestCountedNilInner(t *testing.T) {
	if _, err := NewCounted(nil); err == nil {
		t.Fatal("nil inner must fail")
	}
}

// TestCountedPassthroughAndCounts：装饰后语义透传（结果/错误原样）+
// 计数准确——调用即计（含失败执行，与 Dropped 互补口径），Hits 只计
// 成功轮。
func TestCountedPassthroughAndCounts(t *testing.T) {
	ctx := t.Context()
	s := store.NewMemoryStore()
	idx := newTestIndex(t, s, &fakeProvider{dims: 2, vecs: map[string][]float32{
		"alpha": unit(2, 0),
	}})
	c, err := NewCounted(idx)
	if err != nil {
		t.Fatal(err)
	}
	// 两条 Active item 入索引。
	it1 := putItem(t, ctx, s, "a1", []string{"tenant:a"}, "alpha doc")
	if err := c.Upsert(ctx, it1); err != nil {
		t.Fatal(err)
	}
	it2 := putItem(t, ctx, s, "a2", []string{"tenant:a"}, "other doc")
	if err := c.Upsert(ctx, it2); err != nil {
		t.Fatal(err)
	}
	// Search 命中：结果与裸索引语义一致（透传）。
	hits, err := c.Search(ctx, []string{"tenant:a"}, "alpha query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].Item.ID != "a1" {
		t.Fatalf("hits = %+v, want a1 first", hits)
	}
	// Search 失败（空 query）：Searches 计数、Hits 不计、错误透传。
	if _, err := c.Search(ctx, []string{"tenant:a"}, "", 5); err == nil {
		t.Fatal("empty query must fail")
	}
	m := c.Metrics()
	if m.Upserts != 2 || m.Searches != 2 || m.Hits != uint64(len(hits)) {
		t.Fatalf("metrics = %+v, want upserts=2 searches=2 hits=%d", m, len(hits))
	}
	if err := c.Remove(ctx, "a2"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	m = c.Metrics()
	if m.Removes != 1 || m.Rebuilds != 1 {
		t.Fatalf("metrics = %+v, want removes=1 rebuilds=1", m)
	}
}
