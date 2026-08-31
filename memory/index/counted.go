package index

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/Luo-root/pulse/memory/store"
)

// CountedMetrics 是 Counted 装饰器的累计计数快照。
type CountedMetrics struct {
	// Upserts 是累计 Upsert 调用数（含失败执行——「执行了但失败」也是
	// 真实系统行为；内层叠放时与 AsyncIndexer.Dropped 互补：入队数 =
	// Upserts + Dropped）。
	Upserts uint64
	// Removes 是累计 Remove 调用数。
	Removes uint64
	// Searches 是累计 Search 调用数（含失败）。
	Searches uint64
	// Hits 是累计 Search 命中 item 总数（只计成功返回；次均命中 =
	// Hits/Searches）。
	Hits uint64
	// Rebuilds 是累计 Rebuild 调用数。
	Rebuilds uint64
}

// Counted 是 VectorIndex 计数装饰器（D4 指标面，票 #92）——「召回命中」
// 的运行计数面。
//
// 叠放顺序决定 Upserts 口径：推荐**内层** `AsyncIndexer(Counted(idx))`
// ——Upserts 计「实际执行」，与 Dropped 互补；外层叠放
// `Counted(AsyncIndexer(idx))` 则计「入队调用」（含后续被队列丢弃的，
// 与 Dropped 双计）。Counted 对外仍是普通 VectorIndex，装配层按需包。
//
// 注意：Searches/Hits 是运行计数（次均命中观测），不是 §13.2 的
// Recall@K——那是离线评测指标，两者不可混用。
type Counted struct {
	inner    VectorIndex
	upserts  atomic.Uint64
	removes  atomic.Uint64
	searches atomic.Uint64
	hits     atomic.Uint64
	rebuilds atomic.Uint64
}

// NewCounted 包装 inner 为计数装饰器（nil inner 拒绝）。
func NewCounted(inner VectorIndex) (*Counted, error) {
	if inner == nil {
		return nil, fmt.Errorf("index: inner vector index is required")
	}
	return &Counted{inner: inner}, nil
}

// Upsert 实现 VectorIndex：调用即计数，语义透传。
func (c *Counted) Upsert(ctx context.Context, item store.MemoryItem) error {
	c.upserts.Add(1)
	return c.inner.Upsert(ctx, item)
}

// Remove 实现 VectorIndex：调用即计数，语义透传。
func (c *Counted) Remove(ctx context.Context, id string) error {
	c.removes.Add(1)
	return c.inner.Remove(ctx, id)
}

// Search 实现 VectorIndex：调用数恒计；命中数只在成功返回时累计。结果
// 与错误**原样透传**（错误路径也返回 inner 的 hits——不假设其为 nil）。
func (c *Counted) Search(ctx context.Context, ns []string, query string, k int) ([]ScoredHit, error) {
	c.searches.Add(1)
	hits, err := c.inner.Search(ctx, ns, query, k)
	if err != nil {
		return hits, err
	}
	c.hits.Add(uint64(len(hits)))
	return hits, nil
}

// Rebuild 实现 VectorIndex：调用即计数，语义透传。
func (c *Counted) Rebuild(ctx context.Context) error {
	c.rebuilds.Add(1)
	return c.inner.Rebuild(ctx)
}

// Metrics 返回累计计数快照（atomic 读，-race 安全）。
func (c *Counted) Metrics() CountedMetrics {
	return CountedMetrics{
		Upserts:  c.upserts.Load(),
		Removes:  c.removes.Load(),
		Searches: c.searches.Load(),
		Hits:     c.hits.Load(),
		Rebuilds: c.rebuilds.Load(),
	}
}
