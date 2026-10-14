package pulse

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestCheckpointDeclarationViewsAndInputAuthority(t *testing.T) {
	ctx := checkpointTestContext(t)
	child, parent := mustNew(t, ctx, "view-child"), mustNew(t, ctx, "view-parent")
	in, out := NewKey[int]("in"), NewKey[string]("out")
	declareIn(t, child, NewPort(in))
	declareOut(t, child, NewPort(out))
	mustAdd(t, child, NewNode("convert", "真实类型", Requires(in), Provides(out), nil))
	a, b := NewKey[string]("a"), NewKey[string]("b")
	if err := Sub(parent, "first", "第一次挂载", []SubBind{Fix(in, 1), Out(out, a)}, child); err != nil {
		t.Fatal(err)
	}
	if err := Sub(parent, "second", "第二次挂载", []SubBind{Fix(in, 2), Out(out, b)}, child); err != nil {
		t.Fatal(err)
	}
	nodes := parent.CheckpointNodes()
	if len(nodes) != 4 || nodes[1].Path != "first" || nodes[3].Path != "second" || nodes[1].Requires[0].Type != reflect.TypeFor[int]() || nodes[1].Provides[0].Type != reflect.TypeFor[string]() {
		t.Fatalf("node view: %+v", nodes)
	}
	nodes[1].Provides[0].Name = "changed"
	if parent.CheckpointNodes()[1].Provides[0].Name != "out" {
		t.Fatal("node declaration aliases definition")
	}
	g := mustNew(t, ctx, "input-view")
	x, y := NewKey[int]("external"), NewKey[int]("internal")
	declareIn(t, g, NewPort(x))
	mustAdd(t, g, NewNode("n", "内部值不出现在输入视图", Requires(x), Provides(y), func(rc *RunCtx) error { return Set(rc, y, 8) }))
	var values Inputs
	if err := Seed(&values, x, 3); err != nil {
		t.Fatal(err)
	}
	var events atomic.Int32
	e, err := g.NewExecution(ctx, values, WithExecObserver(ObserverFunc{GraphStarted: func(Site) { events.Add(1) }}))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cancel()
	before := len(e.slots)
	inputs := e.CheckpointInputs()
	if len(inputs) != 1 || inputs[0].Key.Name != "external" || inputs[0].Key.Type != reflect.TypeFor[int]() || inputs[0].State != SlotStateReady || inputs[0].Value != 3 {
		t.Fatal(inputs)
	}
	inputs[0].Key.Name = "internal"
	if e.CheckpointInputs()[0].Key.Name != "external" || len(e.slots) != before || events.Load() != 0 {
		t.Fatal("input inspection mutated runtime")
	}
}

func TestCheckpointRejectsKnownDynamicInputBeforeBusiness(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "dynamic-input")
	input := NewKey[any]("input")
	declareIn(t, g, NewPort(input))
	mustAdd(t, g, NewNode("n", "业务前拒动态channel", Requires(input), nil, func(*RunCtx) error { t.Error("business ran"); return nil }))
	var in Inputs
	if err := Seed[any](&in, input, make(chan int)); err != nil {
		t.Fatal(err)
	}
	p := &checkpointProbe{}
	e, err := g.NewExecution(ctx, in, WithExecCheckpoint("root", p.hooks()))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cancel()
	if err := e.Start(); err == nil {
		t.Fatal("dynamic input channel accepted")
	}
	if g.sealed || e.started || len(p.opens) != 0 {
		t.Fatal("rejected input had startup effects")
	}
}

func TestCheckpointAllowsOrdinaryAspectsAndShortCircuit(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "short-circuit")
	k := NewKey[int]("k")
	short := func(rc *RunCtx, _ func(*RunCtx) error) error { return Set(rc, k, 9) }
	mustAdd(t, g, NewNode("n", "正常短路仍提交", nil, Provides(k), func(*RunCtx) error { t.Error("short-circuited body ran"); return nil }, short))
	p := &checkpointProbe{}
	if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks())); err != nil {
		t.Fatal(err)
	}
	if len(p.commits) != 1 || p.commits[0].Node.Outputs[0].Value != 9 {
		t.Fatal("ordinary aspect was rejected or lost output")
	}
}

func TestCheckpointRetryCannotReenterSameBoundary(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "duplicate-boundary")
	var ran int
	retry := Retry(2, 0)
	composite := func(rc *RunCtx, next func(*RunCtx) error) error {
		if err := retry(rc, next); err != nil {
			return err
		}
		return retry(rc, next)
	}
	mustAdd(t, g, NewNode("n", "无新父边界不能重入", nil, nil, func(*RunCtx) error { ran++; return nil }, composite))
	p := &checkpointProbe{}
	if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks())); err == nil {
		t.Fatal("same persistent Retry entry was reopened")
	}
	if ran != 1 || len(p.commits) != 0 {
		t.Fatalf("body=%d commits=%d", ran, len(p.commits))
	}
}

func TestCheckpointHookCancellationIsFailure(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "hook-context-error")
	mustAdd(t, g, NewNode("n", "后端取消不是宿主中断", nil, nil, nil))
	p := &checkpointProbe{}
	hooks := p.hooks()
	hooks.Commit = func(context.Context, CheckpointCommit) error { return context.Canceled }
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); err == nil || e.Snapshot().Graph.Status != GraphStatusFailed {
		t.Fatalf("backend failure misclassified: %v", err)
	}
}
