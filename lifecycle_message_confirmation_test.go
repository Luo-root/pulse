package pulse

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

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
