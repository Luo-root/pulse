package index

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/Luo-root/pulse/memory/store"
)

// AsyncIndexer 是 VectorIndex 的异步包装：Upsert/Remove 进队列、单
// worker 串行执行——embed 是 IO/LLM 成本，写路径不阻塞（§12 P2-D
// 「embedding provider 和异步索引队列」）。
//
// 取舍：队列满 → 丢弃并计数（Dropped 可见），**不背压阻塞写路径**——
// 索引可重建（Rebuild 兜底一致性），丢索引更新比阻塞记忆写入更符合
// 「派生索引」定位。Rebuild 直接透传底层（不进队列）。
type AsyncIndexer struct {
	idx VectorIndex

	// mu 保护 queue 的关闭与发送互斥：enqueue 持 RLock 发送、Close 持
	// Lock 后才 close(queue)。没有它，「通过 closed 检查的 enqueue」可能
	// 对已关闭 channel 发送（panic: send on closed channel——select 的
	// default 只防满队列，防不了已关闭）。
	mu      sync.RWMutex
	queue   chan indexOp
	dropped atomic.Uint64
	closed  atomic.Bool
	wg      sync.WaitGroup
}

// indexOp 是一次索引变更（upsert 携带 item；remove 只用 id）。
type indexOp struct {
	upsert bool
	item   store.MemoryItem
	id     string
}

// NewAsyncIndexer 包装同步 VectorIndex 并启动 worker。queueSize <= 0
// 用默认 64。idx 必填。
func NewAsyncIndexer(idx VectorIndex, queueSize int) (*AsyncIndexer, error) {
	if idx == nil {
		return nil, fmt.Errorf("index: underlying vector index is required")
	}
	if queueSize <= 0 {
		queueSize = 64
	}
	a := &AsyncIndexer{idx: idx, queue: make(chan indexOp, queueSize)}
	a.wg.Add(1)
	go a.run()
	return a, nil
}

// Upsert 实现 VectorIndex：非阻塞入队；满 → 丢弃计数；Close 后拒绝。
func (a *AsyncIndexer) Upsert(_ context.Context, item store.MemoryItem) error {
	return a.enqueue(indexOp{upsert: true, item: item})
}

// Remove 实现 VectorIndex：非阻塞入队（语义同 Upsert）。
func (a *AsyncIndexer) Remove(_ context.Context, id string) error {
	return a.enqueue(indexOp{id: id})
}

// Search 直接透传底层索引（读路径无异步语义）。
func (a *AsyncIndexer) Search(ctx context.Context, ns []string, query string, k int) ([]ScoredHit, error) {
	return a.idx.Search(ctx, ns, query, k)
}

// Rebuild 直接透传底层索引（重建是运维路径，不进队列——否则排在积压
// 之后失去「兜底一致性」的意义）。
func (a *AsyncIndexer) Rebuild(ctx context.Context) error {
	return a.idx.Rebuild(ctx)
}

// Dropped 返回因队列满被丢弃的索引更新数（可观测最小面；指标体系归 D4）。
func (a *AsyncIndexer) Dropped() uint64 {
	return a.dropped.Load()
}

// Close 关队列并等 worker drain 完已入队的操作。之后 Upsert/Remove 拒绝。
func (a *AsyncIndexer) Close(ctx context.Context) error {
	a.mu.Lock()
	if a.closed.CompareAndSwap(false, true) {
		close(a.queue)
	}
	a.mu.Unlock()
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// enqueue 非阻塞入队；满丢弃计数；Close 后拒绝。
//
// RLock 与 Close 的 Lock 互斥：通过 closed 检查后 queue 必未关闭——
// send on closed channel 的竞态窗口由此消除。
func (a *AsyncIndexer) enqueue(op indexOp) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.closed.Load() {
		return ErrIndexClosed
	}
	select {
	case a.queue <- op:
		return nil
	default:
		a.dropped.Add(1)
		return nil // 丢弃不是错误——Rebuild 兜底；计数经 Dropped() 可见
	}
}

// run 是单 worker：串行执行索引变更（保持与同步实现一致的更新顺序）。
func (a *AsyncIndexer) run() {
	defer a.wg.Done()
	for op := range a.queue {
		if op.upsert {
			// 后台 worker 用独立 context：调用方的 ctx 可能已随请求
			// 结束取消，但索引更新不该因此丢失（可重建≠随意丢）。
			_ = a.idx.Upsert(context.Background(), op.item)
		} else {
			_ = a.idx.Remove(context.Background(), op.id)
		}
	}
}
