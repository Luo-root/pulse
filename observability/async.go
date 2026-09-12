package observability

import (
	"context"
	"sync"
	"sync/atomic"
)

// AsyncOption 配置 AsyncSink。
type AsyncOption func(*asyncOpts)

type asyncOpts struct {
	capacity int
	dropFull bool
}

// WithCapacity 设置队列容量（缺省 defaultAsyncCapacity；n <= 0 视为编程
// 错误，构造时 panic——与 time.NewTicker 同风格）。
func WithCapacity(n int) AsyncOption {
	return func(o *asyncOpts) { o.capacity = n }
}

// DropOnFull 让队列满时丢弃**新**记录（计入 Dropped）而不是阻塞写入方。
// 缺省是阻塞：不丢记录，把背压留给生产者。
func DropOnFull() AsyncOption {
	return func(o *asyncOpts) { o.dropFull = true }
}

const defaultAsyncCapacity = 1024

// AsyncSink 把任意 Sink 包成**异步出口**：Write 只做 Attrs 深拷 + 入队即
// 返回，单后台协程按 FIFO 调 inner.Write。用途是把手慢的出口（文件/网络
// 导出器）从调用方 goroutine 上摘掉——kernel 的事件派发全同步
// （Emit/EmitLocal/Waterfall，Parallel 也等完成），Sink 有多慢，请求与
// agent 步进就有多慢。
//
// 语义（与 #172 的规格一致）：
//   - **有界队列**，缺省容量 1024；满时缺省**阻塞**（回压），DropOnFull()
//     改为丢新（计入 Dropped）；
//   - **Flush(ctx)** 等「调用时刻已入队（含在途）」的记录全部送达；ctx 过期
//     返回 ctx.Err() 且**队列保留、记录不丢**（后台协程继续处理）；Flush
//     之后才入队的记录不保证；
//   - **Close(ctx)** 先停收（并唤醒所有阻塞中的写入方——它们计丢、不 panic），
//     再等排空；ctx 过期则**立即返回** ctx.Err()：队列剩余计入 Dropped，
//     worker 在下一轮循环退出（若它正卡在 inner.Write 内，Go 无法强杀协程，
//     只能等 inner 返回——Close 不把它转嫁成调用方的无限等待）；Close
//     **幂等**（重复调用返回首次结果）；
//   - **inner.Write panic** 被 recover 并计入 Dropped，worker 继续消费：它是
//     唯一消费者，未捕获 panic 会让它静默停摆（比崩溃更隐蔽），且 panic 栈
//     与触发请求无关；需要 fail-fast 的场景自行包 inner Sink；
//   - **Close 之后的 Write** 只计 Dropped，不 panic；
//   - **Dropped() 是合一计数**：满丢弃 / Close 后写入 / Close 唤醒的阻塞写入 /
//     Close 超时残留 / panic 跳过，全部计入；
//   - **深拷 Attrs**：Attrs 是 struct 包 map，值拷贝共享内部 map——入队前重建
//     map（正是 Sink 契约对异步导出器的要求，另见 record.go 的引用语义）。
//
// 生命周期归创建者：进程/宿主关闭路径必须显式 Flush 或 Close，否则队列内
// 记录随进程消失——异步出口改变了「树销毁后 Sink 零残留」的达成方式（先
// Close/Flush 再销毁）。
type AsyncSink struct {
	inner    Sink
	capacity int
	dropFull bool

	mu        sync.Mutex
	ring      []Record // 预分配环形队列（容量固定，无 append 增长）
	head      int
	count     int
	busy      bool // worker 正在写一条（该条已不在队列里）
	closed    bool
	forceDrop bool // Close 超时：worker 丢弃剩余并退出
	work      *sync.Cond
	space     *sync.Cond
	drain     *sync.Cond

	closeOnce   sync.Once
	closeResult error
	workerDone  chan struct{}

	dropped atomic.Uint64
}

// NewAsyncSink 把 inner 包装成异步出口。inner 为 nil 或容量非法一律 panic
// （编程错误，构造期暴露）。队列按容量**预分配**（约 capacity × sizeof(Record)
// 的常驻内存，例如 1024 ≈ 200KB；请按真实突发量选容量）。
func NewAsyncSink(inner Sink, opts ...AsyncOption) *AsyncSink {
	if inner == nil {
		panic("observability: AsyncSink requires a non-nil inner Sink")
	}
	o := asyncOpts{capacity: defaultAsyncCapacity}
	for _, opt := range opts {
		opt(&o)
	}
	if o.capacity <= 0 {
		panic("observability: AsyncSink capacity must be > 0")
	}
	s := &AsyncSink{
		inner:      inner,
		capacity:   o.capacity,
		dropFull:   o.dropFull,
		ring:       make([]Record, o.capacity),
		workerDone: make(chan struct{}),
	}
	s.work = sync.NewCond(&s.mu)
	s.space = sync.NewCond(&s.mu)
	s.drain = sync.NewCond(&s.mu)
	go s.worker()
	return s
}

// Write 实现 Sink：深拷 Attrs + 入队。队列满时按策略阻塞或丢新；
// 已关闭（或阻塞中被 Close 唤醒）时只计 Dropped。
//
// 深拷在**锁外**做：map 分配（可能触发 GC assist）不能拉长临界区——
// worker 每条要拿两次锁，锁内分配会把它和生产者的交接成本放大数倍。
func (s *AsyncSink) Write(r Record) {
	rec := r
	rec.Attrs = r.Attrs.clone() // 锁外：分配与拷贝不占锁

	s.mu.Lock()
	for !s.closed && s.count >= s.capacity && !s.dropFull {
		s.space.Wait()
	}
	switch {
	case s.closed:
		s.mu.Unlock()
		s.dropped.Add(1)
		return
	case s.count >= s.capacity: // DropOnFull
		s.mu.Unlock()
		s.dropped.Add(1)
		return
	}
	idx := (s.head + s.count) % s.capacity
	s.ring[idx] = rec // 一次结构拷贝（Attrs 已指向私有 map）
	s.count++
	if s.count == 1 {
		// 只在「从空到非空」时唤醒 worker：worker 在忙时不叫醒它，
		// 避免每条入队一次 futex/syscall 级唤醒（Windows 上 ~µs）。
		s.work.Signal()
	}
	s.mu.Unlock()
}

// Flush 等「调用时刻已入队（含在途）」的记录全部送达。ctx 过期返回
// ctx.Err()——队列保留、记录不丢（后台协程继续处理）。
func (s *AsyncSink) Flush(ctx context.Context) error {
	return s.waitDrained(ctx)
}

// Close 停收 + 排空 + 停协程（幂等）。ctx 过期 → 立即返回 ctx.Err()（队列
// 剩余计入 Dropped，worker 下一轮退出；卡在 inner.Write 内时不强等）；
// 重复调用返回首次结果。
func (s *AsyncSink) Close(ctx context.Context) error {
	var err error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.work.Broadcast()  // worker：已关闭且队列空 → 退出
		s.space.Broadcast() // 阻塞中的写入方：closed → 计丢（不 panic）
		s.mu.Unlock()

		if derr := s.waitDrained(ctx); derr != nil {
			// 超时：不再等待。置 forceDrop——worker 下一轮循环把队列剩余计入
			// Dropped 后退出；若它正卡在 inner.Write 内，Go 无法强杀协程，只能
			// 等 inner 自己返回（这是 inner 的问题，Close 不把它转嫁成调用方
			// 的无限等待）。因此本路径下 Close 返回时 worker 可能仍在收尾。
			s.mu.Lock()
			s.forceDrop = true
			s.work.Broadcast()
			s.drain.Broadcast()
			s.mu.Unlock()
			err = derr
		} else {
			// 已排空：worker 观察到 closed+空队列后自行退出。
			<-s.workerDone
		}

		s.mu.Lock()
		s.closeResult = err
		s.mu.Unlock()
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeResult
}

// Dropped 返回未送达记录数（合一计数，口径见 AsyncSink 类型注释）。
func (s *AsyncSink) Dropped() uint64 { return s.dropped.Load() }

// waitDrained 等到「队列空且无在途写入」，或 ctx 过期。
//
// 等待走独立协程 + drain 条件变量（worker 每写完一条 Broadcast）：ctx 过期
// 时调用方立即返回，等待协程在下一次 Broadcast 时醒来离场——它不持有任何
// 资源；若此后 worker 永不推进（inner 挂死），该协程存活至进程结束，与
// worker 本身同样处于「inside 不可控」的处境。
func (s *AsyncSink) waitDrained(ctx context.Context) error {
	s.mu.Lock()
	if s.count == 0 && !s.busy {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	done := make(chan struct{})
	go func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for s.count > 0 || s.busy {
			s.drain.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// worker 是唯一的消费者：等活 → 取一条 → 写 → 广播（便于 Flush 判定空）。
func (s *AsyncSink) worker() {
	defer close(s.workerDone)
	for {
		s.mu.Lock()
		for s.count == 0 && !s.closed && !s.forceDrop {
			s.work.Wait()
		}
		if s.forceDrop {
			s.dropped.Add(uint64(s.count))
			s.count, s.head = 0, 0
			s.mu.Unlock()
			return
		}
		if s.count == 0 && s.closed {
			s.mu.Unlock()
			return
		}
		rec := s.ring[s.head]
		s.ring[s.head] = Record{} // 释放 Attrs map/Err 引用，避免常驻
		s.head = (s.head + 1) % s.capacity
		s.count--
		s.busy = true
		s.space.Signal() // 腾出一个空位，唤醒一个阻塞写入方
		s.mu.Unlock()

		s.writeOne(rec)

		s.mu.Lock()
		s.busy = false
		s.drain.Broadcast()
		s.mu.Unlock()
	}
}

// writeOne 调 inner.Write；panic 被 recover 并计入 Dropped，worker 继续——
// 见 AsyncSink 类型注释里的策略说明。
func (s *AsyncSink) writeOne(r Record) {
	defer func() {
		if recover() != nil {
			s.dropped.Add(1)
		}
	}()
	s.inner.Write(r)
}

// clone 深拷 Attrs（重建内部 map）。零值/空返回零值，不分配。
func (a Attrs) clone() Attrs {
	if len(a.m) == 0 {
		return Attrs{}
	}
	m := make(map[string]attrScalar, len(a.m))
	for k, v := range a.m {
		m[k] = v
	}
	return Attrs{m: m}
}
