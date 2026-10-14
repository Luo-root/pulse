package observe

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

// asyncFlush 是交给唯一 worker 的刷新边界，按 target 入队，同目标共享。
// 不同目标不合并：后来的 Write/Flush 不能延长先前调用的快照。worker 在
// 继续消费前已能看到下一边界，交接不依赖等待者重新取得锁。
// 最多 capacity+1 个不同边界（有界记录队列 + 当前在途写入/刷新的边界）；
// 不为调用方另开后台协程。除 err 在 done 关闭后可直接读取外，字段由 mu 保护。
type asyncFlush struct {
	target  uint64
	started bool
	waiters int
	done    chan struct{}
	err     error
	next    *asyncFlush
}

// AsyncSink 把任意 Sink 包成**异步出口**：Write 只做 Attrs 深拷 + 入队即
// 返回，单后台协程按 FIFO 调 inner.Write。用途是把手慢的出口（文件/网络
// 导出器）从调用方 goroutine 上摘掉——引擎的 Observer 回调在节点 goroutine
// 上同步执行，Sink 有多慢，节点就有多慢。
//
// 语义（与 #172 的规格一致）：
//   - **有界队列**，缺省容量 1024；满时缺省**阻塞**（回压），DropOnFull()
//     改为丢新（计入 Dropped）；
//   - **Flush(ctx)** 等「调用时刻已入队（含在途）」的记录全部送达；ctx 过期
//     返回 ctx.Err() 且**队列保留、记录不丢**（后台协程继续处理）；Flush
//     之后才入队的记录不保证；
//   - **Close(ctx)** 先停收（并唤醒所有阻塞中的写入方——它们计丢、不 panic），
//     再等排空；首次调用的 ctx 过期则**立即返回** ctx.Err()：队列剩余计入
//     Dropped，worker 在下一轮循环退出（若它正卡在 inner.Write/Flush 内，
//     Go 无法强杀协程，只能等 inner 返回）；Close **幂等**（首次结果确定后，
//     重复调用返回该结果；尚未确定时，并发调用可用自己的 ctx 结束等待）；
//   - **inner.Write panic** 被 recover 并计入 Dropped，worker 继续消费：它是
//     唯一消费者，未捕获 panic 会让它静默停摆（比崩溃更隐蔽），且 panic 栈
//     与触发请求无关；需要 fail-fast 的场景自行包 inner Sink；
//   - **inner.Flush/Err panic** 被 recover 并作为错误返回等待者；错误文本不含
//     panic payload，error 类型的 payload 可由 Unwrap 取得。已交给 inner.Write
//     的记录不计 Dropped，worker 仍可处理后续记录；
//   - **透传内层出口的 Flush/Err**：内层实现了可选 `Flush() error` /
//     `Err() error` 时，Flush 与 Close 在**排空之后**调用并返回内层结果。
//     内层 Write/Flush/Err 全部由同一 worker 串行执行；ctx 也覆盖等待内层
//     刷新的阶段，已经开始的无 ctx 刷新会在后台继续，并发等待者共享该任务。
//     自带缓冲的出口（LineSink 的 32 KiB 写缓冲）真正落盘发生在它自己的
//     Flush 里——不透传的话「AsyncSink 排空」不等于「记录落地」；
//   - **Close 之后的 Write** 只计 Dropped，不 panic；
//   - **Dropped() 是合一计数**：满丢弃 / Close 后写入 / Close 唤醒的阻塞写入 /
//     Close 超时残留 / panic 跳过，全部计入；
//   - **深拷 Attrs**：Attrs 是结构体包条目切片，值拷贝共享底层数组——入队前
//     复制一份（正是 Sink 契约对异步导出器的要求，另见 record.go 的引用语义）。
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
	enqueued  uint64      // 已入队总数（不含满丢弃/关闭后写入）
	processed uint64      // worker 已完成的 Write 总数（含 recover 的 panic）
	flush     *asyncFlush // 按快照边界递增排列；worker 只执行队首
	flushTail *asyncFlush
	closed    bool
	forceDrop bool // 首次 Close 超时：worker 丢弃剩余并退出
	work      *sync.Cond
	space     *sync.Cond

	closeCtx      context.Context // 首次 Close 控制排空与终态
	closeFinished bool
	closeResult   error
	closeDone     chan struct{}
	workerDone    chan struct{}

	dropped atomic.Uint64
}

// NewAsyncSink 把 inner 包装成异步出口。inner 为 nil 或容量非法一律 panic
// （编程错误，构造期暴露）。队列按容量**预分配**（约 capacity × sizeof(Record)
// 的常驻内存，例如 1024 ≈ 200KB；请按真实突发量选容量）。
func NewAsyncSink(inner Sink, opts ...AsyncOption) *AsyncSink {
	if inner == nil {
		panic("observe: AsyncSink requires a non-nil inner Sink")
	}
	o := asyncOpts{capacity: defaultAsyncCapacity}
	for _, opt := range opts {
		opt(&o)
	}
	if o.capacity <= 0 {
		panic("observe: AsyncSink capacity must be > 0")
	}
	s := &AsyncSink{
		inner:      inner,
		capacity:   o.capacity,
		dropFull:   o.dropFull,
		ring:       make([]Record, o.capacity),
		closeDone:  make(chan struct{}),
		workerDone: make(chan struct{}),
	}
	s.work = sync.NewCond(&s.mu)
	s.space = sync.NewCond(&s.mu)
	go s.worker()
	return s
}

// Write 实现 Sink：深拷 Attrs + 入队。队列满时按策略阻塞或丢新；
// 已关闭（或阻塞中被 Close 唤醒）时只计 Dropped。
//
// 深拷在**锁外**做：切片分配（可能触发 GC assist）不能拉长临界区——
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
	s.ring[idx] = rec // 一次结构拷贝（Attrs 已指向私有条目切片）
	s.count++
	s.enqueued++
	if s.count == 1 {
		// 只在「从空到非空」时唤醒 worker：worker 在忙时不叫醒它，
		// 避免每条入队一次 futex/syscall 级唤醒（Windows 上 ~µs）。
		s.work.Signal()
	}
	s.mu.Unlock()
}

// Flush 等「调用时刻已入队（含在途）」的记录全部送达，再由 worker 透传
// 内层出口的 Flush/Err（见 flushInner）——送达的口径是「已过内层自己的缓冲」。
// ctx 过期返回 ctx.Err()，队列保留、记录不丢；已经开始的内层刷新继续收尾，
// 尚未开始且无人再等待的刷新撤销。Close 停收后，Flush 等待同一关闭结果。
func (s *AsyncSink) Flush(ctx context.Context) error {
	s.mu.Lock()
	target := s.enqueued
	if s.closed {
		s.mu.Unlock()
		return s.waitClosed(ctx, false)
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	op := s.flushTail
	if op == nil || op.target != target {
		op = &asyncFlush{target: target, done: make(chan struct{})}
		if s.flushTail == nil {
			s.flush = op
		} else {
			s.flushTail.next = op
		}
		s.flushTail = op
		s.work.Signal()
	}
	op.waiters++
	s.mu.Unlock()
	return s.waitFlush(ctx, op)
}

func (s *AsyncSink) waitFlush(ctx context.Context, op *asyncFlush) error {
	defer func() {
		s.mu.Lock()
		op.waiters--
		if !op.started && op.waiters == 0 {
			s.finishFlushLocked(op, nil)
		}
		s.mu.Unlock()
	}()
	select {
	case <-op.done:
		if err := ctx.Err(); err != nil {
			return err
		}
		return op.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// finishFlushLocked 完成/撤销一个边界；取消可发生在队列中间或末尾。
// 已完成的边界不再修改（其 err 可由已醒来的等待者直接读取）。
func (s *AsyncSink) finishFlushLocked(op *asyncFlush, err error) {
	var prev *asyncFlush
	for cur := s.flush; cur != nil; cur = cur.next {
		if cur != op {
			prev = cur
			continue
		}
		if prev == nil {
			s.flush = op.next
		} else {
			prev.next = op.next
		}
		if s.flushTail == op {
			s.flushTail = prev
		}
		op.next = nil
		op.err = err
		close(op.done)
		return
	}
}

// asyncFlushPanicError 不格式化 payload；即使 cause.Error 自身 panic，报告
// 此错误也安全。只保留 error cause 供调用方显式检查，不增加公开哨兵。
type asyncFlushPanicError struct{ cause error }

func (*asyncFlushPanicError) Error() string   { return "observe: AsyncSink inner Flush/Err panicked" }
func (e *asyncFlushPanicError) Unwrap() error { return e.cause }

// flushInner 透传内层出口的可选 Flush/Err，并在 worker 的调用边界捕获 panic。
//
// Sink 接口只有 Write：只调 Write 时「排空」只保证记录交给了 inner，不保证
// 它落了盘——自带缓冲的出口（LineSink 的 32 KiB）要等自己的 Flush。两个方法
// 都按需探（裸出口不实现它们，属正常）。
//
// Flush 优先且不叠加 Err：按 bufio.Writer 的约定，Flush 的实现本来就带上
// sticky error（LineSink 就是 `flushLocked` 后返回 `s.err`），再叠一次只会让
// 同一条错误在文案里出现两遍。只有没实现 Flush 的出口才去看 Err。
func flushInner(inner Sink) (err error) {
	returned := false
	defer func() {
		if !returned {
			// 用返回标记判别，兼容 panic(nil) 的旧 GODEBUG 行为。
			cause, _ := recover().(error)
			err = &asyncFlushPanicError{cause: cause}
		}
	}()
	if f, ok := inner.(interface{ Flush() error }); ok {
		err = f.Flush()
	} else if e, ok := inner.(interface{ Err() error }); ok {
		err = e.Err()
	}
	returned = true
	return err
}

// Close 停收 + 排空 + 刷新 + 停协程（幂等）。首次调用的 ctx 过期 → 立即
// 返回 ctx.Err()（队列剩余计入 Dropped，worker 下一轮退出；卡在内层
// Write/Flush/Err 时不强等）。首次结果确定后，重复调用返回该结果；尚未确定
// 时，并发调用可用自己的 ctx 结束等待，但不改变首次调用的排空与最终结果。
func (s *AsyncSink) Close(ctx context.Context) error {
	s.mu.Lock()
	first := !s.closed
	if first {
		s.closed = true
		s.closeCtx = ctx
		if err := ctx.Err(); err != nil {
			s.finishCloseLocked(err)
		}
		s.work.Broadcast()
		s.space.Broadcast() // 阻塞中的写入方：closed → 计丢（不 panic）
	}
	s.mu.Unlock()
	return s.waitClosed(ctx, first)
}

func (s *AsyncSink) waitClosed(ctx context.Context, first bool) error {
	s.mu.Lock()
	if s.closeFinished {
		err := s.closeResult
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	select {
	case <-s.closeDone:
		return s.closeResult
	case <-ctx.Done():
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.closeFinished && first {
			s.finishCloseLocked(ctx.Err())
		}
		if s.closeFinished {
			return s.closeResult
		}
		return ctx.Err()
	}
}

func (s *AsyncSink) finishCloseLocked(err error) {
	if s.closeFinished {
		return
	}
	if cerr := s.closeCtx.Err(); cerr != nil {
		err = cerr
		s.forceDrop = true
		s.work.Broadcast()
	}
	s.closeResult = err
	s.closeFinished = true
	close(s.closeDone)
}

// Dropped 返回未送达记录数（合一计数，口径见 AsyncSink 类型注释）。
func (s *AsyncSink) Dropped() uint64 { return s.dropped.Load() }

// worker 是 inner 的唯一调用方：按 FIFO 写入，在刷新快照边界暂停消费。
// 内层无 ctx 的调用只能等其返回，但取消等待不需要另开后台协程。
func (s *AsyncSink) worker() {
	var closeErr error
	defer func() {
		close(s.workerDone)
		s.mu.Lock()
		if s.closed {
			s.finishCloseLocked(closeErr)
		}
		s.mu.Unlock()
	}()
	for {
		s.mu.Lock()
		for s.count == 0 && s.flush == nil && !s.closed {
			s.work.Wait()
		}
		if s.forceDrop {
			s.dropped.Add(uint64(s.count))
			s.count, s.head = 0, 0
			for s.flush != nil {
				s.finishFlushLocked(s.flush, s.closeResult)
			}
			s.mu.Unlock()
			return
		}
		if op := s.flush; op != nil && s.processed >= op.target {
			op.started = true
			s.mu.Unlock()
			err := flushInner(s.inner)
			s.mu.Lock()
			s.finishFlushLocked(op, err)
			if s.closed && s.count == 0 {
				// 本次刷新已涵盖关闭时的全部记录，共用结果，不重复刷新。
				for s.flush != nil {
					s.finishFlushLocked(s.flush, err)
				}
				closeErr = err
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()
			continue
		}
		if s.count == 0 && s.closed {
			s.mu.Unlock()
			closeErr = flushInner(s.inner)
			return
		}
		rec := s.ring[s.head]
		s.ring[s.head] = Record{} // 释放 Attrs 条目/Err 引用，避免常驻
		s.head = (s.head + 1) % s.capacity
		s.count--
		s.space.Signal() // 腾出一个空位，唤醒一个阻塞写入方
		s.mu.Unlock()

		s.writeOne(rec)

		s.mu.Lock()
		s.processed++
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

// clone 深拷 Attrs（复制条目切片——值拷贝共享底层数组，异步持有必须
// 独立一份）。零值/空返回零值，不分配。
func (a Attrs) clone() Attrs {
	if len(a.entries) == 0 {
		return Attrs{}
	}
	entries := make([]attrEntry, len(a.entries))
	copy(entries, a.entries)
	return Attrs{entries: entries}
}
