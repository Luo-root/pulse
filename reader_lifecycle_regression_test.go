package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func readerLifecycleWait(rc *RunCtx, signal <-chan struct{}) error {
	return rc.WaitStream(func(ctx context.Context) error {
		select {
		case <-signal:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

// 让源在读取链已经完成业务短路之后才发布端点；Done 会合点也证明修复后的
// 收尾已真正进入等待。旧实现通过 Finished 会合点证明它漏记了读取者。
func TestReaderLifecyclePendingShortCircuit(t *testing.T) {
	for _, skip := range []bool{false, true} {
		for _, count := range []int{1, 2} {
			t.Run(fmt.Sprintf("skip=%t/messages=%d", skip, count), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				key := NewKey[Stream[int]]("input")
				arrival := make(chan struct{})
				witness := make(chan struct{})
				g, err := New("late-reader", "", WithMaxRunning(1), WithObserver(ObserverFunc{
					Finished: func(_ Site, id string, _ NodeFinishReason, _ error) {
						if id == "sink" {
							select {
							case <-witness:
							default:
								close(witness)
							}
						}
					},
				}))
				managedTestOK(t, err)
				var endpoint Stream[int]
				managedTestOK(t, g.Add(NewNode("source", "", nil, Provides(key), func(rc *RunCtx) error {
					if err := rc.WaitStream(func(ctx context.Context) error {
						select {
						case <-arrival:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					}); err != nil {
						return err
					}
					writer, err := OpenStream(rc, key, WithBuffer(1))
					if err != nil {
						return err
					}
					endpoint, err = Get(rc, key)
					if err != nil {
						return err
					}
					for i := 0; i < count; i++ {
						if err := writer.Send(i); err != nil {
							return err
						}
					}
					return nil
				})))
				managedTestOK(t, g.Add(NewNode("sink", "", Requires(key), nil, func(*RunCtx) error {
					return errors.New("short-circuited body ran")
				}, func(rc *RunCtx, _ func(*RunCtx) error) error {
					rc.ctx = &managedWaitWitness{Context: rc.ctx, entered: witness}
					if skip {
						return NoValue("aspect")
					}
					return nil
				})))
				e, err := g.NewExecution(ctx, Inputs{})
				managedTestOK(t, err)
				defer e.Cancel()
				managedTestOK(t, e.Start())
				select {
				case <-witness:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				close(arrival)
				err = e.Wait()
				if err == nil || !strings.Contains(err.Error(), "reading obligations") {
					t.Fatalf("late endpoint lost reader exit: Run=%v stream=%+v", err, endpoint.Snapshot())
				}
				if got := endpoint.Snapshot(); got.AckedThrough != 0 {
					t.Fatalf("exit invented acknowledgement: %+v", got)
				}
			})
		}
	}
}
