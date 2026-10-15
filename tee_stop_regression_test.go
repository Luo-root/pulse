package pulse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

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
