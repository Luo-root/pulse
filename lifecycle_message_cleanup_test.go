package pulse

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

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
