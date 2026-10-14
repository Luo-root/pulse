package observe

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"
)

// #341：屏障确认已进入无 ctx 的内层刷新后才取消，覆盖及时返回、串行调用、
// Flush 的快照边界与 Close 首次终态。所有屏障在 cleanup 中释放，失败也不挂住 worker。
type asyncTestBarrier struct {
	done chan struct{}
	once sync.Once
}

func newAsyncTestBarrier() *asyncTestBarrier {
	return &asyncTestBarrier{done: make(chan struct{})}
}

func (b *asyncTestBarrier) release() { b.once.Do(func() { close(b.done) }) }

type asyncFlushBarrierSink struct {
	flushGate    *asyncTestBarrier
	writeGates   map[string]*asyncTestBarrier
	flushEntered chan int
	writeEntered chan string
	flushErr     error

	mu         sync.Mutex
	active     int
	overlap    bool
	events     []string
	flushCalls int
	flushed    [][]string
}

func newAsyncFlushBarrierSink() *asyncFlushBarrierSink {
	return &asyncFlushBarrierSink{
		flushGate:    newAsyncTestBarrier(),
		writeGates:   make(map[string]*asyncTestBarrier),
		flushEntered: make(chan int, 128),
		writeEntered: make(chan string, 128),
	}
}

func (s *asyncFlushBarrierSink) enterLocked() {
	s.active++
	if s.active > 1 {
		s.overlap = true
	}
}

func (s *asyncFlushBarrierSink) Write(r Record) {
	s.mu.Lock()
	s.enterLocked()
	s.mu.Unlock()
	s.writeEntered <- r.Event
	if gate := s.writeGates[r.Event]; gate != nil {
		<-gate.done
	}
	s.mu.Lock()
	s.events = append(s.events, r.Event)
	s.active--
	s.mu.Unlock()
}

func (s *asyncFlushBarrierSink) Flush() error {
	s.mu.Lock()
	s.enterLocked()
	s.flushCalls++
	call := s.flushCalls
	s.flushed = append(s.flushed, append([]string(nil), s.events...))
	s.mu.Unlock()
	s.flushEntered <- call
	<-s.flushGate.done
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return s.flushErr
}

func (s *asyncFlushBarrierSink) snapshot() (events []string, calls int, overlap bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...), s.flushCalls, s.overlap
}

// 没实现 Flush 的出口走 Err，等待边界也必须涵盖这个可选方法。
type asyncErrBarrierSink struct{ inner *asyncFlushBarrierSink }

func (s asyncErrBarrierSink) Write(r Record) { s.inner.Write(r) }
func (s asyncErrBarrierSink) Err() error     { return s.inner.Flush() }

func newAsyncFlushTestSink(t *testing.T, inner Sink, gate *asyncFlushBarrierSink, opts ...AsyncOption) *AsyncSink {
	t.Helper()
	as := NewAsyncSink(inner, opts...)
	t.Cleanup(func() {
		gate.flushGate.release()
		for _, b := range gate.writeGates {
			b.release()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = as.Close(ctx) // 被取消的首次 Close 仍返回其首次错误。
		waitAsyncTestValue(t, as.workerDone)
	})
	return as
}

func runAsyncTestCall(call func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- call() }()
	return done
}

func waitAsyncTestValue[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("async operation did not reach its barrier/result")
		var zero T
		return zero
	}
}

// Err 报到发生在本次 Flush 已取得快照后。用于保证测试中的后续 Write/Close
// 确实晚于快照；不靠 Sleep 猜测调用方的调度顺序。
type asyncCheckedContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (c *asyncCheckedContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
}

func runCheckedAsyncFlush(t *testing.T, as *AsyncSink, ctx context.Context) <-chan error {
	t.Helper()
	checked := &asyncCheckedContext{Context: ctx, checked: make(chan struct{})}
	done := runAsyncTestCall(func() error { return as.Flush(checked) })
	waitAsyncTestValue(t, checked.checked)
	return done
}

// Done 报到确认 Flush 已进入等待分支。只用公开入口构造边界交接窗口，
// 避免靠等待者再次获得执行机会来完成 worker 的下一次刷新。
type asyncWaitingContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *asyncWaitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func runWaitingAsyncFlush(t *testing.T, as *AsyncSink, ctx context.Context) <-chan error {
	t.Helper()
	waiting := &asyncWaitingContext{Context: ctx, waiting: make(chan struct{})}
	done := runAsyncTestCall(func() error { return as.Flush(waiting) })
	waitAsyncTestValue(t, waiting.waiting)
	return done
}

func waitAsyncTestClosed(t *testing.T, as *AsyncSink) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		as.mu.Lock()
		closed := as.closed
		as.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Close did not stop accepting writes")
}

func TestAsyncSinkInnerFlushCancellation(t *testing.T) {
	for _, optional := range []string{"Flush", "Err"} {
		for _, entry := range []string{"Flush", "Close"} {
			t.Run(optional+"/"+entry, func(t *testing.T) {
				gate := newAsyncFlushBarrierSink()
				var inner Sink = gate
				if optional == "Err" {
					inner = asyncErrBarrierSink{inner: gate}
				}
				as := newAsyncFlushTestSink(t, inner, gate)
				as.Write(Record{Event: "a"})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				call := as.Flush
				if entry == "Close" {
					call = as.Close
				}
				done := runAsyncTestCall(func() error { return call(ctx) })
				waitAsyncTestValue(t, gate.flushEntered)
				cancel() // 必须在 inner 已进入后取消。

				var err error
				select {
				case err = <-done:
				case <-time.After(time.Second):
					t.Error("cancellation did not return while inner flush remained blocked")
					gate.flushGate.release()
					err = waitAsyncTestValue(t, done)
				}
				if !errors.Is(err, context.Canceled) {
					t.Errorf("%s = %v, want context.Canceled", entry, err)
				}
				if entry == "Close" {
					err = waitAsyncTestValue(t, runAsyncTestCall(func() error { return as.Close(context.Background()) }))
					if !errors.Is(err, context.Canceled) {
						t.Errorf("repeated Close = %v, want first cancellation", err)
					}
				} else {
					as.Write(Record{Event: "b"})
					as.Write(Record{Event: "c"})
				}
				gate.flushGate.release()
				if entry == "Flush" {
					if err := as.Close(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				waitAsyncTestValue(t, as.workerDone)
				events, _, overlap := gate.snapshot()
				want := []string{"a"}
				if entry == "Flush" {
					want = append(want, "b", "c")
				}
				if !reflect.DeepEqual(events, want) || as.Dropped() != 0 || overlap {
					t.Fatalf("events=%v dropped=%d overlap=%v; want %v, 0, false", events, as.Dropped(), overlap, want)
				}
			})
		}
	}
}

func TestAsyncSinkFlushCancellationKeepsQueuedRecords(t *testing.T) {
	gate := newAsyncFlushBarrierSink()
	gate.writeGates["a"] = newAsyncTestBarrier()
	as := newAsyncFlushTestSink(t, gate, gate, WithCapacity(4))
	as.Write(Record{Event: "a"})
	waitAsyncTestValue(t, gate.writeEntered)
	as.Write(Record{Event: "b"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runCheckedAsyncFlush(t, as, ctx)
	cancel()
	if err := waitAsyncTestValue(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Flush = %v, want context.Canceled", err)
	}
	as.Write(Record{Event: "c"})
	gate.writeGates["a"].release()
	gate.flushGate.release()
	if err := waitAsyncTestValue(t, runAsyncTestCall(func() error { return as.Close(context.Background()) })); err != nil {
		t.Errorf("Close after canceled Flush = %v, want nil", err)
	}
	waitAsyncTestValue(t, as.workerDone)
	events, calls, overlap := gate.snapshot()
	if !reflect.DeepEqual(events, []string{"a", "b", "c"}) || as.Dropped() != 0 || calls != 1 || overlap {
		t.Fatalf("events=%v dropped=%d flushes=%d overlap=%v; want all 3 records, 0, 1, false", events, as.Dropped(), calls, overlap)
	}
}

func TestAsyncSinkFlushSnapshotExcludesLaterWrites(t *testing.T) {
	gate := newAsyncFlushBarrierSink()
	gate.writeGates["early"] = newAsyncTestBarrier()
	gate.writeGates["late"] = newAsyncTestBarrier()
	as := newAsyncFlushTestSink(t, gate, gate, WithCapacity(2))
	as.Write(Record{Event: "early"})
	waitAsyncTestValue(t, gate.writeEntered)
	done := runCheckedAsyncFlush(t, as, context.Background())
	as.Write(Record{Event: "late"})
	gate.writeGates["early"].release()
	waitAsyncTestValue(t, gate.flushEntered)
	events, _, overlap := gate.snapshot()
	if !reflect.DeepEqual(events, []string{"early"}) || overlap {
		t.Fatalf("inner flush events=%v overlap=%v, want only early and no overlapping Write", events, overlap)
	}
	gate.flushGate.release()
	if got := waitAsyncTestValue(t, gate.writeEntered); got != "late" {
		t.Fatalf("next Write = %q, want late", got)
	}
	// late 已进入 Write 但未返回；前一次快照应当已经完成。
	if err := waitAsyncTestValue(t, done); err != nil {
		t.Fatal(err)
	}
	gate.writeGates["late"].release()
	if err := as.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if events, _, overlap := gate.snapshot(); !reflect.DeepEqual(events, []string{"early", "late"}) || overlap || as.Dropped() != 0 {
		t.Fatalf("events=%v dropped=%d overlap=%v", events, as.Dropped(), overlap)
	}
}

// 来自独立复审的公开入口探针：Flush1 卡在内层时，Flush2 已取得 b 的快照；
// 释放 Flush1 后，worker 必须在 b 与 late 之间刷新，不能等 Flush2 重获调度。
func TestAsyncSinkHigherFlushSnapshotExcludesLaterWrites(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	gate := newAsyncFlushBarrierSink()
	gate.writeGates["b"] = newAsyncTestBarrier()
	gate.writeGates["late"] = newAsyncTestBarrier()
	as := newAsyncFlushTestSink(t, gate, gate, WithCapacity(4))
	as.Write(Record{Event: "a"})
	first := runAsyncTestCall(func() error { return as.Flush(context.Background()) })
	waitAsyncTestValue(t, gate.flushEntered)
	waitAsyncTestValue(t, gate.writeEntered) // a 已送达。
	as.Write(Record{Event: "b"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second := runWaitingAsyncFlush(t, as, ctx)
	as.Write(Record{Event: "late"})
	gate.writeGates["b"].release()
	gate.flushGate.release()
	if err := waitAsyncTestValue(t, first); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"b", "late"} {
		if got := waitAsyncTestValue(t, gate.writeEntered); got != want {
			t.Fatalf("Write = %q, want %q", got, want)
		}
	}
	// late 已进入无 ctx 的 Write 且尚未返回，Flush2 应当已经完成。
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		events, calls, _ := gate.snapshot()
		t.Errorf("higher snapshot held by later Write: events=%v flushes=%d", events, calls)
		cancel()
		if err := waitAsyncTestValue(t, second); !errors.Is(err, context.Canceled) {
			t.Errorf("canceled higher Flush = %v, want context.Canceled", err)
		}
	}
	gate.writeGates["late"].release()
	if err := as.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if events, _, overlap := gate.snapshot(); !reflect.DeepEqual(events, []string{"a", "b", "late"}) || overlap || as.Dropped() != 0 {
		t.Fatalf("events=%v dropped=%d overlap=%v", events, as.Dropped(), overlap)
	}
}

func TestAsyncSinkPendingFlushKeepsEarlierSnapshot(t *testing.T) {
	gate := newAsyncFlushBarrierSink()
	gate.writeGates["a"] = newAsyncTestBarrier()
	gate.writeGates["b"] = newAsyncTestBarrier()
	as := newAsyncFlushTestSink(t, gate, gate, WithCapacity(2))
	as.Write(Record{Event: "a"})
	waitAsyncTestValue(t, gate.writeEntered)
	first := runWaitingAsyncFlush(t, as, context.Background())
	as.Write(Record{Event: "b"})
	second := runWaitingAsyncFlush(t, as, context.Background())
	gate.writeGates["a"].release()
	waitAsyncTestValue(t, gate.flushEntered)
	if events, _, overlap := gate.snapshot(); !reflect.DeepEqual(events, []string{"a"}) || overlap {
		t.Fatalf("earlier pending snapshot=%v overlap=%v, want only a", events, overlap)
	}
	gate.flushGate.release()
	if got := waitAsyncTestValue(t, gate.writeEntered); got != "b" {
		t.Fatalf("Write = %q, want b", got)
	}
	if err := waitAsyncTestValue(t, first); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-second:
		t.Fatalf("later snapshot completed while b was still blocked: %v", err)
	default:
	}
	gate.writeGates["b"].release()
	if err := waitAsyncTestValue(t, second); err != nil {
		t.Fatal(err)
	}
}

func TestAsyncSinkQueuedFlushCancellationRemovesMiddleAndTail(t *testing.T) {
	gate := newAsyncFlushBarrierSink()
	as := newAsyncFlushTestSink(t, gate, gate, WithCapacity(4))
	as.Write(Record{Event: "a"})
	first := runAsyncTestCall(func() error { return as.Flush(context.Background()) })
	waitAsyncTestValue(t, gate.flushEntered)
	as.Write(Record{Event: "b"})
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	atB := runWaitingAsyncFlush(t, as, ctxB)
	as.Write(Record{Event: "c"})
	atC := runWaitingAsyncFlush(t, as, context.Background())
	cancelB() // b 是队列中间的唯一等待者。
	if err := waitAsyncTestValue(t, atB); !errors.Is(err, context.Canceled) {
		t.Fatalf("Flush at b = %v, want context.Canceled", err)
	}
	as.Write(Record{Event: "d"})
	ctxD, cancelD := context.WithCancel(context.Background())
	defer cancelD()
	atD := runWaitingAsyncFlush(t, as, ctxD)
	cancelD() // d 是队尾；后续新边界仍必须能正常入队。
	if err := waitAsyncTestValue(t, atD); !errors.Is(err, context.Canceled) {
		t.Fatalf("Flush at d = %v, want context.Canceled", err)
	}
	as.Write(Record{Event: "e"})
	atE := runWaitingAsyncFlush(t, as, context.Background())
	closed := runAsyncTestCall(func() error { return as.Close(context.Background()) })
	waitAsyncTestClosed(t, as)
	gate.flushGate.release()
	for _, done := range []<-chan error{first, atC, atE, closed} {
		if err := waitAsyncTestValue(t, done); err != nil {
			t.Fatal(err)
		}
	}
	gate.mu.Lock()
	flushed := append([][]string(nil), gate.flushed...)
	gate.mu.Unlock()
	want := [][]string{{"a"}, {"a", "b", "c"}, {"a", "b", "c", "d", "e"}}
	if events, calls, overlap := gate.snapshot(); !reflect.DeepEqual(flushed, want) || calls != 3 || overlap || as.Dropped() != 0 {
		t.Fatalf("snapshots=%v events=%v flushes=%d overlap=%v dropped=%d; want %v, 3, false, 0", flushed, events, calls, overlap, as.Dropped(), want)
	}
}

func TestAsyncSinkCanceledFlushWaitersDoNotAccumulate(t *testing.T) {
	gate := newAsyncFlushBarrierSink()
	as := newAsyncFlushTestSink(t, gate, gate)
	as.Write(Record{Event: "a"})
	first := runAsyncTestCall(func() error { return as.Flush(context.Background()) })
	waitAsyncTestValue(t, gate.flushEntered)
	before := runtime.NumGoroutine()
	// 每次都确认调用已加入等待再取消；内层一直挂住，不能留下每调用一个
	// goroutine，也不能重复调用内层刷新。
	for i := 0; i < 64; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := runCheckedAsyncFlush(t, as, ctx)
		cancel()
		if err := waitAsyncTestValue(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter %d = %v, want context.Canceled", i, err)
		}
	}
	runtime.Gosched() // 让刚送出结果的调用方退出。
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("canceled waiters left goroutines: before=%d after=%d", before, after)
	}
	if events, calls, overlap := gate.snapshot(); !reflect.DeepEqual(events, []string{"a"}) || calls != 1 || overlap {
		t.Fatalf("events=%v flushes=%d overlap=%v, want a, 1, false", events, calls, overlap)
	}
	gate.flushGate.release()
	if err := waitAsyncTestValue(t, first); err != nil {
		t.Fatal(err)
	}
}

func TestAsyncSinkConcurrentFlushCloseWriteSerial(t *testing.T) {
	for _, innerErr := range []error{nil, errors.New("inner flush failed")} {
		name := "success"
		if innerErr != nil {
			name = "inner-error"
		}
		t.Run(name, func(t *testing.T) {
			gate := newAsyncFlushBarrierSink()
			gate.flushErr = innerErr
			as := newAsyncFlushTestSink(t, gate, gate, WithCapacity(1))
			as.Write(Record{Event: "a"})
			firstFlush := runAsyncTestCall(func() error { return as.Flush(context.Background()) })
			waitAsyncTestValue(t, gate.flushEntered)
			as.Write(Record{Event: "b"})
			blockedWrite := make(chan struct{})
			go func() {
				as.Write(Record{Event: "blocked"})
				close(blockedWrite)
			}()
			var flushes []<-chan error
			for i := 0; i < 8; i++ {
				flushes = append(flushes, runCheckedAsyncFlush(t, as, context.Background()))
			}
			firstClose := runAsyncTestCall(func() error { return as.Close(context.Background()) })
			waitAsyncTestClosed(t, as)
			waitAsyncTestValue(t, blockedWrite)
			as.Write(Record{Event: "after-close"})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := waitAsyncTestValue(t, runAsyncTestCall(func() error { return as.Close(ctx) })); !errors.Is(err, context.Canceled) {
				t.Fatalf("concurrent Close = %v, want its context.Canceled", err)
			}
			// 并发调用的取消不能替首次 Close 作出终态，也不能停止排空。
			select {
			case err := <-firstClose:
				t.Fatalf("first Close finished before inner was released: %v", err)
			default:
			}
			gate.flushGate.release()
			for _, done := range append(flushes, firstFlush, firstClose) {
				if err := waitAsyncTestValue(t, done); err != innerErr {
					t.Fatalf("inner result = %v, want %v", err, innerErr)
				}
			}
			if err := as.Close(ctx); err != innerErr {
				t.Fatalf("repeated Close = %v, want first result %v even with canceled ctx", err, innerErr)
			}
			select {
			case <-as.workerDone:
			default:
				t.Fatal("successful drain returned before worker stopped")
			}
			events, calls, overlap := gate.snapshot()
			if !reflect.DeepEqual(events, []string{"a", "b"}) || calls != 2 || overlap || as.Dropped() != 2 {
				t.Fatalf("events=%v flushes=%d overlap=%v dropped=%d; want a/b, 2, false, 2", events, calls, overlap, as.Dropped())
			}
		})
	}
}

func TestAsyncSinkCloseSharesDrainedFlush(t *testing.T) {
	gate := newAsyncFlushBarrierSink()
	innerErr := errors.New("shared inner flush failed")
	gate.flushErr = innerErr
	as := newAsyncFlushTestSink(t, gate, gate)
	as.Write(Record{Event: "a"})
	flushed := runAsyncTestCall(func() error { return as.Flush(context.Background()) })
	waitAsyncTestValue(t, gate.flushEntered)
	closed := runAsyncTestCall(func() error { return as.Close(context.Background()) })
	waitAsyncTestClosed(t, as)
	joined := runAsyncTestCall(func() error { return as.Flush(context.Background()) })
	gate.flushGate.release()
	for _, done := range []<-chan error{flushed, closed, joined} {
		if err := waitAsyncTestValue(t, done); err != innerErr {
			t.Fatalf("shared result = %v, want %v", err, innerErr)
		}
	}
	if err := as.Close(context.Background()); err != innerErr {
		t.Fatalf("repeated Close = %v, want first inner error", err)
	}
	events, calls, overlap := gate.snapshot()
	if !reflect.DeepEqual(events, []string{"a"}) || calls != 1 || overlap || as.Dropped() != 0 {
		t.Fatalf("events=%v flushes=%d overlap=%v dropped=%d; want a, 1, false, 0", events, calls, overlap, as.Dropped())
	}
}

func TestAsyncSinkCloseCancellationDuringSharedFlushDropsRemainder(t *testing.T) {
	gate := newAsyncFlushBarrierSink()
	as := newAsyncFlushTestSink(t, gate, gate, WithCapacity(2))
	as.Write(Record{Event: "a"})
	flushed := runAsyncTestCall(func() error { return as.Flush(context.Background()) })
	waitAsyncTestValue(t, gate.flushEntered)
	as.Write(Record{Event: "b"})
	atB := runWaitingAsyncFlush(t, as, context.Background())
	as.Write(Record{Event: "c"})
	atC := runWaitingAsyncFlush(t, as, context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := runAsyncTestCall(func() error { return as.Close(ctx) })
	waitAsyncTestClosed(t, as)
	cancel()
	if err := waitAsyncTestValue(t, closed); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v, want context.Canceled", err)
	}
	if err := as.Close(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("repeated Close = %v, want first cancellation", err)
	}
	gate.flushGate.release()
	if err := waitAsyncTestValue(t, flushed); err != nil {
		t.Fatalf("already drained Flush = %v, want nil", err)
	}
	for _, done := range []<-chan error{atB, atC} {
		if err := waitAsyncTestValue(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("queued Flush after canceled Close = %v, want context.Canceled", err)
		}
	}
	waitAsyncTestValue(t, as.workerDone)
	events, calls, overlap := gate.snapshot()
	if !reflect.DeepEqual(events, []string{"a"}) || calls != 1 || overlap || as.Dropped() != 2 {
		t.Fatalf("events=%v flushes=%d overlap=%v dropped=%d; want a, 1, false, 2", events, calls, overlap, as.Dropped())
	}
}

func TestAsyncSinkDropOnFullDuringInnerFlush(t *testing.T) {
	gate := newAsyncFlushBarrierSink()
	as := newAsyncFlushTestSink(t, gate, gate, WithCapacity(1), DropOnFull())
	as.Write(Record{Event: "a"})
	flushed := runAsyncTestCall(func() error { return as.Flush(context.Background()) })
	waitAsyncTestValue(t, gate.flushEntered)
	as.Write(Record{Event: "b"})
	as.Write(Record{Event: "c"})
	as.Write(Record{Event: "d"})
	if as.Dropped() != 2 {
		t.Fatalf("full queue dropped=%d, want 2 new records", as.Dropped())
	}
	gate.flushGate.release()
	if err := waitAsyncTestValue(t, flushed); err != nil {
		t.Fatal(err)
	}
	if err := as.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _, overlap := gate.snapshot()
	if !reflect.DeepEqual(events, []string{"a", "b"}) || overlap || as.Dropped() != 2 {
		t.Fatalf("events=%v overlap=%v dropped=%d; want a/b, false, 2", events, overlap, as.Dropped())
	}
}
