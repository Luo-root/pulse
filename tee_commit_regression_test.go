package pulse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

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
		if s := stream.Snapshot(); s.AckedThrough != 0 || s.InFlight != 1 || len(stream.state.queue) != 0 {
			t.Fatalf("cancel settled completed delivery: %+v", s)
		}
		managedTestOK(t, got.delivery.Ack())
	case <-writerRC.Context().Done():
		t.Fatal(writerRC.Context().Err())
	}
}
