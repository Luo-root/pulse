package pulse_test

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

	p "github.com/Luo-root/pulse"
)

func streamAspectsMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func streamAspectsGraph(t *testing.T, opts ...p.Option) *p.Graph {
	t.Helper()
	g, err := p.New("stream-aspects", "流节点切面", opts...)
	streamAspectsMust(t, err)
	return g
}

func streamAspectsContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func streamAspectsStart(t *testing.T, ctx context.Context, g *p.Graph, in p.Inputs, opts ...p.ExecOption) *p.Execution {
	t.Helper()
	e, err := g.NewExecution(ctx, in, opts...)
	streamAspectsMust(t, err)
	t.Cleanup(e.Cancel)
	streamAspectsMust(t, e.Start())
	return e
}

func streamAspectsAwait[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatal("rendezvous did not complete: ", ctx.Err())
		var zero T
		return zero
	}
}

type streamAspectTrace struct {
	mu     sync.Mutex
	events map[string][]string
}

func (s *streamAspectTrace) add(id, event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events == nil {
		s.events = make(map[string][]string)
	}
	s.events[id] = append(s.events[id], event)
}

func (s *streamAspectTrace) aspect(label string) p.Aspect {
	return func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
		s.add(rc.NodeID(), label+":in")
		err := next(rc)
		s.add(rc.NodeID(), label+":out")
		return err
	}
}

func (s *streamAspectTrace) expect(t *testing.T, id string, want []string) {
	t.Helper()
	s.mu.Lock()
	got := append([]string(nil), s.events[id]...)
	s.mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s events = %v, want %v", id, got, want)
	}
}

// 三糖与普通节点共用顺序及生命周期；选项可跨节点复用，调用方切片不再控制定义。
func TestStreamAspectsOrderAndScope(t *testing.T) {
	for _, layer := range []string{"none", "graph", "execution", "both"} {
		t.Run(layer, func(t *testing.T) {
			var trace streamAspectTrace
			observer := p.ObserverFunc{
				GraphStarted:  func(p.Site) { trace.add("graph", "started") },
				GraphFinished: func(p.Site, p.NodeFinishReason, error) { trace.add("graph", "finished") },
				Waiting:       func(_ p.Site, id string) { trace.add(id, "waiting") },
				Running:       func(_ p.Site, id string) { trace.add(id, "running") },
				Finished:      func(_ p.Site, id string, _ p.NodeFinishReason, _ error) { trace.add(id, "finished") },
			}
			graphOpts := []p.Option{p.WithObserver(observer)}
			var execOpts []p.ExecOption
			var outer []string
			if layer == "graph" || layer == "both" {
				graphOpts = append(graphOpts, p.WithAspects(trace.aspect("graph")))
				outer = append(outer, "graph")
			}
			if layer == "execution" || layer == "both" {
				execOpts = append(execOpts, p.WithExecAspects(trace.aspect("execution")))
				outer = append(outer, "execution")
			}
			ctx := streamAspectsContext(t)
			g := streamAspectsGraph(t, graphOpts...)
			src, a, b := p.NewKey[<-chan int]("src"), p.NewKey[<-chan int]("a"), p.NewKey[<-chan int]("b")
			caller := []p.Aspect{trace.aspect("local-1"), trace.aspect("local-2")}
			local := p.WithStreamAspects(caller...)
			caller[0] = trace.aspect("changed-before-assembly")
			deadline := func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
				limit, ok := rc.Context().Deadline()
				hostLimit, _ := ctx.Deadline()
				if !ok || !limit.Before(hostLimit) {
					return fmt.Errorf("%s has no local timeout context", rc.NodeID())
				}
				return next(rc)
			}
			opts := []p.StreamOption{nil, p.WithStreamAspects(), local,
				p.WithStreamAspects(trace.aspect("local-3"), p.Timeout(2*time.Second), deadline)}
			streamAspectsMust(t, p.Produce(g, "produce", "", src, func(rc *p.RunCtx, send func(int) error) error {
				trace.add(rc.NodeID(), "body")
				ch, err := p.Get(rc, src)
				if err != nil || cap(ch) != 2 {
					return fmt.Errorf("Produce buffer = %d / %v, want 2 / nil", cap(ch), err)
				}
				return send(7)
			}, append(opts, p.WithBuffer(2))...))
			streamAspectsMust(t, p.Tee(g, "tee", "", src, p.Keys(a, b), append([]p.StreamOption{p.WithBuffer(3)}, opts...)...))
			var got [2][]int
			streamAspectsMust(t, p.Consume(g, "consume", "", a, func(rc *p.RunCtx, v int) error {
				trace.add(rc.NodeID(), "body")
				ch, err := p.Get(rc, a)
				if err != nil || cap(ch) != 3 {
					return fmt.Errorf("Tee buffer = %d / %v, want 3 / nil", cap(ch), err)
				}
				got[0] = append(got[0], v)
				return nil
			}, opts...))
			streamAspectsMust(t, p.Consume(g, "other", "", b, func(rc *p.RunCtx, v int) error {
				trace.add(rc.NodeID(), "body")
				got[1] = append(got[1], v)
				return nil
			}))
			streamAspectsMust(t, g.Add(p.NewNode("ordinary", "", nil, nil, func(rc *p.RunCtx) error {
				trace.add(rc.NodeID(), "body")
				return nil
			}, trace.aspect("local-1"), trace.aspect("local-2"), trace.aspect("local-3"))))
			caller[1] = trace.aspect("changed-after-assembly")
			opts[2] = p.WithStreamAspects(trace.aspect("changed-option-list"))
			e := streamAspectsStart(t, ctx, g, p.Inputs{}, execOpts...)
			streamAspectsMust(t, e.Wait())
			if want := [2][]int{{7}, {7}}; !reflect.DeepEqual(got, want) {
				t.Fatalf("broadcast = %v, want %v", got, want)
			}
			for _, id := range []string{"produce", "tee", "consume", "other", "ordinary"} {
				labels := append([]string(nil), outer...)
				if id != "other" {
					labels = append(labels, "local-1", "local-2", "local-3")
				}
				var want []string
				for _, label := range labels {
					want = append(want, label+":in")
				}
				want = append(want, "waiting", "running")
				if id != "tee" {
					want = append(want, "body")
				}
				for i := len(labels) - 1; i >= 0; i-- {
					want = append(want, labels[i]+":out")
				}
				trace.expect(t, id, append(want, "finished"))
			}
			trace.expect(t, "graph", []string{"started", "finished"})
		})
	}
}

func TestStreamAspectsEmptyOptions(t *testing.T) {
	var empty []p.Aspect
	for name, opts := range map[string][]p.StreamOption{
		"omitted": nil, "nil": {nil}, "empty": {p.WithStreamAspects()},
		"nil-slice": {p.WithStreamAspects(empty...)}, "nonpositive-buffer": {p.WithBuffer(0), p.WithBuffer(-1)},
	} {
		t.Run(name, func(t *testing.T) {
			g := streamAspectsGraph(t)
			src, out := p.NewKey[<-chan int]("src"), p.NewKey[<-chan int]("out")
			streamAspectsMust(t, p.Produce(g, "produce", "", src, func(_ *p.RunCtx, send func(int) error) error { return send(7) }, opts...))
			streamAspectsMust(t, p.Tee(g, "tee", "", src, p.Keys(out), opts...))
			var got []int
			streamAspectsMust(t, p.Consume(g, "consume", "", out, func(_ *p.RunCtx, v int) error { got = append(got, v); return nil }, opts...))
			streamAspectsMust(t, streamAspectsStart(t, streamAspectsContext(t), g, p.Inputs{}).Wait())
			if !reflect.DeepEqual(got, []int{7}) {
				t.Fatalf("values = %v, want [7]", got)
			}
		})
	}
}

func TestStreamAspectsConsumeRejectsBufferAtomically(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			g := streamAspectsGraph(t)
			in := p.NewKey[<-chan int]("in")
			streamAspectsMust(t, g.DeclareInput(p.NewPort(in)))
			before := g.Topology()
			var calls atomic.Int32
			local := p.WithStreamAspects(func(rc *p.RunCtx, next func(*p.RunCtx) error) error { calls.Add(1); return next(rc) })
			opts := []p.StreamOption{local, p.WithBuffer(4), p.WithBuffer(0)}
			if reverse {
				opts = []p.StreamOption{p.WithBuffer(4), local}
			}
			err := p.Consume(g, "consume", "", in, func(*p.RunCtx, int) error { return nil }, opts...)
			if err == nil || !strings.Contains(err.Error(), `Consume "consume"`) || !strings.Contains(err.Error(), "WithBuffer(4)") {
				t.Fatalf("Consume = %v, want named buffer rejection", err)
			}
			if !reflect.DeepEqual(g.Topology(), before) || calls.Load() != 0 {
				t.Fatal("rejected assembly changed the definition or invoked an aspect")
			}
			// 原 ID 仍可用，输入声明也保持原样。
			streamAspectsMust(t, p.Consume(g, "consume", "", in, func(*p.RunCtx, int) error { return nil }, local))
			ch := make(chan int)
			close(ch)
			var inputs p.Inputs
			streamAspectsMust(t, p.Seed(&inputs, in, (<-chan int)(ch)))
			streamAspectsMust(t, streamAspectsStart(t, streamAspectsContext(t), g, inputs).Wait())
			if calls.Load() != 1 {
				t.Fatalf("successful assembly aspect calls = %d, want 1", calls.Load())
			}
		})
	}
}

// 不靠调度 sleep：源没有接收者或输入没有数据，必须由本地 deadline 解除真实阻塞。
func TestStreamAspectsTimeoutCancelsManagedIO(t *testing.T) {
	for _, mode := range []string{"Produce/send", "Consume/receive", "Consume/input", "Tee/receive", "Tee/send", "Tee/input"} {
		t.Run(mode, func(t *testing.T) {
			ctx := streamAspectsContext(t)
			g := streamAspectsGraph(t)
			in, out := p.NewKey[<-chan int]("in"), p.NewKey[<-chan int]("out")
			var inputs p.Inputs
			innerResult := make(chan error, 1)
			probe := func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
				err := next(rc)
				innerResult <- err
				return err
			}
			opts := p.WithStreamAspects(p.Timeout(50*time.Millisecond), probe)
			var callbacks atomic.Int32
			src := make(chan int, 1)
			if mode == "Tee/send" {
				src <- 7
				close(src)
			}
			if strings.HasSuffix(mode, "/input") {
				streamAspectsMust(t, g.Add(p.NewNode("late", "", nil, p.Provides(in), func(rc *p.RunCtx) error {
					<-rc.Context().Done()
					return rc.Context().Err()
				})))
			} else if !strings.HasPrefix(mode, "Produce/") {
				streamAspectsMust(t, g.DeclareInput(p.NewPort(in)))
				streamAspectsMust(t, p.Seed(&inputs, in, (<-chan int)(src)))
			}
			target := strings.Split(mode, "/")[0]
			switch target {
			case "Produce":
				streamAspectsMust(t, p.Produce(g, target, "", out, func(_ *p.RunCtx, send func(int) error) error {
					callbacks.Add(1)
					return send(7)
				}, opts))
			case "Consume":
				streamAspectsMust(t, p.Consume(g, target, "", in, func(*p.RunCtx, int) error { callbacks.Add(1); return nil }, opts))
			case "Tee":
				streamAspectsMust(t, p.Tee(g, target, "", in, p.Keys(out), opts))
			}
			if target != "Consume" {
				// 真正 Requires 出口，但不从 channel 接收，发送必须被超时唤醒。
				streamAspectsMust(t, g.Add(p.NewNode("reader", "", p.Requires(out), nil, func(rc *p.RunCtx) error {
					<-rc.Context().Done()
					return rc.Context().Err()
				})))
			}
			e := streamAspectsStart(t, ctx, g, inputs)
			err := e.Wait()
			if err == nil || errors.Is(err, p.ErrSkipped) || ctx.Err() != nil {
				t.Fatalf("Wait = %v, host = %v; want local timeout failure", err, ctx.Err())
			}
			if innerErr := streamAspectsAwait(t, ctx, innerResult); !errors.Is(innerErr, context.DeadlineExceeded) {
				t.Fatalf("managed operation = %v, want deadline exceeded", innerErr)
			}
			for _, node := range e.Snapshot().Nodes {
				if node.ID == target && (node.Status == p.NodeStatusCompleted || node.Status == p.NodeStatusSkipped) {
					t.Fatalf("truncated stream was reported as %s", node.Status)
				}
			}
			if target == "Produce" && callbacks.Load() != 1 {
				t.Fatalf("producer did not enter its send: %d", callbacks.Load())
			}
			if target == "Consume" && callbacks.Load() != 0 {
				t.Fatalf("empty or pending input invoked callback: %d", callbacks.Load())
			}
			if mode == "Tee/send" && len(src) != 0 {
				t.Fatal("Tee did not consume the value before its blocked send")
			}
		})
	}
}

// 先会合到内层确实收到取消，再用释放门控制退出；Wait 与 Finished 均必须等这个门。
func TestStreamAspectsTimeoutWaitsForInnerExit(t *testing.T) {
	for _, kind := range []string{"Produce", "Consume", "Tee"} {
		t.Run(kind, func(t *testing.T) {
			ctx := streamAspectsContext(t)
			entered, canceled, exited, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var earlyFinish atomic.Bool
			g := streamAspectsGraph(t, p.WithObserver(p.ObserverFunc{
				Finished: func(_ p.Site, id string, _ p.NodeFinishReason, _ error) {
					if id == "target" {
						select {
						case <-exited:
						default:
							earlyFinish.Store(true)
						}
					}
				},
			}))
			hold := func(rc *p.RunCtx) error {
				close(entered)
				<-rc.Context().Done()
				close(canceled)
				<-release
				close(exited)
				return nil // 即使内层在取消后返回 nil，Timeout 仍须保留失败。
			}
			in, out := p.NewKey[<-chan int]("in"), p.NewKey[<-chan int]("out")
			var inputs p.Inputs
			if kind != "Produce" {
				streamAspectsMust(t, g.DeclareInput(p.NewPort(in)))
				src := make(chan int, 1)
				if kind == "Consume" {
					src <- 7
					close(src)
				}
				streamAspectsMust(t, p.Seed(&inputs, in, (<-chan int)(src)))
			}
			opts := p.WithStreamAspects(p.Timeout(50 * time.Millisecond))
			switch kind {
			case "Produce":
				streamAspectsMust(t, p.Produce(g, "target", "", out, func(rc *p.RunCtx, _ func(int) error) error { return hold(rc) }, opts))
			case "Consume":
				streamAspectsMust(t, p.Consume(g, "target", "", in, func(rc *p.RunCtx, _ int) error { return hold(rc) }, opts))
			case "Tee":
				// Tee 无业务回调；同一链中的内层包装控制其退出。
				probe := func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
					close(entered)
					_ = next(rc)
					close(canceled)
					<-release
					close(exited)
					return nil // 包装在观察取消后自行收尾，也不能把超时改成成功。
				}
				streamAspectsMust(t, p.Tee(g, "target", "", in, p.Keys(out), opts, p.WithStreamAspects(probe)))
			}
			if kind != "Consume" {
				streamAspectsMust(t, p.Consume(g, "sink", "", out, func(*p.RunCtx, int) error { return nil }))
			}
			e := streamAspectsStart(t, ctx, g, inputs)
			done := make(chan error, 1)
			go func() { done <- e.Wait() }()
			streamAspectsAwait(t, ctx, entered)
			streamAspectsAwait(t, ctx, canceled)
			select {
			case err := <-done:
				t.Fatalf("Wait returned before inner exit: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			unblock()
			if err := streamAspectsAwait(t, ctx, done); err == nil || errors.Is(err, p.ErrSkipped) || ctx.Err() != nil {
				t.Fatalf("Wait = %v, host = %v; want local timeout failure", err, ctx.Err())
			}
			streamAspectsAwait(t, ctx, exited)
			if earlyFinish.Load() {
				t.Fatal("node finished before its inner work exited")
			}
		})
	}
}

func TestStreamAspectsProducerRetryStillRejected(t *testing.T) {
	for _, kind := range []string{"Produce", "Tee"} {
		t.Run(kind, func(t *testing.T) {
			ctx := streamAspectsContext(t)
			g := streamAspectsGraph(t)
			in, out := p.NewKey[<-chan int]("in"), p.NewKey[<-chan int]("out")
			var inputs p.Inputs
			var callbacks atomic.Int32
			var attempts atomic.Int32
			probe := func(rc *p.RunCtx, next func(*p.RunCtx) error) error { attempts.Add(1); return next(rc) }
			opts := p.WithStreamAspects(p.Retry(2, 0), probe)
			src := make(chan int, 1)
			src <- 7
			close(src)
			if kind == "Produce" {
				streamAspectsMust(t, p.Produce(g, "target", "", out, func(_ *p.RunCtx, send func(int) error) error { callbacks.Add(1); return send(7) }, opts))
			} else {
				streamAspectsMust(t, g.DeclareInput(p.NewPort(in)))
				streamAspectsMust(t, p.Seed(&inputs, in, (<-chan int)(src)))
				streamAspectsMust(t, p.Tee(g, "target", "", in, p.Keys(out), opts))
			}
			streamAspectsMust(t, p.Consume(g, "sink", "", out, func(*p.RunCtx, int) error { callbacks.Add(1); return nil }))
			err := streamAspectsStart(t, ctx, g, inputs).Wait()
			for _, text := range []string{"per-attempt write set", `"target"`, `"out"`, "WithStreamAspects"} {
				if err == nil || !strings.Contains(err.Error(), text) {
					t.Fatalf("Wait = %v, want %q in staging rejection", err, text)
				}
			}
			if ctx.Err() != nil || callbacks.Load() != 0 || attempts.Load() != 2 || len(src) != 1 {
				t.Fatalf("host=%v callbacks=%d attempts=%d input=%d; staging guard did not fail before I/O", ctx.Err(), callbacks.Load(), attempts.Load(), len(src))
			}
		})
	}
}

// 业务体或内层包装已明确看到 deadline 后返回 nil，Timeout 仍须保留失败。
// 重复增加覆盖双就绪分支的机会，不作调度顺序保证；不靠 sleep 安排取消与返回。
func TestStreamAspectsTimeoutRejectsExpiredNil(t *testing.T) {
	for _, kind := range []string{"Produce", "Consume", "Tee"} {
		t.Run(kind, func(t *testing.T) {
			ctx := streamAspectsContext(t)
			for trial := 0; trial < 32; trial++ {
				g := streamAspectsGraph(t)
				in, out := p.NewKey[<-chan int]("in"), p.NewKey[<-chan int]("out")
				var seenDeadline atomic.Bool
				var bodyCalls atomic.Int32
				observe := func(rc *p.RunCtx) error {
					<-rc.Context().Done()
					seenDeadline.Store(rc.Context().Err() == context.DeadlineExceeded)
					return nil
				}
				aspects := []p.Aspect{p.Timeout(10 * time.Millisecond)}
				if kind != "Produce" {
					// Consume 的循环、Tee 的接收本来会返回取消；包装可以自行收尾成 nil。
					aspects = append(aspects, func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
						if err := next(rc); !errors.Is(err, context.DeadlineExceeded) {
							return fmt.Errorf("managed body returned %v, want deadline exceeded", err)
						}
						return observe(rc)
					})
				}
				opts := p.WithStreamAspects(aspects...)
				var inputs p.Inputs
				if kind != "Produce" {
					streamAspectsMust(t, g.DeclareInput(p.NewPort(in)))
					src := make(chan int, 1)
					if kind == "Consume" {
						src <- 7
						close(src)
					}
					streamAspectsMust(t, p.Seed(&inputs, in, (<-chan int)(src)))
				}
				switch kind {
				case "Produce":
					streamAspectsMust(t, p.Produce(g, "target", "", out, func(rc *p.RunCtx, _ func(int) error) error {
						bodyCalls.Add(1)
						return observe(rc)
					}, opts))
				case "Consume":
					streamAspectsMust(t, p.Consume(g, "target", "", in, func(rc *p.RunCtx, _ int) error {
						bodyCalls.Add(1)
						return observe(rc)
					}, opts))
				case "Tee":
					streamAspectsMust(t, p.Tee(g, "target", "", in, p.Keys(out), opts))
				}
				if kind != "Consume" {
					streamAspectsMust(t, p.Consume(g, "sink", "", out, func(*p.RunCtx, int) error { return nil }))
				}
				e := streamAspectsStart(t, ctx, g, inputs)
				err := e.Wait()
				if !seenDeadline.Load() || ctx.Err() != nil || (kind != "Tee" && bodyCalls.Load() != 1) {
					t.Fatalf("trial %d: local deadline observed=%v host=%v body calls=%d", trial, seenDeadline.Load(), ctx.Err(), bodyCalls.Load())
				}
				if err == nil || errors.Is(err, p.ErrSkipped) {
					t.Fatalf("trial %d: inner observed deadline and returned nil, but Wait = %v; snapshot=%+v", trial, err, e.Snapshot().Nodes)
				}
			}
		})
	}
}
