package pulse

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidationPlanFixedSkipModes(t *testing.T) {
	for _, cut := range []bool{false, true} {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			root := streamTestGraph(t, "fixed-root")
			var opts []Option
			if cut {
				opts = append(opts, WithoutAspects())
			}
			child := streamTestGraph(t, "fixed-child", opts...)
			trigger, out := NewKey[int]("trigger"), NewKey[Stream[int]]("stream")
			streamTestMust(t, child.DeclareInput(NewPort(trigger)))
			streamTestMust(t, child.Add(NewNode("source", "", Requires(trigger), Provides(out), func(*RunCtx) error {
				t.Error("fixed skip entered body")
				return nil
			})))
			streamTestMust(t, Sub(root, "child", "", []SubBind{FixSkip(trigger)}, child))
			streamTestMust(t, root.Validate())
			plan := root.validation
			pass := func(rc *RunCtx, next func(*RunCtx) error) error { return next(rc) }
			for _, aspects := range []bool{false, true, false, true} {
				for _, checkpoint := range []bool{false, true} {
					var execOpts []ExecOption
					if aspects {
						execOpts = append(execOpts, WithExecAspects(pass))
					}
					if checkpoint {
						execOpts = append(execOpts, WithExecCheckpoint("root", checkpointStreamTestHooks(&checkpointProbe{})))
					}
					e, err := root.NewExecution(t.Context(), Inputs{}, execOpts...)
					streamTestMust(t, err)
					err = e.Start()
					if aspects && !cut {
						if err == nil || !strings.Contains(err.Error(), "nothing consumes it") {
							t.Fatalf("effective aspect lost: checkpoint=%t err=%v", checkpoint, err)
						}
					} else {
						streamTestMust(t, err)
						streamTestMust(t, e.Wait())
					}
					e.Cancel()
				}
			}
			if root.validation != plan || child.validation != nil {
				t.Fatal("execution recompiled root or detached child model")
			}
			for _, n := range plan.tree.nodes {
				if n.def.run != nil || n.def.child != nil {
					t.Fatal("compiled declarations retained business closure or child definition")
				}
			}
		})
	}
}

func TestValidationPlanCheckpointModeAndRestore(t *testing.T) {
	ctx := checkpointTestContext(t)
	leaf := streamTestGraph(t, "mixed-leaf")
	stream, value := NewKey[Stream[int]]("stream"), NewKey[int]("value")
	var sources, sinks, received atomic.Int32
	checkpointValidationSource(t, leaf, stream, value, nil, &sources)
	checkpointValidationSink(t, leaf, stream, Requires(value), &sinks, &received)
	streamTestMust(t, leaf.DeclareOutput(NewPort(value)))
	root := streamTestGraph(t, "mixed-root")
	streamTestMust(t, Sub(root, "child", "", []SubBind{Out(value, value)}, leaf))
	streamTestMust(t, root.Validate())
	for _, reliable := range []bool{false, true, false, true} {
		probe := &checkpointProbe{}
		var opts []ExecOption
		if reliable {
			opts = append(opts, WithExecCheckpoint("root", checkpointStreamTestHooks(probe)))
		}
		e, err := root.NewExecution(ctx, Inputs{}, opts...)
		streamTestMust(t, err)
		err = e.Start()
		if reliable {
			if err == nil || !strings.Contains(err.Error(), "stream lifecycle cycle") || !strings.Contains(err.Error(), `mounted at "child"`) {
				t.Fatalf("cached ordinary mode bypassed checkpoint publication: %v", err)
			}
			if e.started || len(probe.opens) != 0 || !errors.Is(e.Wait(), ErrGraphNotStarted) {
				t.Fatal("rejected checkpoint execution reached runtime")
			}
		} else {
			streamTestMust(t, err)
			streamTestMust(t, e.Wait())
		}
		e.Cancel()
	}
	if sources.Load() != 2 || sinks.Load() != 2 || received.Load() != 4 {
		t.Fatalf("mode switches changed business work: %d/%d/%d", sources.Load(), sinks.Load(), received.Load())
	}

	// 恢复计划先按开放定义接受，补来源后先由 Validate 冻结；缓存不能
	// 让旧计划在 Start 绕过 required-producer 的本轮复验。
	g := streamTestGraph(t, "restore-frozen")
	x, y := NewKey[int]("x"), NewKey[int]("y")
	streamTestMust(t, g.Add(NewNode("target", "", Requires(x), Provides(y), nil)))
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", (&checkpointProbe{}).hooks()))
	streamTestMust(t, err)
	defer e.Cancel()
	streamTestMust(t, e.RestoreNodes([]NodeCheckpoint{{NodeID: "target", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: CheckpointKey{Name: "y", Type: reflect.TypeFor[int]()}, State: SlotStateReady, Value: 2}}}}))
	streamTestMust(t, g.Add(NewNode("source", "", nil, Provides(x), nil)))
	streamTestMust(t, g.Validate())
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "required producer") {
		t.Fatalf("frozen validation bypassed restore recheck: %v", err)
	}
	if state, _ := e.slotOf(y.asRef()).snapshot(); state != slotPending || e.started {
		t.Fatal("rejected restore installed output or started execution")
	}
}

func TestValidationPlanFailureInstallsNothing(t *testing.T) {
	root, child := streamTestGraph(t, "open-root"), streamTestGraph(t, "open-child")
	fixed, input, missing := NewKey[int]("fixed"), NewKey[any]("input"), NewKey[int]("missing")
	streamTestMust(t, child.DeclareInput(NewPort(fixed)))
	streamTestMust(t, Sub(root, "child", "", []SubBind{Fix(fixed, 7)}, child))
	streamTestMust(t, root.DeclareInput(NewPort(input)))
	e, err := root.NewExecution(t.Context(), Inputs{})
	streamTestMust(t, err)
	defer e.Cancel()
	streamTestMust(t, root.Add(NewNode("late-input", "", Requires(input), nil, nil)))
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "has no value for this run") {
		t.Fatalf("late required inlet accepted: %v", err)
	}
	assertOpen := func() {
		t.Helper()
		for _, g := range []*Graph{root, child} {
			if g.sealed || g.validation != nil {
				t.Fatalf("failed check froze or installed partial model on %s", g.id)
			}
		}
	}
	assertOpen()
	var in Inputs
	streamTestMust(t, Seed(&in, input, any(make(chan int))))
	reliable, err := root.NewExecution(t.Context(), in, WithExecCheckpoint("root", (&checkpointProbe{}).hooks()))
	streamTestMust(t, err)
	defer reliable.Cancel()
	if err := reliable.Start(); err == nil || !strings.Contains(err.Error(), "channel") {
		t.Fatalf("checkpoint dynamic input accepted: %v", err)
	}
	assertOpen()
	streamTestMust(t, child.Add(NewNode("missing", "", Requires(missing), nil, nil)))
	if err := root.Validate(); err == nil {
		t.Fatal("invalid child accepted")
	}
	assertOpen()
	streamTestMust(t, child.Add(NewNode("repair", "", nil, Provides(missing), nil)))
	streamTestMust(t, root.Validate())
	if !root.sealed || !child.sealed || root.validation == nil {
		t.Fatal("repaired tree was not frozen")
	}
	if value := root.validation.tree.nodes[0].def.binds[0].fix.in.entries[fixed.Name()].value; value != nil {
		t.Fatal("compiled Fix retained business value")
	}
}

func TestValidationPlanMountPoolsRemainDistinct(t *testing.T) {
	child := streamTestGraph(t, "pooled-child", WithMaxRunning(2))
	optional, stream := NewKey[int]("optional"), NewKey[<-chan int]("stream")
	streamTestMust(t, child.DeclareInput(NewPort(optional)))
	streamTestProduce(t, child, "produce", stream, 1, 0)
	var received atomic.Int32
	streamTestMust(t, Consume(child, "consume", "", stream, func(*RunCtx, int) error { received.Add(1); return nil }))
	root := streamTestGraph(t, "pooled-root", WithMaxRunning(2))
	for _, mount := range []string{"left", "right"} {
		streamTestMust(t, Sub(root, mount, "", []SubBind{Fix(optional, 1)}, child))
	}
	streamTestMust(t, root.Validate())
	for _, capacity := range []int{2, 1, 0, 2} {
		streamTestMust(t, root.Run(t.Context(), Inputs{}, WithExecMaxRunning(capacity)))
	}
	if received.Load() != 8 || len(root.validation.tree.pools) != 3 {
		t.Fatalf("mounted pool identity lost: received=%d pools=%d", received.Load(), len(root.validation.tree.pools))
	}
}

func validationPlanOrdinarySub(tb testing.TB, size int) (*Graph, *Graph) {
	tb.Helper()
	root, err := New("ordinary-root", "")
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < size; i++ {
		if err := root.Add(NewNode(fmt.Sprint("n", i), "", nil, nil, func(*RunCtx) error { return nil })); err != nil {
			tb.Fatal(err)
		}
	}
	child, err := New("ordinary-child", "")
	if err != nil {
		tb.Fatal(err)
	}
	port := NewKey[int]("optional")
	if err := child.DeclareInput(NewPort(port)); err != nil {
		tb.Fatal(err)
	}
	if err := Sub(root, "child", "", []SubBind{Fix(port, 1)}, child); err != nil {
		tb.Fatal(err)
	}
	return root, child
}

func TestValidationPlanReuseWorkAndLocks(t *testing.T) {
	root, child := validationPlanOrdinarySub(t, 1000)
	streamTestMust(t, root.Validate())
	plan := root.validation
	streamTestMust(t, root.Validate()) // 固定模式求值一次，后续只读既有事实。
	if allocs := testing.AllocsPerRun(20, func() {
		if err := root.Validate(); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("repeated frozen Sub validation allocated %g times", allocs)
	}
	e, err := root.NewExecution(t.Context(), Inputs{})
	streamTestMust(t, err)
	defer e.Cancel()
	assemblyMu.Lock()
	child.mu.Lock()
	done := make(chan error, 2)
	go func() { done <- root.Validate(); done <- e.Start() }()
	var results []error
	timedOut := false
	for range 2 {
		select {
		case err := <-done:
			results = append(results, err)
		case <-time.After(3 * time.Second):
			timedOut = true
		}
		if timedOut {
			break
		}
	}
	child.mu.Unlock()
	assemblyMu.Unlock()
	if timedOut {
		t.Fatal("frozen Validate/Start waited for assembly or a descendant definition lock")
	}
	for _, err := range results {
		streamTestMust(t, err)
	}
	streamTestMust(t, e.Wait())
	if root.validation != plan || len(plan.tree.nodes) != 1001 || len(plan.tree.sites) != 2 {
		t.Fatal("repeated validation rebuilt or changed the mounted declaration model")
	}
}

func BenchmarkValidationPlanOrdinarySub(b *testing.B) {
	g, _ := validationPlanOrdinarySub(b, 1000)
	if err := g.Validate(); err != nil {
		b.Fatal(err)
	}
	b.Run("frozen", func(b *testing.B) {
		if err := g.Validate(); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		for b.Loop() {
			if err := g.Validate(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("rebuild-reference", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			g.mu.Lock()
			problems := g.problemsLocked(g.declaredCapacity())
			g.mu.Unlock()
			if len(problems) != 0 {
				b.Fatal(problems)
			}
		}
	})
}
