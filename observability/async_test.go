package observability

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 本文件是 #172 的验收面：AsyncSink 的契约（FIFO/完整性、Flush 与 Close
// 的 ctx 语义、满时策略、Close 唤醒阻塞写入、Attrs 深拷、panic 策略）。

// gateSink 是阻塞式出口：每条 Write 先报到（entered 非阻塞投递一次），
// 再等 release 放行，然后记录。
type gateSink struct {
	entered chan struct{}
	release chan struct{}

	mu      sync.Mutex
	records []Record
}

func (g *gateSink) Write(r Record) {
	if g.entered != nil {
		select {
		case g.entered <- struct{}{}:
		default:
		}
	}
	<-g.release
	g.mu.Lock()
	g.records = append(g.records, r)
	g.mu.Unlock()
}

func (g *gateSink) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.records)
}

// panicSink 对指定 Event panic，其余正常记录。
type panicSink struct {
	panicOn string

	mu      sync.Mutex
	records []Record
}

func (p *panicSink) Write(r Record) {
	if r.Event == p.panicOn {
		panic("panicSink: boom")
	}
	p.mu.Lock()
	p.records = append(p.records, r)
	p.mu.Unlock()
}

// TestAsyncSinkIntegrityAndOrder 完整性 + 零增量 + FIFO：200 条全部送达、
// 顺序不变、无多余记录、零丢弃。
func TestAsyncSinkIntegrityAndOrder(t *testing.T) {
	inner := &MemorySink{}
	as := NewAsyncSink(inner, WithCapacity(64))
	for i := 0; i < 200; i++ {
		as.Write(Record{Event: fmt.Sprintf("ev-%03d", i)})
	}
	if err := as.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := inner.Snapshot()
	if len(got) != 200 {
		t.Fatalf("delivered %d, want 200（零增量 + 全送达）", len(got))
	}
	for i, r := range got {
		if want := fmt.Sprintf("ev-%03d", i); r.Event != want {
			t.Fatalf("FIFO broken at %d: got %q, want %q", i, r.Event, want)
		}
	}
	if d := as.Dropped(); d != 0 {
		t.Fatalf("dropped = %d, want 0", d)
	}
	if err := as.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := as.Close(context.Background()); err != nil {
		t.Fatalf("second Close = %v, want nil（幂等）", err)
	}
}

// TestAsyncSinkWriteAfterCloseCountsDropped Close 之后写入只计丢，不 panic。
func TestAsyncSinkWriteAfterCloseCountsDropped(t *testing.T) {
	as := NewAsyncSink(&MemorySink{})
	if err := as.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	as.Write(Record{Event: "late"})
	if got := as.Dropped(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
}

// TestAsyncSinkFlushCtxExpiryKeepsRecords Flush 的 ctx 过期：返回错误但
// 队列保留——inner 放行后记录仍全部送达。
func TestAsyncSinkFlushCtxExpiryKeepsRecords(t *testing.T) {
	gate := &gateSink{entered: make(chan struct{}, 1), release: make(chan struct{})}
	as := NewAsyncSink(gate, WithCapacity(8))
	released := false
	release := func() {
		if !released {
			released = true
			close(gate.release) // 一次性放行：此后所有读直接通过
		}
	}
	defer func() {
		release()
		_ = as.Close(context.Background())
	}()

	as.Write(Record{Event: "a"})
	as.Write(Record{Event: "b"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := as.Flush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Flush err = %v, want DeadlineExceeded（inner 阻塞中）", err)
	}

	release() // 放行 inner
	if err := as.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := gate.count(); got != 2 {
		t.Fatalf("delivered %d, want 2（Flush 超时不丢记录）", got)
	}
	if d := as.Dropped(); d != 0 {
		t.Fatalf("dropped = %d, want 0", d)
	}
}

// TestAsyncSinkCloseWakesBlockedWriter Close 唤醒阻塞在满队列上的写入方：
// 不 panic、按丢弃计数；Close 超时返回 ctx 错误且队列剩余计丢。
func TestAsyncSinkCloseWakesBlockedWriter(t *testing.T) {
	gate := &gateSink{entered: make(chan struct{}, 2), release: make(chan struct{})}
	as := NewAsyncSink(gate, WithCapacity(1))

	as.Write(Record{Event: "a"})
	<-gate.entered               // worker 已取走 a（队列空、busy）
	as.Write(Record{Event: "b"}) // 入队（容量 1，满）
	blocked := make(chan struct{})
	go func() {
		as.Write(Record{Event: "c"}) // 队列满 → 阻塞等待空位
		close(blocked)
	}()
	time.Sleep(50 * time.Millisecond) // 让写入方进入等待（非正确性依赖：见下）

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := as.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close err = %v, want DeadlineExceeded（inner 挂住）", err)
	}

	select {
	case <-blocked: // Close 必须唤醒它（无论此刻它是否已在 Wait 上）
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Write was not woken by Close")
	}

	close(gate.release) // 放行 inner，让 worker 收尾（下一轮把队列剩余计丢并退出）
	waitDropped(t, as, 2)
	if got := gate.count(); got != 1 {
		t.Fatalf("delivered %d, want 1（在被 Close 打断前的 a）", got)
	}
}

// waitDropped 轮询等待丢弃计数到达 want（worker 收尾是异步的）。
func waitDropped(t *testing.T, as *AsyncSink, want uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if as.Dropped() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("dropped = %d, want >= %d", as.Dropped(), want)
}

// TestAsyncSinkDropOnFull DropOnFull：满队列时丢新、计数、不阻塞；
// 放行后已入队的记录仍按序送达。
func TestAsyncSinkDropOnFull(t *testing.T) {
	gate := &gateSink{entered: make(chan struct{}, 2), release: make(chan struct{})}
	as := NewAsyncSink(gate, WithCapacity(1), DropOnFull())

	as.Write(Record{Event: "a"})
	<-gate.entered               // worker 取走 a
	as.Write(Record{Event: "b"}) // 入队
	as.Write(Record{Event: "c"}) // 满 → 丢新
	as.Write(Record{Event: "d"}) // 满 → 丢新

	if got := as.Dropped(); got != 2 {
		t.Fatalf("dropped = %d, want 2（c/d 被丢）", got)
	}

	close(gate.release)
	if err := as.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := gate.count(); got != 2 {
		t.Fatalf("delivered %d, want 2（a/b）", got)
	}
}

// TestAsyncSinkAttrsDeepCopy 入队即深拷：调用方随后改写自己的 Attrs 不影响
// 已入队记录（Sink 契约对异步导出器的要求）。
func TestAsyncSinkAttrsDeepCopy(t *testing.T) {
	inner := &MemorySink{}
	as := NewAsyncSink(inner, WithCapacity(4))
	defer as.Close(context.Background())

	rec := Record{Event: "evt"}
	Set(&rec.Attrs, "k.keep", "before")
	as.Write(rec)

	// 调用方（产出方）在 Write 之后改写自己的 Attrs 与 map 结构。
	Set(&rec.Attrs, "k.keep", "after")
	Set(&rec.Attrs, "k.late", "added")

	if err := as.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := inner.Snapshot()
	if len(got) != 1 {
		t.Fatalf("delivered %d, want 1", len(got))
	}
	if v, _ := Get[string](got[0].Attrs, "k.keep"); v != "before" {
		t.Fatalf("k.keep = %q, want before（深拷）", v)
	}
	if _, ok := Get[string](got[0].Attrs, "k.late"); ok {
		t.Fatal("已入队记录不得看到调用方后续写入的 k.late")
	}
}

// TestAsyncSinkInnerPanicRecovered inner panic：被 recover、计入丢弃、worker
// 继续消费后续记录（不静默停摆）。
func TestAsyncSinkInnerPanicRecovered(t *testing.T) {
	inner := &panicSink{panicOn: "boom"}
	as := NewAsyncSink(inner, WithCapacity(4))

	as.Write(Record{Event: "ok-1"})
	as.Write(Record{Event: "boom"})
	as.Write(Record{Event: "ok-2"})
	if err := as.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	inner.mu.Lock()
	n := len(inner.records)
	inner.mu.Unlock()
	if n != 2 {
		t.Fatalf("delivered %d, want 2（panic 那条不计入送达）", n)
	}
	if got := as.Dropped(); got != 1 {
		t.Fatalf("dropped = %d, want 1（panic 跳过）", got)
	}
}

// TestAsyncSinkConstructorValidation 构造期校验：nil inner / 非法容量 panic。
func TestAsyncSinkConstructorValidation(t *testing.T) {
	assertPanics := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s: want panic", name)
			}
		}()
		fn()
	}
	assertPanics("nil inner", func() { NewAsyncSink(nil) })
	assertPanics("zero capacity", func() { NewAsyncSink(&MemorySink{}, WithCapacity(0)) })
	assertPanics("negative capacity", func() { NewAsyncSink(&MemorySink{}, WithCapacity(-1)) })
}
