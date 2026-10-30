package pulse

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
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
			for _, d := range []*validationDefinition{plan.definition, child.validationDefinition} {
				for _, n := range d.nodes {
					if n.def.run != nil || n.def.child != nil {
						t.Fatal("compiled declarations retained business closure or child graph")
					}
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
			if g.sealed || g.validation != nil || g.validationDefinition != nil {
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
	if value := root.validation.definition.nodes[0].def.binds[0].fix.in.entries[fixed.Name()].value; value != nil {
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
	facts := root.validation.modes[0]
	if received.Load() != 8 || len(facts.tree.pools) != 3 {
		t.Fatalf("mounted pool identity lost: received=%d pools=%d", received.Load(), len(facts.tree.pools))
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
	if root.validation != plan || len(plan.definition.nodes) != 1001 ||
		plan.definition.nodes[1000].child != child.validationDefinition || plan.modes[0].tree != nil {
		t.Fatal("repeated validation rebuilt or retained the expanded ordinary model")
	}
}

func TestValidationPlanSharedDefinitionsAcrossRoots(t *testing.T) {
	port := NewKey[int]("optional")
	leaf := streamTestGraph(t, "shared-leaf")
	streamTestMust(t, leaf.DeclareInput(NewPort(port)))
	streamTestMust(t, leaf.Add(NewNode("body", "", nil, nil, func(*RunCtx) error { return nil })))
	streamTestMust(t, leaf.Validate())
	graphs := []*Graph{leaf}
	for i := range 32 {
		parent := streamTestGraph(t, fmt.Sprint("shared-", i))
		streamTestMust(t, parent.DeclareInput(NewPort(port)))
		streamTestMust(t, Sub(parent, "mount", "", []SubBind{In(port, port)}, graphs[len(graphs)-1]))
		streamTestMust(t, parent.Validate())
		graphs = append(graphs, parent)
	}
	for i, g := range graphs {
		streamTestMust(t, g.Validate())
		if g.validationDefinition == nil || len(g.validationDefinition.nodes) != 1 {
			t.Fatal("root retained declarations outside its own layer")
		}
		if i != 0 && (g.validation.definition != g.validationDefinition ||
			g.validation.definition.nodes[0].child != graphs[i-1].validationDefinition) {
			t.Fatal("independently validated root copied its child declarations")
		}
		for _, f := range g.validation.modes {
			if f != nil && (f.tree != nil || f.lifecycle != nil) {
				t.Fatal("ordinary root retained expanded work instead of its lifecycle result")
			}
		}
	}
}

func TestValidationPlanPlainRootAvoidsUnusedDeclarations(t *testing.T) {
	g := streamTestGraph(t, "plain-root")
	for i := range 128 {
		key := NewKey[int](fmt.Sprint("value", i))
		streamTestMust(t, g.Add(NewNode(fmt.Sprint("source", i), "", nil, Provides(key), nil)))
	}
	streamTestMust(t, g.Validate())
	if g.validationDefinition != nil || g.validation.definition != nil {
		t.Fatal("standalone ordinary validation retained an unused declaration tree")
	}
	assemblyMu.Lock()
	done := make(chan error, 1)
	go func() { done <- g.Validate() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(3 * time.Second):
		assemblyMu.Unlock()
		<-done
		t.Fatal("ordinary frozen validation waited for assembly")
	}
	assemblyMu.Unlock()
	streamTestMust(t, err)
}

func TestValidationPlanSealedChildFirstRootUsesDeclarations(t *testing.T) {
	middle, leaf := validationPlanOrdinarySub(t, 2)
	root := streamTestGraph(t, "outer")
	port := NewKey[int]("optional")
	streamTestMust(t, middle.DeclareInput(NewPort(port)))
	streamTestMust(t, Sub(root, "middle", "", []SubBind{Fix(port, 1)}, middle))
	streamTestMust(t, root.Validate())
	if middle.validation != nil || leaf.validation != nil {
		t.Fatal("mounting eagerly installed an independent root plan")
	}
	assemblyMu.Lock()
	leaf.mu.Lock()
	done := make(chan error, 1)
	go func() { done <- middle.Validate() }()
	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-time.After(3 * time.Second):
		timedOut = true
	}
	leaf.mu.Unlock()
	assemblyMu.Unlock()
	if timedOut {
		<-done
		t.Fatal("first independent root validation locked assembly or descendants")
	}
	streamTestMust(t, err)
	if middle.validation.definition != root.validation.definition.nodes[0].child || middle.validation.pending {
		t.Fatal("detached root did not publish the already frozen declarations")
	}
}

// 同一组封口声明分别在无流读者、双挂载、切面继承/切断、checkpoint
// 完整链与普通值发布混合下复验，逐字对照仍按层加锁的原判据。
func validationPlanComparisonGraph(t *testing.T, native, cut, own, gated, consume bool) *Graph {
	t.Helper()
	var opts []Option
	if cut {
		opts = append(opts, WithoutAspects())
	}
	if own {
		opts = append(opts, WithAspects(func(rc *RunCtx, next func(*RunCtx) error) error { return next(rc) }))
	}
	child := streamTestGraph(t, "comparison-child", opts...)
	trigger, value := NewKey[int]("trigger"), NewKey[int]("value")
	streamTestMust(t, child.DeclareInput(NewPort(trigger)))
	streamTestMust(t, child.DeclareOutput(NewPort(value)))
	var stream keyRef
	if native {
		stream = NewKey[<-chan int]("stream").asRef()
	} else {
		stream = NewKey[Stream[int]]("stream").asRef()
	}
	source := NewNode("source", "", Requires(trigger), []keyRef{stream, value.asRef()}, nil)
	if native {
		source.streamKind = "produce"
	}
	streamTestMust(t, child.Add(source))
	if consume {
		requires := []keyRef{stream}
		if gated {
			requires = append(requires, value.asRef())
		}
		sink := NewNode("sink", "", requires, nil, nil)
		sink.streamKind = "consume"
		streamTestMust(t, child.Add(sink))
	} else {
		streamTestMust(t, child.DeclareOutput(PortRef(stream)))
	}
	root := streamTestGraph(t, "comparison-root")
	streamTestMust(t, root.DeclareInput(NewPort(trigger)))
	for _, mount := range []string{"left", "right"} {
		binds := []SubBind{In(trigger, trigger), Out(value, NewKey[int](mount+"-value"))}
		if !consume {
			out := stream
			out.name = mount
			binds = append(binds, OutRef(stream, out))
			streamTestMust(t, root.DeclareOutput(PortRef(out)))
		}
		streamTestMust(t, Sub(root, mount, "", binds, child))
	}
	streamTestMust(t, root.Validate())
	return root
}

func validationPlanReference(g *Graph, capacity int, known streamKnownInputs, definition bool) error {
	// 直接从 Graph 按原挂载顺序复制，不走共享声明或模式缓存。
	tree := &streamTree{}
	pool := &streamPool{capacity: capacity}
	tree.pools = append(tree.pools, pool)
	root := tree.collectLocked(g, "", mountChain{g}, pool, nil, known.hasAspects)
	if definition {
		for _, ref := range g.outPortOrder {
			if isManagedStreamType(ref.typ) {
				tree.key(root, ref)
			}
		}
	}
	tree.propagateSkips(known.skipped)
	tree.connectAliases()
	if definition {
		for _, ref := range g.outPortOrder {
			if isManagedStreamType(ref.typ) {
				root.keys[ref.name].group.outlet = true
			}
		}
	}
	tree.collectHostReaders(known.hostReads)
	tree.collectReaders()
	streams := make(map[string][]error)
	tree.problemsSlots(streams)
	tree.problemsReaders(streams)
	tree.problemsLifecycle(streams, known.checkpoint)
	if problems := g.problemsWithStreamsAtLocked("", mountChain{g}, capacity, streams); len(problems) != 0 {
		return problems[0]
	}
	return nil
}

func TestValidationPlanSharedModesMatchReference(t *testing.T) {
	cases := 0
	for _, native := range []bool{false, true} {
		for _, cut := range []bool{false, true} {
			for _, own := range []bool{false, true} {
				for _, gated := range []bool{false, true} {
					for _, consume := range []bool{false, true} {
						if native && !consume {
							continue
						}
						g := validationPlanComparisonGraph(t, native, cut, own, gated, consume)
						if g.validation.definition.nodes[0].child != g.validation.definition.nodes[1].child {
							t.Fatal("two mounts copied their shared child declarations")
						}
						for _, capacity := range []int{0, 1, 4} {
							for mode := range 4 {
								for _, skip := range []map[string]bool{nil, {"trigger": true}, {"trigger": false}} {
									for _, host := range []map[string]bool{nil, {"left": true}, {"left": true, "right": true}} {
										known := streamKnownInputs{skipped: skip, hasAspects: mode&1 != 0, checkpoint: mode&2 != 0, hostReads: host}
										for _, definition := range []bool{false, true} {
											// Host 事实只由 Start 使用，定义检查没有这个组合。
											if definition && len(host) != 0 {
												continue
											}
											want := validationPlanReference(g, capacity, known, definition)
											got := g.validation.check(capacity, known, definition)
											if fmt.Sprint(got) != fmt.Sprint(want) {
												t.Fatalf("native=%t cut=%t own=%t gated=%t consume=%t capacity=%d mode=%d skip=%v host=%v definition=%t\ngot %v\nwant %v", native, cut, own, gated, consume, capacity, mode, skip, host, definition, got, want)
											}
											cases++
										}
									}
								}
							}
						}
					}
				}
			}
		}
	}
	t.Logf("%d first-error strings matched the uncached per-layer reference", cases)
}

func TestValidationPlanSealedChildFailureDiscardsCandidate(t *testing.T) {
	root := validationPlanComparisonGraph(t, false, false, false, false, false)
	child := root.nodes[0].child
	declaration := child.validationDefinition
	var in Inputs
	streamTestMust(t, Seed(&in, NewKey[int]("trigger"), 1))
	e, err := child.NewExecution(t.Context(), in)
	streamTestMust(t, err)
	defer e.Cancel()
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "nothing consumes it") {
		t.Fatalf("detached child accepted a stream without its parent's host readers: %v", err)
	}
	if child.validation != nil || child.validationDefinition != declaration || e.started {
		t.Fatal("failed independent root check published a candidate or changed the shared declarations")
	}
	streamTestMust(t, child.Validate()) // 自身受管出口允许稍后登记宿主。
	if child.validation.definition != declaration || child.validation.pending {
		t.Fatal("successful independent root check did not publish its complete candidate")
	}
}

func TestValidationPlanSharedModesConcurrentAndDisposable(t *testing.T) {
	g := validationPlanComparisonGraph(t, false, false, false, true, true)
	shared := g.nodes[0].child
	other := streamTestGraph(t, "other-root", WithAspects(func(rc *RunCtx, next func(*RunCtx) error) error { return next(rc) }))
	trigger, value := NewKey[int]("trigger"), NewKey[int]("value")
	streamTestMust(t, Sub(other, "fixed", "", []SubBind{FixSkip(trigger), Out(value, value)}, shared))
	streamTestMust(t, other.Validate())
	if g.validation.definition.nodes[0].child != other.validation.definition.nodes[0].child || shared.validation != nil {
		t.Fatal("multiple roots did not share the sealed child's pure declarations")
	}
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Go(func() {
			root := []*Graph{g, other}[i%2]
			known := streamKnownInputs{hasAspects: i&2 != 0, checkpoint: i&4 != 0}
			if i&8 != 0 {
				known.skipped = map[string]bool{"trigger": i&16 != 0}
			}
			root.mu.Lock()
			defer root.mu.Unlock()
			want := validationPlanReference(root, 0, known, false)
			before := root.validation.modes
			got := root.validation.check(0, known, false)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("concurrent shared root changed result: got %v want %v", got, want)
			}
			if len(known.skipped) != 0 && root.validation.modes != before {
				t.Error("an arbitrary skip combination was retained in the bounded mode cache")
			}
		})
	}
	wg.Wait()
	// 两处挂载仍有不同的求值节点/键；已发布模式没有继承临时 skip 状态。
	f := g.validation.modes[0]
	left, right := f.tree.sites[1], f.tree.sites[2]
	if left == right || left.nodes[0] == right.nodes[0] || left.keys["stream"] == right.keys["stream"] ||
		left.nodes[0].def != right.nodes[0].def || left.keys["trigger"].skipped || right.keys["trigger"].skipped {
		t.Fatal("shared declarations conflated mount identities or retained disposable skip facts")
	}
}

func BenchmarkValidationPlanStreamEvaluation(b *testing.B) {
	root, err := New("stream-root", "")
	if err != nil {
		b.Fatal(err)
	}
	child, err := New("stream-child", "")
	if err != nil {
		b.Fatal(err)
	}
	trigger := NewKey[int]("trigger")
	for _, g := range []*Graph{root, child} {
		if err := g.DeclareInput(NewPort(trigger)); err != nil {
			b.Fatal(err)
		}
	}
	for i := range 128 {
		stream := NewKey[Stream[int]](fmt.Sprint("stream", i))
		if err := child.Add(NewNode(fmt.Sprint("source", i), "", Requires(trigger), Provides(stream), nil)); err != nil {
			b.Fatal(err)
		}
		if err := child.Add(NewNode(fmt.Sprint("sink", i), "", Requires(stream), nil, nil)); err != nil {
			b.Fatal(err)
		}
	}
	for _, mount := range []string{"left", "right"} {
		if err := Sub(root, mount, "", []SubBind{In(trigger, trigger)}, child); err != nil {
			b.Fatal(err)
		}
	}
	if err := root.Validate(); err != nil {
		b.Fatal(err)
	}
	b.Run("first-mode", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			p := &validationPlan{definition: root.validation.definition}
			if err := p.check(0, streamKnownInputs{}, false); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("dynamic-skip", func(b *testing.B) {
		known := streamKnownInputs{skipped: map[string]bool{"trigger": true}}
		b.ReportAllocs()
		for b.Loop() {
			if err := root.validation.check(0, known, false); err != nil {
				b.Fatal(err)
			}
		}
	})
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
