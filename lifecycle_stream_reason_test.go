package pulse

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestLifecycleStreamFailureReasonBeforePublication(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, panicking := range []bool{false, true} {
			for _, cause := range []error{errors.New("business failure"), context.Canceled, context.DeadlineExceeded} {
				t.Run(fmt.Sprintf("checkpoint=%t/panic=%t/%v", checkpoint, panicking, cause), func(t *testing.T) {
					ctx := checkpointTestContext(t)
					g := mustNew(t, ctx, "stream-reason")
					key := NewKey[Stream[int]]("messages")
					var stream Stream[int]
					mustAdd(t, g, NewNode("source", "业务和 panic 中的 context cause 不等于宿主取消", nil, Provides(key), func(rc *RunCtx) error {
						writer, err := OpenStream(rc, key, WithStreamSource("source", true))
						if err != nil {
							return err
						}
						stream = Stream[int]{state: writer.state}
						if panicking {
							return nil
						}
						return fmt.Errorf("independent RPC: %w", cause)
					}, func(rc *RunCtx, next func(*RunCtx) error) error {
						if err := next(rc); err != nil {
							return err
						}
						panic(cause)
					}))
					mustAdd(t, g, NewNode("sink", "流错误可能先被下游观察", Requires(key), nil, func(rc *RunCtx) error {
						s, err := Get(rc, key)
						if err != nil {
							return err
						}
						_, err = s.Receive(rc)
						return err
					}))
					probe := &checkpointProbe{}
					var finishes []CheckpointStreamFinish
					var opts []ExecOption
					if checkpoint {
						hooks := checkpointStreamTestHooks(probe)
						hooks.StreamHooks.Finish = func(_ context.Context, finish CheckpointStreamFinish) error {
							finishes = append(finishes, finish)
							return nil
						}
						opts = append(opts, WithExecCheckpoint("root", hooks))
					}
					e, err := g.NewExecution(ctx, Inputs{}, opts...)
					managedTestOK(t, err)
					defer e.Cancel()
					managedTestOK(t, e.Start())
					if err := e.Wait(); !errors.Is(err, cause) {
						t.Fatalf("original cause lost: %v", err)
					}
					if ctx.Err() != nil {
						t.Fatalf("host was canceled by the execution: %v", ctx.Err())
					}
					if snapshot := stream.Snapshot(); snapshot.Terminal != StreamFailed || !errors.Is(snapshot.Err, cause) {
						t.Errorf("stream failure was misclassified: %+v", snapshot)
					}
					if snapshot := e.Snapshot(); snapshot.Graph.Status != GraphStatusFailed || snapshot.Nodes[0].Status != NodeStatusFailed {
						t.Errorf("source/graph failure reason changed during propagation: %+v", snapshot)
					}
					if checkpoint && (len(finishes) != 1 || finishes[0].Terminal != StreamFailed) {
						t.Errorf("persistent stream failure was misclassified: %+v", finishes)
					}
				})
			}
		}
	}
}

func TestLifecycleStreamRealHostCancellation(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			ctx, cancel := context.WithCancel(checkpointTestContext(t))
			defer cancel()
			g := mustNew(t, ctx, "stream-real-cancel")
			key := NewKey[Stream[int]]("messages")
			var stream Stream[int]
			mustAdd(t, g, NewNode("source", "", nil, Provides(key), func(rc *RunCtx) error {
				writer, err := OpenStream(rc, key, WithStreamSource("source", true))
				if err != nil {
					return err
				}
				stream = Stream[int]{state: writer.state}
				cancel()
				return rc.Context().Err()
			}))
			mustAdd(t, g, NewNode("sink", "", Requires(key), nil, func(rc *RunCtx) error {
				s, err := Get(rc, key)
				if err != nil {
					return err
				}
				_, err = s.Receive(rc)
				return err
			}))
			var opts []ExecOption
			if checkpoint {
				opts = append(opts, WithExecCheckpoint("root", checkpointStreamTestHooks(&checkpointProbe{})))
			}
			if err := g.Run(ctx, Inputs{}, opts...); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if got := stream.Snapshot(); got.Terminal != StreamCanceled {
				t.Fatalf("real host cancellation became business failure: %+v", got)
			}
		})
	}
}
