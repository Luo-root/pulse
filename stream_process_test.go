package pulse

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func messageTestOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type messageTestFixture struct {
	rc, sourceRC, sinkRC *RunCtx
	input, output        Stream[int]
	other                Stream[string]
	source, writer       *StreamWriter[int]
	otherWriter          *StreamWriter[string]
	value, conflict      Key[int]
}

func newMessageTestFixture(t *testing.T, capacity int) *messageTestFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	input, output := NewKey[Stream[int]]("input"), NewKey[Stream[int]]("output")
	other := NewKey[Stream[string]]("other")
	f := &messageTestFixture{value: NewKey[int]("a-value"), conflict: NewKey[int]("z-conflict")}
	g, err := New("message-test", "")
	messageTestOK(t, err)
	source := NewNode("source", "", nil, Provides(input), nil)
	mapper := NewNode("map", "", Requires(input), Deps(Provides(output), Provides(other), Provides(f.value, f.conflict)), nil)
	sink := NewNode("sink", "", Deps(Requires(output), Requires(other)), nil, nil)
	for _, node := range []*Node{source, mapper, sink} {
		messageTestOK(t, g.Add(node))
	}
	e, err := g.NewExecution(ctx, Inputs{})
	messageTestOK(t, err)
	t.Cleanup(e.Cancel)
	f.sourceRC, f.rc, f.sinkRC = newRunCtx(e, source, ctx), newRunCtx(e, mapper, ctx), newRunCtx(e, sink, ctx)
	for _, rc := range []*RunCtx{f.sourceRC, f.rc, f.sinkRC} {
		t.Cleanup(rc.Cancel)
	}
	f.source, err = OpenStream(f.sourceRC, input, WithBuffer(3))
	messageTestOK(t, err)
	f.input, err = Get(f.rc, input)
	messageTestOK(t, err)
	f.writer, err = OpenStream(f.rc, output, WithBuffer(capacity))
	messageTestOK(t, err)
	f.otherWriter, err = OpenStream(f.rc, other, WithBuffer(capacity))
	messageTestOK(t, err)
	f.output, err = Get(f.sinkRC, output)
	messageTestOK(t, err)
	f.other, err = Get(f.sinkRC, other)
	messageTestOK(t, err)
	return f
}

func (f *messageTestFixture) delivery(t *testing.T, value int) Delivery[int] {
	t.Helper()
	messageTestOK(t, f.source.Send(value))
	delivery, err := f.input.Receive(f.rc)
	messageTestOK(t, err)
	return delivery
}

func messageTestNoCommit(t *testing.T, f *messageTestFixture) {
	t.Helper()
	if got := f.input.Snapshot(); got.AckedThrough != 0 || got.InFlight != 1 {
		t.Fatalf("failed message was acknowledged: %+v", got)
	}
	if got := f.output.Snapshot(); got.LastSequence != 0 || got.InFlight != 0 {
		t.Fatalf("failed attempt published output: %+v", got)
	}
	if got := f.other.Snapshot(); got.LastSequence != 0 || got.InFlight != 0 {
		t.Fatalf("failed attempt published another output: %+v", got)
	}
	if state, _ := f.rc.exec.slotOf(f.value.asRef()).snapshot(); state != slotPending {
		t.Fatalf("failed attempt published ordinary value: %v", state)
	}
}

func TestMessageRetryKeepsDeliveryAndIsolatesEveryAttempt(t *testing.T) {
	f := newMessageTestFixture(t, 3)
	for _, value := range []int{1, 2, 3} {
		messageTestOK(t, f.source.Send(value))
	}
	messageTestOK(t, f.sourceRC.finishStreams(nil))
	var called []int
	failed := false
	for range 3 {
		delivery, err := f.input.Receive(f.rc)
		messageTestOK(t, err)
		ack := delivery.ack
		messageTestOK(t, HandleMessage(f.rc, delivery, func(rc *RunCtx, value int) error {
			called = append(called, value)
			if delivery.ack != ack || delivery.Sequence != uint64(value) || !delivery.active() {
				t.Fatal("message retry replaced its delivery identity")
			}
			if _, ready, skipped, err := TryGet(rc, f.value); err != nil || (value == 1 && (ready || skipped)) {
				t.Fatalf("fresh attempt read prior staged value: ready=%t skipped=%t err=%v", ready, skipped, err)
			}
			if value == 2 && !failed {
				failed = true
				messageTestOK(t, Set(rc, f.conflict, -20))
				messageTestOK(t, f.writer.Send(-20))
				if f.output.Snapshot().LastSequence != 1 {
					t.Fatal("failed attempt became visible before its callback returned")
				}
				return errors.New("retry message two")
			}
			if value == 2 {
				if _, ready, _, err := TryGet(rc, f.conflict); ready || err != nil {
					t.Fatalf("failed Set leaked into retry: ready=%t err=%v", ready, err)
				}
			}
			messageTestOK(t, Set(rc, f.value, value))
			return f.writer.Send(value * 10)
		}, WithMessageRetry(2, 0)))
		messageTestOK(t, delivery.Ack()) // 自动确认后重复手动确认仍幂等。
	}
	if want := []int{1, 2, 2, 3}; !reflect.DeepEqual(called, want) {
		t.Fatalf("callbacks = %v, want %v", called, want)
	}
	for _, want := range []int{10, 20, 30} {
		delivery, err := f.output.Receive(f.sinkRC)
		messageTestOK(t, err)
		if delivery.Value != want {
			t.Fatalf("published %d, want %d", delivery.Value, want)
		}
		messageTestOK(t, delivery.Ack())
	}
	if got := f.input.Snapshot(); got.AckedThrough != 3 || got.InFlight != 0 {
		t.Fatalf("input confirmation = %+v", got)
	}
	if f.rc.writes != nil || f.rc.message != nil || f.writer.state.failure() != nil {
		t.Fatal("attempt state or failed Send polluted the actual writer")
	}
}

func TestMessageExhaustionLatchesEvenWhenCallerSwallowsError(t *testing.T) {
	f := newMessageTestFixture(t, 3)
	delivery := f.delivery(t, 2)
	cause := errors.New("message stays broken")
	attempts := 0
	err := HandleMessage(f.rc, delivery, func(rc *RunCtx, value int) error {
		attempts++
		messageTestOK(t, Set(rc, f.value, value))
		messageTestOK(t, f.writer.Send(value))
		return cause
	}, WithMessageRetry(3, 0))
	if !errors.Is(err, cause) || attempts != 3 {
		t.Fatalf("exhaustion: attempts=%d err=%v", attempts, err)
	}
	messageTestNoCommit(t, f)
	if err := f.rc.finishStreams(nil); !errors.Is(err, cause) {
		t.Fatalf("swallowed exhaustion became success: %v", err)
	}
}

func TestMessageLimitsFailInsideCallbackAndCannotBeSwallowed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		capacity int
		limit    MessageOption
		accepted int
		want     string
	}{
		{"default", 65, nil, 64, "limit 64"},
		{"explicit", 3, WithMessageLimit(2), 2, "limit 2"},
		{"capacity", 1, WithMessageLimit(4), 1, "capacity 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMessageTestFixture(t, tc.capacity)
			delivery := f.delivery(t, 1)
			var emitErr error
			err := HandleMessage(f.rc, delivery, func(rc *RunCtx, value int) error {
				for range tc.accepted {
					messageTestOK(t, f.writer.Send(value))
				}
				emitErr = f.writer.Send(value)
				if emitErr == nil || !strings.Contains(emitErr.Error(), tc.want) {
					t.Fatalf("overflow was not reported before callback returned: %v", emitErr)
				}
				return NoValue() // 正常跳过不能藏住暂存错误。
			}, tc.limit)
			if err != emitErr {
				t.Fatalf("staging error changed or disappeared: %v, want %v", err, emitErr)
			}
			messageTestNoCommit(t, f)
			if f.writer.state.failure() != nil {
				t.Fatal("attempt staging polluted stream sendErr")
			}
		})
	}
}

func TestMessageStagingFailureCanRetryWithFreshBudget(t *testing.T) {
	f := newMessageTestFixture(t, 1)
	delivery := f.delivery(t, 7)
	attempts := 0
	messageTestOK(t, HandleMessage(f.rc, delivery, func(rc *RunCtx, value int) error {
		attempts++
		messageTestOK(t, f.writer.Send(value))
		if attempts == 1 {
			_ = f.writer.Send(-value)
		}
		return nil
	}, WithMessageRetry(2, 0), WithMessageLimit(1)))
	if attempts != 2 || f.writer.state.failure() != nil || f.output.Snapshot().LastSequence != 1 {
		t.Fatalf("fresh retry budget: attempts=%d snapshot=%+v err=%v", attempts, f.output.Snapshot(), f.writer.state.failure())
	}
}

func TestMessageEmptySkipAndConfirmationFrontier(t *testing.T) {
	f := newMessageTestFixture(t, 1)
	var deliveries []Delivery[int]
	for _, value := range []int{1, 2, 3} {
		deliveries = append(deliveries, f.delivery(t, value))
	}
	messageTestOK(t, f.sourceRC.finishStreams(nil))
	calls := 0
	for _, index := range []int{2, 0, 1} {
		messageTestOK(t, HandleMessage(f.rc, deliveries[index], func(*RunCtx, int) error {
			calls++
			return NoValue()
		}, WithMessageRetry(3, 0)))
		want := []uint64{1, 3, 0}[index]
		if got := f.input.Snapshot().AckedThrough; got != want {
			t.Fatalf("after Ack %d: frontier=%d, want %d", index+1, got, want)
		}
	}
	if calls != 3 || f.output.Snapshot().LastSequence != 0 {
		t.Fatalf("skip retried or invented output: calls=%d snapshot=%+v", calls, f.output.Snapshot())
	}
}

func TestMessageInvalidDeliveryAndCommitConflictNeverPublish(t *testing.T) {
	for _, mode := range []string{"expired-before", "expired-during", "slot-conflict", "output-stopped", "output-finished", "reader-failed"} {
		t.Run(mode, func(t *testing.T) {
			f := newMessageTestFixture(t, 2)
			delivery := f.delivery(t, 1)
			if mode == "expired-before" {
				messageTestOK(t, delivery.Nack())
			}
			if mode == "slot-conflict" {
				messageTestOK(t, Skip(f.rc, f.conflict))
			}
			called := false
			err := HandleMessage(f.rc, delivery, func(rc *RunCtx, value int) error {
				called = true
				messageTestOK(t, Set(rc, f.value, value))
				messageTestOK(t, Set(rc, f.conflict, value))
				messageTestOK(t, f.writer.Send(value))
				messageTestOK(t, f.otherWriter.Send("second output"))
				switch mode {
				case "expired-during":
					return delivery.Nack()
				case "output-stopped":
					return f.other.Stop(f.sinkRC)
				case "output-finished":
					f.other.state.finish(nil)
				case "reader-failed":
					f.other.state.mu.Lock()
					f.other.state.readerErr = errors.New("reader exited")
					f.other.state.mu.Unlock()
				}
				return nil
			})
			if err == nil || (mode == "expired-before" && called) {
				t.Fatalf("invalid commit was accepted: callback=%t err=%v", called, err)
			}
			if strings.HasPrefix(mode, "expired") && !errors.Is(err, ErrDeliveryExpired) {
				t.Fatalf("expired delivery error = %v", err)
			}
			if mode == "slot-conflict" && !errors.Is(err, ErrConflict) {
				t.Fatalf("slot conflict error = %v", err)
			}
			messageTestNoCommit(t, f)
		})
	}
}

type messageWaitWitness struct {
	context.Context
	waiting chan struct{}
	again   chan struct{}
	calls   atomic.Int32
}

func (c *messageWaitWitness) Done() <-chan struct{} {
	switch c.calls.Add(1) {
	case 1:
		close(c.waiting)
	case 2:
		if c.again != nil {
			close(c.again)
		}
	}
	return c.Context.Done()
}

func messageTestWait(t *testing.T, ctx context.Context, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestMessageWaitsForWholeBatchBeforeAnyPublication(t *testing.T) {
	f := newMessageTestFixture(t, 3)
	delivery := f.delivery(t, 1)
	for _, value := range []int{8, 9, 10} {
		messageTestOK(t, f.writer.Send(value))
	}
	old, err := f.output.Receive(f.sinkRC)
	messageTestOK(t, err)
	witness := &messageWaitWitness{Context: f.rc.Context(), waiting: make(chan struct{}), again: make(chan struct{})}
	f.rc.ctx = witness
	result := make(chan error, 1)
	go func() {
		result <- HandleMessage(f.rc, delivery, func(rc *RunCtx, value int) error {
			if err := Set(rc, f.value, value); err != nil {
				return err
			}
			if err := f.otherWriter.Send("whole batch"); err != nil {
				return err
			}
			if err := f.writer.Send(11); err != nil {
				return err
			}
			return f.writer.Send(12)
		})
	}()
	messageTestWait(t, f.sourceRC.Context(), witness.waiting)
	// 三个旧义务都在；整批不能只接纳第一条，普通槽位和其它流也不能先落地。
	if f.output.Snapshot().LastSequence != 3 || f.other.Snapshot().LastSequence != 0 || f.input.Snapshot().AckedThrough != 0 {
		t.Fatal("partial batch became visible before enough capacity")
	}
	if state, _ := f.rc.exec.slotOf(f.value.asRef()).snapshot(); state != slotPending {
		t.Fatal("ordinary Set was committed before stream capacity became available")
	}
	messageTestOK(t, old.Ack())
	messageTestWait(t, f.sourceRC.Context(), witness.again)
	if f.output.Snapshot().LastSequence != 3 || f.other.Snapshot().LastSequence != 0 {
		t.Fatal("one free position published part of a two-message batch")
	}
	// 仅一个空位容不下两条新消息，下面两项读取都是已接纳的旧消息。
	for _, want := range []int{9, 10} {
		old, err = f.output.Receive(f.sinkRC)
		messageTestOK(t, err)
		if old.Value != want {
			t.Fatalf("partial batch displaced old values: got %d, want %d", old.Value, want)
		}
		if want == 9 {
			messageTestOK(t, old.Ack())
		}
	}
	select {
	case err := <-result:
		messageTestOK(t, err)
	case <-f.sourceRC.Context().Done():
		t.Fatal(f.sourceRC.Context().Err())
	}
	if f.output.Snapshot().LastSequence != 5 || f.other.Snapshot().LastSequence != 1 || f.input.Snapshot().AckedThrough != 1 {
		t.Fatalf("complete batch not committed: input=%+v output=%+v other=%+v", f.input.Snapshot(), f.output.Snapshot(), f.other.Snapshot())
	}
}

func TestMessageCanceledCapacityWaitDoesNotConfirmOrPublish(t *testing.T) {
	f := newMessageTestFixture(t, 1)
	delivery := f.delivery(t, 1)
	messageTestOK(t, f.writer.Send(99))
	witness := &messageWaitWitness{Context: f.rc.Context(), waiting: make(chan struct{})}
	f.rc.ctx = witness
	result := make(chan error, 1)
	go func() {
		result <- HandleMessage(f.rc, delivery, func(rc *RunCtx, value int) error {
			if err := Set(rc, f.value, value); err != nil {
				return err
			}
			return f.writer.Send(value)
		})
	}()
	messageTestWait(t, f.sourceRC.Context(), witness.waiting)
	f.rc.Cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled commit = %v", err)
		}
	case <-f.sourceRC.Context().Done():
		t.Fatal(f.sourceRC.Context().Err())
	}
	if got := f.input.Snapshot(); got.AckedThrough != 0 || got.InFlight != 1 || !delivery.active() {
		t.Fatalf("cancellation confirmed or invalidated input: %+v", got)
	}
	if got := f.output.Snapshot(); got.LastSequence != 1 || got.InFlight != 1 {
		t.Fatalf("cancellation published tentative output: %+v", got)
	}
	if state, _ := f.rc.exec.slotOf(f.value.asRef()).snapshot(); state != slotPending {
		t.Fatal("canceled ordinary Set became visible")
	}
}

func TestMessageRejectsNestedRetryFrameAndRestoresAfterPanic(t *testing.T) {
	f := newMessageTestFixture(t, 1)
	delivery := f.delivery(t, 1)
	err := HandleMessage(f.rc, delivery, func(rc *RunCtx, value int) error {
		return Retry(1, 0)(rc, func(*RunCtx) error { return f.writer.Send(value) })
	})
	if err == nil || !strings.Contains(err.Error(), "ordinary Retry") {
		t.Fatalf("ordinary Retry inside message = %v", err)
	}
	messageTestNoCommit(t, f)
	func() {
		defer func() {
			if got := recover(); got != "message panic" {
				t.Errorf("panic = %v", got)
			}
		}()
		_ = HandleMessage(f.rc, delivery, func(*RunCtx, int) error { panic("message panic") })
	}()
	if f.rc.writes != nil || f.rc.message != nil {
		t.Fatal("panic left an active message frame")
	}
}

func TestMessageRetryDelayYieldsAndRestoresQuotaOnCancel(t *testing.T) {
	f := newMessageTestFixture(t, 1)
	delivery := f.delivery(t, 1)
	pool := newLimiter(1)
	f.rc.lease = &runLease{pool: pool}
	messageTestOK(t, f.rc.lease.acquire(f.rc.Context()))
	t.Cleanup(f.rc.lease.release)
	witness := &messageWaitWitness{Context: f.rc.Context(), waiting: make(chan struct{}), again: make(chan struct{})}
	f.rc.ctx = witness
	result := make(chan error, 1)
	attempts := 0
	go func() {
		result <- HandleMessage(f.rc, delivery, func(*RunCtx, int) error {
			attempts++
			if len(pool.sem) != 1 {
				return errors.New("callback resumed without quota")
			}
			return errors.New("retry after delay")
		}, WithMessageRetry(2, time.Hour))
	}()
	messageTestWait(t, f.sourceRC.Context(), witness.waiting)
	messageTestOK(t, pool.acquire(f.sourceRC.Context())) // 延迟期间确实让出了唯一额度。
	var released atomic.Bool
	releaseOther := func() {
		if released.CompareAndSwap(false, true) {
			pool.release()
		}
	}
	t.Cleanup(releaseOther)
	f.rc.Cancel()
	messageTestWait(t, f.sourceRC.Context(), witness.again) // 已在尝试取回额度。
	select {
	case err := <-result:
		t.Fatalf("canceled delay returned before recovering quota: %v", err)
	default:
	}
	releaseOther()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || attempts != 1 || len(pool.sem) != 1 {
			t.Fatalf("delay cancellation: attempts=%d held=%d err=%v", attempts, len(pool.sem), err)
		}
	case <-f.sourceRC.Context().Done():
		t.Fatal(f.sourceRC.Context().Err())
	}
	messageTestNoCommit(t, f)
}

func TestMessageInputInvalidationInterruptsCapacityWait(t *testing.T) {
	for _, invalidate := range []string{"ack", "nack", "stop"} {
		t.Run(invalidate, func(t *testing.T) {
			f := newMessageTestFixture(t, 1)
			delivery := f.delivery(t, 1)
			messageTestOK(t, f.writer.Send(99))
			witness := &messageWaitWitness{Context: f.rc.Context(), waiting: make(chan struct{})}
			f.rc.ctx = witness
			result := make(chan error, 1)
			go func() {
				result <- HandleMessage(f.rc, delivery, func(*RunCtx, int) error { return f.writer.Send(1) })
			}()
			messageTestWait(t, f.sourceRC.Context(), witness.waiting)
			switch invalidate {
			case "ack":
				messageTestOK(t, delivery.Ack())
			case "nack":
				messageTestOK(t, delivery.Nack())
			case "stop":
				messageTestOK(t, f.input.Stop(f.sourceRC))
			}
			select {
			case err := <-result:
				if !errors.Is(err, ErrDeliveryExpired) {
					t.Fatalf("invalidated capacity wait = %v", err)
				}
			case <-f.sourceRC.Context().Done():
				t.Fatal(f.sourceRC.Context().Err())
			}
			if f.output.Snapshot().LastSequence != 1 {
				t.Fatal("expired input committed a tentative message")
			}
		})
	}
}

type messageCancelOnConfirmation struct {
	*managedAck[int]
	cancel context.CancelFunc
}

func (a messageCancelOnConfirmation) confirmMessageLocked() {
	a.managedAck.confirmMessageLocked()
	a.cancel()
}

func TestMessageAcceptedCommitSurvivesSubsequentCancellation(t *testing.T) {
	f := newMessageTestFixture(t, 1)
	delivery := f.delivery(t, 1)
	frame := &messageAttempt{writes: &writeSet{}, input: &f.input.state.mu, limit: 64, outputs: make(map[any]messageBatch)}
	messageTestOK(t, runMessageAttempt(f.rc, frame, func() error { return f.writer.Send(7) }))
	// 包内确认替身在真正确认的那一刻取消；提交完成后再看 ctx 的变异必定会被击中。
	messageTestOK(t, frame.commit(f.rc, messageCancelOnConfirmation{delivery.ack, f.rc.Cancel}))
	if f.rc.Context().Err() != context.Canceled || f.output.Snapshot().LastSequence != 1 || f.input.Snapshot().AckedThrough != 1 {
		t.Fatal("completed message was rewritten as an uncommitted cancellation")
	}
}
