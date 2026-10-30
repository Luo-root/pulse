package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func streamTestMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func streamTestGraph(t *testing.T, id string, opts ...Option) *Graph {
	t.Helper()
	g, err := New(id, "", opts...)
	streamTestMust(t, err)
	return g
}

func streamTestReject(t *testing.T, g *Graph, in Inputs, wants ...string) {
	t.Helper()
	problems := g.Topology().Problems
	if len(problems) == 0 {
		t.Fatal("Topology accepted the invalid stream")
	}
	first := problems[0].Error()
	for _, want := range wants {
		if !strings.Contains(first, want) {
			t.Fatalf("diagnostic %q does not contain %q", first, want)
		}
	}
	for i := 0; i < 3; i++ {
		if got := fmt.Sprint(g.Topology().Problems); got != fmt.Sprint(problems) {
			t.Fatalf("unstable diagnostics: %s != %v", got, problems)
		}
	}
	if err := g.Validate(); err == nil || err.Error() != first {
		t.Fatalf("Validate = %v; Topology first = %s", err, first)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	e, err := g.NewExecution(ctx, in)
	streamTestMust(t, err)
	defer e.Cancel()
	before := len(e.slots)
	if err := e.Start(); err == nil || err.Error() != first {
		t.Fatalf("Start = %v; Topology first = %s", err, first)
	}
	if len(e.slots) != before || !errors.Is(e.Wait(), ErrGraphNotStarted) {
		t.Fatal("failed Start created slots or started the execution")
	}
	// 失败检查不能封任何一层；本层与子层仍允许装入无关节点。
	streamTestMust(t, g.Add(NewNode("after-rejected-check", "", nil, nil, func(*RunCtx) error { return nil })))
	for _, n := range g.nodes {
		if n.child != nil {
			streamTestMust(t, n.child.Add(NewNode("after-rejected-child-check", "", nil, nil, func(*RunCtx) error { return nil })))
			break
		}
	}
	t.Log(first)
}

func streamTestRun(t *testing.T, g *Graph) {
	t.Helper()
	if problems := g.Topology().Problems; len(problems) != 0 {
		t.Fatalf("legal graph rejected: %v", problems)
	}
	streamTestMust(t, g.Validate())
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	streamTestMust(t, g.Run(ctx, Inputs{}))
}

type streamTestValues struct {
	mu   sync.Mutex
	rows map[string][]int
}

func (v *streamTestValues) add(id string, value int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.rows == nil {
		v.rows = make(map[string][]int)
	}
	v.rows[id] = append(v.rows[id], value)
}

func (v *streamTestValues) full(t *testing.T, count int, ids ...string) {
	t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, id := range ids {
		row := v.rows[id]
		if len(row) != count {
			t.Fatalf("%s received %v, want %d items", id, row, count)
		}
		for i, value := range row {
			if value != i+1 {
				t.Fatalf("%s received out of order: %v", id, row)
			}
		}
	}
}

func streamTestProduce(t *testing.T, g *Graph, id string, out Key[<-chan int], count, buf int) {
	t.Helper()
	streamTestMust(t, Produce(g, id, "", out, func(rc *RunCtx, send func(int) error) error {
		for i := 1; i <= count; i++ {
			if err := send(i); err != nil {
				return err
			}
		}
		return nil
	}, WithBuffer(buf)))
}

func streamTestConsume(t *testing.T, g *Graph, id string, in Key[<-chan int], values *streamTestValues, label string) {
	t.Helper()
	streamTestMust(t, Consume(g, id, "", in, func(rc *RunCtx, value int) error {
		values.add(label, value)
		return nil
	}))
}

func streamTestBroadcastSub(t *testing.T, capacity int, childOpts []Option, nested bool, buf, count int) (*Graph, *streamTestValues) {
	t.Helper()
	g := streamTestGraph(t, "broadcast-parent", WithMaxRunning(capacity))
	child := streamTestGraph(t, "broadcast-child", childOpts...)
	values := &streamTestValues{}
	src, a, b := NewKey[<-chan int]("src"), NewKey[<-chan int]("a"), NewKey[<-chan int]("b")
	ca, cb := NewKey[<-chan int]("child.a"), NewKey[<-chan int]("child.b")
	streamTestMust(t, child.DeclareInput(NewPort(ca), NewPort(cb)))
	streamTestConsume(t, child, "ca", ca, values, "a")
	streamTestConsume(t, child, "cb", cb, values, "b")
	if nested {
		outer := streamTestGraph(t, "broadcast-outer")
		streamTestMust(t, outer.DeclareInput(NewPort(ca), NewPort(cb)))
		streamTestMust(t, Sub(outer, "inner", "", []SubBind{In(ca, ca), In(cb, cb)}, child))
		child = outer
	}
	streamTestProduce(t, g, "produce", src, count, buf)
	streamTestMust(t, Tee(g, "tee", "", src, Keys(a, b), WithBuffer(buf)))
	streamTestMust(t, Sub(g, "sub", "", []SubBind{In(a, ca), In(b, cb)}, child))
	return g, values
}

func TestStreamSubSharedQuota(t *testing.T) {
	for _, c := range []struct {
		name   string
		buf, n int
		nested bool
	}{
		{"unbuffered", 0, 2, false},
		{"buffer-fills", 1, 5, false},
		{"buffer-masks-small-input", 8, 2, false},
		{"nested", 0, 2, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, _ := streamTestBroadcastSub(t, 3, nil, c.nested, c.buf, c.n)
			path := "sub/ca"
			if c.nested {
				path = "sub/inner/ca"
			}
			streamTestReject(t, g, Inputs{}, "needs 4 slots", "WithMaxRunning(3)", path, "cb")
		})
	}
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("quota4-nested-%t", nested), func(t *testing.T) {
			g, values := streamTestBroadcastSub(t, 4, nil, nested, 0, 20)
			streamTestRun(t, g)
			values.full(t, 20, "a", "b")
		})
	}
}

func TestStreamSubIndependentPools(t *testing.T) {
	for _, c := range []struct {
		name   string
		opts   []Option
		nested bool
	}{
		{"same-capacity", []Option{WithMaxRunning(3)}, false},
		{"same-capacity-nested", []Option{WithMaxRunning(3)}, true},
		{"explicit-unlimited", []Option{WithMaxRunning(0)}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, values := streamTestBroadcastSub(t, 3, c.opts, c.nested, 0, 20)
			streamTestRun(t, g)
			values.full(t, 20, "a", "b")
		})
	}
}

func TestStreamSubExecQuotaOverride(t *testing.T) {
	g, _ := streamTestBroadcastSub(t, 3, nil, false, 0, 20)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	// 这次运行的 root override 传给未声明的子层，Sub 本身不占第五个额度。
	streamTestMust(t, g.Run(ctx, Inputs{}, WithExecMaxRunning(4)))
	g2, _ := streamTestBroadcastSub(t, 4, nil, false, 0, 20)
	e, err := g2.NewExecution(ctx, Inputs{}, WithExecMaxRunning(3))
	streamTestMust(t, err)
	defer e.Cancel()
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "needs 4 slots") {
		t.Fatalf("Start did not use root override: %v", err)
	}
}

func TestStreamSubRejectsTwoReadersOfOneInlet(t *testing.T) {
	g := streamTestGraph(t, "split-parent")
	child := streamTestGraph(t, "split-child")
	src, in := NewKey[<-chan int]("src"), NewKey[<-chan int]("child.in")
	streamTestMust(t, child.DeclareInput(NewPort(in)))
	values := &streamTestValues{}
	streamTestConsume(t, child, "ca", in, values, "a")
	streamTestConsume(t, child, "cb", in, values, "b")
	streamTestProduce(t, g, "produce", src, 1, 0)
	streamTestMust(t, Sub(g, "sub", "", []SubBind{In(src, in)}, child))
	streamTestReject(t, g, Inputs{}, "split between them", "sub/ca", "sub/cb")
}

func TestStreamSubPassThrough(t *testing.T) {
	for _, readers := range []int{1, 2} {
		t.Run(fmt.Sprintf("readers-%d", readers), func(t *testing.T) {
			g := streamTestGraph(t, "pass-parent", WithMaxRunning(3))
			pass := streamTestGraph(t, "pass-child")
			src, port := NewKey[<-chan int]("src"), NewKey[<-chan int]("pass")
			streamTestMust(t, pass.DeclareInput(NewPort(port)))
			streamTestMust(t, pass.DeclareOutput(NewPort(port)))
			streamTestProduce(t, g, "produce", src, 20, 0)
			values := &streamTestValues{}
			for i := 0; i < readers; i++ {
				out := NewKey[<-chan int](fmt.Sprintf("returned-%d", i))
				id := fmt.Sprintf("pass-%d", i)
				streamTestMust(t, Sub(g, id, "", []SubBind{In(src, port), Out(port, out)}, pass))
				streamTestConsume(t, g, fmt.Sprintf("read-%d", i), out, values, id)
			}
			if readers == 2 {
				streamTestReject(t, g, Inputs{}, "split between them", "read-0", "read-1")
			} else {
				streamTestRun(t, g)
				values.full(t, 20, "pass-0")
			}
		})
	}
}

func TestStreamSubRepeatedDefinition(t *testing.T) {
	for _, own := range []bool{false, true} {
		for _, capacity := range []int{3, 4} {
			t.Run(fmt.Sprintf("own-%t-capacity-%d", own, capacity), func(t *testing.T) {
				var opts []Option
				if own {
					opts = []Option{WithMaxRunning(1)}
				}
				g := streamTestGraph(t, "mount-parent", WithMaxRunning(capacity))
				child := streamTestGraph(t, "same-definition", opts...)
				in := NewKey[<-chan int]("in")
				streamTestMust(t, child.DeclareInput(NewPort(in)))
				values := &streamTestValues{}
				streamTestMust(t, Consume(child, "read", "", in, func(rc *RunCtx, value int) error {
					values.add(rc.exec.Site().Path, value)
					return nil
				}))
				src, a, b := NewKey[<-chan int]("src"), NewKey[<-chan int]("a"), NewKey[<-chan int]("b")
				streamTestProduce(t, g, "produce", src, 20, 0)
				streamTestMust(t, Tee(g, "tee", "", src, Keys(a, b)))
				streamTestMust(t, Sub(g, "left", "", []SubBind{In(a, in)}, child))
				streamTestMust(t, Sub(g, "right", "", []SubBind{In(b, in)}, child))
				if !own && capacity == 3 {
					streamTestReject(t, g, Inputs{}, "needs 4 slots", "left/read", "right/read")
				} else {
					streamTestRun(t, g)
					values.full(t, 20, "left", "right")
				}
			})
		}
	}
}

func TestStreamSubFanOutCompetition(t *testing.T) {
	for _, mounts := range []int{1, 2} {
		t.Run(fmt.Sprintf("mounts-%d", mounts), func(t *testing.T) {
			g := streamTestGraph(t, "fanout-parent")
			child := streamTestGraph(t, "fanout-child")
			src, in := NewKey[<-chan int]("src"), NewKey[<-chan int]("in")
			streamTestMust(t, child.DeclareInput(NewPort(in)))
			streamTestMust(t, FanOut(child, "workers", "", in, Keys(NewKey[int]("one"), NewKey[int]("two")),
				func(rc *RunCtx, shard int, ch <-chan int) (int, error) {
					count := 0
					for {
						select {
						case <-rc.Context().Done():
							return 0, rc.Context().Err()
						case _, ok := <-ch:
							if !ok {
								return count, nil
							}
							count++
						}
					}
				}))
			streamTestProduce(t, g, "produce", src, 20, 0)
			for i := 0; i < mounts; i++ {
				streamTestMust(t, Sub(g, fmt.Sprintf("mount-%d", i), "", []SubBind{In(src, in)}, child))
			}
			if mounts > 1 {
				streamTestReject(t, g, Inputs{}, "read by 4 nodes", "mount-0/workers", "mount-1/workers")
			} else {
				streamTestRun(t, g)
			}
		})
	}
}

func TestStreamSubTwoInputAliases(t *testing.T) {
	for _, twoReaders := range []bool{false, true} {
		t.Run(fmt.Sprintf("two-readers-%t", twoReaders), func(t *testing.T) {
			g := streamTestGraph(t, "two-input-parent", WithMaxRunning(3))
			child := streamTestGraph(t, "two-input-child")
			src, one, two := NewKey[<-chan int]("src"), NewKey[<-chan int]("one"), NewKey[<-chan int]("two")
			streamTestMust(t, child.DeclareInput(NewPort(one), NewPort(two)))
			values := &streamTestValues{}
			if twoReaders {
				streamTestConsume(t, child, "readOne", one, values, "one")
				streamTestConsume(t, child, "readTwo", two, values, "two")
			} else {
				streamTestMust(t, child.Add(NewNode("readBothAliases", "", Requires(one, two), nil, func(rc *RunCtx) error {
					ch1, err := Get(rc, one)
					if err != nil {
						return err
					}
					ch2, err := Get(rc, two)
					if err != nil {
						return err
					}
					if ch1 != ch2 {
						return fmt.Errorf("In aliases did not preserve channel identity")
					}
					for {
						select {
						case <-rc.Context().Done():
							return rc.Context().Err()
						case value, ok := <-ch1:
							if !ok {
								return nil
							}
							values.add("both", value)
						}
					}
				})))
			}
			streamTestProduce(t, g, "produce", src, 20, 0)
			streamTestMust(t, Sub(g, "sub", "", []SubBind{In(src, one), In(src, two)}, child))
			if twoReaders {
				streamTestReject(t, g, Inputs{}, "split between them", "sub/readOne", "sub/readTwo")
			} else {
				// 同一真实读取者经过两个入口，不能重复计入额度或误判瓜分。
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer cancel()
				streamTestMust(t, g.Run(ctx, Inputs{}, WithExecMaxRunning(2)))
				values.full(t, 20, "both")
			}
		})
	}
}

func TestStreamSubSiblingStreamsSharePool(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(fmt.Sprintf("own-%t", own), func(t *testing.T) {
			g := streamTestGraph(t, "sibling-parent", WithMaxRunning(3))
			var opts []Option
			if own {
				opts = []Option{WithMaxRunning(2)}
			}
			child := streamTestGraph(t, "sibling-definition", opts...)
			src, done := NewKey[<-chan int]("src"), NewKey[bool]("done")
			streamTestProduce(t, child, "produce", src, 20, 0)
			values := &streamTestValues{}
			streamTestMust(t, Consume(child, "consume", "", src, func(rc *RunCtx, value int) error {
				values.add(rc.exec.Site().Path, value)
				return nil
			}))
			streamTestMust(t, child.DeclareOutput(NewPort(done)))
			streamTestMust(t, child.Add(NewNode("done", "", nil, Provides(done), func(rc *RunCtx) error { return Set(rc, done, true) })))
			for _, id := range []string{"left", "right"} {
				streamTestMust(t, Sub(g, id, "", []SubBind{Out(done, NewKey[bool](id+".done"))}, child))
			}
			if !own {
				streamTestReject(t, g, Inputs{}, "needs 4 slots", "left/produce", "right/consume")
			} else {
				streamTestRun(t, g)
				values.full(t, 20, "left", "right")
			}
		})
	}
}

func TestStreamSubCountsHandwrittenReader(t *testing.T) {
	g := streamTestGraph(t, "manual-parent", WithMaxRunning(2))
	child := streamTestGraph(t, "manual-child")
	src, branch, in := NewKey[<-chan int]("src"), NewKey[<-chan int]("branch"), NewKey[<-chan int]("in")
	streamTestMust(t, child.DeclareInput(NewPort(in)))
	streamTestMust(t, child.Add(NewNode("manual", "", Requires(in), nil, func(*RunCtx) error { return nil })))
	streamTestProduce(t, g, "produce", src, 1, 0)
	streamTestMust(t, Tee(g, "tee", "", src, Keys(branch)))
	streamTestMust(t, Sub(g, "sub", "", []SubBind{In(branch, in)}, child))
	streamTestReject(t, g, Inputs{}, "needs 3 slots", "sub/manual")
}

// B 的 done 在内部提前 Set，但普通 Out 仍须等 B 的消费节点收尾。
// A 的无关触发入口不妨碍流消费者；只有消费者自己 Requires(done) 才形成真环。
func streamTestCompletion(t *testing.T, gated, nested, pass bool, buf int, aFirst bool, readerGate ...bool) (*Graph, *streamTestValues) {
	t.Helper()
	g := streamTestGraph(t, "completion-unlimited")
	values := &streamTestValues{}
	src, a, b := NewKey[<-chan int]("src"), NewKey[<-chan int]("a"), NewKey[<-chan int]("b")
	done := NewKey[bool]("done")
	bin, bdone := NewKey[<-chan int]("B.in"), NewKey[bool]("B.done")
	ain, trigger := NewKey[<-chan int]("A.in"), NewKey[bool]("A.trigger")
	B := streamTestGraph(t, "B-child")
	streamTestMust(t, B.DeclareInput(NewPort(bin)))
	streamTestMust(t, B.DeclareOutput(NewPort(bdone)))
	streamTestConsume(t, B, "consumeB", bin, values, "b")
	streamTestMust(t, B.Add(NewNode("publishDoneEarly", "", nil, Provides(bdone), func(rc *RunCtx) error {
		return Set(rc, bdone, true)
	})))
	if nested {
		outer := streamTestGraph(t, "B-outer")
		streamTestMust(t, outer.DeclareInput(NewPort(bin)))
		streamTestMust(t, outer.DeclareOutput(NewPort(bdone)))
		streamTestMust(t, Sub(outer, "inner", "", []SubBind{In(bin, bin), Out(bdone, bdone)}, B))
		B = outer
	}
	A := streamTestGraph(t, "A-child")
	streamTestMust(t, A.DeclareInput(NewPort(ain)))
	if pass {
		streamTestMust(t, A.DeclareOutput(NewPort(ain)))
	} else if len(readerGate) != 0 && readerGate[0] {
		streamTestMust(t, A.Add(NewNode("consumeA", "", Deps(Requires(ain), Requires(trigger)), nil, func(rc *RunCtx) error {
			ch, err := Get(rc, ain)
			if err != nil {
				return err
			}
			for {
				select {
				case <-rc.Context().Done():
					return rc.Context().Err()
				case value, ok := <-ch:
					if !ok {
						return nil
					}
					values.add("a", value)
				}
			}
		})))
	} else {
		streamTestConsume(t, A, "consumeA", ain, values, "a")
	}
	passPort := ain
	bindsA := []SubBind{In(a, ain)}
	if gated {
		streamTestMust(t, A.DeclareInput(NewPort(trigger)))
		streamTestMust(t, A.Add(NewNode("useTrigger", "", Requires(trigger), nil, func(rc *RunCtx) error {
			_, err := Get(rc, trigger)
			return err
		})))
		bindsA = append(bindsA, In(done, trigger))
	}
	if nested {
		outer := streamTestGraph(t, "A-outer")
		streamTestMust(t, outer.DeclareInput(NewPort(ain)))
		binds := []SubBind{In(ain, ain)}
		if gated {
			streamTestMust(t, outer.DeclareInput(NewPort(trigger)))
			binds = append(binds, In(trigger, trigger))
		}
		if pass {
			passPort = NewKey[<-chan int]("A.outer.returned")
			streamTestMust(t, outer.DeclareOutput(NewPort(passPort)))
			binds = append(binds, Out(ain, passPort))
		}
		streamTestMust(t, Sub(outer, "inner", "", binds, A))
		A = outer
	}
	streamTestProduce(t, g, "produce", src, 20, buf)
	outs := Keys(b, a)
	if aFirst {
		outs = Keys(a, b)
	}
	streamTestMust(t, Tee(g, "tee", "", src, outs, WithBuffer(buf)))
	streamTestMust(t, Sub(g, "B", "", []SubBind{In(b, bin), Out(bdone, done)}, B))
	if pass {
		out := NewKey[<-chan int]("A.returned")
		bindsA = append(bindsA, Out(passPort, out))
		streamTestConsume(t, g, "consumeReturned", out, values, "a")
	}
	streamTestMust(t, Sub(g, "A", "", bindsA, A))
	streamTestMust(t, g.DeclareOutput(NewPort(done)))
	return g, values
}

func TestStreamLifecycleUnrelatedCompletionInputDoesNotGateChild(t *testing.T) {
	for _, c := range []struct {
		name         string
		nested, pass bool
		buf          int
		aFirst       bool
	}{
		{name: "unlimited-B-first"},
		{name: "unlimited-A-first", aFirst: true},
		{name: "buffer-fills", buf: 1},
		{name: "buffer-masks-small-input", buf: 128},
		{name: "nested", nested: true},
		{name: "pass-through", pass: true},
		{name: "nested-pass-through", nested: true, pass: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, values := streamTestCompletion(t, true, c.nested, c.pass, c.buf, c.aFirst)
			streamTestRun(t, g)
			values.full(t, 20, "a", "b")
		})
	}
}

func TestStreamLifecycleLegalTeeWithoutGate(t *testing.T) {
	for _, c := range []struct{ nested, pass bool }{{}, {nested: true}, {pass: true}, {nested: true, pass: true}} {
		t.Run(fmt.Sprintf("nested-%t-pass-%t", c.nested, c.pass), func(t *testing.T) {
			g, values := streamTestCompletion(t, false, c.nested, c.pass, 0, false)
			streamTestRun(t, g)
			values.full(t, 20, "a", "b")
		})
	}
}

func TestStreamLifecycleLiveChildStreamExport(t *testing.T) {
	for _, buf := range []int{0, 64} {
		t.Run(fmt.Sprintf("buffer-%d", buf), func(t *testing.T) {
			g := streamTestGraph(t, "export-parent")
			child := streamTestGraph(t, "export-child")
			inside, outside := NewKey[<-chan int]("inside"), NewKey[<-chan int]("outside")
			streamTestProduce(t, child, "produceInside", inside, 20, buf)
			streamTestMust(t, child.DeclareOutput(NewPort(inside)))
			// 整树校验识别父层真实读取者，child.Start 复用这一挂载环境。
			streamTestMust(t, Sub(g, "export", "", []SubBind{Out(inside, outside)}, child))
			values := &streamTestValues{}
			streamTestConsume(t, g, "consumeOutside", outside, values, "outside")
			streamTestRun(t, g)
			values.full(t, 20, "outside")
		})
	}
}

func TestStreamLifecycleIndependentStreamsWithCompletionGate(t *testing.T) {
	g := streamTestGraph(t, "independent-completion", WithMaxRunning(4))
	a, b := NewKey[<-chan int]("a"), NewKey[<-chan int]("b")
	ain, bin := NewKey[<-chan int]("A.in"), NewKey[<-chan int]("B.in")
	trigger, done, bdone := NewKey[bool]("trigger"), NewKey[bool]("done"), NewKey[bool]("B.done")
	values := &streamTestValues{}
	B := streamTestGraph(t, "independent-B")
	streamTestMust(t, B.DeclareInput(NewPort(bin)))
	streamTestMust(t, B.DeclareOutput(NewPort(bdone)))
	streamTestConsume(t, B, "consumeB", bin, values, "b")
	streamTestMust(t, B.Add(NewNode("early", "", nil, Provides(bdone), func(rc *RunCtx) error { return Set(rc, bdone, true) })))
	A := streamTestGraph(t, "independent-A")
	streamTestMust(t, A.DeclareInput(NewPort(ain), NewPort(trigger)))
	streamTestConsume(t, A, "consumeA", ain, values, "a")
	streamTestMust(t, A.Add(NewNode("gate", "", Requires(trigger), nil, func(*RunCtx) error { return nil })))
	streamTestProduce(t, g, "produceA", a, 20, 0)
	streamTestProduce(t, g, "produceB", b, 20, 0)
	streamTestMust(t, Sub(g, "B", "", []SubBind{In(b, bin), Out(bdone, done)}, B))
	streamTestMust(t, Sub(g, "A", "", []SubBind{In(a, ain), In(done, trigger)}, A))
	streamTestRun(t, g)
	values.full(t, 20, "a", "b")
}

func TestStreamLifecycleRootOutputStillNeedsReader(t *testing.T) {
	for _, buf := range []int{0, 64} {
		t.Run(fmt.Sprintf("buffer-%d", buf), func(t *testing.T) {
			g := streamTestGraph(t, "root-stream-outlet")
			out := NewKey[<-chan int]("out")
			streamTestMust(t, g.DeclareOutput(NewPort(out)))
			streamTestProduce(t, g, "produce", out, 1, buf)
			streamTestReject(t, g, Inputs{}, "nothing consumes it", "out")
		})
	}
	g := streamTestGraph(t, "root-stream-outlet-with-reader")
	out := NewKey[<-chan int]("out")
	streamTestMust(t, g.DeclareOutput(NewPort(out)))
	streamTestProduce(t, g, "produce", out, 20, 0)
	values := &streamTestValues{}
	streamTestConsume(t, g, "consume", out, values, "out")
	streamTestRun(t, g)
	values.full(t, 20, "out")
}

func TestStreamLifecyclePartialInputInterface(t *testing.T) {
	g := streamTestGraph(t, "external-consume")
	in := NewKey[<-chan int]("external")
	streamTestMust(t, g.DeclareInput(NewPort(in)))
	streamTestConsume(t, g, "consume", in, &streamTestValues{}, "external")
	streamTestMust(t, g.Validate())
	ch := make(chan int, 1)
	ch <- 1
	close(ch)
	var inputs Inputs
	streamTestMust(t, Seed(&inputs, in, (<-chan int)(ch)))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	streamTestMust(t, g.Run(ctx, inputs))
}

func TestStreamLifecycleDiagnosticsAreOnePerCycle(t *testing.T) {
	g, _ := streamTestCompletion(t, true, true, false, 0, false, true)
	problems := g.Topology().Problems
	if len(problems) != 1 {
		t.Fatalf("one wait SCC must produce one diagnostic: %v", problems)
	}
	for _, want := range []string{"B/inner/consumeB", "A", "synchronous send", "graph completion"} {
		if !strings.Contains(problems[0].Error(), want) {
			t.Fatalf("cycle witness omitted %q: %v", want, problems)
		}
	}
}

func streamSkipWorkMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func streamSkipWorkGraph(t *testing.T, id string) *Graph {
	t.Helper()
	g, err := New(id, "")
	streamSkipWorkMust(t, err)
	return g
}

func streamSkipWorkTree(g *Graph) *streamTree {
	g.mu.Lock()
	defer g.mu.Unlock()
	tree := &streamTree{}
	pool := &streamPool{}
	tree.collectLocked(g, "", mountChain{g}, pool, nil, len(g.aspects) != 0)
	return tree
}

func streamSkipWorkBound(t *testing.T, tree *streamTree, work streamSkipWork) {
	t.Helper()
	requires, provides, binds, children := 0, 0, 0, 0
	for _, n := range tree.nodes {
		requires += len(n.def.requires)
		provides += len(n.def.provides)
		if n.child != nil {
			binds += len(n.def.binds)
			children += len(n.child.nodes)
		}
	}
	// 同时要求已统计建索引的访问，避免「漏计工作」让退化悄悄通过。
	if work.keys < len(tree.keys) || work.keys > 2*len(tree.keys) ||
		work.nodes < len(tree.nodes) || work.nodes > 2*len(tree.nodes) ||
		work.requires < requires || work.requires > 2*requires ||
		work.binds < binds || work.binds > 2*binds ||
		work.provides > provides || work.mounts > children {
		t.Fatalf("skip work is not bounded by declarations: work=%+v, keys=%d nodes=%d requires=%d provides=%d binds=%d children=%d",
			work, len(tree.keys), len(tree.nodes), requires, provides, binds, children)
	}
}

func TestStreamSkipWorkDeclarationOrderLinear(t *testing.T) {
	for _, size := range []int{1024, 4096} {
		var forward streamSkipWork
		haveForward := false
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("size=%d/reverse=%t", size, reverse), func(t *testing.T) {
				g := streamSkipWorkGraph(t, "chain")
				keys := make([]Key[bool], size+1)
				for i := range keys {
					keys[i] = NewKey[bool](fmt.Sprint("k", i))
				}
				src, out := NewKey[<-chan int]("src"), NewKey[<-chan int]("out")
				streamSkipWorkMust(t, g.DeclareInput(NewPort(keys[0])))
				terminal := func() {
					streamSkipWorkMust(t, g.Add(NewNode("channel", "", Requires(keys[size]), Provides(src), func(*RunCtx) error { return nil })))
					streamSkipWorkMust(t, Tee(g, "tee", "", src, Keys(out)))
					streamSkipWorkMust(t, Consume(g, "consume", "", out, func(*RunCtx, int) error { return nil }))
				}
				if reverse {
					terminal()
				}
				for j := 0; j < size; j++ {
					i := j
					if reverse {
						i = size - 1 - j
					}
					streamSkipWorkMust(t, g.Add(NewNode(fmt.Sprint("node", i), "", Requires(keys[i]), Provides(keys[i+1]), func(*RunCtx) error { return nil })))
				}
				if !reverse {
					terminal()
				}
				tree := streamSkipWorkTree(g)
				// 检查真实形状：每条链边都由前一个节点产出，倒序确实倒着声明。
				if len(tree.nodes) != size+3 || len(tree.keys) != size+3 {
					t.Fatalf("unexpected chain size: nodes=%d keys=%d", len(tree.nodes), len(tree.keys))
				}
				for i := 0; i < size; i++ {
					at := i
					if reverse {
						at = size + 2 - i
					}
					n := tree.nodes[at]
					if len(n.def.requires) != 1 || len(n.def.provides) != 1 ||
						n.def.requires[0].name != keys[i].Name() || n.def.provides[0].name != keys[i+1].Name() ||
						tree.sites[0].keys[keys[i+1].Name()].writer != n {
						t.Fatalf("chain link %d is not the declared dependency at %d", i, at)
					}
				}
				work := tree.propagateSkips(map[string]bool{keys[0].Name(): true})
				streamSkipWorkBound(t, tree, work)
				for _, n := range tree.nodes {
					if !n.bodySkipped {
						t.Fatalf("skip did not reach %q", n.at)
					}
				}
				for _, k := range tree.keys {
					if !k.skipped {
						t.Fatalf("skip did not reach key %q", k.ref.name)
					}
				}
				if work.inputs != 1 || work.provides != size+2 {
					t.Fatalf("source/output visits were not counted: %+v", work)
				}
				if !reverse {
					forward, haveForward = work, true
				} else if haveForward && work != forward {
					t.Fatalf("declaration order changed work: forward=%+v reverse=%+v", forward, work)
				}
				t.Logf("size=%d reverse=%t work=%+v", size, reverse, work)
			})
		}
	}
}

func TestStreamSkipWorkBindingsLinear(t *testing.T) {
	for _, size := range []int{128, 512} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			g, pass := streamSkipWorkGraph(t, "root"), streamSkipWorkGraph(t, "pass")
			port, warm := NewKey[bool]("port"), NewKey[bool]("warm")
			streamSkipWorkMust(t, pass.DeclareInput(NewPort(port), NewPort(warm)))
			streamSkipWorkMust(t, pass.DeclareOutput(NewPort(port)))
			streamSkipWorkMust(t, pass.Add(NewNode("warm", "", Requires(warm), nil, func(*RunCtx) error { return nil })))
			keys := make([]Key[bool], size+1)
			for i := range keys {
				keys[i] = NewKey[bool](fmt.Sprint("k", i))
			}
			streamSkipWorkMust(t, g.DeclareInput(NewPort(keys[0]), NewPort(warm)))
			// 第二个未知/ready 入口使每处 Sub body 保持可能执行，必须实际沿 In/Out
			// 推导 skip，不能用整步跳过绕过绑定链。挂载和节点均倒序声明。
			src, out := NewKey[<-chan int]("src"), NewKey[<-chan int]("out")
			streamSkipWorkMust(t, Consume(g, "consume", "", out, func(*RunCtx, int) error { return nil }))
			streamSkipWorkMust(t, Tee(g, "tee", "", src, Keys(out)))
			streamSkipWorkMust(t, g.Add(NewNode("channel", "", Requires(keys[size]), Provides(src), func(*RunCtx) error { return nil })))
			for i := size - 1; i >= 0; i-- {
				streamSkipWorkMust(t, Sub(g, fmt.Sprint("pass", i), "", []SubBind{In(keys[i], port), In(warm, warm), Out(port, keys[i+1])}, pass))
			}
			tree := streamSkipWorkTree(g)
			if len(tree.sites) != size+1 || len(tree.nodes) != 2*size+3 || len(tree.keys) != 3*size+4 {
				t.Fatalf("unexpected mounted chain: sites=%d nodes=%d keys=%d", len(tree.sites), len(tree.nodes), len(tree.keys))
			}
			work := tree.propagateSkips(map[string]bool{keys[0].Name(): true})
			streamSkipWorkBound(t, tree, work)
			if !tree.sites[0].keys[out.Name()].skipped || work.binds != 5*size || work.mounts != 0 {
				t.Fatalf("did not traverse the real In/Out chain once: %+v", work)
			}
			seen := make(map[*streamKey]bool)
			for _, n := range tree.sites[0].nodes {
				if n.child == nil {
					continue
				}
				k := n.child.keys[port.Name()]
				if n.bodySkipped || n.def.child != pass || len(n.def.requires) != 2 || len(n.def.binds) != 3 ||
					!k.skipped || seen[k] || n.child.keys[warm.Name()].skipped || n.child.nodes[0].bodySkipped {
					t.Fatalf("mount %q lost its live gate or isolated facts", n.at)
				}
				seen[k] = true
			}
			t.Logf("mounts=%d work=%+v", size, work)
		})
	}
}

func TestStreamSkipWorkSubKeepsItsOwnInputGates(t *testing.T) {
	root, middle, leaf := streamSkipWorkGraph(t, "root"), streamSkipWorkGraph(t, "middle"), streamSkipWorkGraph(t, "leaf")
	trigger, fixed := NewKey[bool]("trigger"), NewKey[bool]("fixed")
	streamSkipWorkMust(t, root.DeclareInput(NewPort(trigger)))
	streamSkipWorkMust(t, middle.DeclareInput(NewPort(trigger)))
	streamSkipWorkMust(t, leaf.DeclareInput(NewPort(trigger), NewPort(fixed)))
	streamSkipWorkMust(t, leaf.Add(NewNode("gate", "", Requires(trigger), nil, func(*RunCtx) error { return nil })))
	for i := 0; i < 128; i++ {
		out := NewKey[bool](fmt.Sprint("out", i))
		streamSkipWorkMust(t, leaf.Add(NewNode(fmt.Sprint("node", i), "", Requires(fixed), Provides(out), func(*RunCtx) error { return nil })))
	}
	for i := 0; i < 16; i++ {
		streamSkipWorkMust(t, Sub(middle, fmt.Sprint("leaf", i), "", []SubBind{In(trigger, trigger), FixSkip(fixed)}, leaf))
	}
	streamSkipWorkMust(t, Sub(root, "middle", "", []SubBind{In(trigger, trigger)}, middle))
	tree := streamSkipWorkTree(root)
	work := tree.propagateSkips(map[string]bool{trigger.Name(): true})
	streamSkipWorkBound(t, tree, work)
	if work.mounts != 0 || work.provides != 16*128 {
		t.Fatalf("Sub must propagate bindings and let each ordinary node decide its own skip: %+v", work)
	}
	for _, n := range tree.nodes {
		if n.bodySkipped == n.def.sub {
			t.Fatalf("node %q skipped=%t; structural Sub must start, ordinary all-skip node must skip", n.at, n.bodySkipped)
		}
		for _, ref := range n.def.provides {
			if !n.site.keys[ref.name].skipped {
				t.Fatalf("skipped child node did not settle output %q", ref.name)
			}
		}
	}
	// 同一定义的下一次检查没有本次 root skip；FixSkip 仍逐挂载生效，
	// 但它不能反过来污染根入口或其它未知入口。
	next := streamSkipWorkTree(root)
	streamSkipWorkBound(t, next, next.propagateSkips(nil))
	if next.sites[0].keys[trigger.Name()].skipped || next.nodes[0].bodySkipped {
		t.Fatal("execution skip facts leaked into the shared definition")
	}
	for _, n := range next.nodes {
		if len(n.def.provides) != 0 && !n.site.keys[n.def.provides[0].name].skipped {
			t.Fatalf("mount-local FixSkip fact was lost at %q", n.at)
		}
	}
}

func TestStreamHostReadersFollowActualAliases(t *testing.T) {
	for _, c := range []struct {
		name          string
		hosts         map[string]bool
		graphReader   bool
		sourceSkipped bool
		want          []string
	}{
		{name: "unconsumed", want: []string{"nothing consumes it"}},
		{name: "one-host", hosts: map[string]bool{"left": true}},
		{name: "ignored-false-registration", hosts: map[string]bool{"left": true, "right": false}},
		{name: "two-outlet-aliases", hosts: map[string]bool{"right": true, "left": true}, want: []string{"2 readers", "host output \"left\"", "host output \"right\"", "split between them"}},
		{name: "host-and-node", hosts: map[string]bool{"left": true}, graphReader: true, want: []string{"2 readers", "host output \"left\"", "node \"reader\"", "split between them"}},
		{name: "two-aliases-of-skipped-source", hosts: map[string]bool{"left": true, "right": true}, sourceSkipped: true, want: []string{"2 readers", "host output \"left\"", "host output \"right\""}},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, child := streamTestGraph(t, "parent"), streamTestGraph(t, "child")
			inside := NewKey[Stream[int]]("inside")
			left, right := NewKey[Stream[int]]("left"), NewKey[Stream[int]]("right")
			streamTestMust(t, child.DeclareOutput(NewPort(inside)))
			binds := []SubBind{Out(inside, left), Out(inside, right)}
			var requires []keyRef
			if c.sourceSkipped {
				trigger := NewKey[bool]("trigger")
				streamTestMust(t, child.DeclareInput(NewPort(trigger)))
				requires = Requires(trigger)
				binds = append(binds, FixSkip(trigger))
			}
			streamTestMust(t, child.Add(NewNode("produce", "", requires, Provides(inside), func(*RunCtx) error { return nil })))
			streamTestMust(t, Sub(g, "child", "", binds, child))
			streamTestMust(t, g.DeclareOutput(NewPort(left), NewPort(right)))
			if c.graphReader {
				streamTestMust(t, g.Add(NewNode("reader", "", Requires(right), nil, func(*RunCtx) error { return nil })))
			}
			g.mu.Lock()
			err := g.validateRunLocked(0, streamKnownInputs{hostReads: c.hosts})
			g.mu.Unlock()
			if len(c.want) == 0 {
				if err != nil {
					t.Fatalf("one actual host reader was rejected: %v", err)
				}
				// 定义接受稍后订阅的可能；不能把它当成本次已登记的真实读者。
				if err := g.Validate(); err != nil {
					t.Fatalf("declared managed outlet could not be subscribed later: %v", err)
				}
				g.mu.Lock()
				withoutHost := g.validateRunLocked(0, streamKnownInputs{})
				g.mu.Unlock()
				if withoutHost == nil || !strings.Contains(withoutHost.Error(), "nothing consumes it") {
					t.Fatalf("definition outlet silently registered a host: %v", withoutHost)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid reader group was accepted")
			}
			for _, want := range c.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("diagnostic %q omitted %q", err, want)
				}
			}
		})
	}
}

func TestStreamSenderFactsPreserveOutputKinds(t *testing.T) {
	for _, c := range []struct {
		name, kind string
		managedAt  int
		sub        bool
		wantSender bool
	}{
		{name: "native-only", managedAt: -1},
		{name: "managed-first", managedAt: 0, wantSender: true},
		{name: "managed-last", managedAt: 8191, wantSender: true},
		{name: "structural-sub", managedAt: 8191, sub: true},
		{name: "native-produce", kind: "produce", managedAt: -1, wantSender: true},
		{name: "native-tee", kind: "tee", managedAt: -1, wantSender: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := streamTestGraph(t, c.name)
			provides := make([]keyRef, 8192)
			for i := range provides {
				name := fmt.Sprint("out", i)
				provides[i] = NewKey[<-chan int](name).asRef()
				if i == c.managedAt {
					provides[i] = NewKey[Stream[int]](name).asRef()
				}
			}
			n := NewNode("writer", "", nil, provides, nil)
			n.streamKind, n.sub = c.kind, c.sub
			streamTestMust(t, g.Add(n))
			tree := streamSkipWorkTree(g)
			tree.connectAliases()
			if streamSender(tree.nodes[0]) != c.wantSender || len(tree.groups) != len(provides) {
				t.Fatal("sender facts changed with output count or stream kind")
			}
			for i, gr := range tree.groups {
				wantSource := c.wantSender && (c.kind != "" || i == c.managedAt)
				if (gr.source != nil) != wantSource {
					t.Fatalf("output %d acquired the wrong source: %v", i, gr.source)
				}
			}
		})
	}
}

func TestStreamHostFactsDoNotSubscribeNativeChannel(t *testing.T) {
	g := streamTestGraph(t, "native")
	out := NewKey[<-chan int]("out")
	streamTestMust(t, g.DeclareOutput(NewPort(out)))
	streamTestProduce(t, g, "produce", out, 1, 0)
	g.mu.Lock()
	err := g.validateRunLocked(0, streamKnownInputs{hostReads: map[string]bool{"out": true}})
	g.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "nothing consumes it") {
		t.Fatalf("native channel acquired an unsupported host reader: %v", err)
	}
}
