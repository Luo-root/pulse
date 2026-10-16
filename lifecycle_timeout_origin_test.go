package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLifecycleTimeoutKeepsDeadlineOrigin(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, shape := range []string{"host-deadline", "host-cancel", "node-timer", "nested-timer"} {
			t.Run(fmt.Sprintf("checkpoint=%t/%s", checkpoint, shape), func(t *testing.T) {
				var host context.Context
				var cancel context.CancelFunc
				if shape == "host-deadline" {
					host, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
				} else {
					host, cancel = context.WithCancel(checkpointTestContext(t))
				}
				defer cancel()
				obs := &recordingObserver{}
				g := mustNew(t, host, "timeout-origin", WithObserver(obs))
				aspects := []Aspect{Timeout(time.Hour)}
				if shape == "node-timer" {
					aspects = []Aspect{Timeout(time.Millisecond)}
				} else if shape == "nested-timer" {
					aspects = append([]Aspect{Timeout(time.Millisecond)}, aspects...)
				}
				mustAdd(t, g, NewNode("n", "父域截止不是本层一小时预算耗尽", nil, nil, func(rc *RunCtx) error {
					if shape == "host-cancel" {
						cancel()
					}
					<-rc.Context().Done()
					return rc.Context().Err()
				}, aspects...))
				p := &checkpointProbe{}
				var opts []ExecOption
				if checkpoint {
					opts = append(opts, WithExecCheckpoint("root", p.hooks()))
				}
				e, err := g.NewExecution(host, Inputs{}, opts...)
				managedTestOK(t, err)
				defer e.Cancel()
				managedTestOK(t, e.Start())
				err = e.Wait()
				want := NodeFailed
				if shape == "host-deadline" || shape == "host-cancel" {
					want = NodeCanceled
					cause := context.Canceled
					if shape == "host-deadline" {
						cause = context.DeadlineExceeded
					}
					if !errors.Is(err, cause) {
						t.Errorf("parent cancellation cause lost: %v", err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "timeout after 1ms") || host.Err() != nil {
					t.Errorf("own timer attributed to parent/wrong Timeout: err=%v host=%v", err, host.Err())
				}
				state := e.Snapshot()
				if state.Graph.Status != GraphStatus(want) || state.Nodes[0].Status != NodeStatus(want) {
					t.Errorf("want %s, graph=%s node=%s", want, state.Graph.Status, state.Nodes[0].Status)
				}
				if log := obs.snapshot(); countPref(log, "F:n:"+string(want)) != 1 || countPref(log, "GF:"+string(want)) != 1 {
					t.Errorf("wrong observer origin: %v", log)
				}
				if checkpoint {
					wantFailures := 1
					if want == NodeCanceled {
						wantFailures = 0
					}
					if len(p.failures) != wantFailures || len(p.commits) != 0 {
						t.Errorf("failure budget changed: failures=%v commits=%d", p.failures, len(p.commits))
					}
				}
			})
		}
	}
}

func TestLifecycleTimeoutPreservesPanicAfterLocalCancel(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, localCancel := range []bool{false, true} {
			for _, timeout := range []bool{false, true} {
				t.Run(fmt.Sprintf("checkpoint=%t/cancel=%t/timeout=%t", checkpoint, localCancel, timeout), func(t *testing.T) {
					ctx := checkpointTestContext(t)
					obs := &recordingObserver{}
					g := mustNew(t, ctx, "timeout-panic-origin", WithObserver(obs))
					cause := &lifecyclePanicCause{errors.New("panic after local cleanup")}
					var aspects []Aspect
					if timeout {
						aspects = append(aspects, Timeout(time.Second))
					}
					aspects = append(aspects, func(rc *RunCtx, next func(*RunCtx) error) error {
						if err := next(rc); err != nil {
							return err
						}
						panic(cause)
					})
					mustAdd(t, g, NewNode("n", "已捕获 panic 不能被本层清理覆盖", nil, nil, func(rc *RunCtx) error {
						if localCancel {
							rc.Cancel()
						}
						return nil
					}, aspects...))
					p := &checkpointProbe{}
					var opts []ExecOption
					if checkpoint {
						opts = append(opts, WithExecCheckpoint("root", p.hooks()))
					}
					e, err := g.NewExecution(ctx, Inputs{}, opts...)
					managedTestOK(t, err)
					defer e.Cancel()
					managedTestOK(t, e.Start())
					err = e.Wait()
					var typed *lifecyclePanicCause
					if !errors.Is(err, cause) || !errors.As(err, &typed) || typed != cause {
						t.Errorf("Timeout replaced the captured panic: %v", err)
					}
					state := e.Snapshot()
					if ctx.Err() != nil || state.Graph.Status != GraphStatusFailed || state.Nodes[0].Status != NodeStatusFailed || !errors.Is(state.Nodes[0].Err, cause) {
						t.Errorf("panic origin lost: host=%v graph=%+v node=%+v", ctx.Err(), state.Graph, state.Nodes[0])
					}
					if log := obs.snapshot(); countPref(log, "F:n:failed") != 1 || countPref(log, "GF:failed") != 1 {
						t.Errorf("wrong panic callbacks: %v", log)
					}
					if checkpoint && (len(p.failures) != 1 || len(p.commits) != 0) {
						t.Errorf("panic bypassed confirmation: failures=%v commits=%d", p.failures, len(p.commits))
					}
				})
			}
		}
	}
}

func TestLifecycleTimeoutKeepsOwnTimerDuringParentCleanup(t *testing.T) {
	_, _, rc, _ := managedTestEndpoint[int](t, 1)
	err := Timeout(time.Millisecond)(rc, func(inner *RunCtx) error {
		<-inner.Context().Done() // 本层计时器先触发，之后父层才被清理取消。
		rc.Cancel()
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "timeout after 1ms") || finishReason(err) != NodeFailed {
		t.Fatalf("later parent cleanup erased the first timer: %v", err)
	}
}

func TestLifecycleTimeoutPreservesReliableFailureAfterCancel(t *testing.T) {
	_, _, rc, _ := managedTestEndpoint[int](t, 1)
	cause := errors.New("reliable confirmation failed")
	failure := &CheckpointError{Operation: "inner confirmation", Err: cause}
	err := Timeout(time.Hour)(rc, func(inner *RunCtx) error {
		inner.Cancel()
		return failure
	})
	var typed *CheckpointError
	if !errors.Is(err, cause) || !errors.As(err, &typed) || typed != failure || finishReason(err) != NodeFailed {
		t.Fatalf("Timeout discarded reliable failure: %v", err)
	}
}
