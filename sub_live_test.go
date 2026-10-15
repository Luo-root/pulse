package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type subLiveMessage struct {
	execution string
	observed  chan struct{}
	exited    chan struct{}
}

// 首消息的消费者释放生产者的返回闩；生产者退出前没有观察到首消息就不可能成功。
// 同一张定义在两处、嵌套、透传、兄弟组合和下一轮运行都必须保持各自的真实流身份。
func TestSubLiveStreamRendezvous(t *testing.T) {
	for _, shape := range []string{"direct", "nested", "pass-through", "sibling", "same-definition-twice"} {
		t.Run(shape, func(t *testing.T) {
			g, source := streamTestGraph(t, "parent", WithMaxRunning(4)), streamTestGraph(t, "source")
			inside := NewKey[<-chan subLiveMessage]("inside")
			streamTestMust(t, source.DeclareOutput(NewPort(inside)))
			streamTestMust(t, Produce(source, "produce", "", inside, func(rc *RunCtx, send func(subLiveMessage) error) error {
				m := subLiveMessage{execution: rc.exec.ID(), observed: make(chan struct{}), exited: make(chan struct{})}
				defer close(m.exited)
				if err := send(m); err != nil {
					return err
				}
				select {
				case <-m.observed:
					return nil
				case <-rc.Context().Done():
					return rc.Context().Err()
				}
			}))
			if shape == "nested" {
				outer := streamTestGraph(t, "outer")
				streamTestMust(t, outer.DeclareOutput(NewPort(inside)))
				streamTestMust(t, Sub(outer, "inner", "", []SubBind{Out(inside, inside)}, source))
				source = outer
			}
			var mu sync.Mutex
			seen, counts := make(map[string]bool), make(map[string]int)
			consume := func(label string) func(*RunCtx, subLiveMessage) error {
				return func(_ *RunCtx, m subLiveMessage) error {
					select {
					case <-m.exited:
						return fmt.Errorf("producer exited before its first message was observed")
					default:
					}
					mu.Lock()
					duplicate := seen[m.execution]
					seen[m.execution] = true
					counts[label]++
					mu.Unlock()
					if duplicate {
						return fmt.Errorf("stream execution %s leaked across a mount or run", m.execution)
					}
					close(m.observed)
					return nil
				}
			}
			labels := []string{"source"}
			if shape == "same-definition-twice" {
				labels = []string{"left", "right"}
			}
			for _, label := range labels {
				outside := NewKey[<-chan subLiveMessage](label + ".outside")
				streamTestMust(t, Sub(g, label, "", []SubBind{Out(inside, outside)}, source))
				if shape == "pass-through" {
					pass := streamTestGraph(t, "pass")
					port, returned := NewKey[<-chan subLiveMessage]("port"), NewKey[<-chan subLiveMessage]("returned")
					streamTestMust(t, pass.DeclareInput(NewPort(port)))
					streamTestMust(t, pass.DeclareOutput(NewPort(port)))
					streamTestMust(t, Sub(g, "pass", "", []SubBind{In(outside, port), Out(port, returned)}, pass))
					outside = returned
				}
				if shape == "sibling" {
					reader := streamTestGraph(t, "reader")
					port := NewKey[<-chan subLiveMessage]("port")
					streamTestMust(t, reader.DeclareInput(NewPort(port)))
					streamTestMust(t, Consume(reader, "consume", "", port, consume(label)))
					streamTestMust(t, Sub(g, "reader", "", []SubBind{In(outside, port)}, reader))
				} else {
					streamTestMust(t, Consume(g, "consume-"+label, "", outside, consume(label)))
				}
			}
			for round := 0; round < 2; round++ {
				streamTestRun(t, g)
			}
			for _, label := range labels {
				if counts[label] != 2 {
					t.Fatalf("mount %s observed %d runs, want 2", label, counts[label])
				}
			}
		})
	}
}

func TestSubLiveStreamAllowsArrivalFeedback(t *testing.T) {
	g, child := streamTestGraph(t, "feedback", WithMaxRunning(2)), streamTestGraph(t, "child")
	inside, outside := NewKey[<-chan int]("inside"), NewKey[<-chan int]("outside")
	done, childDone := NewKey[bool]("done"), NewKey[bool]("child.done")
	streamTestMust(t, child.DeclareInput(NewPort(childDone)))
	streamTestMust(t, child.DeclareOutput(NewPort(inside)))
	streamTestProduce(t, child, "produce", inside, 3, 0)
	var acknowledged atomic.Int32
	streamTestMust(t, child.Add(NewNode("acknowledge", "", Requires(childDone), nil, func(rc *RunCtx) error {
		if _, err := Get(rc, childDone); err != nil {
			return err
		}
		acknowledged.Add(1)
		return nil
	})))
	streamTestMust(t, Sub(g, "child", "", []SubBind{In(done, childDone), Out(inside, outside)}, child))
	streamTestMust(t, g.Add(NewNode("read-and-return-done", "", Requires(outside), Provides(done), func(rc *RunCtx) error {
		ch, err := Get(rc, outside)
		if err != nil {
			return err
		}
		sum := 0
		for {
			select {
			case <-rc.Context().Done():
				return rc.Context().Err()
			case v, ok := <-ch:
				if !ok {
					if sum != 6 {
						return fmt.Errorf("stream sum=%d, want 6", sum)
					}
					return Set(rc, done, true)
				}
				sum += v
			}
		}
	})))
	// 旧的原子 Sub 模型确实会把此图报环，展开检查必须独立证明它合法。
	g.mu.Lock()
	atomicProblems := g.problemsCycleLocked()
	g.mu.Unlock()
	if len(atomicProblems) == 0 {
		t.Fatal("fixture no longer distinguishes expanded arrival from an atomic Sub gate")
	}
	streamTestRun(t, g)
	if acknowledged.Load() != 1 {
		t.Fatal("the child did not receive the parent's completion feedback")
	}
}

func TestSubPendingInputDoesNotBlockReadyChild(t *testing.T) {
	g, child := streamTestGraph(t, "parent"), streamTestGraph(t, "child")
	ready, late := NewKey[int]("ready"), NewKey[int]("late")
	streamTestMust(t, g.DeclareInput(NewPort(ready)))
	streamTestMust(t, child.DeclareInput(NewPort(ready), NewPort(late)))
	earlyRan := make(chan struct{})
	streamTestMust(t, child.Add(NewNode("ready-node", "", Requires(ready), nil, func(*RunCtx) error {
		close(earlyRan)
		return nil
	})))
	streamTestMust(t, child.Add(NewNode("late-node", "", Requires(late), nil, func(rc *RunCtx) error {
		_, err := Get(rc, late)
		return err
	})))
	streamTestMust(t, g.Add(NewNode("late-source", "", nil, Provides(late), func(rc *RunCtx) error {
		select {
		case <-earlyRan:
			return Set(rc, late, 2)
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	})))
	streamTestMust(t, Sub(g, "child", "", []SubBind{In(ready, ready), In(late, late)}, child))
	var in Inputs
	streamTestMust(t, Seed(&in, ready, 1))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	streamTestMust(t, g.Run(ctx, in))
}

func subUnusedInputFeedbackGraph(t *testing.T) *Graph {
	t.Helper()
	g, child := streamTestGraph(t, "parent"), streamTestGraph(t, "child")
	optional, output := NewKey[int]("optional"), NewKey[int]("output")
	streamTestMust(t, child.DeclareInput(NewPort(optional)))
	streamTestMust(t, child.DeclareOutput(NewPort(output)))
	streamTestMust(t, child.Add(NewNode("independent", "", nil, Provides(output), func(rc *RunCtx) error {
		return Set(rc, output, 42)
	})))
	streamTestMust(t, Sub(g, "child", "", []SubBind{In(optional, optional), Out(output, output)}, child))
	streamTestMust(t, g.Add(NewNode("feedback", "", Requires(output), Provides(optional), func(rc *RunCtx) error {
		value, err := Get(rc, output)
		if err != nil {
			return err
		}
		return Set(rc, optional, value)
	})))
	return g
}

func TestSubUnusedInputFeedbackDoesNotCreateCycle(t *testing.T) {
	streamTestRun(t, subUnusedInputFeedbackGraph(t))
}

func TestSubCheckpointAllowsPendingInputBinding(t *testing.T) {
	g := subUnusedInputFeedbackGraph(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var commits, opened atomic.Int32
	hooks := coreContractHooks(&commits)
	hooks.OpenChild = func(_ context.Context, request CheckpointChild) ([]NodeCheckpoint, error) {
		opened.Add(1)
		// optional 只能由 child 成功后的父反馈节点产出，所以此刻必为 pending。
		// OpenChild 必须描述稳定来源，不得等实际值，也不能拿 pending 做值指纹。
		if len(request.Inputs) != 0 || len(request.Bindings) != 1 {
			return nil, fmt.Errorf("pending In must use stable Bindings, got inputs=%+v bindings=%+v", request.Inputs, request.Bindings)
		}
		binding := request.Bindings[0]
		if binding.Parent.Name != "optional" || binding.Child.Name != "optional" ||
			binding.Source.Graph != "parent" || binding.Scope != "root" || binding.Attempt != request.Attempt {
			return nil, fmt.Errorf("pending In lost its stable mount source: %+v", binding)
		}
		return nil, nil
	}
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks))
	streamTestMust(t, err)
	defer func() { e.Cancel(); _ = e.Wait() }()
	streamTestMust(t, e.Start())
	streamTestMust(t, e.Wait())
	if opened.Load() != 1 || commits.Load() == 0 {
		t.Fatalf("pending-bound child did not run: opened=%d commits=%d", opened.Load(), commits.Load())
	}
}

func TestSubAllSkippedInputsKeepIndependentChildNodes(t *testing.T) {
	g, child := streamTestGraph(t, "parent"), streamTestGraph(t, "child")
	input, output := NewKey[int]("input"), NewKey[int]("output")
	streamTestMust(t, g.DeclareInput(NewPort(input)))
	streamTestMust(t, child.DeclareInput(NewPort(input)))
	streamTestMust(t, child.DeclareOutput(NewPort(output)))
	streamTestMust(t, child.Add(NewNode("skipped", "", Requires(input), nil, func(*RunCtx) error {
		return errors.New("all-skip ordinary node entered its body")
	})))
	streamTestMust(t, child.Add(NewNode("independent", "", nil, Provides(output), func(rc *RunCtx) error {
		return Set(rc, output, 42)
	})))
	streamTestMust(t, Sub(g, "child", "", []SubBind{In(input, input), Out(output, output)}, child))
	streamTestMust(t, g.DeclareOutput(NewPort(output)))
	var in Inputs
	streamTestMust(t, SkipSeed(&in, input))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, err := g.NewExecution(ctx, in)
	streamTestMust(t, err)
	streamTestMust(t, e.Start())
	streamTestMust(t, e.Wait())
	if value, state, err := ReadOutput(e, output); value != 42 || state != SlotStateReady || err != nil {
		t.Fatalf("independent output=(%d, %s, %v)", value, state, err)
	}
}

func TestSubLiveRejectsProducerCompletionGate(t *testing.T) {
	g, child := streamTestGraph(t, "parent"), streamTestGraph(t, "child")
	inside, outside := NewKey[<-chan int]("inside"), NewKey[<-chan int]("outside")
	insideDone, outsideDone := NewKey[bool]("inside.done"), NewKey[bool]("outside.done")
	n := NewNode("produce-and-done", "", nil, Deps(Provides(inside), Provides(insideDone)), func(rc *RunCtx) error {
		ch := make(chan int)
		defer close(ch)
		if err := Set(rc, inside, (<-chan int)(ch)); err != nil {
			return err
		}
		select {
		case ch <- 1:
			return Set(rc, insideDone, true)
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	})
	// 与 Produce 相同的已知同步发送形状，同时提供只能在发送后给出的 done。
	n.streamKind = "produce"
	streamTestMust(t, child.Add(n))
	streamTestMust(t, child.DeclareOutput(NewPort(inside), NewPort(insideDone)))
	streamTestMust(t, Sub(g, "source", "", []SubBind{Out(inside, outside), Out(insideDone, outsideDone)}, child))
	streamTestMust(t, g.Add(NewNode("wait-done-before-read", "", Deps(Requires(outside), Requires(outsideDone)), nil, func(*RunCtx) error {
		return errors.New("statically cyclic reader must never run")
	})))
	streamTestReject(t, g, Inputs{}, "stream lifecycle cycle", "produce-and-done", "ordinary Out", "input gate")
}

func TestSubLiveExpandedModelStillRejectsOrdinaryCycle(t *testing.T) {
	g, child := streamTestGraph(t, "parent"), streamTestGraph(t, "child")
	in, out := NewKey[int]("in"), NewKey[int]("out")
	streamTestMust(t, child.DeclareInput(NewPort(in)))
	streamTestMust(t, child.DeclareOutput(NewPort(out)))
	streamTestMust(t, child.Add(NewNode("work", "", Requires(in), Provides(out), func(rc *RunCtx) error { return Set(rc, out, 1) })))
	streamTestMust(t, Sub(g, "child", "", []SubBind{In(in, in), Out(out, out)}, child))
	streamTestMust(t, g.Add(NewNode("back", "", Requires(out), Provides(in), func(rc *RunCtx) error { return Set(rc, in, 1) })))
	stream := NewKey[<-chan int]("unrelated-stream")
	streamTestProduce(t, g, "produce", stream, 1, 0)
	streamTestConsume(t, g, "consume", stream, &streamTestValues{}, "unrelated")
	streamTestReject(t, g, Inputs{}, "dependency cycle", "child", "work", "back")
}

func TestSubLiveRetryRejectedBeforeChildBusiness(t *testing.T) {
	g, child := streamTestGraph(t, "parent"), streamTestGraph(t, "child")
	inside, outside := NewKey[<-chan int]("inside"), NewKey[<-chan int]("outside")
	var calls atomic.Int32
	streamTestMust(t, child.DeclareOutput(NewPort(inside)))
	streamTestMust(t, Produce(child, "produce", "", inside, func(*RunCtx, func(int) error) error {
		calls.Add(1)
		return nil
	}))
	streamTestMust(t, Sub(g, "child", "", []SubBind{Out(inside, outside)}, child, Retry(2, 0)))
	streamTestConsume(t, g, "consume", outside, &streamTestValues{}, "outside")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, err := g.NewExecution(ctx, Inputs{})
	streamTestMust(t, err)
	streamTestMust(t, e.Start())
	if err := e.Wait(); err == nil || !strings.Contains(err.Error(), "live stream output") || !strings.Contains(err.Error(), "per-attempt write set") {
		t.Fatalf("staged live export was not rejected clearly: %v", err)
	}
	if calls.Load() != 0 || e.Snapshot().Nodes[0].Child != nil {
		t.Fatal("staged live export advanced child business before rejection")
	}
}

func TestSubLiveSkippedOutputRemainsNormal(t *testing.T) {
	g, child := streamTestGraph(t, "parent"), streamTestGraph(t, "child")
	inside, outside := NewKey[<-chan int]("inside"), NewKey[<-chan int]("outside")
	streamTestMust(t, child.DeclareOutput(NewPort(inside)))
	streamTestMust(t, child.Add(NewNode("no-stream", "", nil, Provides(inside), func(*RunCtx) error { return NoValue() })))
	streamTestMust(t, Sub(g, "source", "", []SubBind{Out(inside, outside)}, child))
	streamTestMust(t, Consume(g, "read", "", outside, func(*RunCtx, int) error { return errors.New("skipped stream delivered a message") }))
	streamTestRun(t, g)
}

func TestSubOrdinaryOutputWaitsForSuccessfulChild(t *testing.T) {
	g, child := streamTestGraph(t, "parent"), streamTestGraph(t, "child")
	inside, outside := NewKey[int]("inside"), NewKey[int]("outside")
	published, releaseFailure := make(chan struct{}), make(chan struct{})
	failure := errors.New("late child failure")
	streamTestMust(t, child.DeclareOutput(NewPort(inside)))
	streamTestMust(t, child.Add(NewNode("early", "", nil, Provides(inside), func(rc *RunCtx) error {
		if err := Set(rc, inside, 42); err != nil {
			return err
		}
		close(published)
		return nil
	})))
	streamTestMust(t, child.Add(NewNode("late-failure", "", nil, nil, func(rc *RunCtx) error {
		select {
		case <-releaseFailure:
			return failure
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	})))
	streamTestMust(t, Sub(g, "source", "", []SubBind{Out(inside, outside)}, child))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, err := g.NewExecution(ctx, Inputs{})
	streamTestMust(t, err)
	streamTestMust(t, e.Start())
	select {
	case <-published:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	state, _ := e.slotOf(outside.asRef()).snapshot()
	close(releaseFailure)
	if err := e.Wait(); !errors.Is(err, failure) {
		t.Fatalf("child failure was not preserved: %v", err)
	}
	if state != slotPending {
		t.Fatalf("ordinary output arrived before child completion: %v", state)
	}
	if state, _ := e.slotOf(outside.asRef()).snapshot(); state == slotReady {
		t.Fatal("failed child's scalar output was published")
	}
}

func TestSubRetryKeepsInternalStreamScalarIsolation(t *testing.T) {
	g, child := streamTestGraph(t, "parent"), streamTestGraph(t, "child")
	stream, sum, outside := NewKey[<-chan int]("stream"), NewKey[int]("sum"), NewKey[int]("outside")
	var attempts atomic.Int32
	streamTestMust(t, Produce(child, "produce", "", stream, func(_ *RunCtx, send func(int) error) error {
		return send(int(attempts.Add(1)))
	}))
	streamTestMust(t, child.Add(NewNode("sum", "", Requires(stream), Provides(sum), func(rc *RunCtx) error {
		ch, err := Get(rc, stream)
		if err != nil {
			return err
		}
		total := 0
		for {
			select {
			case <-rc.Context().Done():
				return rc.Context().Err()
			case v, ok := <-ch:
				if ok {
					total += v
					continue
				}
				if err := Set(rc, sum, total); err != nil {
					return err
				}
				if total == 1 {
					return errors.New("discard this attempt's scalar")
				}
				return nil
			}
		}
	})))
	streamTestMust(t, child.DeclareOutput(NewPort(sum)))
	streamTestMust(t, Sub(g, "child", "", []SubBind{Out(sum, outside)}, child, Retry(2, 0)))
	values := &streamTestValues{}
	streamTestMust(t, g.Add(NewNode("result", "", Requires(outside), nil, func(rc *RunCtx) error {
		value, err := Get(rc, outside)
		if err == nil {
			values.add("result", value)
		}
		return err
	})))
	streamTestRun(t, g)
	if attempts.Load() != 2 || fmt.Sprint(values.rows["result"]) != "[2]" {
		t.Fatalf("attempts=%d published=%v", attempts.Load(), values.rows["result"])
	}
}
