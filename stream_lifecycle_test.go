package pulse

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 对应公开 API 复现：B 的 done 在内部提前 Set，但 Out 仍须等 B 的消费
// 节点收尾。A 的 mount gate 等父 done；Tee 又要 A 开始消费才能结束发送。
func streamTestCompletion(t *testing.T, gated, nested, pass bool, buf int, aFirst bool) (*Graph, *streamTestValues) {
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
	streamTestMust(t, Tee(g, "tee", src, outs, WithBuffer(buf)))
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

func TestStreamLifecycleRejectsCompletionGate(t *testing.T) {
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
			g, _ := streamTestCompletion(t, true, c.nested, c.pass, c.buf, c.aFirst)
			// 普通 Requires/Provides DAG 没有环；必须由关闭/完成关系拒绝。
			g.mu.Lock()
			ordinary := g.problemsCycleLocked()
			g.mu.Unlock()
			if len(ordinary) != 0 {
				t.Fatalf("fixture has an ordinary dependency cycle: %v", ordinary)
			}
			streamTestReject(t, g, Inputs{}, "stream lifecycle cycle", "close", "B", "A", "child Wait", "input gate")
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

func TestStreamLifecycleRejectsChildStreamExport(t *testing.T) {
	for _, buf := range []int{0, 64} {
		t.Run(fmt.Sprintf("buffer-%d", buf), func(t *testing.T) {
			g := streamTestGraph(t, "export-parent")
			child := streamTestGraph(t, "export-child")
			inside, outside := NewKey[<-chan int]("inside"), NewKey[<-chan int]("outside")
			streamTestProduce(t, child, "produceInside", inside, 20, buf)
			streamTestMust(t, child.DeclareOutput(NewPort(inside)))
			// 未挂载的子定义暂时缺真实读取者，不单独 Validate；允许 Sub
			// 先挂上，整树校验才知道最终读取者在父层且桥回要等子图完成。
			streamTestMust(t, Sub(g, "export", "", []SubBind{Out(inside, outside)}, child))
			streamTestConsume(t, g, "consumeOutside", outside, &streamTestValues{}, "outside")
			problems := g.Topology().Problems
			if len(problems) != 1 || !strings.Contains(problems[0].Error(), "stream lifecycle cycle") {
				t.Fatalf("missing export completion cycle: %v", problems)
			}
			if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "child Wait") {
				t.Fatalf("Validate must explain delayed bridge: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			e, err := g.NewExecution(ctx, Inputs{})
			streamTestMust(t, err)
			defer e.Cancel()
			if err := e.Start(); err == nil || err.Error() != problems[0].Error() {
				t.Fatalf("Start = %v; want %v", err, problems[0])
			}
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
	g, _ := streamTestCompletion(t, true, true, false, 0, false)
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
