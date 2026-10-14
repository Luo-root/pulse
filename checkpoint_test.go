package pulse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 这个小探针只记录 Core 的钩子调用；真实持久作用域与进程恢复由 durable 联调验收。
type checkpointProbe struct {
	mu       sync.Mutex
	seq      int
	latest   map[CheckpointAttemptRequest]CheckpointAttempt
	failed   map[string]bool
	opens    []CheckpointAttemptRequest
	failures []string
	commits  []CheckpointCommit
}

func (p *checkpointProbe) hooks() CheckpointHooks {
	return CheckpointHooks{
		OpenAttempt: func(_ context.Context, req CheckpointAttemptRequest) (CheckpointAttempt, error) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.opens = append(p.opens, req)
			req.Site.Execution = "" // 该探针只在一次执行中使用。
			if p.latest == nil {
				p.latest, p.failed = make(map[CheckpointAttemptRequest]CheckpointAttempt), make(map[string]bool)
			}
			a := p.latest[req]
			if a.Scope != "" && !p.failed[a.Scope] {
				return a, nil
			}
			if a.Index == req.Limit {
				a.Exhausted = true
				return a, nil
			}
			p.seq++
			a = CheckpointAttempt{Scope: fmt.Sprintf("attempt-%d", p.seq), Index: a.Index + 1}
			p.latest[req] = a
			return a, nil
		},
		FailAttempt: func(_ context.Context, scope string) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.failed[scope] = true
			p.failures = append(p.failures, scope)
			return nil
		},
		OpenChild: func(context.Context, CheckpointChild) ([]NodeCheckpoint, error) { return nil, nil },
		Commit: func(_ context.Context, commit CheckpointCommit) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.commits = append(p.commits, commit)
			return nil
		},
	}
}

func checkpointTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCheckpointPublishesOnlyAfterConfirmation(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "confirmation")
	a, b := NewKey[int]("a"), NewKey[int]("b")
	entered, release, downstream := make(chan NodeCheckpoint, 1), make(chan struct{}), make(chan struct{})
	mustAdd(t, g, NewNode("source", "完整双输出", nil, Provides(a, b), func(rc *RunCtx) error { return Set(rc, a, 7) }))
	mustAdd(t, g, NewNode("sink", "确认后才能进入", Requires(a, b), nil, func(*RunCtx) error { close(downstream); return nil }))
	p := &checkpointProbe{}
	hooks := p.hooks()
	hooks.Commit = func(ctx context.Context, c CheckpointCommit) error {
		if c.Node.NodeID == "source" {
			entered <- c.Node
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case record := <-entered:
		if record.Reason != NodeCompleted || len(record.Outputs) != 2 || record.Outputs[0].State != SlotStateReady || record.Outputs[1].State != SlotStateSkipped {
			t.Fatalf("incomplete final checkpoint: %+v", record)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, node := range e.Snapshot().Nodes {
		for _, o := range node.Outputs {
			if o.State != SlotStatePending {
				t.Fatalf("output visible before confirmation: %+v", o)
			}
		}
	}
	select {
	case <-downstream:
		t.Fatal("downstream entered before confirmation")
	default:
	}
	close(release)
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-downstream:
	default:
		t.Fatal("downstream did not enter after confirmation")
	}
}

func TestCheckpointWholeAspectChainIsStaged(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "whole-chain")
	k := NewKey[int]("value")
	cause := errors.New("outer aspect failed")
	outer := func(rc *RunCtx, next func(*RunCtx) error) error {
		if err := next(rc); err != nil {
			return err
		}
		return cause
	}
	mustAdd(t, g, NewNode("n", "Retry成功但外层失败", nil, Provides(k), func(rc *RunCtx) error { return Set(rc, k, 4) }, outer, Retry(2, 0)))
	p := &checkpointProbe{}
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks()))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); !errors.Is(err, cause) {
		t.Fatalf("got %v", err)
	}
	if len(p.commits) != 0 || len(p.failures) != 1 {
		t.Fatalf("commits=%d failures=%v", len(p.commits), p.failures)
	}
	if got := e.Snapshot().Nodes[0].Outputs[0].State; got != SlotStatePending {
		t.Fatalf("failed full chain published %s", got)
	}
}

func TestCheckpointReliabilityFailureCannotBeSwallowed(t *testing.T) {
	for _, panicHook := range []bool{false, true} {
		t.Run(fmt.Sprint(panicHook), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			child, parent := mustNew(t, ctx, "child"), mustNew(t, ctx, "parent")
			k, out := NewKey[int]("child-result"), NewKey[int]("result")
			mustAdd(t, child, NewNode("leaf", "提交故障", nil, Provides(k), func(rc *RunCtx) error { return Set(rc, k, 8) }))
			declareOut(t, child, NewPort(k))
			swallow := func(rc *RunCtx, next func(*RunCtx) error) error { _ = next(rc); return nil }
			if err := Sub(parent, "mount", "祖先吞错也必须失败", []SubBind{Out(k, out)}, child, swallow, Retry(3, 0)); err != nil {
				t.Fatal(err)
			}
			p := &checkpointProbe{}
			hooks := p.hooks()
			cause := fmt.Errorf("uncertain commit: %w", ErrSkipped)
			var calls atomic.Int32
			hooks.Commit = func(context.Context, CheckpointCommit) error {
				calls.Add(1)
				if panicHook {
					panic("backend panic")
				}
				return cause
			}
			e, err := parent.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			err = e.Wait()
			var reliable *CheckpointError
			if !errors.As(err, &reliable) || isSkipped(err) || e.Snapshot().Graph.Status != GraphStatusFailed {
				t.Fatalf("reliable failure swallowed: %v", err)
			}
			if !panicHook && !errors.Is(err, ErrSkipped) {
				t.Fatalf("lost original cause: %v", err)
			}
			if calls.Load() != 1 || len(p.failures) != 0 {
				t.Fatalf("uncertain commit retried/invalidated: calls=%d failed=%v", calls.Load(), p.failures)
			}
			if got := e.Snapshot().Nodes[0].Outputs[0].State; got != SlotStatePending {
				t.Fatalf("unconfirmed output published as %s", got)
			}
		})
	}
}

func TestCheckpointRejectedCommitLeavesActualLeafPending(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "root-leaf"
		if nested {
			name = "child-leaf"
		}
		t.Run(name, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			leafGraph := mustNew(t, ctx, "leaf-graph")
			ready, skipped, implicit := NewKey[int]("ready"), NewKey[int]("skipped"), NewKey[int]("implicit")
			mustAdd(t, leafGraph, NewNode("leaf", "业务成功但可靠提交拒绝", nil, Provides(ready, skipped, implicit), func(rc *RunCtx) error {
				if err := Set(rc, ready, 42); err != nil {
					return err
				}
				return Skip(rc, skipped)
			}))
			g := leafGraph
			if nested {
				declareOut(t, leafGraph, NewPort(ready))
				g = mustNew(t, ctx, "parent")
				out := NewKey[int]("parent-output")
				if err := Sub(g, "mount", "检查实际子执行的发布状态", []SubBind{Out(ready, out)}, leafGraph); err != nil {
					t.Fatal(err)
				}
			}
			p := &checkpointProbe{}
			hooks := p.hooks()
			cause := errors.New("store refused checkpoint")
			var commits atomic.Int32
			hooks.Commit = func(_ context.Context, c CheckpointCommit) error {
				commits.Add(1)
				if c.Node.NodeID != "leaf" {
					t.Errorf("ancestor committed after leaf rejection: %s", c.Node.NodeID)
				}
				return cause
			}
			e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			err = e.Wait()
			var reliable *CheckpointError
			if !errors.Is(err, cause) || !errors.As(err, &reliable) || reliable.Operation != "commit" || e.Snapshot().Graph.Status != GraphStatusFailed {
				t.Fatalf("commit failure not preserved: %v", err)
			}
			if commits.Load() != 1 || len(p.failures) != 0 {
				t.Fatalf("uncertain commit retried/invalidated: commits=%d failed=%v", commits.Load(), p.failures)
			}
			leafExecution := e
			if nested {
				leafExecution = e.Snapshot().Nodes[0].Child
				if leafExecution == nil {
					t.Fatal("actual child execution missing")
				}
			}
			// 父 mount 的 pending 不能证明 leaf 没发布：可靠失败锁存会阻止父侧桥回。
			leaf := leafExecution.Snapshot().Nodes[0]
			if leaf.ID != "leaf" || len(leaf.Outputs) != 3 {
				t.Fatalf("wrong leaf observation: %+v", leaf)
			}
			for _, output := range leaf.Outputs {
				if output.State != SlotStatePending {
					t.Errorf("unconfirmed actual leaf output %q published: %s", output.Name, output.State)
				}
			}
		})
	}
}

func TestCheckpointTerminalOutputStates(t *testing.T) {
	cases := []struct {
		name   string
		run    func(*RunCtx, Key[int]) error
		reason NodeFinishReason
		state  SlotState
	}{
		{"completed", func(rc *RunCtx, k Key[int]) error { return Set(rc, k, 0) }, NodeCompleted, SlotStateReady},
		{"implicit", func(*RunCtx, Key[int]) error { return nil }, NodeCompleted, SlotStateSkipped},
		{"explicit-slot-skip", func(rc *RunCtx, k Key[int]) error { return Skip(rc, k) }, NodeCompleted, SlotStateSkipped},
		{"node-skip", func(*RunCtx, Key[int]) error { return NoValue() }, NodeSkipped, SlotStateSkipped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, tc.name)
			k := NewKey[int]("k")
			mustAdd(t, g, NewNode("n", "终态", nil, Provides(k), func(rc *RunCtx) error { return tc.run(rc, k) }))
			p := &checkpointProbe{}
			if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks())); err != nil {
				t.Fatal(err)
			}
			if len(p.commits) != 1 || p.commits[0].Node.Reason != tc.reason || p.commits[0].Node.Outputs[0].State != tc.state {
				t.Fatalf("wrong checkpoint: %+v", p.commits)
			}
		})
	}
	t.Run("all-inputs-skipped-and-zero-outputs", func(t *testing.T) {
		ctx := checkpointTestContext(t)
		g := mustNew(t, ctx, "all-skipped")
		k := NewKey[int]("in")
		declareIn(t, g, NewPort(k))
		mustAdd(t, g, NewNode("n", "不进入业务", Requires(k), nil, func(*RunCtx) error { t.Error("business ran on all skipped inputs"); return nil }))
		var in Inputs
		if err := SkipSeed(&in, k); err != nil {
			t.Fatal(err)
		}
		p := &checkpointProbe{}
		if err := g.Run(ctx, in, WithExecCheckpoint("root", p.hooks())); err != nil {
			t.Fatal(err)
		}
		if len(p.commits) != 1 || p.commits[0].Node.Reason != NodeSkipped || len(p.commits[0].Node.Outputs) != 0 {
			t.Fatal(p.commits)
		}
	})
	t.Run("nil-values-are-ready", func(t *testing.T) {
		ctx := checkpointTestContext(t)
		g := mustNew(t, ctx, "nil-values")
		a, b := NewKey[any]("a"), NewKey[*int]("b")
		mustAdd(t, g, NewNode("n", "合法空值", nil, Deps(Provides(a), Provides(b)), func(rc *RunCtx) error {
			if err := Set[any](rc, a, nil); err != nil {
				return err
			}
			return Set[*int](rc, b, nil)
		}))
		p := &checkpointProbe{}
		if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks())); err != nil {
			t.Fatal(err)
		}
		for _, o := range p.commits[0].Node.Outputs {
			if o.State != SlotStateReady {
				t.Fatal(o.State)
			}
		}
	})
}

func TestCheckpointRestoreBatchAndLifecycle(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "restore")
	a, b := NewKey[int]("a"), NewKey[int]("b")
	var business, aspects, events atomic.Int32
	aspect := func(rc *RunCtx, next func(*RunCtx) error) error { aspects.Add(1); return next(rc) }
	mustAdd(t, g, NewNode("A", "来源", nil, Provides(a), func(*RunCtx) error { business.Add(1); return nil }, aspect))
	mustAdd(t, g, NewNode("B", "依赖", Requires(a), Provides(b), func(*RunCtx) error { business.Add(1); return nil }, aspect))
	declareOut(t, g, NewPort(b))
	valid := []NodeCheckpoint{
		{NodeID: "A", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: CheckpointKey{Name: "a", Type: reflect.TypeFor[int]()}, State: SlotStateReady, Value: 1}}},
		{NodeID: "B", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: CheckpointKey{Name: "b", Type: reflect.TypeFor[int]()}, State: SlotStateReady, Value: 2}}},
	}
	p := &checkpointProbe{}
	obs := ObserverFunc{GraphStarted: func(Site) { events.Add(1) }, Waiting: func(Site, string) { t.Error("restored node waited") }, Running: func(Site, string) { t.Error("restored node ran") }, Finished: func(Site, string, NodeFinishReason, error) { events.Add(1) }}
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks()), WithExecObserver(obs))
	if err != nil {
		t.Fatal(err)
	}
	invalid := []struct {
		name   string
		change func([]NodeCheckpoint) []NodeCheckpoint
	}{
		{"unknown", func(r []NodeCheckpoint) []NodeCheckpoint { r[1].NodeID = "unknown"; return r }},
		{"duplicate-node", func(r []NodeCheckpoint) []NodeCheckpoint { r[1] = r[0]; return r }},
		{"missing", func(r []NodeCheckpoint) []NodeCheckpoint { r[1].Outputs = nil; return r }},
		{"extra", func(r []NodeCheckpoint) []NodeCheckpoint {
			r[1].Outputs = append(r[1].Outputs, r[1].Outputs[0])
			return r
		}},
		{"type", func(r []NodeCheckpoint) []NodeCheckpoint {
			r[1].Outputs[0].Key.Type = reflect.TypeFor[string]()
			return r
		}},
		{"value", func(r []NodeCheckpoint) []NodeCheckpoint { r[1].Outputs[0].Value = "wrong"; return r }},
		{"pending", func(r []NodeCheckpoint) []NodeCheckpoint { r[1].Outputs[0].State = SlotStatePending; return r }},
		{"failed", func(r []NodeCheckpoint) []NodeCheckpoint { r[1].Reason = NodeFailed; return r }},
		{"closure", func(r []NodeCheckpoint) []NodeCheckpoint { return r[1:] }},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if err := e.RestoreNodes(tc.change(copyCheckpoints(valid))); err == nil {
				t.Fatal("invalid batch accepted")
			}
			if e.started || g.sealed || events.Load() != 0 || business.Load() != 0 {
				t.Fatal("invalid restore had side effects")
			}
			for _, n := range e.Snapshot().Nodes {
				for _, o := range n.Outputs {
					if o.State != SlotStatePending {
						t.Fatal("partial batch was installed")
					}
				}
			}
		})
	}
	if err := e.RestoreNodes(valid); err != nil {
		t.Fatal(err)
	}
	valid[1].Outputs[0].Value = 99 // 计划容器必须隔离。
	if err := e.RestoreNodes([]NodeCheckpoint{{NodeID: "bad"}}); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
	value, state, err := ReadOutput(e, b)
	if value != 2 || state != SlotStateReady || err != nil {
		t.Fatalf("restored output=%v %s %v", value, state, err)
	}
	if business.Load() != 0 || aspects.Load() != 0 || len(p.commits) != 0 || len(p.opens) != 0 || events.Load() != 3 {
		t.Fatalf("restoration reran work/events: business=%d aspects=%d events=%d", business.Load(), aspects.Load(), events.Load())
	}
	if err := e.RestoreNodes(nil); !errors.Is(err, ErrGraphStarted) {
		t.Fatal(err)
	}
}

func TestCheckpointStartRechecksRestoreAgainstLateAssembly(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "late-restore")
	x, y := NewKey[int]("x"), NewKey[int]("y")
	mustAdd(t, g, NewNode("A", "先缺来源", Requires(x), Provides(y), func(*RunCtx) error { t.Error("ran"); return nil }))
	p := &checkpointProbe{}
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks()))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RestoreNodes([]NodeCheckpoint{{NodeID: "A", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: CheckpointKey{Name: "y", Type: reflect.TypeFor[int]()}, State: SlotStateReady, Value: 2}}}}); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("source", "装配期补来源", nil, Provides(x), func(*RunCtx) error { t.Error("ran source"); return nil }))
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "required producer") {
		t.Fatalf("Start did not recheck restore: %v", err)
	}
	if g.sealed || e.started || len(p.opens) != 0 {
		t.Fatal("rejected Start sealed or ran")
	}
	if got := e.Snapshot().Nodes[0].Outputs[0].State; got != SlotStatePending {
		t.Fatalf("rejected Start installed %s", got)
	}
}

func TestCheckpointPersistentRetryCursorAndNestedBoundaries(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "nested")
	var runs int
	cause := errors.New("business failure")
	mustAdd(t, g, NewNode("n", "嵌套重试", nil, nil, func(*RunCtx) error {
		runs++
		if runs < 3 {
			return cause
		}
		return nil
	}, Retry(2, 0), Retry(2, 0)))
	p := &checkpointProbe{}
	if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks())); err != nil {
		t.Fatal(err)
	}
	if runs != 3 || len(p.failures) != 3 {
		t.Fatalf("runs=%d failures=%v", runs, p.failures)
	}
	boundaries := map[string]map[string]bool{}
	for _, r := range p.opens {
		if r.Boundary != "" {
			if boundaries[r.Boundary] == nil {
				boundaries[r.Boundary] = map[string]bool{}
			}
			boundaries[r.Boundary][r.Parent] = true
		}
	}
	if len(boundaries["aspect/0"]) != 1 || len(boundaries["aspect/1"]) != 2 {
		t.Fatalf("boundary identity mismatch: %v", boundaries)
	}
	t.Run("resume-index-two", func(t *testing.T) {
		ctx := checkpointTestContext(t)
		g := mustNew(t, ctx, "resume-cursor")
		var ran, retryOpens int
		cause := errors.New("second and third attempts failed")
		mustAdd(t, g, NewNode("n", "认回第二轮", nil, nil, func(*RunCtx) error { ran++; return cause }, Retry(3, 0)))
		p := &checkpointProbe{}
		hooks := p.hooks()
		base := hooks.OpenAttempt
		hooks.OpenAttempt = func(ctx context.Context, r CheckpointAttemptRequest) (CheckpointAttempt, error) {
			if r.Boundary != "" {
				retryOpens++
				if retryOpens > 2 {
					return CheckpointAttempt{}, errors.New("retry budget was replenished")
				}
				return CheckpointAttempt{Scope: fmt.Sprintf("persisted-attempt-%d", retryOpens+1), Index: retryOpens + 1}, nil
			}
			return base(ctx, r)
		}
		if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks)); !errors.Is(err, cause) {
			t.Fatal(err)
		}
		if ran != 2 || retryOpens != 2 {
			t.Fatalf("body=%d opens=%d: remaining budget must be exactly two", ran, retryOpens)
		}
	})
	t.Run("exhausted-root", func(t *testing.T) {
		ctx := checkpointTestContext(t)
		g := mustNew(t, ctx, "exhausted")
		mustAdd(t, g, NewNode("n", "失败终态", nil, nil, func(*RunCtx) error { t.Error("exhausted body reran"); return nil }, Retry(2, 0)))
		p := &checkpointProbe{}
		hooks := p.hooks()
		hooks.OpenAttempt = func(context.Context, CheckpointAttemptRequest) (CheckpointAttempt, error) {
			return CheckpointAttempt{Scope: "failed-root", Index: 1, Exhausted: true}, nil
		}
		if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks)); err == nil {
			t.Fatal("exhaustion was success")
		}
	})
	t.Run("cancel-keeps-active", func(t *testing.T) {
		ctx, cancel := context.WithCancel(checkpointTestContext(t))
		g := mustNew(t, ctx, "cancel-active")
		mustAdd(t, g, NewNode("n", "取消保留活跃", nil, nil, func(*RunCtx) error { cancel(); return ctx.Err() }, Retry(2, 0)))
		p := &checkpointProbe{}
		if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks())); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if len(p.failures) != 0 || len(p.commits) != 0 {
			t.Fatal("cancellation confirmed a failure/result")
		}
	})
}

func TestCheckpointDeclarationsAndChannelBoundaries(t *testing.T) {
	ctx := checkpointTestContext(t)
	child, parent := mustNew(t, ctx, "child-ports"), mustNew(t, ctx, "parent-ports")
	k := NewKey[int]("pass")
	declareIn(t, child, NewPort(k))
	declareOut(t, child, NewPort(k))
	if err := Sub(parent, "one", "同图第一处", []SubBind{Fix(k, 1)}, child); err != nil {
		t.Fatal(err)
	}
	if err := Sub(parent, "two", "同图第二处", []SubBind{Fix(k, 2)}, child); err != nil {
		t.Fatal(err)
	}
	ports := parent.CheckpointPorts()
	if len(ports) != 2 || ports[0].Path != "one" || ports[1].Path != "two" || !ports[0].Input || !ports[0].Output || ports[0].Key.Type != reflect.TypeFor[int]() {
		t.Fatalf("ports=%+v", ports)
	}
	ports[0].Key.Name = "changed"
	if parent.CheckpointPorts()[0].Key.Name != "pass" {
		t.Fatal("port view aliases definition")
	}
	p := &checkpointProbe{}
	t.Run("static-channel", func(t *testing.T) {
		g := mustNew(t, ctx, "channel")
		ch := NewKey[chan int]("channel")
		declareIn(t, g, NewPort(ch))
		e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks()))
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Start(); err == nil || g.sealed || e.started {
			t.Fatalf("channel declaration allowed: %v", err)
		}
	})
	t.Run("dynamic-channel", func(t *testing.T) {
		g := mustNew(t, ctx, "dynamic-channel")
		k := NewKey[any]("k")
		mustAdd(t, g, NewNode("n", "接口动态channel", nil, Provides(k), func(rc *RunCtx) error { return Set[any](rc, k, make(chan int)) }))
		if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks())); err == nil {
			t.Fatal("dynamic channel committed")
		}
	})
}
