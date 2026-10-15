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

// 本文件独立于 checkpointProbe，失败确认与根预算的回归可随桥接实现一起落地。
func TestCheckpointFailureUsesExecutionContext(t *testing.T) {
	businessErr := errors.New("business failed")
	cases := []struct {
		name        string
		cause       error
		localCancel bool
		hostCancel  bool
	}{
		{name: "business", cause: businessErr},
		{name: "defer-cancel", cause: businessErr, localCancel: true},
		{name: "rpc-canceled", cause: fmt.Errorf("RPC: %w", context.Canceled)},
		{name: "rpc-deadline", cause: fmt.Errorf("RPC: %w", context.DeadlineExceeded)},
		{name: "host-cancel", hostCancel: true},
	}
	for _, tc := range cases {
		for _, retry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retry=%t", tc.name, retry), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				host, cancelHost := context.WithCancel(ctx)
				defer cancelHost()
				g := mustNew(t, ctx, "failure-context")
				var calls atomic.Int32
				var aspects []Aspect
				if retry {
					aspects = []Aspect{Retry(2, 0)}
				}
				mustAdd(t, g, NewNode("leaf", "业务清理与宿主取消分别判断", nil, nil, func(rc *RunCtx) error {
					call := calls.Add(1)
					if tc.localCancel {
						defer rc.Cancel()
					}
					if tc.hostCancel {
						if call == 1 {
							cancelHost()
							return rc.Context().Err()
						}
						return businessErr
					}
					return tc.cause
				}, aspects...))

				// 只模拟权威尝试游标；确认失败后才能推进，活跃游标跨 Execution 认回。
				var mu sync.Mutex
				var seq, failCalls, commits int
				latest := make(map[CheckpointAttemptRequest]CheckpointAttempt)
				failed := make(map[string]bool)
				var requests []CheckpointAttemptRequest
				var opened []CheckpointAttempt
				hooks := CheckpointHooks{
					OpenAttempt: func(ctx context.Context, req CheckpointAttemptRequest) (result CheckpointAttempt, err error) {
						if err := ctx.Err(); err != nil {
							return result, err
						}
						mu.Lock()
						defer mu.Unlock()
						req.Site.Execution = ""
						requests = append(requests, req)
						defer func() { opened = append(opened, result) }()
						result = latest[req]
						if result.Scope != "" && !failed[result.Scope] {
							return result, nil
						}
						if result.Index == req.Limit {
							result.Exhausted = true
							return result, nil
						}
						seq++
						result = CheckpointAttempt{Scope: fmt.Sprintf("attempt-%d", seq), Index: result.Index + 1}
						latest[req] = result
						return result, nil
					},
					FailAttempt: func(ctx context.Context, scope string) error {
						mu.Lock()
						defer mu.Unlock()
						failCalls++
						// 后端必须能遵守传入 ctx；不能靠一个忽略取消的钩子掩盖缺陷。
						if err := ctx.Err(); err != nil {
							return err
						}
						failed[scope] = true
						return nil
					},
					OpenChild: func(context.Context, CheckpointChild) ([]NodeCheckpoint, error) { return nil, nil },
					Commit: func(context.Context, CheckpointCommit) error {
						mu.Lock()
						defer mu.Unlock()
						commits++
						return nil
					},
				}
				firstErr := g.Run(host, Inputs{}, WithExecCheckpoint("root", hooks))
				var reliable *CheckpointError
				wantCause := tc.cause
				if tc.hostCancel {
					wantCause = context.Canceled
				}
				if !errors.Is(firstErr, wantCause) || errors.As(firstErr, &reliable) {
					t.Fatalf("business/interruption cause replaced by checkpoint failure: %v", firstErr)
				}
				if (host.Err() != nil) != tc.hostCancel {
					t.Fatalf("host context = %v, hostCancel = %t", host.Err(), tc.hostCancel)
				}
				wantCalls, wantFailures := int32(1), 1
				if retry && !tc.localCancel && !tc.hostCancel {
					wantCalls, wantFailures = 2, 3 // 两次 Retry 失败，加完整链根失败。
				}
				if tc.hostCancel {
					wantFailures = 0
				}
				if calls.Load() != wantCalls || failCalls != wantFailures || len(failed) != wantFailures || commits != 0 {
					t.Fatalf("calls=%d fail hooks=%d confirmed=%d commits=%d; want calls=%d failures=%d", calls.Load(), failCalls, len(failed), commits, wantCalls, wantFailures)
				}
				root := opened[0]
				if requests[0].Boundary != "" || requests[0].Limit != 1 || root.Index != 1 || failed[root.Scope] == tc.hostCancel {
					t.Fatalf("incorrect root budget/state: request=%+v cursor=%+v failed=%t", requests[0], root, failed[root.Scope])
				}

				resumeStart := len(opened)
				resumeErr := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
				if resumeErr == nil || errors.As(resumeErr, &reliable) {
					t.Fatalf("unexpected resume result: %v", resumeErr)
				}
				resumed := opened[resumeStart]
				if resumed.Scope != root.Scope || resumed.Index != root.Index {
					t.Fatalf("resume replaced root cursor: before=%+v after=%+v", root, resumed)
				}
				if tc.hostCancel {
					wantCalls++
					wantFailures = 1
					if retry {
						wantCalls++
						wantFailures = 3
						if !reflect.DeepEqual(opened[resumeStart+1], opened[1]) {
							t.Fatalf("resume did not recover active Retry cursor: before=%+v after=%+v", opened[1], opened[resumeStart+1])
						}
					}
					if !errors.Is(resumeErr, businessErr) || resumed.Exhausted {
						t.Fatalf("host interruption was made terminal: %v cursor=%+v", resumeErr, resumed)
					}
				} else if !resumed.Exhausted {
					t.Fatalf("confirmed business failure replenished root budget: %+v", resumed)
				}
				if calls.Load() != wantCalls || failCalls != wantFailures || len(failed) != wantFailures || commits != 0 {
					t.Fatalf("resume calls=%d fail hooks=%d confirmed=%d commits=%d; want calls=%d failures=%d", calls.Load(), failCalls, len(failed), commits, wantCalls, wantFailures)
				}
			})
		}
	}
}

func TestCheckpointBusinessContextErrorConfirmsRootFailure(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "business-context-error")
			var calls int
			mustAdd(t, g, NewNode("n", "独立RPC失败不是宿主中断", nil, nil, func(*RunCtx) error {
				calls++
				return fmt.Errorf("independent RPC failed: %w", cause)
			}))
			p := &checkpointProbe{}
			if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks())); err == nil {
				t.Fatal("first failure lost")
			}
			if ctx.Err() != nil || len(p.failures) != 1 {
				t.Fatalf("live host did not confirm failure: ctx=%v failures=%v", ctx.Err(), p.failures)
			}
			if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks())); err == nil {
				t.Fatal("exhausted root succeeded")
			}
			if calls != 1 {
				t.Fatalf("final failed body reran %d times", calls)
			}
		})
	}
}

func TestCheckpointNestedDynamicChannelsFailClosed(t *testing.T) {
	ch := make(chan int)
	cyclic := map[string]any{}
	cyclic["self"], cyclic["channel"] = cyclic, ch
	values := []any{[]any{ch}, map[string]any{"channel": ch}, struct{ Payload any }{ch}, cyclic}
	for i, value := range values {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "nested-dynamic-channel")
			k := NewKey[any]("k")
			mustAdd(t, g, NewNode("n", "接口藏在容器里面", nil, Provides(k), func(rc *RunCtx) error { return Set[any](rc, k, value) }))
			p := &checkpointProbe{}
			e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks()))
			if err != nil {
				t.Fatal(err)
			}
			bad := []NodeCheckpoint{{NodeID: "n", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: CheckpointKey{Name: "k", Type: reflect.TypeFor[any]()}, State: SlotStateReady, Value: value}}}}
			if err := e.RestoreNodes(bad); err == nil {
				t.Fatal("channel entered a restore plan")
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			if err := e.Wait(); err == nil || len(p.commits) != 0 {
				t.Fatalf("channel reached commit: %v", err)
			}
		})
	}
}

func TestCheckpointValueCyclesWithoutChannelsRemainLegal(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "normal-cycle")
	k := NewKey[any]("k")
	m := map[string]any{}
	m["self"] = m
	var slice []any
	slice = make([]any, 1)
	slice[0] = slice
	value := []any{m, slice}
	mustAdd(t, g, NewNode("n", "业务对象可以自指", nil, Provides(k), func(rc *RunCtx) error { return Set[any](rc, k, value) }))
	p := &checkpointProbe{}
	if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", p.hooks())); err != nil {
		t.Fatal(err)
	}
	if len(p.commits) != 1 {
		t.Fatal("non-channel cycles were over-rejected")
	}
}

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
