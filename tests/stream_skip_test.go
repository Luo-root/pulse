package pulse_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/Luo-root/pulse"
)

func skipStreamMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func skipStreamGraph(t *testing.T, id string, opts ...p.Option) *p.Graph {
	t.Helper()
	g, err := p.New(id, "", opts...)
	skipStreamMust(t, err)
	return g
}

// 输入还未决定的完成等待环：Tee 的 A 出口等待 A 的真实消费节点开始，
// 该节点自己 Requires B 的 done；B 的 done 桥回又等消费节点看到 Tee 关闭。
func skipStreamFlow(t *testing.T, opts ...p.Option) (*p.Graph, p.Key[<-chan int], *atomic.Int32) {
	t.Helper()
	flow := skipStreamGraph(t, "flow", opts...)
	src, a, b := p.NewKey[<-chan int]("src"), p.NewKey[<-chan int]("a"), p.NewKey[<-chan int]("b")
	in, trigger, done := p.NewKey[<-chan int]("in"), p.NewKey[bool]("trigger"), p.NewKey[bool]("done")
	count := &atomic.Int32{}
	A, B := skipStreamGraph(t, "A"), skipStreamGraph(t, "B")
	skipStreamMust(t, flow.DeclareInput(p.NewPort(src)))
	skipStreamMust(t, A.DeclareInput(p.NewPort(in), p.NewPort(trigger)))
	skipStreamMust(t, B.DeclareInput(p.NewPort(in), p.NewPort(done)))
	skipStreamMust(t, B.DeclareOutput(p.NewPort(done)))
	skipStreamMust(t, A.Add(p.NewNode("consume", "", p.Deps(p.Requires(in), p.Requires(trigger)), nil, func(rc *p.RunCtx) error {
		ch, err := p.Get(rc, in)
		if err != nil {
			return err
		}
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return nil
				}
				count.Add(1)
			case <-rc.Context().Done():
				return rc.Context().Err()
			}
		}
	})))
	skipStreamMust(t, p.Consume(B, "consume", "", in, func(*p.RunCtx, int) error { count.Add(1); return nil }))
	// 固定普通出口仍等 B 的整体完成才桥回；不额外声明第二个流读取者。
	skipStreamMust(t, p.Tee(flow, "tee", "", src, p.Keys(a, b)))
	skipStreamMust(t, p.Sub(flow, "B", "", []p.SubBind{p.In(b, in), p.Fix(done, true), p.Out(done, done)}, B))
	skipStreamMust(t, p.Sub(flow, "A", "", []p.SubBind{p.In(a, in), p.In(done, trigger)}, A))
	return flow, src, count
}

func skipStreamExecution(t *testing.T, g *p.Graph, in p.Inputs, opts ...p.ExecOption) *p.Execution {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	t.Cleanup(cancel)
	e, err := g.NewExecution(ctx, in, opts...)
	skipStreamMust(t, err)
	skipStreamMust(t, e.Start())
	skipStreamMust(t, e.Wait())
	return e
}

func skipStreamChild(t *testing.T, e *p.Execution, id string) *p.Execution {
	t.Helper()
	for _, n := range e.Snapshot().Nodes {
		if n.ID == id && n.Child != nil {
			return n.Child
		}
	}
	t.Fatalf("child %q was not created; child.Start was not exercised", id)
	return nil
}

func skipStreamAssertFlow(t *testing.T, e *p.Execution, count *atomic.Int32) {
	t.Helper()
	if count.Load() != 0 {
		t.Fatalf("skipped flow executed business callbacks: %d", count.Load())
	}
	for _, n := range e.Snapshot().Nodes {
		if n.ID == "tee" && n.Status != p.NodeStatusSkipped {
			t.Fatalf("Tee status = %s, want skipped", n.Status)
		}
	}
}

func skipStreamAssertTopology(t *testing.T, top *p.Topology) {
	t.Helper()
	if len(top.Problems) != 0 {
		t.Fatalf("skipped mounted view at %q was rejected: %v", top.Graph.Path, top.Problems)
	}
	for _, n := range top.Nodes {
		if n.Sub != nil {
			skipStreamAssertTopology(t, n.Sub)
		}
	}
}

func TestStreamSkipFixedLifecycle(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "nested"}[nested], func(t *testing.T) {
			flow, src, count := skipStreamFlow(t)
			root := skipStreamGraph(t, "root", p.WithMaxRunning(1))
			if nested {
				middle := skipStreamGraph(t, "middle")
				skipStreamMust(t, p.Sub(middle, "flow", "", []p.SubBind{p.FixSkip(src)}, flow))
				// 让 middle 有一个无运行依赖的出口，root 的挂载仍实际启动子执行。
				done := p.NewKey[bool]("middle.done")
				skipStreamMust(t, middle.DeclareOutput(p.NewPort(done)))
				skipStreamMust(t, middle.Add(p.NewNode("done", "", nil, p.Provides(done), func(rc *p.RunCtx) error { return p.Set(rc, done, true) })))
				skipStreamMust(t, p.Sub(root, "middle", "", []p.SubBind{p.Out(done, p.NewKey[bool]("root.done"))}, middle))
			} else {
				skipStreamMust(t, p.Sub(root, "flow", "", []p.SubBind{p.FixSkip(src)}, flow))
			}
			skipStreamAssertTopology(t, root.Topology())
			skipStreamMust(t, root.Validate())
			e := skipStreamExecution(t, root, p.Inputs{})
			if nested {
				e = skipStreamChild(t, e, "middle")
			}
			skipStreamAssertFlow(t, skipStreamChild(t, e, "flow"), count)
		})
	}
}

func TestStreamSkipSeededRootStart(t *testing.T) {
	flow, src, count := skipStreamFlow(t)
	// 定义没有决定输入值，仍应报原来的 P1 环；本次明确 skip 才能运行。
	if err := flow.Validate(); err == nil || !strings.Contains(err.Error(), "stream lifecycle cycle") {
		t.Fatalf("unknown input must retain the lifecycle rejection: %v", err)
	}
	var in p.Inputs
	skipStreamMust(t, p.SkipSeed(&in, src))
	e := skipStreamExecution(t, flow, in)
	skipStreamAssertFlow(t, e, count)
}

func TestStreamSkipSeededChildStart(t *testing.T) {
	flow, src, count := skipStreamFlow(t)
	warm := p.NewKey[bool]("warm")
	skipStreamMust(t, flow.DeclareInput(p.NewPort(warm)))
	var warmed atomic.Int32
	skipStreamMust(t, flow.Add(p.NewNode("warm", "", p.Requires(warm), nil, func(*p.RunCtx) error { warmed.Add(1); return nil })))
	root := skipStreamGraph(t, "root")
	skipStreamMust(t, root.DeclareInput(p.NewPort(src), p.NewPort(warm)))
	// 一个 ready 输入使 Sub 真正进入 body，不能靠整步自动 skip 绕开 child.Start。
	skipStreamMust(t, p.Sub(root, "flow", "", []p.SubBind{p.In(src, src), p.In(warm, warm)}, flow))
	var in p.Inputs
	skipStreamMust(t, p.SkipSeed(&in, src))
	skipStreamMust(t, p.Seed(&in, warm, true))
	e := skipStreamExecution(t, root, in)
	skipStreamAssertFlow(t, skipStreamChild(t, e, "flow"), count)
	if warmed.Load() != 1 {
		t.Fatalf("child's ready path did not execute: %d", warmed.Load())
	}
}

func TestStreamSkipUnknownAndReadyInputsRemainRejected(t *testing.T) {
	flow, src, _ := skipStreamFlow(t)
	if problems := flow.Topology().Problems; len(problems) != 1 || !strings.Contains(problems[0].Error(), "stream lifecycle cycle") {
		t.Fatalf("an ordinary unknown inlet was treated as skip: %v", problems)
	}
	ch := make(chan int)
	close(ch) // 即使空流能碰巧跑完，也不把具体业务数据当结构安全证明。
	var in p.Inputs
	skipStreamMust(t, p.Seed(&in, src, (<-chan int)(ch)))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	e, err := flow.NewExecution(ctx, in)
	skipStreamMust(t, err)
	defer e.Cancel()
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "stream lifecycle cycle") {
		t.Fatalf("ready input bypassed the lifecycle check: %v", err)
	}
}

// 普通节点全输入 skip 时 body 不进入，但节点/图级/执行级切面仍可以
// 在 gate 前写出 ready channel。没有切面时才能推导其输出为 skip。
func TestStreamSkipSubAspectOutputStaysUnknown(t *testing.T) {
	flow, src, _ := skipStreamFlow(t)
	root, pass := skipStreamGraph(t, "root"), skipStreamGraph(t, "pass")
	port := p.NewKey[<-chan int]("port")
	skipStreamMust(t, pass.DeclareInput(p.NewPort(port)))
	skipStreamMust(t, pass.DeclareOutput(p.NewPort(port)))
	var called atomic.Int32
	publish := func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
		called.Add(1)
		ch := make(chan int)
		close(ch)
		return p.Set(rc, src, (<-chan int)(ch)) // 不推进 Sub body，父侧输出仍可 ready
	}
	skipStreamMust(t, p.Sub(root, "source", "", []p.SubBind{p.FixSkip(port), p.Out(port, src)}, pass, publish))
	skipStreamMust(t, p.Sub(root, "flow", "", []p.SubBind{p.In(src, src)}, flow))
	if problems := root.Topology().Problems; len(problems) != 1 || !strings.Contains(problems[0].Error(), "stream lifecycle cycle") {
		t.Fatalf("Sub aspect-written output was inferred skipped: %v", problems)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	e, err := root.NewExecution(ctx, p.Inputs{})
	skipStreamMust(t, err)
	defer e.Cancel()
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "stream lifecycle cycle") {
		t.Fatalf("Start lost the Sub aspect boundary: %v", err)
	}
	if called.Load() != 0 {
		t.Fatal("failed Start executed the Sub aspect")
	}
}

func TestStreamSkipAspectOutputStaysUnknown(t *testing.T) {
	for _, where := range []string{"none", "node", "graph", "execution", "cut-inherited"} {
		t.Run(where, func(t *testing.T) {
			flow, src, count := skipStreamFlow(t)
			root := skipStreamGraph(t, "root")
			trigger := p.NewKey[bool]("source.trigger")
			// 独立组装一层，保留 flow 内的未知 channel 接口。
			var bodies, aspects atomic.Int32
			publish := func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
				if rc.NodeID() == "generic-source" {
					aspects.Add(1)
					ch := make(chan int)
					close(ch)
					if err := p.Set(rc, src, (<-chan int)(ch)); err != nil {
						return err
					}
				}
				return next(rc)
			}
			var options []p.Option
			if where == "graph" {
				options = append(options, p.WithAspects(publish))
			}
			if where == "cut-inherited" {
				options = append(options, p.WithoutAspects())
			}
			wrapper := skipStreamGraph(t, "wrapper", options...)
			skipStreamMust(t, wrapper.DeclareInput(p.NewPort(trigger)))
			var nodeAspects []p.Aspect
			if where == "node" {
				nodeAspects = []p.Aspect{publish}
			}
			skipStreamMust(t, wrapper.Add(p.NewNode("generic-source", "", p.Requires(trigger), p.Provides(src), func(*p.RunCtx) error { bodies.Add(1); return nil }, nodeAspects...)))
			// warm ready 使 flow 的 mount body 真正执行，保留 child.Start 的覆盖。
			warm := p.NewKey[bool]("warm")
			skipStreamMust(t, flow.DeclareInput(p.NewPort(warm)))
			skipStreamMust(t, flow.Add(p.NewNode("warm", "", p.Requires(warm), nil, func(*p.RunCtx) error { return nil })))
			skipStreamMust(t, wrapper.Add(p.NewNode("warm", "", nil, p.Provides(warm), func(rc *p.RunCtx) error { return p.Set(rc, warm, true) })))
			skipStreamMust(t, p.Sub(wrapper, "flow", "", []p.SubBind{p.In(src, src), p.In(warm, warm)}, flow))
			skipStreamMust(t, p.Sub(root, "wrapper", "", []p.SubBind{p.FixSkip(trigger)}, wrapper))
			var execOptions []p.ExecOption
			if where == "execution" || where == "cut-inherited" {
				execOptions = []p.ExecOption{p.WithExecAspects(publish)}
			}
			if where == "none" || where == "cut-inherited" {
				e := skipStreamExecution(t, root, p.Inputs{}, execOptions...)
				child := skipStreamChild(t, skipStreamChild(t, e, "wrapper"), "flow")
				skipStreamAssertFlow(t, child, count)
				if bodies.Load() != 0 || aspects.Load() != 0 {
					t.Fatal("known skipped source executed its body or a cut aspect")
				}
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer cancel()
				e, err := root.NewExecution(ctx, p.Inputs{}, execOptions...)
				skipStreamMust(t, err)
				defer e.Cancel()
				if err := e.Start(); err == nil || !strings.Contains(err.Error(), "stream lifecycle cycle") {
					t.Fatalf("aspect-written output was incorrectly inferred skipped: %v", err)
				}
				if bodies.Load() != 0 || aspects.Load() != 0 {
					t.Fatal("failed Start executed user code")
				}
			}
		})
	}
}
