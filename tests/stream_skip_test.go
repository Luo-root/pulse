package pulse_test

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// body 跳过不等于没有端点：切面在 next 前仍能 OpenStream / Send。
// 定义只知道 FixSkip；Start 还知道本轮 SkipSeed 和有效执行级切面。
func TestStreamSkipReaderChecksOutputFacts(t *testing.T) {
	for _, seed := range []string{"fixed", "root-seed"} {
		for _, where := range []string{"none", "node", "graph", "inherited", "execution", "cut-inherited", "cut-node", "cut-local"} {
			for _, reader := range []string{"none", "buffer-fits", "node", "outlet", "host"} {
				t.Run(seed+"/"+where+"/"+reader, func(t *testing.T) {
					trigger, key := p.NewKey[bool]("trigger"), p.NewKey[p.Stream[int]]("stream")
					var opened, sent, bodies, sum atomic.Int32
					messages := 2
					if reader == "buffer-fits" {
						messages = 1 // 能放进缓冲也不能替代声明读者。
					}
					publish := func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
						if rc.NodeID() == "writer" {
							writer, err := p.OpenStream(rc, key, p.WithBuffer(1))
							if err != nil {
								return err
							}
							opened.Add(1)
							for value := 1; value <= messages; value++ {
								if err := writer.Send(value); err != nil {
									return err
								}
								sent.Add(1)
							}
						}
						return next(rc)
					}
					var childOptions, rootOptions []p.Option
					if strings.HasPrefix(where, "cut-") {
						childOptions = append(childOptions, p.WithoutAspects())
					}
					if where == "graph" || where == "cut-local" {
						childOptions = append(childOptions, p.WithAspects(publish))
					}
					if where == "inherited" || where == "cut-inherited" {
						rootOptions = append(rootOptions, p.WithAspects(publish))
					}
					child, root := skipStreamGraph(t, "child", childOptions...), skipStreamGraph(t, "root", rootOptions...)
					skipStreamMust(t, child.DeclareInput(p.NewPort(trigger)))
					var aspects []p.Aspect
					if where == "node" || where == "cut-node" {
						aspects = []p.Aspect{publish}
					}
					skipStreamMust(t, child.Add(p.NewNode("writer", "", p.Requires(trigger), p.Provides(key), func(*p.RunCtx) error { bodies.Add(1); return nil }, aspects...)))
					if reader == "node" {
						skipStreamMust(t, p.ConsumeStream(child, "reader", "", key, func(_ *p.RunCtx, value int) error { sum.Add(int32(value)); return nil }))
					}
					binds := []p.SubBind{p.FixSkip(trigger)}
					var in p.Inputs
					if seed == "root-seed" {
						skipStreamMust(t, root.DeclareInput(p.NewPort(trigger)))
						skipStreamMust(t, p.SkipSeed(&in, trigger))
						binds = []p.SubBind{p.In(trigger, trigger)}
					}
					outlet := reader == "outlet" || reader == "host"
					if outlet {
						skipStreamMust(t, child.DeclareOutput(p.NewPort(key)))
						skipStreamMust(t, root.DeclareOutput(p.NewPort(key)))
						binds = append(binds, p.Out(key, key))
					}
					skipStreamMust(t, p.Sub(root, "mount", "", binds, child))
					effective := where != "none" && where != "cut-inherited"
					hasReader := reader == "node" || reader == "host"
					// 未给本轮输入的定义不能假定 root SkipSeed；仅执行级切面不在定义里。
					definitionRejects := !hasReader && !outlet && (seed == "root-seed" || (effective && where != "execution"))
					problems := root.Topology().Problems
					validation := root.Validate()
					if definitionRejects {
						if len(problems) != 1 || validation == nil || validation.Error() != problems[0].Error() || !strings.Contains(validation.Error(), "nothing consumes it") {
							t.Fatalf("Topology=%v Validate=%v; want one missing-reader diagnostic", problems, validation)
						}
					} else if len(problems) != 0 || validation != nil {
						t.Fatalf("definition rejected normal skip/reader/managed outlet: Topology=%v Validate=%v", problems, validation)
					}
					var options []p.ExecOption
					if where == "execution" || where == "cut-inherited" {
						options = append(options, p.WithExecAspects(publish))
					}
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					defer cancel()
					e, err := root.NewExecution(ctx, in, options...)
					skipStreamMust(t, err)
					defer e.Cancel()
					var subscription *p.StreamSubscription[int]
					if reader == "host" {
						subscription, err = p.SubscribeStream(e, key)
						skipStreamMust(t, err)
					}
					start := e.Start()
					if effective && !hasReader {
						if start == nil || !strings.Contains(start.Error(), "nothing consumes it") {
							t.Fatalf("Start=%v; possible live output must have a reader", start)
						}
						if opened.Load() != 0 || sent.Load() != 0 || bodies.Load() != 0 || !errors.Is(e.Wait(), p.ErrGraphNotStarted) {
							t.Fatal("rejected Start executed user I/O or started the execution")
						}
						return
					}
					skipStreamMust(t, start)
					if subscription != nil {
						if effective {
							for value := 1; value <= messages; value++ {
								delivery, err := subscription.Receive(ctx)
								skipStreamMust(t, err)
								if delivery.Value != value {
									t.Fatalf("host value=%d; want %d", delivery.Value, value)
								}
								sum.Add(int32(delivery.Value))
								skipStreamMust(t, delivery.Ack())
							}
							if _, err := subscription.Receive(ctx); !errors.Is(err, io.EOF) {
								t.Fatalf("host terminal=%v; want EOF", err)
							}
						} else if _, err := subscription.Receive(ctx); !errors.Is(err, p.ErrSkipped) {
							t.Fatalf("absent host endpoint=%v; want skipped", err)
						}
					}
					skipStreamMust(t, e.Wait())
					if bodies.Load() != 0 || (!effective && (opened.Load() != 0 || sent.Load() != 0 || sum.Load() != 0)) {
						t.Fatal("normal skip entered the body or a cut/inexistent aspect")
					}
					if effective && (opened.Load() != 1 || sent.Load() != 2 || sum.Load() != 3) {
						t.Fatalf("reader failed to complete both sends: open=%d sent=%d sum=%d", opened.Load(), sent.Load(), sum.Load())
					}
					if outlet && !effective {
						_, state, err := p.ReadOutput(e, key)
						skipStreamMust(t, err)
						if state != p.SlotStateSkipped {
							t.Fatalf("normal skip output=%s", state)
						}
					}
				})
			}
		}
	}
}

func TestStreamSkipReaderWithoutAspectsKeepsExplicitExecAspects(t *testing.T) {
	trigger, key := p.NewKey[bool]("trigger"), p.NewKey[p.Stream[int]]("stream")
	g := skipStreamGraph(t, "root", p.WithoutAspects())
	var called atomic.Int32
	publish := func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
		called.Add(1)
		if _, err := p.OpenStream(rc, key); err != nil {
			return err
		}
		return next(rc)
	}
	skipStreamMust(t, g.DeclareInput(p.NewPort(trigger)))
	skipStreamMust(t, g.Add(p.NewNode("writer", "", p.Requires(trigger), p.Provides(key), func(*p.RunCtx) error { called.Add(1); return nil })))
	var in p.Inputs
	skipStreamMust(t, p.SkipSeed(&in, trigger))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, err := g.NewExecution(ctx, in, p.WithExecAspects(publish))
	skipStreamMust(t, err)
	defer e.Cancel()
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "nothing consumes it") {
		t.Fatalf("WithoutAspects erased explicit ExecOption: %v", err)
	}
	if called.Load() != 0 {
		t.Fatal("rejected Start ran user code")
	}
}

// 原生流必须用已知 Tee 声明来检查，不能给手写 channel 凭空认一个发送端。
func TestStreamSkipReaderNativeOutputFacts(t *testing.T) {
	for _, aspect := range []bool{false, true} {
		for _, reader := range []string{"none", "node", "outlet"} {
			t.Run(fmt.Sprintf("aspect=%t/%s", aspect, reader), func(t *testing.T) {
				input, output := p.NewKey[<-chan int]("input"), p.NewKey[<-chan int]("output")
				child, root := skipStreamGraph(t, "child"), skipStreamGraph(t, "root")
				skipStreamMust(t, child.DeclareInput(p.NewPort(input)))
				var called, sum atomic.Int32
				var options []p.StreamOption
				if aspect {
					options = []p.StreamOption{p.WithStreamAspects(func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
						called.Add(1)
						ch := make(chan int, 1)
						defer close(ch)
						if err := p.Set(rc, output, (<-chan int)(ch)); err != nil {
							return err
						}
						for _, value := range []int{1, 2} {
							select {
							case ch <- value:
							case <-rc.Context().Done():
								return rc.Context().Err()
							}
						}
						return next(rc)
					})}
				}
				skipStreamMust(t, p.Tee(child, "writer", "", input, p.Keys(output), options...))
				if reader == "node" {
					skipStreamMust(t, p.Consume(child, "reader", "", output, func(_ *p.RunCtx, value int) error { sum.Add(int32(value)); return nil }))
				}
				binds := []p.SubBind{p.FixSkip(input)}
				if reader == "outlet" {
					skipStreamMust(t, child.DeclareOutput(p.NewPort(output)))
					skipStreamMust(t, root.DeclareOutput(p.NewPort(output)))
					binds = append(binds, p.Out(output, output))
				}
				skipStreamMust(t, p.Sub(root, "mount", "", binds, child))
				problems, validation := root.Topology().Problems, root.Validate()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				e, err := root.NewExecution(ctx, p.Inputs{})
				skipStreamMust(t, err)
				defer e.Cancel()
				start := e.Start()
				if aspect && reader != "node" {
					if len(problems) != 1 || validation == nil || start == nil || !strings.Contains(start.Error(), "nothing consumes it") {
						t.Fatalf("native output lost its reader obligation: Topology=%v Validate=%v Start=%v", problems, validation, start)
					}
					if called.Load() != 0 {
						t.Fatal("rejected native graph ran aspect")
					}
					return
				}
				if len(problems) != 0 {
					t.Fatalf("legal native control rejected: %v", problems)
				}
				skipStreamMust(t, validation)
				skipStreamMust(t, start)
				skipStreamMust(t, e.Wait())
				if (!aspect && (called.Load() != 0 || sum.Load() != 0)) || (aspect && (called.Load() != 1 || sum.Load() != 3)) {
					t.Fatalf("native control calls=%d sum=%d", called.Load(), sum.Load())
				}
			})
		}
	}
}
