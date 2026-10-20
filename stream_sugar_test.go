package pulse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestStreamSugarMessageOptionCopies(t *testing.T) {
	caller := []MessageOption{WithMessageRetry(2, 0)}
	option := WithMessageHandling(caller...)
	caller[0] = WithMessageRetry(99, 0)
	base := streamConfig{messageOptions: make([]MessageOption, 1, 8)}
	base.messageOptions[0] = WithMessageRetry(3, 0)
	first, second := base, base
	option(&first)
	option(&second)
	first.messageOptions[0], first.messageOptions[1] = WithMessageRetry(99, 0), WithMessageRetry(99, 0)
	for _, cfg := range []streamConfig{second, newStreamConfig([]StreamOption{WithMessageHandling(), option})} {
		var message messageConfig
		for _, apply := range cfg.messageOptions {
			apply(&message)
		}
		if message.attempts != 2 {
			t.Fatalf("message option applications share caller storage: attempts=%d", message.attempts)
		}
	}
	open := newStreamConfig([]StreamOption{streamOpenOption(streamConfig{
		buf: 7, checkpointSource: CheckpointStreamSource{ID: "source", Resumable: true},
		aspects: []Aspect{Retry(2, 0)}, messageOptions: []MessageOption{WithMessageRetry(2, 0)},
	})})
	if len(open.aspects) != 0 || len(open.messageOptions) != 0 || open.buf != 7 || open.checkpointSource.ID != "source" || !open.checkpointSource.Resumable {
		t.Fatalf("OpenStream helper changed endpoint configuration: %+v", open)
	}
}

func TestStreamSugarCapturedConfigurationDoesNotChangeNode(t *testing.T) {
	g, err := New("sugar-captured-config", "", WithMaxRunning(1))
	if err != nil {
		t.Fatal(err)
	}
	input, output := NewKey[Stream[int]]("input"), NewKey[Stream[int]]("output")
	var saved []*streamConfig
	capture := func(cfg *streamConfig) { saved = append(saved, cfg) }
	if err := ProduceStream(g, "source", "", input, func(_ *RunCtx, writer *StreamWriter[int]) error { return writer.Send(7) }, WithStreamAspects(), capture); err != nil {
		t.Fatal(err)
	}
	option := WithMessageHandling(WithMessageRetry(2, 0))
	if err := TeeStream(g, "tee", "", input, Keys(output), option, capture); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := ConsumeStream(g, "sink", "", output, func(*RunCtx, int) error {
		calls++
		if calls == 1 {
			return errors.New("retry once")
		}
		return nil
	}, option, capture); err != nil {
		t.Fatal(err)
	}
	for _, cfg := range saved {
		cfg.buf = 99
		for i := range cfg.messageOptions {
			cfg.messageOptions[i] = WithMessageRetry(1, 0)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := g.Run(ctx, Inputs{}); err != nil || calls != 2 {
		t.Fatalf("assembled configuration changed: err=%v calls=%d", err, calls)
	}
}

// 源先接纳两条，Tee 第二次提交因分支容量 1 确定阻塞。下游随后在持有唯一
// 业务额度时 Ack 第一条并 Stop，故 Tee 恢复前必定看见停止，第二条不能确认。
func TestTeeStopBeforeCommitDoesNotAckUnforwardedInput(t *testing.T) {
	for _, reliable := range []bool{false, true} {
		t.Run(fmt.Sprintf("reliable=%t", reliable), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			g, err := New("tee-commit-stop", "", WithMaxRunning(1))
			managedTestOK(t, err)
			in, out := NewKey[Stream[int]]("input"), NewKey[Stream[int]]("output")
			sent, waiting := make(chan struct{}), make(chan struct{})
			var source, branch Stream[int]
			managedTestOK(t, ProduceStream(g, "source", "", in, func(rc *RunCtx, writer *StreamWriter[int]) error {
				var err error
				source, err = Get(rc, in)
				if err != nil {
					return err
				}
				if err := writer.Send(1); err != nil {
					return err
				}
				if err := writer.Send(2); err != nil {
					return err
				}
				close(sent)
				return nil
			}, WithBuffer(2), WithStreamSource("finite", true)))
			managedTestOK(t, TeeStream(g, "tee", "", in, Keys(out), WithBuffer(1), WithStreamAspects(
				func(rc *RunCtx, next func(*RunCtx) error) error {
					if err := teeStopWait(rc, sent); err != nil {
						return err
					}
					input, err := Get(rc, in)
					if err != nil {
						return err
					}
					rc.ctx = &teeReceiveWitness{Context: rc.ctx, input: input, entered: waiting}
					return next(rc)
				})))
			managedTestOK(t, g.Add(NewNode("sink", "", Requires(out), nil, func(rc *RunCtx) error {
				var err error
				branch, err = Get(rc, out)
				if err != nil {
					return err
				}
				first, err := branch.Receive(rc)
				if err != nil {
					return err
				}
				if first.Value != 1 {
					return fmt.Errorf("first=%v", first.Value)
				}
				if err := teeStopWait(rc, waiting); err != nil {
					return err
				}
				if got := source.Snapshot(); got.LastSequence != 2 || got.AckedThrough != 1 {
					return fmt.Errorf("did not reach second pending commit: %+v", got)
				}
				if err := first.Ack(); err != nil {
					return err
				}
				return branch.Stop(rc)
			})))
			var options []ExecOption
			var mu sync.Mutex
			var acknowledged []uint64
			if reliable {
				hooks := checkpointStreamTestHooks(&checkpointProbe{})
				hooks.StreamHooks.Commit = func(_ context.Context, record CheckpointMessageCommit) error {
					mu.Lock()
					defer mu.Unlock()
					for _, ack := range record.Inputs {
						if ack.Stream.Key == "input" {
							acknowledged = append(acknowledged, ack.Sequence)
						}
					}
					return nil
				}
				options = append(options, WithExecCheckpoint("root", hooks))
			}
			managedTestOK(t, g.Run(ctx, Inputs{}, options...))
			if got := source.Snapshot(); got.LastSequence != 2 || got.AckedThrough != 1 || got.InFlight != 0 || !got.Stopped {
				t.Fatalf("discarded input was confirmed: %+v", got)
			}
			if got := branch.Snapshot(); got.LastSequence != 1 || got.AckedThrough != 1 || !got.Stopped {
				t.Fatalf("branch accepted work after Stop: %+v", got)
			}
			if reliable && !reflect.DeepEqual(acknowledged, []uint64{1}) {
				t.Fatalf("fabricated reliable acknowledgement: %v", acknowledged)
			}
		})
	}
}

func TestTeeStopBranchFailureIsNotNormalStop(t *testing.T) {
	writer, stream, writerRC, readerRC := managedTestEndpoint[int](t, 1)
	managedTestOK(t, stream.Stop(readerRC))
	cause := errors.New("branch writer failed after Stop")
	writer.state.recordFailure(cause)
	if _, err := streamWriterChanges([]*StreamWriter[int]{writer}); !errors.Is(err, cause) {
		t.Fatalf("stopped branch failure became normal stop: %v", err)
	}
	if err := writerRC.finishStreams(nil); !errors.Is(err, cause) {
		t.Fatalf("branch chain swallowed failure: %v", err)
	}
}

// 分支变化与取消均可能发生在恢复额度时；已完成的 Receive 仍须交还交付，
// 不能为避免 Tee 多确认而把它伪报成“尚未取得”，也不能自动 Ack / Nack。
func TestTeeReceiveCompletionSurvivesCancelWhileResumingQuota(t *testing.T) {
	writer, stream, writerRC, readerRC := managedTestEndpoint[int](t, 1)
	pool := newLimiter(1)
	readerRC.lease = &runLease{pool: pool}
	managedTestOK(t, readerRC.lease.acquire(readerRC.Context()))
	defer readerRC.lease.release()
	witness := &managedResumeWitness{Context: readerRC.ctx, waiting: make(chan struct{}), resuming: make(chan struct{})}
	readerRC.ctx = witness
	changed := make(chan struct{})
	type result struct {
		delivery Delivery[int]
		err      error
	}
	outcome := make(chan result, 1)
	go func() {
		delivery, err := stream.receive(readerRC, []<-chan struct{}{changed})
		outcome <- result{delivery, err}
	}()
	select {
	case <-witness.waiting:
	case <-writerRC.Context().Done():
		t.Fatal(writerRC.Context().Err())
	}
	managedTestOK(t, pool.acquire(writerRC.Context()))
	var once sync.Once
	release := func() { once.Do(pool.release) }
	defer release()
	managedTestOK(t, writer.Send(9))
	select {
	case <-witness.resuming:
	case <-writerRC.Context().Done():
		t.Fatal(writerRC.Context().Err())
	}
	failure := errors.New("input failed after Tee obtained delivery")
	if err := writerRC.finishStreams(failure); err != failure {
		t.Fatal(err)
	}
	readerRC.Cancel()
	close(changed)
	select {
	case got := <-outcome:
		t.Fatalf("returned without quota: %+v", got)
	default:
	}
	release()
	select {
	case got := <-outcome:
		if got.err != nil || got.delivery.Value != 9 || !got.delivery.active() {
			t.Fatalf("completed Receive changed: %+v", got)
		}
		if s := stream.Snapshot(); s.Terminal != StreamFailed || s.Err != failure || s.AckedThrough != 0 || s.InFlight != 1 || len(stream.state.queue) != 0 {
			t.Fatalf("failure/cancel settled completed delivery: %+v", s)
		}
		managedTestOK(t, got.delivery.Ack())
	case <-writerRC.Context().Done():
		t.Fatal(writerRC.Context().Err())
	}
}

func teeStopWait(rc *RunCtx, signal <-chan struct{}) error {
	return rc.WaitStream(func(ctx context.Context) error {
		select {
		case <-signal:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

type teeReceiveWitness struct {
	context.Context
	input   Stream[int]
	entered chan struct{}
	once    sync.Once
}

func (w *teeReceiveWitness) Done() <-chan struct{} {
	if w.input.Snapshot().AckedThrough == 1 {
		w.once.Do(func() { close(w.entered) })
	}
	return w.Context.Done()
}

// 下游等到 Tee 已确认第一条并进入下一次 Receive 才 Stop；无须靠 sleep 猜窗口。
func TestTeeStopInterruptsPendingReceive(t *testing.T) {
	for _, next := range []bool{false, true} {
		t.Run(fmt.Sprintf("next=%t", next), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			g, err := New("tee-stop", "", WithMaxRunning(1))
			managedTestOK(t, err)
			in, out := NewKey[Stream[int]]("input"), NewKey[Stream[int]]("output")
			waiting, stopped := make(chan struct{}), make(chan struct{})
			var endpoint Stream[int]
			managedTestOK(t, ProduceStream(g, "source", "", in, func(rc *RunCtx, writer *StreamWriter[int]) error {
				var err error
				endpoint, err = Get(rc, in)
				if err != nil {
					return err
				}
				if err := writer.Send(1); err != nil {
					return err
				}
				if err := rc.WaitStream(func(ctx context.Context) error {
					select {
					case <-stopped:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}); err != nil {
					return err
				}
				if next {
					if err := writer.Send(2); err != nil && err != ErrStreamStopped {
						return err
					}
				}
				for {
					endpoint.state.mu.Lock()
					done, changed := endpoint.state.stopped, endpoint.state.changed
					endpoint.state.mu.Unlock()
					if done {
						return nil
					}
					if err := waitMessageCapacity(rc, changed); err != nil {
						return err
					}
				}
			}, WithBuffer(1)))
			managedTestOK(t, TeeStream(g, "tee", "", in, Keys(out), WithStreamAspects(func(rc *RunCtx, next func(*RunCtx) error) error {
				input, err := Get(rc, in)
				if err != nil {
					return err
				}
				rc.ctx = &teeReceiveWitness{Context: rc.ctx, input: input, entered: waiting}
				return next(rc)
			})))
			managedTestOK(t, g.Add(NewNode("sink", "", Requires(out), nil, func(rc *RunCtx) error {
				stream, err := Get(rc, out)
				if err != nil {
					return err
				}
				delivery, err := stream.Receive(rc)
				if err != nil {
					return err
				}
				if err := delivery.Ack(); err != nil {
					return err
				}
				if err := rc.WaitStream(func(ctx context.Context) error {
					select {
					case <-waiting:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}); err != nil {
					return err
				}
				if err := stream.Stop(rc); err != nil {
					return err
				}
				close(stopped)
				return nil
			})))
			err = g.Run(ctx, Inputs{})
			if err != nil || endpoint.Snapshot().AckedThrough != 1 || !endpoint.Snapshot().Stopped {
				t.Fatalf("all branch Stop failed to interrupt input: Run=%v input=%+v", err, endpoint.Snapshot())
			}
		})
	}
}

// 独立确认层控制，防止为修 Tee 而把普通零输出处理也拒绝。
func TestTeeStopPreservesExplicitZeroOutputHandling(t *testing.T) {
	writer, stream, writerRC, readerRC := managedTestEndpoint[int](t, 1)
	managedTestOK(t, writer.Send(7))
	delivery, err := stream.Receive(readerRC)
	managedTestOK(t, err)
	managedTestOK(t, HandleMessage(readerRC, delivery, func(*RunCtx, int) error { return NoValue() }))
	managedTestOK(t, writerRC.finishStreams(nil))
	if _, err := stream.Receive(readerRC); !errors.Is(err, io.EOF) {
		t.Fatalf("Receive=%v", err)
	}
	if got := stream.Snapshot(); got.AckedThrough != 1 {
		t.Fatalf("zero-output processing=%+v", got)
	}
}
