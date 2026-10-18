package pulse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
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

// 只验证 Core 消息协议；真正的存储恢复/所有权检查由 Durable 联调验证。
func messageTestCheckpoint(f *messageTestFixture, hooks *CheckpointStreamHooks) {
	e := f.rc.exec
	e.checkpoint = &checkpointConfig{scope: "run", hooks: CheckpointHooks{StreamHooks: hooks}, failure: &checkpointFailure{root: e}}
	f.rc.checkpointAttempt = CheckpointAttempt{Scope: "node-attempt", Index: 1}
	f.rc.checkpointWrites = &writeSet{}
	f.rc.writes = f.rc.checkpointWrites
	f.input.state.checkpoint = &checkpointStream{exec: e, descriptor: CheckpointStreamDescriptor{
		ID:      CheckpointStreamID{Scope: "run", NodeID: "source", Key: "input", Group: "default", Generation: 1},
		Attempt: "source-attempt", Element: reflect.TypeFor[int](), Capacity: f.input.state.capacity,
	}}
	f.output.state.checkpoint = &checkpointStream{exec: e, descriptor: CheckpointStreamDescriptor{
		ID:      CheckpointStreamID{Scope: "run", NodeID: "map", Key: "output", Group: "default", Generation: 1},
		Attempt: "node-attempt", Element: reflect.TypeFor[int](), Capacity: f.output.state.capacity,
	}}
	f.other.state.checkpoint = &checkpointStream{exec: e, descriptor: CheckpointStreamDescriptor{
		ID:      CheckpointStreamID{Scope: "run", NodeID: "map", Key: "other", Group: "default", Generation: 1},
		Attempt: "node-attempt", Element: reflect.TypeFor[string](), Capacity: f.other.state.capacity,
	}}
}

func TestMessageCheckpointResumesBudgetAndCommitsOnceBeforePublish(t *testing.T) {
	f := newMessageTestFixture(t, 2)
	delivery := f.delivery(t, 2)
	var order []string
	var commit CheckpointMessageCommit
	hooks := &CheckpointStreamHooks{
		OpenMessage: func(_ context.Context, req CheckpointMessageAttemptRequest) (CheckpointMessageAttempt, error) {
			index := 2
			if len(order) != 0 {
				if !reflect.DeepEqual(order, []string{"open-2", "fail-message-2"}) {
					t.Fatalf("opened next attempt before confirmed failure: %v", order)
				}
				index = 3
			}
			if req.Limit != 3 || req.Input.Sequence != 1 || req.Input.Stream.Key != "input" || req.Attempt != "node-attempt" {
				t.Fatalf("unstable message attempt identity: %+v", req)
			}
			order = append(order, fmt.Sprintf("open-%d", index))
			return CheckpointMessageAttempt{Attempt: CheckpointAttempt{Scope: fmt.Sprintf("message-%d", index), Index: index},
				State: []CheckpointState{{Name: "count", Type: reflect.TypeFor[int](), Value: 5}}}, nil
		},
		FailMessage: func(_ context.Context, scope string) error { order = append(order, "fail-"+scope); return nil },
		Commit: func(_ context.Context, record CheckpointMessageCommit) error {
			order = append(order, "commit")
			for _, mu := range []*sync.Mutex{&f.input.state.mu, &f.output.state.mu, &f.other.state.mu} {
				if !mu.TryLock() {
					t.Fatal("reliable Commit ran under a stream state lock")
				}
				mu.Unlock()
			}
			for _, op := range []*streamOperation{&f.input.state.operation, &f.output.state.operation, &f.other.state.operation} {
				if len(op.gate) != 1 {
					t.Fatal("Commit did not retain all transaction operation gates")
				}
			}
			messageTestNoCommit(t, f)
			commit = record
			return nil
		},
	}
	messageTestCheckpoint(f, hooks)
	calls := 0
	messageTestOK(t, HandleMessage(f.rc, delivery, func(rc *RunCtx, value int) error {
		calls++
		state, present, err := GetMessageState[int](rc, "count")
		if err != nil || !present || state != 5 {
			t.Fatalf("state leaked from failed attempt: value=%d present=%t err=%v", state, present, err)
		}
		messageTestOK(t, SetMessageState(rc, "count", state+calls))
		messageTestOK(t, Set(rc, f.value, value*10))
		messageTestOK(t, f.writer.Send(value*10))
		if calls == 1 {
			return errors.New("failed persistent attempt two")
		}
		return f.otherWriter.Send("committed")
	}, WithMessageRetry(3, 0)))
	if calls != 2 || !reflect.DeepEqual(order, []string{"open-2", "fail-message-2", "open-3", "commit"}) {
		t.Fatalf("persistent budget replenished or success double-committed: calls=%d order=%v", calls, order)
	}
	if commit.Attempt != "message-3" || len(commit.Inputs) != 1 || len(commit.Outputs) != 2 || len(commit.Values) != 1 || len(commit.State) != 1 || commit.State[0].Value != 7 {
		t.Fatalf("incomplete atomic reliable record: %+v", commit)
	}
	if commit.Outputs[0].Message.Sequence != 1 || commit.Outputs[0].Message.Value != 20 || commit.Values[0].Value != 20 {
		t.Fatalf("wrong output identities or values: %+v", commit)
	}
	if f.input.Snapshot().AckedThrough != 1 || f.output.Snapshot().LastSequence != 1 || f.other.Snapshot().LastSequence != 1 {
		t.Fatal("confirmed reliable message was not locally published")
	}
	if state, _ := f.rc.exec.slotOf(f.value.asRef()).snapshot(); state != slotPending {
		t.Fatal("message ordinary value escaped complete-chain checkpoint staging")
	}
	if state, value := f.rc.checkpointWrites.peek(f.value.asRef()).snapshot(); state != slotReady || value != 20 || f.rc.writes != f.rc.checkpointWrites {
		t.Fatalf("ordinary value was not merged into complete-chain frame: state=%v value=%v", state, value)
	}
}

func TestMessageCheckpointBatchInstallsAcceptedCursor(t *testing.T) {
	for _, mode := range []string{"no-prefix", "direct", "message", "retry", "callback-error", "commit-error"} {
		t.Run(mode, func(t *testing.T) {
			f := newMessageTestFixture(t, 3)
			delivery := f.delivery(t, 7)
			cause := errors.New("failed batch")
			var accepted []CheckpointStreamAppend
			messageTestCheckpoint(f, &CheckpointStreamHooks{
				Append: func(_ context.Context, record CheckpointStreamAppend) error {
					accepted = append(accepted, record)
					return nil
				},
				OpenMessage: func(context.Context, CheckpointMessageAttemptRequest) (CheckpointMessageAttempt, error) {
					return CheckpointMessageAttempt{Attempt: CheckpointAttempt{Scope: "message", Index: 1}}, nil
				},
				FailMessage: func(context.Context, string) error { return nil },
				Commit: func(_ context.Context, record CheckpointMessageCommit) error {
					if mode == "commit-error" {
						return cause
					}
					accepted = append(accepted, record.Outputs...)
					return nil
				},
			})
			if mode != "no-prefix" {
				messageTestOK(t, f.writer.SendAt(-1, 99))
			}
			before := f.writer.Position()
			calls := 0
			var err error
			if mode == "direct" {
				err = f.writer.Send(70)
			} else {
				err = HandleMessage(f.rc, delivery, func(_ *RunCtx, value int) error {
					calls++
					if got := f.writer.Position(); got != before {
						t.Fatalf("unconfirmed attempt changed position: %+v, want %+v", got, before)
					}
					if err := f.writer.Send(value * 10); err != nil {
						return err
					}
					if mode == "callback-error" || (mode == "retry" && calls == 1) {
						return cause
					}
					return nil
				}, WithMessageRetry(2, 0))
			}
			if mode == "callback-error" || mode == "commit-error" {
				if !errors.Is(err, cause) || f.writer.Position() != before || len(accepted) != 1 {
					t.Fatalf("failed batch changed position: position=%+v accepted=%+v err=%v", f.writer.Position(), accepted, err)
				}
				return
			}
			messageTestOK(t, err)
			position := f.writer.Position()
			last := accepted[len(accepted)-1]
			if position.Sequence != before.Sequence+1 || position.Sequence != last.Message.Sequence || position.Cursor != last.Cursor || position.Cursor != nil {
				t.Fatalf("local and accepted positions differ: position=%+v append=%+v", position, last)
			}
			if mode == "retry" && calls != 2 {
				t.Fatalf("retry callbacks = %d, want 2", calls)
			}
			finish, ok := f.writer.state.checkpointFinish(nil)
			if !ok || finish.LastSequence != position.Sequence || finish.Cursor != position.Cursor {
				t.Fatalf("complete-chain finish used stale position: %+v", finish)
			}
			for _, record := range accepted {
				got, err := f.output.Receive(f.sinkRC)
				messageTestOK(t, err)
				if got.Sequence != record.Message.Sequence || got.Value != record.Message.Value {
					t.Fatalf("published delivery=%+v, accepted=%+v", got, record)
				}
			}
		})
	}
}

func TestMessageCheckpointCommitExcludesConcurrentNack(t *testing.T) {
	f := newMessageTestFixture(t, 1)
	delivery := f.delivery(t, 1)
	witness := &messageWaitWitness{Context: f.rc.Context(), waiting: make(chan struct{})}
	f.rc.ctx = witness
	nacked := make(chan error, 1)
	messageTestCheckpoint(f, &CheckpointStreamHooks{
		OpenMessage: func(context.Context, CheckpointMessageAttemptRequest) (CheckpointMessageAttempt, error) {
			return CheckpointMessageAttempt{Attempt: CheckpointAttempt{Scope: "message", Index: 1}}, nil
		},
		Commit: func(context.Context, CheckpointMessageCommit) error {
			go func() { nacked <- delivery.Nack() }()
			messageTestWait(t, f.sourceRC.Context(), witness.waiting)
			select {
			case err := <-nacked:
				t.Fatalf("Nack interposed during reliable confirmation: %v", err)
			default:
			}
			if !delivery.active() {
				t.Fatal("reliable confirmation lost its current delivery")
			}
			return nil
		},
	})
	messageTestOK(t, HandleMessage(f.rc, delivery, func(_ *RunCtx, value int) error { return f.writer.Send(value) }))
	select {
	case err := <-nacked:
		if !errors.Is(err, ErrDeliveryExpired) {
			t.Fatalf("Nack after committed Ack = %v", err)
		}
	case <-f.sourceRC.Context().Done():
		t.Fatal(f.sourceRC.Context().Err())
	}
	if f.input.Snapshot().AckedThrough != 1 || f.output.Snapshot().LastSequence != 1 {
		t.Fatal("Nack interposed between reliable Commit and local publication")
	}
}

func TestMessageCheckpointConfirmedCommitSurvivesCancellation(t *testing.T) {
	f := newMessageTestFixture(t, 1)
	delivery := f.delivery(t, 1)
	messageTestCheckpoint(f, &CheckpointStreamHooks{
		OpenMessage: func(context.Context, CheckpointMessageAttemptRequest) (CheckpointMessageAttempt, error) {
			return CheckpointMessageAttempt{Attempt: CheckpointAttempt{Scope: "message", Index: 1}}, nil
		},
		Commit: func(context.Context, CheckpointMessageCommit) error {
			f.rc.Cancel() // 存储已经确认；取消不能把已提交结果改报为未提交。
			return nil
		},
	})
	messageTestOK(t, HandleMessage(f.rc, delivery, func(rc *RunCtx, value int) error {
		messageTestOK(t, Set(rc, f.value, value))
		return f.writer.Send(value)
	}))
	if f.rc.Context().Err() != context.Canceled || f.input.Snapshot().AckedThrough != 1 || f.output.Snapshot().LastSequence != 1 {
		t.Fatal("confirmed message became an uncommitted cancellation")
	}
}

func TestMessageCheckpointFailuresDoNotPublishOrReplenishBudget(t *testing.T) {
	for _, mode := range []string{"exhausted", "last-attempt", "open-error", "commit-error", "fail-error", "host-cancel", "preflight"} {
		t.Run(mode, func(t *testing.T) {
			f := newMessageTestFixture(t, 2)
			delivery := f.delivery(t, 1)
			opens, failures, commits, calls := 0, 0, 0, 0
			cause := errors.New("persistent failure")
			hooks := &CheckpointStreamHooks{
				OpenMessage: func(context.Context, CheckpointMessageAttemptRequest) (CheckpointMessageAttempt, error) {
					opens++
					if mode == "open-error" {
						return CheckpointMessageAttempt{}, cause
					}
					index := 1
					if mode == "exhausted" || mode == "last-attempt" {
						index = 3
					}
					return CheckpointMessageAttempt{Attempt: CheckpointAttempt{Scope: "message", Index: index, Exhausted: mode == "exhausted"}}, nil
				},
				FailMessage: func(context.Context, string) error {
					failures++
					if mode == "fail-error" {
						return cause
					}
					return nil
				},
				Commit: func(context.Context, CheckpointMessageCommit) error { commits++; return cause },
			}
			messageTestCheckpoint(f, hooks)
			if mode == "preflight" {
				messageTestOK(t, Skip(f.rc, f.conflict))
			}
			err := HandleMessage(f.rc, delivery, func(rc *RunCtx, value int) error {
				calls++
				messageTestOK(t, Set(rc, f.value, value))
				messageTestOK(t, f.writer.Send(value))
				switch mode {
				case "host-cancel":
					f.rc.exec.Cancel()
					return cause
				case "last-attempt", "fail-error":
					return cause
				case "preflight":
					return Set(rc, f.conflict, value)
				}
				return nil
			}, WithMessageRetry(3, 0))
			if err == nil || opens != 1 || calls > 1 {
				t.Fatalf("failure retried locally or became success: opens=%d callbacks=%d err=%v", opens, calls, err)
			}
			wantFailures := 0
			if mode == "last-attempt" || mode == "fail-error" {
				wantFailures = 1
			}
			if failures != wantFailures || (mode == "commit-error" && commits != 1) || (mode != "commit-error" && commits != 0) {
				t.Fatalf("wrong failure/commit protocol: failed=%d commits=%d err=%v", failures, commits, err)
			}
			if strings.HasSuffix(mode, "error") && !isCheckpointFailure(err) {
				t.Fatalf("storage failure lost reliable error identity: %v", err)
			}
			messageTestNoCommit(t, f)
		})
	}
}

func TestLifecycleMessageRealNodeCancellationPropagation(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, propagate := range []bool{false, true} {
			for _, phase := range []string{"before-callback", "commit"} {
				t.Run(fmt.Sprintf("checkpoint=%t/propagate=%t/%s", checkpoint, propagate, phase), func(t *testing.T) {
					ctx := checkpointTestContext(t)
					obs := &recordingObserver{}
					g := mustNew(t, ctx, "message-node-cancel", WithObserver(obs))
					key := NewKey[Stream[int]]("input")
					var input Stream[int]
					mustAdd(t, g, NewNode("source", "提供交付", nil, Provides(key), func(rc *RunCtx) error {
						writer, err := OpenStream(rc, key, WithStreamSource("source", true))
						if err != nil {
							return err
						}
						input = Stream[int]{state: writer.state}
						return writer.Send(1)
					}))
					mustAdd(t, g, NewNode("processor", "取消事实随返回值和消息账一致传播", Requires(key), nil, func(rc *RunCtx) error {
						stream, err := Get(rc, key)
						if err != nil {
							return err
						}
						delivery, err := stream.Receive(rc)
						if err != nil {
							return err
						}
						if phase == "before-callback" {
							rc.Cancel()
						}
						err = HandleMessage(rc, delivery, func(inner *RunCtx, _ int) error {
							inner.Cancel()
							return nil
						})
						if propagate {
							return err
						}
						return nil
					}))
					var opts []ExecOption
					if checkpoint {
						opts = append(opts, WithExecCheckpoint("root", checkpointStreamTestHooks(&checkpointProbe{})))
					}
					e, err := g.NewExecution(ctx, Inputs{}, opts...)
					managedTestOK(t, err)
					defer e.Cancel()
					managedTestOK(t, e.Start())
					if err := e.Wait(); !errors.Is(err, context.Canceled) {
						t.Fatalf("lost actual cancellation: %v", err)
					}
					state := e.Snapshot()
					if ctx.Err() != nil || state.Graph.Status != GraphStatusCanceled || snapshotNode(t, state, "processor").Status != NodeStatusCanceled {
						t.Errorf("actual node cancellation changed: host=%v graph=%s processor=%s", ctx.Err(), state.Graph.Status, snapshotNode(t, state, "processor").Status)
					}
					if log := obs.snapshot(); countPref(log, "F:processor:canceled") != 1 || countPref(log, "GF:canceled") != 1 {
						t.Errorf("wrong observer reason: %v", log)
					}
					if got := input.Snapshot(); got.AckedThrough != 0 || got.InFlight != 1 {
						t.Errorf("canceled message was confirmed: %+v", got)
					}
				})
			}
		}
	}
}

func TestLifecycleMessageFrameFailureSurvivesCallbackCleanup(t *testing.T) {
	for _, budget := range []int{1, 3} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			f := newMessageTestFixture(t, 1)
			delivery := f.delivery(t, 1)
			var sendErr error
			calls := 0
			err := HandleMessage(f.rc, delivery, func(inner *RunCtx, value int) error {
				calls++
				defer inner.Cancel()
				if err := f.writer.Send(value); err != nil {
					return err
				}
				sendErr = f.writer.Send(value) // 第二条超过单批容量，帧错误优先于业务错误。
				return fmt.Errorf("independent RPC: %w", context.DeadlineExceeded)
			}, WithMessageRetry(budget, 0))
			if calls != 1 || sendErr == nil || err != sendErr || finishReason(f.rc.finishStreams(nil)) != NodeFailed {
				t.Fatalf("frame failure lost after cleanup: calls=%d send=%v err=%v", calls, sendErr, err)
			}
			messageTestNoCommit(t, f)
		})
	}
}

func TestLifecycleMessageFailureSurvivesCallbackCleanup(t *testing.T) {
	for _, config := range []struct {
		checkpoint bool
		budget     int
	}{{false, 1}, {true, 1}, {false, 3}, {true, 3}} {
		checkpoint, budget := config.checkpoint, config.budget
		for _, propagate := range []bool{false, true} {
			for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
				t.Run(fmt.Sprintf("checkpoint=%t/budget=%d/propagate=%t/%v", checkpoint, budget, propagate, cause), func(t *testing.T) {
					ctx := checkpointTestContext(t)
					obs := &recordingObserver{}
					g := mustNew(t, ctx, "message-cleanup", WithObserver(obs))
					key := NewKey[Stream[int]]("input")
					var input Stream[int]
					mustAdd(t, g, NewNode("source", "提供一条未确认消息", nil, Provides(key), func(rc *RunCtx) error {
						writer, err := OpenStream(rc, key, WithStreamSource("source", true))
						if err != nil {
							return err
						}
						input = Stream[int]{state: writer.state}
						return writer.Send(1)
					}))
					typed := &lifecycleRPCCause{cause}
					original := fmt.Errorf("independent message RPC: %w", typed)
					mustAdd(t, g, NewNode("processor", "回调清理不改业务失败来源", Requires(key), nil, func(rc *RunCtx) error {
						stream, err := Get(rc, key)
						if err != nil {
							return err
						}
						delivery, err := stream.Receive(rc)
						if err != nil {
							return err
						}
						err = HandleMessage(rc, delivery, func(inner *RunCtx, _ int) error {
							defer inner.Cancel()
							return original
						}, WithMessageRetry(budget, 0))
						if propagate {
							return err
						}
						return nil // 失败账必须与直接透传返回相同。
					}))
					probe := &checkpointProbe{}
					var messageFailures, messageCommits atomic.Int32
					var opts []ExecOption
					if checkpoint {
						hooks := checkpointStreamTestHooks(probe)
						hooks.StreamHooks.FailMessage = func(context.Context, string) error { messageFailures.Add(1); return nil }
						hooks.StreamHooks.Commit = func(context.Context, CheckpointMessageCommit) error { messageCommits.Add(1); return nil }
						opts = append(opts, WithExecCheckpoint("root", hooks))
					}
					e, err := g.NewExecution(ctx, Inputs{}, opts...)
					managedTestOK(t, err)
					defer e.Cancel()
					managedTestOK(t, e.Start())
					err = e.Wait()
					var gotTyped *lifecycleRPCCause
					if !errors.Is(err, original) || !errors.Is(err, cause) || !errors.As(err, &gotTyped) || gotTyped != typed {
						t.Fatalf("original RPC error lost: %v", err)
					}
					state := e.Snapshot()
					if ctx.Err() != nil || state.Graph.Status != GraphStatusFailed || snapshotNode(t, state, "processor").Status != NodeStatusFailed {
						t.Errorf("callback cleanup changed failure: host=%v graph=%s processor=%s", ctx.Err(), state.Graph.Status, snapshotNode(t, state, "processor").Status)
					}
					if log := obs.snapshot(); countPref(log, "F:processor:failed") != 1 || countPref(log, "GF:failed") != 1 {
						t.Errorf("wrong observer reason: %v", log)
					}
					if got := input.Snapshot(); got.AckedThrough != 0 || got.InFlight != 1 {
						t.Errorf("failed callback confirmed its delivery: %+v", got)
					}
					if checkpoint {
						if messageFailures.Load() != 1 || messageCommits.Load() != 0 {
							t.Errorf("message attempts: failures=%d commits=%d", messageFailures.Load(), messageCommits.Load())
						}
						processorFailures := 0
						for _, request := range probe.opens {
							if request.NodeID == "processor" {
								request.Site.Execution = ""
								if probe.failed[probe.latest[request].Scope] {
									processorFailures++
								}
							}
						}
						if processorFailures != 1 {
							t.Errorf("processor failure confirmations=%d", processorFailures)
						}
						for _, commit := range probe.commits {
							if commit.Node.NodeID == "processor" {
								t.Error("failed processor committed successfully")
							}
						}
					}
				})
			}
		}
	}
}

func TestLifecycleMessageEngineCancellationKeepsOrigin(t *testing.T) {
	for _, phase := range []string{"before-callback", "commit-wait", "forwarded"} {
		t.Run(phase, func(t *testing.T) {
			f := newMessageTestFixture(t, 1)
			delivery := f.delivery(t, 1)
			calls := 0
			if phase == "before-callback" {
				f.rc.Cancel()
			}
			err := HandleMessage(f.rc, delivery, func(inner *RunCtx, _ int) error {
				calls++
				if phase == "forwarded" {
					return &executionCancellation{cause: context.Canceled}
				}
				inner.Cancel() // callback 成功，随后提交遇到的是真实本层取消。
				return nil
			})
			wantCalls := 1
			if phase == "before-callback" {
				wantCalls = 0
			}
			latched := f.rc.finishStreams(nil)
			if calls != wantCalls || !errors.Is(err, context.Canceled) || finishReason(latched) != NodeCanceled {
				t.Fatalf("engine cancellation changed: calls=%d err=%v latched=%v reason=%s", calls, err, latched, finishReason(latched))
			}
			messageTestNoCommit(t, f)
		})
	}
}

func TestLifecycleMessageCancellationDuringFailureConfirmation(t *testing.T) {
	for _, budget := range []int{1, 3} {
		for _, mode := range []string{"host-cancel", "node-cleanup", "reliable-failure"} {
			t.Run(fmt.Sprintf("budget=%d/%s", budget, mode), func(t *testing.T) {
				ctx, cancel := context.WithCancel(checkpointTestContext(t))
				defer cancel()
				g := mustNew(t, ctx, "message-confirmation-cancel")
				key := NewKey[Stream[int]]("input")
				original := fmt.Errorf("retryable independent RPC: %w", context.DeadlineExceeded)
				storeErr := errors.New("FailMessage storage failure")
				var input Stream[int]
				var processorRC *RunCtx
				var callbacks, failures, commits atomic.Int32
				mustAdd(t, g, NewNode("source", "先完整结束来源", nil, Provides(key), func(rc *RunCtx) error {
					writer, err := OpenStream(rc, key, WithStreamSource("source", true))
					if err != nil {
						return err
					}
					return writer.Send(1)
				}))
				mustAdd(t, g, NewNode("processor", "确认钩子返回后重新检查执行域", Requires(key), nil, func(rc *RunCtx) error {
					processorRC = rc
					var err error
					input, err = Get(rc, key)
					if err != nil {
						return err
					}
					delivery, err := input.Receive(rc)
					if err != nil {
						return err
					}
					// 来源先结束，排除宿主取消与来源节点竞争首错而遮住结果。
					select {
					case <-input.Done():
					case <-rc.Context().Done():
						return rc.Context().Err()
					}
					return HandleMessage(rc, delivery, func(*RunCtx, int) error {
						callbacks.Add(1)
						return original
					}, WithMessageRetry(budget, 0))
				}))
				probe := &checkpointProbe{}
				hooks := checkpointStreamTestHooks(probe)
				hooks.StreamHooks.FailMessage = func(c context.Context, _ string) error {
					if c.Err() != nil {
						t.Error("host canceled before entering confirmation hook")
					}
					failures.Add(1)
					switch mode {
					case "host-cancel":
						cancel()
					case "node-cleanup":
						processorRC.Cancel()
					case "reliable-failure":
						cancel() // 已发生宿主取消，可靠失败仍应优先。
						return storeErr
					}
					return nil
				}
				hooks.StreamHooks.Commit = func(context.Context, CheckpointMessageCommit) error { commits.Add(1); return nil }
				e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
				managedTestOK(t, err)
				defer e.Cancel()
				managedTestOK(t, e.Start())
				err = e.Wait()
				state := e.Snapshot()
				want, wantFailures := GraphStatusFailed, 1
				switch mode {
				case "host-cancel":
					want, wantFailures = GraphStatusCanceled, 0
					if !errors.Is(err, context.Canceled) || errors.Is(err, original) {
						t.Errorf("host interruption replaced by attempt failure: %v", err)
					}
				case "node-cleanup":
					if !errors.Is(err, original) || ctx.Err() != nil {
						t.Errorf("local cleanup replaced attempt failure: %v", err)
					}
				case "reliable-failure":
					wantFailures = 0
					if !isCheckpointFailure(err) || !errors.Is(err, storeErr) {
						t.Errorf("reliable failure lost priority: %v", err)
					}
				}
				if state.Graph.Status != want || snapshotNode(t, state, "processor").Status != NodeStatus(want) || len(probe.failures) != wantFailures {
					t.Errorf("wrong confirmation outcome: graph=%s processor=%s node failures=%d", state.Graph.Status, snapshotNode(t, state, "processor").Status, len(probe.failures))
				}
				if callbacks.Load() != 1 || failures.Load() != 1 || commits.Load() != 0 || input.Snapshot().AckedThrough != 0 {
					t.Errorf("failure confirmation changed budget/publication: callbacks=%d failed=%d commits=%d input=%+v", callbacks.Load(), failures.Load(), commits.Load(), input.Snapshot())
				}
			})
		}
	}
}
