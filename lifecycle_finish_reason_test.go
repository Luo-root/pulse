package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type lifecycleRPCCause struct{ error }

func (e *lifecycleRPCCause) Unwrap() error { return e.error }

func TestLifecycleFinishReasonUsesExecutionCancellation(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, shape := range []string{"rpc-deadline", "rpc-cancel", "host-cancel", "host-deadline", "execution-cancel", "node-cancel", "timeout"} {
			t.Run(fmt.Sprintf("checkpoint=%v/%s", checkpoint, shape), func(t *testing.T) {
				var host context.Context
				var cancel context.CancelFunc
				if shape == "host-deadline" {
					host, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
				} else {
					host, cancel = context.WithCancel(t.Context())
				}
				defer cancel()
				obs := &recordingObserver{}
				g := mustNew(t, host, "finish-reason", WithObserver(obs))
				cause := context.Canceled
				if shape == "rpc-deadline" || shape == "host-deadline" {
					cause = context.DeadlineExceeded
				}
				typed := &lifecycleRPCCause{cause}
				original := fmt.Errorf("independent RPC: %w", typed)
				var e *Execution
				var aspects []Aspect
				if shape == "timeout" {
					aspects = append(aspects, Timeout(time.Millisecond))
				}
				mustAdd(t, g, NewNode("n", "独立 RPC 错误与执行中断分开归因", nil, nil, func(rc *RunCtx) error {
					switch shape {
					case "host-cancel":
						cancel()
					case "execution-cancel":
						e.Cancel()
					case "node-cancel":
						rc.Cancel()
					}
					if shape == "host-deadline" || shape == "timeout" {
						<-rc.Context().Done()
					}
					return original
				}, aspects...))
				p := &checkpointProbe{}
				var opts []ExecOption
				if checkpoint {
					opts = append(opts, WithExecCheckpoint("root", p.hooks()))
				}
				var err error
				e, err = g.NewExecution(host, Inputs{}, opts...)
				if err != nil {
					t.Fatal(err)
				}
				defer e.Cancel()
				if err := e.Start(); err != nil {
					t.Fatal(err)
				}
				err = e.Wait()
				if shape == "timeout" {
					if err == nil || !strings.Contains(err.Error(), "timeout") {
						t.Fatalf("Timeout error changed: %v", err)
					}
				} else {
					var gotTyped *lifecycleRPCCause
					if !errors.Is(err, original) || !errors.Is(err, cause) || !errors.As(err, &gotTyped) || gotTyped != typed {
						t.Fatalf("original RPC cause lost: %v", err)
					}
				}
				want := NodeFailed
				if shape == "host-cancel" || shape == "host-deadline" || shape == "execution-cancel" {
					want = NodeCanceled
				}
				log := obs.snapshot()
				if countPref(log, "F:n:"+string(want)) != 1 || countPref(log, "GF:"+string(want)) != 1 {
					t.Errorf("host.Err=%v: want %s, callbacks=%v", host.Err(), want, log)
				}
				state := e.Snapshot()
				if state.Graph.Status != GraphStatus(want) || state.Nodes[0].Status != NodeStatus(want) {
					t.Errorf("want %s, graph=%s node=%s", want, state.Graph.Status, state.Nodes[0].Status)
				}
				if state.Graph.Err != err || state.Nodes[0].Err != err || e.Err() != err || e.Wait() != err {
					t.Error("node, graph and repeated Wait did not share the latched error")
				}
				if (shape == "rpc-cancel" || shape == "rpc-deadline" || shape == "node-cancel" || shape == "timeout") && host.Err() != nil {
					t.Fatal("node failure canceled the host")
				}
				if checkpoint {
					wantFailures := 1
					if want == NodeCanceled {
						wantFailures = 0
					}
					if len(p.failures) != wantFailures || len(p.commits) != 0 {
						t.Fatalf("checkpoint failures=%v commits=%d want failures=%d", p.failures, len(p.commits), wantFailures)
					}
				}
			})
		}
	}
}

func TestLifecycleRPCFailureKeepsOriginAcrossSub(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpoint), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			obs := &recordingObserver{}
			parent := mustNew(t, ctx, "parent", WithObserver(obs))
			child := mustNew(t, ctx, "child")
			cause := fmt.Errorf("child RPC: %w", context.DeadlineExceeded)
			input := NewKey[int]("input")
			declareIn(t, child, NewPort(input))
			mustAdd(t, child, NewNode("leaf", "子执行真实失败", Requires(input), nil, func(*RunCtx) error { return cause }))
			if err := Sub(parent, "mount", "失败来源跨挂载保留", []SubBind{Fix(input, 1)}, child); err != nil {
				t.Fatal(err)
			}
			p := &checkpointProbe{}
			var opts []ExecOption
			if checkpoint {
				opts = append(opts, WithExecCheckpoint("root", p.hooks()))
			}
			e, err := parent.NewExecution(ctx, Inputs{}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Cancel()
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			if err := e.Wait(); !errors.Is(err, cause) {
				t.Fatal(err)
			}
			state := e.Snapshot()
			childState := state.Nodes[0].Child.Snapshot()
			if state.Graph.Status != GraphStatusFailed || state.Nodes[0].Status != NodeStatusFailed || childState.Graph.Status != GraphStatusFailed || childState.Nodes[0].Status != NodeStatusFailed {
				t.Fatalf("origin changed across Sub: parent=%s/%s child=%s/%s", state.Graph.Status, state.Nodes[0].Status, childState.Graph.Status, childState.Nodes[0].Status)
			}
			if checkpoint && (len(p.failures) != 2 || len(p.commits) != 0) {
				t.Fatalf("Sub failure confirmation: failures=%v commits=%d", p.failures, len(p.commits))
			}
		})
	}
}

func TestLifecycleIndependentRPCFailureCancelsSibling(t *testing.T) {
	ctx := checkpointTestContext(t)
	obs := &recordingObserver{}
	g := mustNew(t, ctx, "rpc-sibling", WithObserver(obs))
	entered := make(chan struct{})
	cause := fmt.Errorf("independent RPC: %w", context.DeadlineExceeded)
	mustAdd(t, g, NewNode("rpc", "自身 RPC 失败", nil, nil, func(*RunCtx) error {
		<-entered
		return cause
	}))
	mustAdd(t, g, NewNode("sibling", "被首错取消", nil, nil, func(rc *RunCtx) error {
		close(entered)
		<-rc.Context().Done()
		return fmt.Errorf("sibling interrupted: %w", rc.Context().Err())
	}))
	e, err := g.NewExecution(ctx, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cancel()
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); !errors.Is(err, cause) {
		t.Fatal(err)
	}
	state := e.Snapshot()
	if state.Graph.Status != GraphStatusFailed || state.Nodes[0].Status != NodeStatusFailed || state.Nodes[1].Status != NodeStatusCanceled {
		t.Fatalf("wrong failure source: graph=%s rpc=%s sibling=%s callbacks=%v", state.Graph.Status, state.Nodes[0].Status, state.Nodes[1].Status, obs.snapshot())
	}
}
