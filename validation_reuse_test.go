package pulse

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlainValidationReuseKeepsEachRunsInputsAndSkip(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	g, err := New("plain-validation-reuse", "封口只复用静态事实")
	streamTestMust(t, err)
	input, optional, output := NewKey[int]("input"), NewKey[int]("optional"), NewKey[int]("output")
	streamTestMust(t, g.DeclareInput(NewPort(input), NewPort(optional)))
	streamTestMust(t, g.DeclareOutput(NewPort(output)))
	var ran atomic.Int32
	streamTestMust(t, g.Add(NewNode("double", "", Requires(input), Provides(output), func(rc *RunCtx) error {
		ran.Add(1)
		value, err := Get(rc, input)
		if err != nil {
			return err
		}
		return Set(rc, output, value*2)
	})))
	streamTestMust(t, g.Validate())
	if _, err := g.NewExecution(ctx, Inputs{}); err == nil || !strings.Contains(err.Error(), "has no value for this run") {
		t.Fatalf("sealed graph forgot the next run's required input: %v", err)
	}
	for i, value := range []int{7, 0, 11} {
		var in Inputs
		if i == 1 {
			streamTestMust(t, SkipSeed(&in, input))
		} else {
			streamTestMust(t, Seed(&in, input, value))
		}
		// 未被读的optional始终不必提供；普通图改变本轮额度仍合法。
		e, err := g.NewExecution(ctx, in, WithExecMaxRunning(i))
		streamTestMust(t, err)
		streamTestMust(t, e.Start())
		streamTestMust(t, e.Wait())
		got, state, err := ReadOutput(e, output)
		streamTestMust(t, err)
		if i == 1 {
			if state != SlotStateSkipped {
				t.Fatalf("normal skip became %s", state)
			}
		} else if state != SlotStateReady || got != value*2 {
			t.Fatalf("run %d: (%d,%s), want (%d,ready)", i, got, state, value*2)
		}
		streamTestMust(t, g.Validate())
	}
	if ran.Load() != 2 {
		t.Fatalf("body executions=%d, want 2", ran.Load())
	}
}

// 不能把“已封口”扩大成“任何图都不必重新判本轮额度”。
func TestSealedValidationStillChecksNativeQuota(t *testing.T) {
	for _, shape := range []string{"direct", "child", "nested-child"} {
		t.Run(shape, func(t *testing.T) {
			var g *Graph
			if shape == "direct" {
				g = streamTestGraph(t, "direct-sealed-stream", WithMaxRunning(4))
				source, left, right := NewKey[<-chan int]("source"), NewKey[<-chan int]("left"), NewKey[<-chan int]("right")
				streamTestProduce(t, g, "source", source, 2, 0)
				streamTestMust(t, Tee(g, "tee", "", source, Keys(left, right)))
				values := &streamTestValues{}
				streamTestConsume(t, g, "left", left, values, "left")
				streamTestConsume(t, g, "right", right, values, "right")
			} else {
				g, _ = streamTestBroadcastSub(t, 4, nil, shape == "nested-child", 0, 2)
			}
			streamTestMust(t, g.Validate())
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			e, err := g.NewExecution(ctx, Inputs{}, WithExecMaxRunning(3))
			streamTestMust(t, err)
			defer e.Cancel()
			if err := e.Start(); err == nil || !strings.Contains(err.Error(), "needs 4 slots") {
				t.Fatalf("sealed stream ignored this run's quota: %v", err)
			}
			if !errors.Is(e.Wait(), ErrGraphNotStarted) {
				t.Fatal("rejected execution was marked started")
			}
			streamTestMust(t, g.Run(ctx, Inputs{}, WithExecMaxRunning(4)))
		})
	}
}

func TestSealedValidationStillRequiresManagedHostReader(t *testing.T) {
	g := streamTestGraph(t, "sealed-host-output")
	out := NewKey[Stream[int]]("messages")
	var ran atomic.Bool
	streamTestMust(t, g.Add(NewNode("source", "", nil, Provides(out), func(*RunCtx) error {
		ran.Store(true)
		return nil
	})))
	streamTestMust(t, g.DeclareOutput(NewPort(out)))
	// 定义允许宿主稍后订阅；封口不是登记了本轮真实读者。
	streamTestMust(t, g.Validate())
	e, err := g.NewExecution(t.Context(), Inputs{})
	streamTestMust(t, err)
	defer e.Cancel()
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "nothing consumes it") {
		t.Fatalf("sealed definition invented a host reader: %v", err)
	}
	if ran.Load() || !errors.Is(e.Wait(), ErrGraphNotStarted) {
		t.Fatal("missing host reader reached business")
	}
}

func TestPlainValidationReuseStillChecksCheckpointInputs(t *testing.T) {
	g := streamTestGraph(t, "sealed-checkpoint-input")
	input := NewKey[any]("input")
	streamTestMust(t, g.DeclareInput(NewPort(input)))
	var ran atomic.Int32
	streamTestMust(t, g.Add(NewNode("read", "", Requires(input), nil, func(rc *RunCtx) error {
		ran.Add(1)
		_, err := Get(rc, input)
		return err
	})))
	streamTestMust(t, g.Validate())
	var in Inputs
	streamTestMust(t, Seed(&in, input, any(make(chan int))))
	probe := &checkpointProbe{}
	e, err := g.NewExecution(t.Context(), in, WithExecCheckpoint("root", probe.hooks()))
	streamTestMust(t, err)
	defer e.Cancel()
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "channel") {
		t.Fatalf("plain static reuse bypassed dynamic checkpoint checks: %v", err)
	}
	if ran.Load() != 0 || len(probe.opens) != 0 || !errors.Is(e.Wait(), ErrGraphNotStarted) {
		t.Fatal("rejected checkpoint input started attempts or business")
	}
	// 相同普通值在非checkpoint模式仍合法，不能把可靠存储限制扩大到本地执行。
	streamTestMust(t, g.Run(t.Context(), in))
	if ran.Load() != 1 {
		t.Fatalf("ordinary execution body count=%d, want 1", ran.Load())
	}
}

func TestRequiredInputsWithoutPortsDoesNotHideStaticErrors(t *testing.T) {
	g := streamTestGraph(t, "no-inlet-unsourced")
	missing := NewKey[int]("missing")
	streamTestMust(t, g.Add(NewNode("waiter", "", Requires(missing), nil, nil)))
	e, err := g.NewExecution(t.Context(), Inputs{})
	streamTestMust(t, err)
	defer e.Cancel()
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "nothing provides it") {
		t.Fatalf("no-inlet shortcut hid static missing source: %v", err)
	}
	if g.sealed || !errors.Is(e.Wait(), ErrGraphNotStarted) {
		t.Fatal("invalid graph was sealed or started")
	}
	streamTestMust(t, g.Add(NewNode("source", "", nil, Provides(missing), func(rc *RunCtx) error {
		return Set(rc, missing, 1)
	})))
	streamTestMust(t, g.Run(context.Background(), Inputs{}))
}
