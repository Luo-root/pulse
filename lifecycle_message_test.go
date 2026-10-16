package pulse

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
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
