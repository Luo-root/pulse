package pulse_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	p "github.com/Luo-root/pulse"
)

func sugarSource(t *testing.T, g *p.Graph, key p.Key[p.Stream[int]], values ...int) {
	t.Helper()
	managedPublicOK(t, p.ProduceStream(g, "source", "渐进来源", key, func(_ *p.RunCtx, writer *p.StreamWriter[int]) error {
		for _, value := range values {
			if err := writer.Send(value); err != nil {
				return err
			}
		}
		return nil
	}, p.WithBuffer(1)))
}

func TestStreamSugarPipelineOneQuotaAndAspects(t *testing.T) {
	var trace streamAspectTrace
	observer := p.ObserverFunc{
		Waiting:  func(_ p.Site, id string) { trace.add(id, "waiting") },
		Running:  func(_ p.Site, id string) { trace.add(id, "running") },
		Finished: func(_ p.Site, id string, _ p.NodeFinishReason, _ error) { trace.add(id, "finished") },
	}
	g := managedPublicGraph(t, "sugar-pipeline", p.WithMaxRunning(1), p.WithObserver(observer), p.WithAspects(trace.aspect("graph")))
	input, a, b := p.NewKey[p.Stream[int]]("input"), p.NewKey[p.Stream[int]]("a"), p.NewKey[p.Stream[int]]("b")
	caller := []p.Aspect{trace.aspect("local-1"), trace.aspect("local-2")}
	local := p.WithStreamAspects(caller...)
	caller[0] = trace.aspect("changed")
	opts := []p.StreamOption{nil, local, p.WithStreamAspects(trace.aspect("local-3"))}
	var active, peak atomic.Int32
	business := func() {
		n := active.Add(1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		runtime.Gosched()
		active.Add(-1)
	}
	var want []int
	for i := 1; i <= 20; i++ {
		want = append(want, i)
	}
	managedPublicOK(t, p.ProduceStream(g, "source", "生产说明", input, func(_ *p.RunCtx, writer *p.StreamWriter[int]) error {
		for _, value := range want {
			business()
			if err := writer.Send(value); err != nil {
				return err
			}
		}
		return nil
	}, append(opts, p.WithBuffer(1))...))
	outs := p.Keys(a, b)
	managedPublicOK(t, p.TeeStream(g, "tee", "广播说明", input, outs, append(opts, p.WithBuffer(1))...))
	outs[0] = p.NewKey[p.Stream[int]]("changed-output")
	var got [2][]int
	for i, key := range p.Keys(a, b) {
		managedPublicOK(t, p.ConsumeStream(g, fmt.Sprint("sink", i), "消费说明", key, func(_ *p.RunCtx, value int) error {
			business()
			got[i] = append(got[i], value)
			return nil
		}, opts...))
	}
	managedPublicOK(t, g.Add(p.NewNode("ordinary", "普通说明", nil, nil, func(*p.RunCtx) error { business(); return nil },
		trace.aspect("local-1"), trace.aspect("local-2"), trace.aspect("local-3"))))
	caller[1] = trace.aspect("changed-after-assembly")
	opts[1] = p.WithStreamAspects(trace.aspect("changed-options"))
	e := streamAspectsStart(t, managedPublicContext(t), g, p.Inputs{}, p.WithExecAspects(trace.aspect("exec")))
	managedPublicOK(t, e.Wait())
	if !reflect.DeepEqual(got, [2][]int{want, want}) || peak.Load() != 1 {
		t.Fatalf("broadcast=%v peak business=%d, want complete ordered copies / 1", got, peak.Load())
	}
	for _, id := range []string{"source", "tee", "sink0", "sink1", "ordinary"} {
		trace.expect(t, id, []string{"graph:in", "exec:in", "local-1:in", "local-2:in", "local-3:in", "waiting", "running",
			"local-3:out", "local-2:out", "local-1:out", "exec:out", "graph:out", "finished"})
	}
	for i, description := range []string{"生产说明", "广播说明", "消费说明", "消费说明", "普通说明"} {
		if g.Topology().Nodes[i].Description != description || e.Snapshot().Nodes[i].Description != description {
			t.Fatalf("node %d lost its description", i)
		}
	}
}

// 慢支的第一条尚未完成时，输入第一条已确认；但第二批不能突破该支的容量。
func TestStreamSugarTeeBackpressureWaitsForCapacity(t *testing.T) {
	g := managedPublicGraph(t, "sugar-backpressure", p.WithMaxRunning(1))
	input, slow, fast := p.NewKey[p.Stream[int]]("input"), p.NewKey[p.Stream[int]]("slow"), p.NewKey[p.Stream[int]]("fast")
	sentTwo, sentThree, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var source p.Stream[int]
	managedPublicOK(t, p.ProduceStream(g, "source", "", input, func(rc *p.RunCtx, writer *p.StreamWriter[int]) error {
		var err error
		source, err = p.Get(rc, input)
		if err != nil {
			return err
		}
		for value := 1; value <= 3; value++ {
			if err := writer.Send(value); err != nil {
				return err
			}
			if value == 2 {
				close(sentTwo)
			}
		}
		close(sentThree)
		return nil
	}, p.WithBuffer(1)))
	managedPublicOK(t, p.TeeStream(g, "tee", "", input, p.Keys(slow, fast), p.WithBuffer(1)))
	seen := [2]chan p.Stream[int]{make(chan p.Stream[int], 1), make(chan p.Stream[int], 1)}
	var values [2][]int
	for i, key := range p.Keys(slow, fast) {
		managedPublicOK(t, p.ConsumeStream(g, fmt.Sprint("sink", i), "", key, func(rc *p.RunCtx, value int) error {
			values[i] = append(values[i], value)
			if value == 1 {
				endpoint, err := p.Get(rc, key)
				if err != nil {
					return err
				}
				seen[i] <- endpoint
				if i == 0 {
					return rc.WaitStream(func(ctx context.Context) error {
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					})
				}
			}
			return nil
		}))
	}
	ctx := managedPublicContext(t)
	e := streamAspectsStart(t, ctx, g, p.Inputs{})
	slowEndpoint, fastEndpoint := streamAspectsAwait(t, ctx, seen[0]), streamAspectsAwait(t, ctx, seen[1])
	streamAspectsAwait(t, ctx, sentTwo)
	if got := source.Snapshot(); got.LastSequence != 2 || got.AckedThrough != 1 || got.InFlight != 1 {
		t.Fatalf("Tee waited for descendant work or overran input capacity: %+v", got)
	}
	for _, endpoint := range []p.Stream[int]{slowEndpoint, fastEndpoint} {
		if got := endpoint.Snapshot(); got.LastSequence != 1 || got.InFlight > 1 {
			t.Fatalf("Tee overran a branch while the slow callback was active: %+v", got)
		}
	}
	select {
	case <-sentThree:
		t.Fatal("producer exceeded bounded backpressure")
	default:
	}
	close(release)
	managedPublicOK(t, e.Wait())
	if !reflect.DeepEqual(values, [2][]int{{1, 2, 3}, {1, 2, 3}}) {
		t.Fatalf("bounded broadcast=%v", values)
	}
}

func TestStreamSugarConsumeRetriesOnlyCurrentMessage(t *testing.T) {
	g := managedPublicGraph(t, "sugar-message-retry", p.WithMaxRunning(1))
	key := p.NewKey[p.Stream[int]]("input")
	sugarSource(t, g, key, 1, 2, 3)
	var callbacks []int
	failed := false
	var endpoint p.Stream[int]
	managedPublicOK(t, p.ConsumeStream(g, "sink", "消息重试", key, func(rc *p.RunCtx, value int) error {
		var err error
		endpoint, err = p.Get(rc, key)
		if err != nil {
			return err
		}
		callbacks = append(callbacks, value)
		if value == 1 {
			return p.NoValue("normal-message-skip")
		}
		if value == 2 && !failed {
			failed = true
			return errors.New("retry only two")
		}
		return nil
	}, p.WithMessageHandling(nil, p.WithMessageRetry(2, 0))))
	managedPublicOK(t, g.Run(managedPublicContext(t), p.Inputs{}))
	if !reflect.DeepEqual(callbacks, []int{1, 2, 2, 3}) || endpoint.Snapshot().AckedThrough != 3 {
		t.Fatalf("callbacks=%v stream=%+v", callbacks, endpoint.Snapshot())
	}
}

// 第一支已暂存、第二支超预算，整批必须不发布，输入也不能确认。
func TestStreamSugarTeeFailedAttemptPublishesNothing(t *testing.T) {
	g := managedPublicGraph(t, "sugar-atomic-tee", p.WithMaxRunning(1))
	input, a, b := p.NewKey[p.Stream[int]]("input"), p.NewKey[p.Stream[int]]("a"), p.NewKey[p.Stream[int]]("b")
	sugarSource(t, g, input, 7)
	var endpoints [3]p.Stream[int]
	capture := func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
		err := next(rc)
		for i, key := range p.Keys(input, a, b) {
			endpoint, ok, _, readErr := p.TryGet(rc, key)
			if readErr != nil || !ok {
				return fmt.Errorf("capture %d: ready=%t err=%v", i, ok, readErr)
			}
			endpoints[i] = endpoint
		}
		return err
	}
	managedPublicOK(t, p.TeeStream(g, "tee", "不发布失败尝试", input, p.Keys(a, b), p.WithBuffer(1),
		p.WithMessageHandling(p.WithMessageRetry(2, 0), p.WithMessageLimit(1)), p.WithStreamAspects(capture)))
	var called atomic.Int32
	for i, key := range p.Keys(a, b) {
		managedPublicOK(t, p.ConsumeStream(g, fmt.Sprint("sink", i), "", key, func(*p.RunCtx, int) error { called.Add(1); return nil }))
	}
	if err := g.Run(managedPublicContext(t), p.Inputs{}); err == nil || !strings.Contains(err.Error(), "message output limit 1 exceeded") {
		t.Fatalf("limited TeeStream = %v", err)
	}
	if called.Load() != 0 || endpoints[0].Snapshot().AckedThrough != 0 {
		t.Fatalf("failed attempt ran downstream or confirmed input: calls=%d input=%+v", called.Load(), endpoints[0].Snapshot())
	}
	for _, endpoint := range endpoints[1:] {
		if got := endpoint.Snapshot(); got.LastSequence != 0 || got.InFlight != 0 {
			t.Fatalf("failed Tee attempt leaked output: %+v", got)
		}
	}
}

func TestStreamSugarProgressiveBeforeProducerReturns(t *testing.T) {
	g := managedPublicGraph(t, "sugar-progressive", p.WithMaxRunning(1))
	key := p.NewKey[p.Stream[int]]("input")
	seen, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	managedPublicOK(t, p.ProduceStream(g, "source", "", key, func(_ *p.RunCtx, writer *p.StreamWriter[int]) error {
		defer close(returned)
		if err := writer.Send(1); err != nil {
			return err
		}
		return writer.Send(2)
	}, p.WithBuffer(1)))
	var values []int
	managedPublicOK(t, p.ConsumeStream(g, "sink", "", key, func(rc *p.RunCtx, value int) error {
		values = append(values, value)
		if value == 1 {
			close(seen)
			return rc.WaitStream(func(ctx context.Context) error {
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}
		return nil
	}))
	ctx := managedPublicContext(t)
	e := streamAspectsStart(t, ctx, g, p.Inputs{})
	streamAspectsAwait(t, ctx, seen)
	select {
	case <-returned:
		t.Fatal("first message was hidden until the producer callback returned")
	default:
	}
	close(release)
	managedPublicOK(t, e.Wait())
	if !reflect.DeepEqual(values, []int{1, 2}) {
		t.Fatalf("progressive values=%v", values)
	}
}

func TestStreamSugarEmptyAndUnopened(t *testing.T) {
	for _, opened := range []bool{false, true} {
		t.Run(fmt.Sprint(opened), func(t *testing.T) {
			g := managedPublicGraph(t, "sugar-empty", p.WithMaxRunning(1))
			input, output := p.NewKey[p.Stream[int]]("input"), p.NewKey[p.Stream[int]]("output")
			if opened {
				sugarSource(t, g, input)
			} else {
				managedPublicOK(t, g.Add(p.NewNode("source", "未打开", nil, p.Provides(input), func(*p.RunCtx) error { return nil })))
			}
			managedPublicOK(t, p.TeeStream(g, "tee", "", input, p.Keys(output)))
			var callbacks atomic.Int32
			managedPublicOK(t, p.ConsumeStream(g, "sink", "", output, func(*p.RunCtx, int) error { callbacks.Add(1); return nil }))
			e := streamAspectsStart(t, managedPublicContext(t), g, p.Inputs{})
			managedPublicOK(t, e.Wait())
			status := p.NodeStatusSkipped
			if opened {
				status = p.NodeStatusCompleted
			}
			for _, node := range e.Snapshot().Nodes[1:] {
				if node.Status != status || callbacks.Load() != 0 {
					t.Fatalf("empty/unopened node=%+v callbacks=%d", node, callbacks.Load())
				}
			}
		})
	}
}

func TestStreamSugarConfigurationRejectedAtomically(t *testing.T) {
	for _, target := range []string{"consume-buffer", "produce-message", "tee-duplicate", "tee-empty", "produce-nil-fn", "consume-nil-fn"} {
		t.Run(target, func(t *testing.T) {
			g := managedPublicGraph(t, "sugar-config")
			input, out := p.NewKey[p.Stream[int]]("input"), p.NewKey[p.Stream[int]]("out")
			before := g.Topology()
			var err error
			switch target {
			case "consume-buffer":
				err = p.ConsumeStream(g, "node", "", input, func(*p.RunCtx, int) error { return nil }, p.WithBuffer(2))
			case "produce-message":
				err = p.ProduceStream(g, "node", "", out, func(*p.RunCtx, *p.StreamWriter[int]) error { return nil }, p.WithMessageHandling(p.WithMessageRetry(2, 0)))
			case "tee-duplicate":
				err = p.TeeStream(g, "node", "", input, p.Keys(out, out))
			case "tee-empty":
				err = p.TeeStream(g, "node", "", input, nil)
			case "produce-nil-fn":
				err = p.ProduceStream(g, "node", "", out, nil)
			case "consume-nil-fn":
				err = p.ConsumeStream(g, "node", "", input, nil)
			}
			if err == nil || !reflect.DeepEqual(before, g.Topology()) {
				t.Fatalf("invalid configuration changed definition: err=%v topology=%+v", err, g.Topology())
			}
			managedPublicOK(t, p.ProduceStream(g, "node", "可复用原 ID", out, func(*p.RunCtx, *p.StreamWriter[int]) error { return nil }))
		})
	}
}

func TestStreamSugarWholeNodeRetryStillRejected(t *testing.T) {
	for _, target := range []string{"produce", "tee", "consume"} {
		t.Run(target, func(t *testing.T) {
			g := managedPublicGraph(t, "sugar-retry-guard", p.WithMaxRunning(1))
			input, output := p.NewKey[p.Stream[int]]("input"), p.NewKey[p.Stream[int]]("output")
			option := func(kind string) p.StreamOption {
				if kind == target {
					return p.WithStreamAspects(p.Retry(2, 0))
				}
				return nil
			}
			managedPublicOK(t, p.ProduceStream(g, "source", "", input, func(_ *p.RunCtx, writer *p.StreamWriter[int]) error { return writer.Send(1) }, option("produce")))
			managedPublicOK(t, p.TeeStream(g, "tee", "", input, p.Keys(output), option("tee")))
			managedPublicOK(t, p.ConsumeStream(g, "sink", "", output, func(*p.RunCtx, int) error { return nil }, option("consume")))
			if err := g.Run(managedPublicContext(t), p.Inputs{}); err == nil || !strings.Contains(err.Error(), "write set") {
				t.Fatalf("ordinary Retry on %s = %v", target, err)
			}
		})
	}
}

func TestStreamSugarProducerFullChainLateFailure(t *testing.T) {
	g := managedPublicGraph(t, "sugar-late-failure", p.WithMaxRunning(1))
	input, output := p.NewKey[p.Stream[int]]("input"), p.NewKey[p.Stream[int]]("output")
	bodyReturned, release := make(chan struct{}), make(chan struct{})
	late := errors.New("outer producer aspect failed")
	var source p.Stream[int]
	aspect := func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
		if err := next(rc); err != nil {
			return err
		}
		var err error
		source, err = p.Get(rc, input)
		if err != nil {
			return err
		}
		close(bodyReturned)
		if err := rc.WaitStream(func(ctx context.Context) error {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}); err != nil {
			return err
		}
		return late
	}
	managedPublicOK(t, p.ProduceStream(g, "source", "", input, func(*p.RunCtx, *p.StreamWriter[int]) error { return nil }, p.WithStreamAspects(aspect)))
	results := make(chan error, 2)
	probe := p.WithStreamAspects(func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
		err := next(rc)
		results <- err
		return err
	})
	managedPublicOK(t, p.TeeStream(g, "tee", "", input, p.Keys(output), probe))
	managedPublicOK(t, p.ConsumeStream(g, "sink", "", output, func(*p.RunCtx, int) error { return nil }, probe))
	ctx := managedPublicContext(t)
	e := streamAspectsStart(t, ctx, g, p.Inputs{})
	streamAspectsAwait(t, ctx, bodyReturned)
	select {
	case <-source.Done():
		t.Fatal("source completed before its outer aspect")
	case err := <-results:
		t.Fatalf("downstream completed before source chain: %v", err)
	default:
	}
	close(release)
	if err := e.Wait(); !errors.Is(err, late) {
		t.Fatalf("late producer failure=%v", err)
	}
	for range 2 {
		if err := streamAspectsAwait(t, ctx, results); err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("late failure turned into successful EOF: %v", err)
		}
	}
	if got := source.Snapshot(); got.Terminal != p.StreamFailed || !errors.Is(got.Err, late) {
		t.Fatalf("source terminal=%+v", got)
	}
}

func TestStreamSugarConsumeExplicitStop(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(fmt.Sprint(wrapped), func(t *testing.T) {
			g := managedPublicGraph(t, "sugar-stop", p.WithMaxRunning(1))
			key := p.NewKey[p.Stream[int]]("input")
			var endpoint p.Stream[int]
			managedPublicOK(t, p.ProduceStream(g, "source", "", key, func(rc *p.RunCtx, writer *p.StreamWriter[int]) error {
				var err error
				endpoint, err = p.Get(rc, key)
				if err != nil {
					return err
				}
				for value := 1; ; value++ {
					err := writer.Send(value)
					if err == p.ErrStreamStopped {
						return nil
					}
					if err != nil {
						return err
					}
				}
			}, p.WithBuffer(1)))
			cause := errors.New("business failed")
			var values []int
			managedPublicOK(t, p.ConsumeStream(g, "sink", "", key, func(_ *p.RunCtx, value int) error {
				values = append(values, value)
				if value == 2 {
					if wrapped {
						return errors.Join(cause, p.ErrStreamStopped)
					}
					return p.ErrStreamStopped
				}
				return nil
			}))
			err := g.Run(managedPublicContext(t), p.Inputs{})
			got := endpoint.Snapshot()
			if wrapped {
				if !errors.Is(err, cause) || got.Stopped || got.AckedThrough != 1 {
					t.Fatalf("wrapped failure swallowed: err=%v stream=%+v", err, got)
				}
			} else if err != nil || !got.Stopped || got.AckedThrough != 2 || got.InFlight != 0 || got.Terminal != p.StreamStopped {
				t.Fatalf("explicit stop: err=%v stream=%+v", err, got)
			}
			if !reflect.DeepEqual(values, []int{1, 2}) {
				t.Fatalf("stop processed extra messages: %v", values)
			}
		})
	}
}

func TestStreamSugarTeeStoppedBranchesAndLateSourceFailure(t *testing.T) {
	for _, allStopped := range []bool{false, true} {
		for _, lateFailure := range []bool{false, true} {
			t.Run(fmt.Sprint(allStopped, lateFailure), func(t *testing.T) {
				g := managedPublicGraph(t, "sugar-broadcast-stop", p.WithMaxRunning(1))
				input, a, b := p.NewKey[p.Stream[int]]("input"), p.NewKey[p.Stream[int]]("a"), p.NewKey[p.Stream[int]]("b")
				sourceReturned, release, fullSeen := make(chan struct{}), make(chan struct{}), make(chan struct{})
				stopped := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
				cause := errors.New("source failed after branches stopped")
				var source p.Stream[int]
				sourceAspect := func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
					err := next(rc)
					close(sourceReturned)
					if err != nil || !lateFailure {
						return err
					}
					if err := rc.WaitStream(func(ctx context.Context) error {
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					}); err != nil {
						return err
					}
					return cause
				}
				managedPublicOK(t, p.ProduceStream(g, "source", "", input, func(rc *p.RunCtx, writer *p.StreamWriter[int]) error {
					var err error
					source, err = p.Get(rc, input)
					if err != nil {
						return err
					}
					for value := 1; allStopped || value <= 3; value++ {
						if err := writer.Send(value); err != nil {
							return err // 全分支 Stop 后交给 ProduceStream 核验精确信号。
						}
					}
					return nil
				}, p.WithBuffer(1), p.WithStreamAspects(sourceAspect)))
				managedPublicOK(t, p.TeeStream(g, "tee", "", input, p.Keys(a, b), p.WithBuffer(1)))
				var values [2][]int
				var endpoints [2]p.Stream[int]
				for i, key := range p.Keys(a, b) {
					stop := i == 0 || allStopped
					probe := p.WithStreamAspects(func(rc *p.RunCtx, next func(*p.RunCtx) error) error {
						err := next(rc)
						endpoints[i], _, _, _ = p.TryGet(rc, key)
						if stop {
							close(stopped[i])
						}
						return err
					})
					managedPublicOK(t, p.ConsumeStream(g, fmt.Sprint("sink", i), "", key, func(_ *p.RunCtx, value int) error {
						values[i] = append(values[i], value)
						if stop {
							return p.ErrStreamStopped
						}
						if value == 3 {
							close(fullSeen)
						}
						return nil
					}, probe))
				}
				ctx := managedPublicContext(t)
				e := streamAspectsStart(t, ctx, g, p.Inputs{})
				streamAspectsAwait(t, ctx, stopped[0])
				if allStopped {
					streamAspectsAwait(t, ctx, stopped[1])
				} else {
					streamAspectsAwait(t, ctx, fullSeen)
				}
				streamAspectsAwait(t, ctx, sourceReturned)
				if lateFailure {
					select {
					case <-source.Done():
						t.Fatal("Stop settled source before its complete chain")
					default:
					}
					close(release)
				}
				err := e.Wait()
				if lateFailure && !errors.Is(err, cause) || !lateFailure && err != nil {
					t.Fatalf("broadcast Stop lost source conclusion: %v", err)
				}
				want := [2][]int{{1}, {1, 2, 3}}
				if allStopped {
					want[1] = []int{1}
				}
				if !reflect.DeepEqual(values, want) || source.Snapshot().Stopped != allStopped {
					t.Fatalf("stopped broadcast values=%v source=%+v", values, source.Snapshot())
				}
				for i, endpoint := range endpoints {
					if i == 0 || allStopped {
						if got := endpoint.Snapshot(); !got.Stopped || got.AckedThrough != 1 || got.InFlight != 0 {
							t.Fatalf("Stop confirmed unread branch messages: %+v", got)
						}
					}
				}
				if lateFailure && !errors.Is(source.Snapshot().Err, cause) {
					t.Fatalf("Stop hid late source failure: %+v", source.Snapshot())
				}
			})
		}
	}
}

func TestStreamSugarProducerStopNormalizationRequiresExactError(t *testing.T) {
	for _, mode := range []string{"not-stopped", "wrapped", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			g := managedPublicGraph(t, "sugar-stop-normalization", p.WithMaxRunning(1))
			key := p.NewKey[p.Stream[int]]("input")
			cause := errors.New("wrapped source failure")
			want := cause
			if mode == "not-stopped" {
				want = p.ErrStreamStopped
			} else if mode == "canceled" {
				want = context.Canceled
			}
			managedPublicOK(t, p.ProduceStream(g, "source", "", key, func(_ *p.RunCtx, writer *p.StreamWriter[int]) error {
				if mode != "wrapped" {
					return want
				}
				for value := 1; ; value++ {
					if err := writer.Send(value); err != nil {
						return errors.Join(cause, err)
					}
				}
			}))
			managedPublicOK(t, p.ConsumeStream(g, "sink", "", key, func(*p.RunCtx, int) error { return p.ErrStreamStopped }))
			if err := g.Run(managedPublicContext(t), p.Inputs{}); !errors.Is(err, want) {
				t.Fatalf("source failure was normalized: mode=%s err=%v want=%v", mode, err, want)
			}
		})
	}
}
