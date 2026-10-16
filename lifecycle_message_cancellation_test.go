package pulse

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

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
