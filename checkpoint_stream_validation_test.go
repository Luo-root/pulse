package pulse

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
)

func checkpointValidationSource(t *testing.T, g *Graph, stream Key[Stream[int]], ordinary Key[int], requires []keyRef, calls *atomic.Int32) {
	t.Helper()
	provides := Provides(stream)
	if ordinary.Name() != "" {
		provides = Deps(provides, Provides(ordinary))
	}
	mustAdd(t, g, NewNode("source", "手写流节点的普通值随完整链提交", requires, provides, func(rc *RunCtx) error {
		calls.Add(1)
		writer, err := OpenStream(rc, stream, WithBuffer(1), WithStreamSource("numbers", true))
		if err != nil {
			return err
		}
		if ordinary.Name() != "" {
			if err := Set(rc, ordinary, 7); err != nil {
				return err
			}
		}
		for value := 1; value <= 2; value++ {
			if err := writer.Send(value); err != nil {
				return err
			}
		}
		return nil
	}))
}

func checkpointValidationSink(t *testing.T, g *Graph, stream Key[Stream[int]], extra []keyRef, calls, received *atomic.Int32) {
	t.Helper()
	mustAdd(t, g, NewNode("sink", "读取并确认全部消息", Deps(Requires(stream), extra), nil, func(rc *RunCtx) error {
		calls.Add(1)
		endpoint, err := Get(rc, stream)
		if err != nil {
			return err
		}
		for {
			delivery, err := endpoint.Receive(rc)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			received.Add(1)
			if err := delivery.Ack(); err != nil {
				return err
			}
		}
	}))
}

func TestCheckpointStreamLifecycleRejectsOrdinaryOutputGate(t *testing.T) {
	for _, depth := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("depth-%d", depth), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			leaf := mustNew(t, ctx, "mixed-output", WithoutAspects())
			stream, ordinary := NewKey[Stream[int]]("stream"), NewKey[int]("ordinary")
			var sources, sinks, received atomic.Int32
			checkpointValidationSource(t, leaf, stream, ordinary, nil, &sources)
			checkpointValidationSink(t, leaf, stream, Requires(ordinary), &sinks, &received)
			graphs := []*Graph{leaf}
			root := leaf
			for level := 0; level < depth; level++ {
				if err := root.DeclareOutput(NewPort(ordinary)); err != nil {
					t.Fatal(err)
				}
				parent := mustNew(t, ctx, fmt.Sprintf("parent-%d", level))
				if err := Sub(parent, "child", "继承执行级 checkpoint", []SubBind{Out(ordinary, ordinary)}, root); err != nil {
					t.Fatal(err)
				}
				graphs = append(graphs, parent)
				root = parent
			}
			if problems := root.Topology().Problems; len(problems) != 0 {
				t.Fatalf("ordinary execution definition rejected: %v", problems)
			}
			probe := &checkpointProbe{}
			pass := func(rc *RunCtx, next func(*RunCtx) error) error { return next(rc) }
			e, err := root.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", checkpointStreamTestHooks(probe)), WithExecAspects(pass))
			if err != nil {
				t.Fatal(err)
			}
			defer e.Cancel()
			err = e.Start()
			if err == nil {
				waitErr := e.Wait()
				t.Fatalf("Start accepted checkpoint wait cycle: Wait=%v source_entries=%d consumer_entries=%d received=%d", waitErr, sources.Load(), sinks.Load(), received.Load())
			}
			for _, want := range []string{"stream lifecycle cycle", "source", "sink", `ordinary`, "complete-chain commit"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("diagnostic %q does not identify %q", err, want)
				}
			}
			t.Log(err)
			if !errors.Is(e.Wait(), ErrGraphNotStarted) || sources.Load() != 0 || sinks.Load() != 0 || len(probe.opens) != 0 {
				t.Fatal("rejected Start entered a body/hook or marked execution started")
			}
			// 失败校验不得封口，也不得把 checkpoint 事实写回共享定义。
			for _, g := range graphs {
				mustAdd(t, g, NewNode("after-rejection", "仍可装配", nil, nil, nil))
			}
			if problems := root.Topology().Problems; len(problems) != 0 {
				t.Fatalf("run facts contaminated Topology: %v", problems)
			}
			if err := root.Validate(); err != nil {
				t.Fatalf("run facts contaminated Validate: %v", err)
			}
			if err := root.Run(ctx, Inputs{}, WithExecAspects(pass)); err != nil {
				t.Fatalf("ordinary early Set no longer progresses: %v", err)
			}
			if sources.Load() != 1 || sinks.Load() != 1 || received.Load() != 2 {
				t.Fatalf("ordinary run source=%d sink=%d received=%d", sources.Load(), sinks.Load(), received.Load())
			}
		})
	}
}

func TestCheckpointStreamLifecycleAllowsLivePipelines(t *testing.T) {
	for _, shape := range []string{"pure-stream", "mixed-unrelated", "live-sub-out", "live-sub-with-late-unused-in", "live-sub-pass-through"} {
		t.Run(shape, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			root := mustNew(t, ctx, "live-pipeline", WithMaxRunning(1))
			stream, ordinary := NewKey[Stream[int]]("stream"), NewKey[int]("ordinary")
			var sources, sinks, received, followers atomic.Int32
			if shape == "pure-stream" {
				checkpointValidationSource(t, root, stream, Key[int]{}, nil, &sources)
			} else if shape == "live-sub-out" || shape == "live-sub-with-late-unused-in" {
				child := mustNew(t, ctx, "live-child", WithoutAspects())
				inside, value := NewKey[Stream[int]]("inside"), NewKey[int]("value")
				checkpointValidationSource(t, child, inside, value, nil, &sources)
				if err := child.DeclareOutput(NewPort(inside), NewPort(value)); err != nil {
					t.Fatal(err)
				}
				binds := []SubBind{Out(inside, stream), Out(value, ordinary)}
				if shape == "live-sub-with-late-unused-in" {
					late := NewKey[int]("late")
					if err := child.DeclareInput(NewPort(late)); err != nil {
						t.Fatal(err)
					}
					// follower 等普通 Out；未使用的 In 不能变成整个 Sub 的启动门。
					binds = append(binds, In(NewKey[int]("finished"), late))
				}
				if err := Sub(root, "child", "实时 Out 不等 child.Wait 或挂载 Commit", binds, child); err != nil {
					t.Fatal(err)
				}
			} else {
				checkpointValidationSource(t, root, stream, ordinary, nil, &sources)
				if shape == "live-sub-pass-through" {
					child := mustNew(t, ctx, "pass-child")
					port, returned := NewKey[Stream[int]]("port"), NewKey[Stream[int]]("returned")
					if err := child.DeclareInput(NewPort(port)); err != nil {
						t.Fatal(err)
					}
					if err := child.DeclareOutput(NewPort(port)); err != nil {
						t.Fatal(err)
					}
					if err := Sub(root, "pass", "实时透传不等待源节点完成", []SubBind{In(stream, port), Out(port, returned)}, child); err != nil {
						t.Fatal(err)
					}
					stream = returned
				}
			}
			checkpointValidationSink(t, root, stream, nil, &sinks, &received)
			if shape != "pure-stream" {
				finished := NewKey[int]("finished")
				mustAdd(t, root, NewNode("ordinary-follower", "普通值可在流读取者启动后等待提交", Requires(ordinary), Provides(finished), func(rc *RunCtx) error {
					value, err := Get(rc, ordinary)
					if err != nil {
						return err
					}
					if value != 7 {
						return fmt.Errorf("ordinary value = %d", value)
					}
					followers.Add(1)
					return Set(rc, finished, value)
				}))
			}
			if err := root.Validate(); err != nil {
				t.Fatal(err)
			}
			probe := &checkpointProbe{}
			if err := root.Run(ctx, Inputs{}, WithExecCheckpoint("root", checkpointStreamTestHooks(probe))); err != nil {
				t.Fatalf("legal checkpoint pipeline rejected: %v", err)
			}
			if sources.Load() != 1 || sinks.Load() != 1 || received.Load() != 2 || (shape != "pure-stream" && followers.Load() != 1) {
				t.Fatalf("incomplete pipeline: source=%d sink=%d received=%d follower=%d", sources.Load(), sinks.Load(), received.Load(), followers.Load())
			}
			sourceCommits := 0
			for _, commit := range probe.commits {
				if commit.Node.NodeID == "source" {
					sourceCommits++
					if (shape == "live-sub-out" || shape == "live-sub-with-late-unused-in") && commit.Site.Path != "child" {
						t.Fatalf("source checkpoint has wrong mount path: %+v", commit)
					}
				}
			}
			if sourceCommits != 1 {
				t.Fatalf("source did not inherit checkpoint environment: commits=%d", sourceCommits)
			}
		})
	}
}

func TestCheckpointStreamLifecycleAllowsDefiniteSkips(t *testing.T) {
	for _, mounted := range []bool{false, true} {
		t.Run(fmt.Sprint(mounted), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			leaf := mustNew(t, ctx, "skipped-mixed-output")
			trigger := NewKey[int]("trigger")
			stream, ordinary := NewKey[Stream[int]]("stream"), NewKey[int]("ordinary")
			if err := leaf.DeclareInput(NewPort(trigger)); err != nil {
				t.Fatal(err)
			}
			var sources, sinks, received atomic.Int32
			checkpointValidationSource(t, leaf, stream, ordinary, Requires(trigger), &sources)
			checkpointValidationSink(t, leaf, stream, Requires(ordinary), &sinks, &received)
			root := leaf
			var in Inputs
			if mounted {
				root = mustNew(t, ctx, "fixed-skip-parent")
				if err := Sub(root, "child", "FixSkip 是正常到达", []SubBind{FixSkip(trigger)}, leaf); err != nil {
					t.Fatal(err)
				}
			} else if err := SkipSeed(&in, trigger); err != nil {
				t.Fatal(err)
			}
			if err := root.Run(ctx, in, WithExecCheckpoint("root", checkpointStreamTestHooks(&checkpointProbe{}))); err != nil {
				t.Fatalf("definitely skipped sender must not require a reader start: %v", err)
			}
			if sources.Load() != 0 || sinks.Load() != 0 || received.Load() != 0 {
				t.Fatal("skipped bodies entered")
			}
		})
	}
}

func TestCheckpointStreamLifecycleKeepsOrdinarySubBoundary(t *testing.T) {
	ctx := checkpointTestContext(t)
	root, child := mustNew(t, ctx, "ordinary-boundary"), mustNew(t, ctx, "ordinary-child")
	inside, outside := NewKey[Stream[int]]("inside"), NewKey[Stream[int]]("outside")
	value, ordinary := NewKey[int]("value"), NewKey[int]("ordinary")
	var sources, sinks, received atomic.Int32
	checkpointValidationSource(t, child, inside, Key[int]{}, nil, &sources)
	mustAdd(t, child, NewNode("early-value", "独立普通值仍等整个 child.Wait", nil, Provides(value), func(rc *RunCtx) error { return Set(rc, value, 7) }))
	if err := child.DeclareOutput(NewPort(inside), NewPort(value)); err != nil {
		t.Fatal(err)
	}
	if err := Sub(root, "child", "普通 Out 保持真实完成边界", []SubBind{Out(inside, outside), Out(value, ordinary)}, child); err != nil {
		t.Fatal(err)
	}
	checkpointValidationSink(t, root, outside, Requires(ordinary), &sinks, &received)
	for _, reliable := range []bool{false, true} {
		var opts []ExecOption
		if reliable {
			opts = append(opts, WithExecCheckpoint("root", checkpointStreamTestHooks(&checkpointProbe{})))
		}
		e, err := root.NewExecution(ctx, Inputs{}, opts...)
		if err != nil {
			t.Fatal(err)
		}
		defer e.Cancel()
		if err := e.Start(); err == nil || !strings.Contains(err.Error(), "stream lifecycle cycle") {
			t.Fatalf("checkpoint=%t ordinary Sub boundary accepted: %v", reliable, err)
		}
	}
}
