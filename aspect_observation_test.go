package pulse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type aspectTrace struct {
	mu     sync.Mutex
	events []AspectEvent
}

func (p *aspectTrace) observer() Observer {
	return ObserverFunc{Aspect: func(e AspectEvent) {
		p.mu.Lock()
		p.events = append(p.events, e)
		p.mu.Unlock()
	}}
}

func (p *aspectTrace) snapshot() []AspectEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]AspectEvent(nil), p.events...)
}

func aspectPhases(events []AspectEvent) []AspectPhase {
	var phases []AspectPhase
	for _, e := range events {
		phases = append(phases, e.Phase)
	}
	return phases
}

func TestAspectObservationRetryFacts(t *testing.T) {
	cause := errors.New("attempt failed")
	for _, tc := range []struct {
		name   string
		fails  int
		skip   bool
		panic  bool
		limit  int
		calls  int
		reason NodeFinishReason
	}{
		{name: "first-success", limit: 3, calls: 1, reason: NodeCompleted},
		{name: "retry-success", fails: 1, limit: 3, calls: 2, reason: NodeCompleted},
		{name: "exhausted", fails: 9, limit: 3, calls: 3, reason: NodeFailed},
		{name: "skip", skip: true, limit: 3, calls: 1, reason: NodeSkipped},
		{name: "panic", panic: true, limit: 2, calls: 2, reason: NodeFailed},
		{name: "clamped", limit: 0, calls: 1, reason: NodeCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			trace := &aspectTrace{}
			var waiting, running, finished atomic.Int32
			legacy := ObserverFunc{
				Waiting:  func(Site, string) { waiting.Add(1) },
				Running:  func(Site, string) { running.Add(1) },
				Finished: func(Site, string, NodeFinishReason, error) { finished.Add(1) },
			}
			g := mustNew(t, ctx, "retry-facts")
			calls := 0
			mustAdd(t, g, NewNode("n", "自动事实", nil, nil, func(*RunCtx) error {
				calls++
				if tc.panic {
					panic(cause)
				}
				if tc.skip {
					return NoValue()
				}
				if calls <= tc.fails {
					return cause
				}
				return nil
			}, Retry(tc.limit, time.Millisecond)))
			err := g.Run(ctx, Inputs{}, WithExecObserver(MultiObserver{legacy, trace.observer()}))
			if (tc.reason == NodeFailed) != (err != nil) || calls != tc.calls {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			if waiting.Load() != 1 || running.Load() != 1 || finished.Load() != 1 {
				t.Fatalf("legacy counts=%d/%d/%d", waiting.Load(), running.Load(), finished.Load())
			}
			events := trace.snapshot()
			want := []AspectPhase{AspectStarted}
			for i := 1; i <= tc.calls; i++ {
				if i > 1 {
					want = append(want, AspectBackoffStarted, AspectBackoffFinished, AspectRetrying)
				}
				want = append(want, AspectAttemptStarted, AspectAttemptFinished)
			}
			want = append(want, AspectFinished)
			if got := aspectPhases(events); !reflect.DeepEqual(got, want) {
				t.Fatalf("phases=%v want=%v", got, want)
			}
			attempt := 0
			for _, e := range events {
				limit := max(1, tc.limit)
				if e.Site.Graph != "retry-facts" || e.Site.Execution == "" || e.Site.Path != "" || e.NodeID != "n" ||
					e.Kind != AspectRetry || e.Boundary != 1 || e.ParentBoundary != 0 || e.Limit != limit || e.Time.IsZero() {
					t.Fatalf("identity=%+v", e)
				}
				if e.Phase == AspectAttemptStarted {
					attempt++
					if e.Attempt != attempt || e.Origin != AttemptNew || e.Scope != "" {
						t.Fatalf("attempt=%+v", e)
					}
				}
				if e.Phase == AspectAttemptFinished {
					wantReason := NodeFailed
					if e.Attempt == tc.calls {
						wantReason = tc.reason
					}
					if e.Reason != wantReason || (e.Err != nil) != (wantReason == NodeFailed) || e.Duration < 0 {
						t.Fatalf("result=%+v want=%s", e, wantReason)
					}
				}
				if e.Phase == AspectBackoffFinished && (e.Delay != time.Millisecond || e.Duration <= 0 || e.Reason != NodeCompleted) {
					t.Fatalf("backoff=%+v", e)
				}
			}
			if events[len(events)-1].Reason != tc.reason {
				t.Fatalf("final=%+v", events[len(events)-1])
			}
		})
	}
}

func TestAspectObservationNestedBoundaries(t *testing.T) {
	ctx := checkpointTestContext(t)
	trace := &aspectTrace{}
	g := mustNew(t, ctx, "nested")
	calls := 0
	mustAdd(t, g, NewNode("n", "两个内层调用有不同边界", nil, nil, func(*RunCtx) error {
		calls++
		if calls <= 2 {
			return errors.New("retry outer")
		}
		return nil
	}, Retry(2, 0), Retry(2, 0)))
	if err := g.Run(ctx, Inputs{}, WithExecObserver(trace.observer())); err != nil {
		t.Fatal(err)
	}
	started := map[uint64]AspectEvent{}
	for _, e := range trace.snapshot() {
		if e.Phase == AspectStarted {
			if _, duplicate := started[e.Boundary]; duplicate {
				t.Fatalf("boundary reused: %+v", e)
			}
			started[e.Boundary] = e
		}
	}
	if calls != 3 || len(started) != 3 || started[1].ParentBoundary != 0 ||
		started[2].ParentBoundary != 1 || started[2].ParentAttempt != 1 ||
		started[3].ParentBoundary != 1 || started[3].ParentAttempt != 2 {
		t.Fatalf("calls=%d boundaries=%+v", calls, started)
	}
}

func TestRunCtxSiteConcurrentMountsAndFork(t *testing.T) {
	ctx := checkpointTestContext(t)
	trace := &aspectTrace{}
	child := mustNew(t, ctx, "child")
	value := NewKey[int]("value")
	declareOut(t, child, NewPort(value))
	sites := make(chan Site, 4)
	release := make(chan struct{})
	mustAdd(t, child, NewNode("leaf", "派生后位置仍一致", nil, Provides(value), func(rc *RunCtx) error {
		site := rc.Site()
		fork := rc.Fork()
		defer fork.Cancel()
		if fork.Site() != site {
			return fmt.Errorf("Fork changed site")
		}
		site.Graph = "local-copy"
		if rc.Site().Graph != "child" {
			return fmt.Errorf("Site exposed mutable state")
		}
		sites <- rc.Site()
		select {
		case <-release:
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
		return Set(rc, value, 7)
	}, Timeout(time.Second), Retry(2, 0)))
	root := mustNew(t, ctx, "root")
	for _, mount := range []string{"left", "right"} {
		if err := Sub(root, mount, "共用子图定义", []SubBind{Out(value, NewKey[int](mount))}, child); err != nil {
			t.Fatal(err)
		}
	}
	execs := make([]*Execution, 2)
	for i := range execs {
		var err error
		execs[i], err = root.NewExecution(ctx, Inputs{}, WithExecObserver(trace.observer()))
		if err != nil {
			t.Fatal(err)
		}
		if err := execs[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	var bodySites []Site
	for range 4 {
		select {
		case site := <-sites:
			bodySites = append(bodySites, site)
		case <-ctx.Done():
			t.Fatal("both root executions must overlap at both mounts")
		}
	}
	close(release)
	seen := map[string]Site{}
	for _, e := range execs {
		if err := e.Wait(); err != nil {
			t.Fatal(err)
		}
		for _, n := range e.Snapshot().Nodes {
			childState := n.Child.Snapshot().Graph
			seen[childState.Execution] = Site{Graph: childState.ID, Execution: childState.Execution, Path: childState.Path}
		}
	}
	if len(seen) != 4 {
		t.Fatalf("executions=%v", seen)
	}
	for _, site := range bodySites {
		if seen[site.Execution] != site || (site.Path != "left" && site.Path != "right") {
			t.Fatalf("body site=%+v", site)
		}
	}
	for _, e := range trace.snapshot() {
		if seen[e.Site.Execution] != e.Site {
			t.Fatalf("event site=%+v", e)
		}
	}
	if (&RunCtx{}).Site() != (Site{}) || (*RunCtx)(nil).Site() != (Site{}) {
		t.Fatal("zero context site")
	}
}

func TestAspectObservationCancelBackoffAndErrorOrigin(t *testing.T) {
	for _, mode := range []string{"backoff", "body-cancel", "rpc-error", "reliable-skip"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(checkpointTestContext(t))
			defer cancel()
			trace := &aspectTrace{}
			g := mustNew(t, ctx, "cancel")
			observer := MultiObserver{trace.observer(), ObserverFunc{Aspect: func(e AspectEvent) {
				if mode == "backoff" && e.Phase == AspectBackoffStarted {
					cancel()
				}
			}}}
			calls := 0
			limit, delay := 3, time.Hour
			if mode == "rpc-error" {
				limit, delay = 1, 0
			}
			mustAdd(t, g, NewNode("n", "取消与独立RPC错误分开", nil, nil, func(*RunCtx) error {
				calls++
				switch mode {
				case "body-cancel":
					cancel()
					return context.Canceled
				case "rpc-error":
					return context.DeadlineExceeded
				case "reliable-skip":
					return &CheckpointError{Operation: "test", Err: NoValue()}
				default:
					return errors.New("retry")
				}
			}, Retry(limit, delay)))
			if err := g.Run(ctx, Inputs{}, WithExecObserver(observer)); err == nil || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			events := trace.snapshot()
			last := events[len(events)-1]
			wantReason := NodeFailed
			if mode == "backoff" || mode == "body-cancel" {
				wantReason = NodeCanceled
			}
			if last.Reason != wantReason || last.Err == nil {
				t.Fatalf("final=%+v", last)
			}
			for _, e := range events {
				if e.Phase == AspectRetrying {
					t.Fatalf("canceled/nonretryable invocation reported a retry: %+v", e)
				}
				if e.Phase == AspectBackoffFinished && e.Reason != NodeCanceled {
					t.Fatalf("wait=%+v", e)
				}
			}
		})
	}
}

func TestAspectObservationTimeoutReportsOwnDeadline(t *testing.T) {
	for _, mode := range []string{"success", "own", "parent", "nested", "panic", "reliable"} {
		t.Run(mode, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			trace := &aspectTrace{}
			g := mustNew(t, ctx, "timeout-facts")
			aspects := []Aspect{Timeout(time.Millisecond)}
			if mode == "success" {
				// 此用例检查计时器未到期时的业务错误。
				aspects = []Aspect{Timeout(time.Hour)}
			}
			if mode == "parent" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Millisecond)
				defer cancel()
				aspects = []Aspect{Timeout(time.Hour)}
			}
			if mode == "nested" {
				aspects = append(aspects, Timeout(time.Hour))
			}
			mustAdd(t, g, NewNode("n", "到期不等于业务退出", nil, nil, func(rc *RunCtx) error {
				if mode == "success" {
					return errors.New("business says timeout, but timer never expired")
				}
				<-rc.Context().Done()
				if mode == "panic" {
					panic(NoValue())
				}
				if mode == "reliable" {
					return &CheckpointError{Operation: "test", Err: NoValue()}
				}
				return rc.Context().Err()
			}, aspects...))
			if err := g.Run(ctx, Inputs{}, WithExecObserver(trace.observer())); err == nil {
				t.Fatal("expected failure/cancellation")
			}
			deadlines := 0
			for _, e := range trace.snapshot() {
				if e.Phase == AspectDeadline {
					deadlines++
					if e.Boundary != 1 || e.Timeout != time.Millisecond || e.Reason != "" || e.Err != nil {
						t.Fatalf("deadline=%+v", e)
					}
				}
				if e.Phase == AspectFinished && e.Boundary == 1 {
					want := NodeFailed
					if mode == "parent" {
						want = NodeCanceled
					}
					if e.Reason != want || e.Err == nil {
						t.Fatalf("finished=%+v", e)
					}
				}
			}
			want := 1
			if mode == "parent" || mode == "success" {
				want = 0
			}
			if deadlines != want {
				t.Fatalf("deadlines=%d want=%d events=%+v", deadlines, want, trace.snapshot())
			}
		})
	}
}

func TestAspectObservationDeadlinePrecedesInnerExit(t *testing.T) {
	ctx := checkpointTestContext(t)
	deadline, release := make(chan struct{}), make(chan struct{})
	trace := &aspectTrace{}
	obs := MultiObserver{trace.observer(), ObserverFunc{Aspect: func(e AspectEvent) {
		if e.Phase == AspectDeadline {
			close(deadline)
		}
	}}}
	g := mustNew(t, ctx, "cooperation-facts")
	mustAdd(t, g, NewNode("n", "真实到期早于结束", nil, nil, func(rc *RunCtx) error {
		<-rc.Context().Done()
		<-release
		return nil
	}, Timeout(time.Millisecond)))
	e, err := g.NewExecution(ctx, Inputs{}, WithExecObserver(obs))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-deadline:
	case <-ctx.Done():
		close(release)
		t.Fatal("deadline was withheld until body exit")
	}
	if got := aspectPhases(trace.snapshot()); !reflect.DeepEqual(got, []AspectPhase{AspectStarted, AspectDeadline}) {
		t.Errorf("before exit=%v", got)
	}
	close(release)
	if err := e.Wait(); err == nil {
		t.Fatal("timeout lost")
	}
	if got := aspectPhases(trace.snapshot()); !reflect.DeepEqual(got, []AspectPhase{AspectStarted, AspectDeadline, AspectFinished}) {
		t.Fatalf("phases=%v", got)
	}
}

func TestAspectObservationNamedAndPanicIsolation(t *testing.T) {
	for _, panicBody := range []bool{false, true} {
		t.Run(fmt.Sprint(panicBody), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			trace := &aspectTrace{}
			var e *Execution
			bad := ObserverFunc{Aspect: func(AspectEvent) { panic(nil) }}
			read := ObserverFunc{Aspect: func(AspectEvent) { _ = e.Snapshot(); _ = e.Err() }}
			g := mustNew(t, ctx, "custom")
			custom := func(rc *RunCtx, next func(*RunCtx) error) error {
				if panicBody {
					panic(NoValue())
				}
				return next(rc)
			}
			mustAdd(t, g, NewNode("n", "显式名字", nil, nil, nil, NamedAspect("approval", custom)))
			var err error
			e, err = g.NewExecution(ctx, Inputs{}, WithExecObserver(MultiObserver{bad, read, trace.observer()}))
			if err != nil {
				t.Fatal(err)
			}
			if err = e.Start(); err != nil {
				t.Fatal(err)
			}
			err = e.Wait()
			if (err != nil) != panicBody {
				t.Fatalf("result=%v", err)
			}
			events := trace.snapshot()
			if len(events) != 2 || events[0].Kind != AspectCustom || events[0].Name != "approval" ||
				events[1].Phase != AspectFinished || (events[1].Reason == NodeFailed) != panicBody {
				t.Fatalf("events=%+v", events)
			}
		})
	}
}

// panic 发生在切面自己的错误归类中，而不是在已受保护的业务体内。
type aspectClassificationPanic struct{ value any }

func (*aspectClassificationPanic) Error() string   { return "diagnostic failure" }
func (e *aspectClassificationPanic) Unwrap() error { panic(e.value) }

func TestAspectObservationClassificationPanicFinishesAsFailure(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, observed := range []bool{false, true} {
			for _, nilPanic := range []bool{false, true} {
				for _, kind := range []AspectKind{AspectRetry, AspectTimeout} {
					t.Run(fmt.Sprintf("checkpoint=%t/observed=%t/nil=%t/%s", checkpoint, observed, nilPanic, kind), func(t *testing.T) {
						ctx := checkpointTestContext(t)
						trace := &aspectTrace{}
						var value any = "diagnostic Unwrap panicked"
						if nilPanic {
							value = nil
						}
						calls := 0
						aspect := Retry(2, 0)
						if kind == AspectTimeout {
							aspect = Timeout(time.Second)
						}
						g := mustNew(t, ctx, "classification-panic")
						mustAdd(t, g, NewNode("n", "本层归类异常不伪造成功", nil, nil, func(*RunCtx) error {
							calls++
							return &aspectClassificationPanic{value: value}
						}, aspect))
						var opts []ExecOption
						if checkpoint {
							opts = append(opts, WithExecCheckpoint("root", (&checkpointProbe{}).hooks()))
						}
						if observed {
							opts = append(opts, WithExecObserver(trace.observer()))
						}
						err := g.Run(ctx, Inputs{}, opts...)
						if !isPanicFailure(err) || calls != 1 {
							t.Fatalf("control panic changed business outcome: err=%v calls=%d", err, calls)
						}
						finished, started, attemptFinished := 0, 0, 0
						for _, event := range trace.snapshot() {
							switch event.Phase {
							case AspectAttemptStarted:
								started++
							case AspectAttemptFinished:
								attemptFinished++
								fallthrough
							case AspectFinished:
								if event.Reason != NodeFailed || !isPanicFailure(event.Err) {
									t.Fatalf("control panic reported a successful exit: %+v", event)
								}
								if event.Phase == AspectFinished {
									finished++
								}
							}
						}
						if observed && (finished != 1 || started != attemptFinished || (kind == AspectRetry && started != 1)) {
							t.Fatalf("unpaired control facts: finished=%d attempts=%d/%d", finished, started, attemptFinished)
						}
					})
				}
			}
		}
	}
}

func TestRetryStopsReliableFailureWithoutCheckpoint(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(fmt.Sprint(observed), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "reliable-failure")
			failure := &CheckpointError{Operation: "external-tool", Err: errors.New("unconfirmed")}
			calls := 0
			mustAdd(t, g, NewNode("n", "不重复不确定的可靠提交", nil, nil, func(*RunCtx) error {
				calls++
				return failure
			}, Retry(3, 0)))
			var opts []ExecOption
			if observed {
				opts = append(opts, WithExecObserver((&aspectTrace{}).observer()))
			}
			if err := g.Run(ctx, Inputs{}, opts...); !errors.Is(err, failure) || calls != 1 {
				t.Fatalf("reliable failure retried: err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestAspectObservationCleanupDoesNotCancelRPCFailure(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, observed := range []bool{false, true} {
			for _, kind := range []AspectKind{AspectRetry, AspectCustom} {
				for _, cleanup := range []bool{false, true} {
					for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
						t.Run(fmt.Sprintf("checkpoint=%t/observed=%t/%s/cleanup=%t/%v", checkpoint, observed, kind, cleanup, cause), func(t *testing.T) {
							ctx := checkpointTestContext(t)
							trace := &aspectTrace{}
							aspect := Retry(1, 0)
							if kind == AspectCustom {
								aspect = NamedAspect("rpc", func(rc *RunCtx, next func(*RunCtx) error) error { return next(rc) })
							}
							failure := fmt.Errorf("independent RPC: %w", cause)
							calls := 0
							g := mustNew(t, ctx, "rpc-cleanup-origin")
							mustAdd(t, g, NewNode("n", "清理本层上下文不改写RPC错误来源", nil, nil, func(rc *RunCtx) error {
								calls++
								if cleanup {
									defer rc.Cancel()
								}
								return failure
							}, aspect))
							var opts []ExecOption
							if checkpoint {
								opts = append(opts, WithExecCheckpoint("root", (&checkpointProbe{}).hooks()))
							}
							if observed {
								opts = append(opts, WithExecObserver(trace.observer()))
							}
							e, err := g.NewExecution(ctx, Inputs{}, opts...)
							managedTestOK(t, err)
							defer e.Cancel()
							managedTestOK(t, e.Start())
							if err := e.Wait(); !errors.Is(err, failure) || calls != 1 || ctx.Err() != nil {
								t.Fatalf("business result changed: err=%v calls=%d host=%v", err, calls, ctx.Err())
							}
							state := e.Snapshot()
							if state.Graph.Status != GraphStatusFailed || state.Nodes[0].Status != NodeStatusFailed {
								t.Fatalf("RPC failure lost: graph=%s node=%s", state.Graph.Status, state.Nodes[0].Status)
							}
							terminals := 0
							for _, event := range trace.snapshot() {
								if event.Phase != AspectFinished && event.Phase != AspectAttemptFinished {
									continue
								}
								terminals++
								if event.Reason != NodeFailed || !errors.Is(event.Err, failure) {
									t.Errorf("cleanup changed RPC observation: %+v", event)
								}
							}
							want := 0
							if observed {
								want = 1
								if kind == AspectRetry {
									want = 2
								}
							}
							if terminals != want {
								t.Fatalf("terminal event count=%d want=%d", terminals, want)
							}
						})
					}
				}
			}
		}
	}
}

func TestAspectObservationUnmarkedForkCancelIsFailure(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprint(deferred), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			trace := &aspectTrace{}
			g := mustNew(t, ctx, "unmarked-fork-origin")
			forked := func(rc *RunCtx, next func(*RunCtx) error) error {
				fork := rc.Fork()
				defer fork.Cancel()
				return next(fork)
			}
			mustAdd(t, g, NewNode("n", "无来源标记的局部取消不猜业务意图", nil, nil, func(rc *RunCtx) error {
				if deferred {
					defer rc.Cancel()
					return context.Canceled // 独立 RPC 错误，随后清理。
				}
				rc.Cancel()
				return rc.Context().Err() // 主动 Fork 取消，最终可读事实与上面相同。
			}, forked, NamedAspect("fork", func(rc *RunCtx, next func(*RunCtx) error) error { return next(rc) })))
			if err := g.Run(ctx, Inputs{}, WithExecObserver(trace.observer())); finishReason(err) != NodeFailed || !errors.Is(err, context.Canceled) {
				t.Fatalf("unmarked local result changed: %v", err)
			}
			events := trace.snapshot()
			if len(events) != 2 || events[1].Phase != AspectFinished || events[1].Reason != NodeFailed {
				t.Fatalf("unmarked Fork cancellation guessed intent: %+v", events)
			}
		})
	}
}

func TestAspectObservationPreservesCancellationOrigins(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		for _, kind := range []AspectKind{AspectRetry, AspectCustom} {
			for _, origin := range []string{"host", "outer-timeout", "forwarded"} {
				t.Run(fmt.Sprintf("checkpoint=%t/%s/%s", checkpoint, kind, origin), func(t *testing.T) {
					ctx, cancel := context.WithCancel(checkpointTestContext(t))
					defer cancel()
					trace := &aspectTrace{}
					aspect := Retry(1, 0)
					if kind == AspectCustom {
						aspect = NamedAspect("inner", func(rc *RunCtx, next func(*RunCtx) error) error { return next(rc) })
					}
					var aspects []Aspect
					if origin == "outer-timeout" {
						aspects = append(aspects, Timeout(time.Millisecond))
					}
					aspects = append(aspects, aspect)
					g := mustNew(t, ctx, "control-cancellation-origin")
					mustAdd(t, g, NewNode("n", "真实控制取消仍按取消记录", nil, nil, func(rc *RunCtx) error {
						if origin == "forwarded" {
							return &executionCancellation{cause: context.Canceled}
						}
						if origin == "host" {
							cancel()
						}
						<-rc.Context().Done()
						return rc.Context().Err()
					}, aspects...))
					var opts []ExecOption
					if checkpoint {
						opts = append(opts, WithExecCheckpoint("root", (&checkpointProbe{}).hooks()))
					}
					opts = append(opts, WithExecObserver(trace.observer()))
					if err := g.Run(ctx, Inputs{}, opts...); err == nil {
						t.Fatal("control cancellation was lost")
					}
					terminals := 0
					for _, event := range trace.snapshot() {
						if event.Kind != kind || (event.Phase != AspectFinished && event.Phase != AspectAttemptFinished) {
							continue
						}
						terminals++
						if event.Reason != NodeCanceled || !isCanceled(event.Err) {
							t.Errorf("real cancellation was reclassified: %+v", event)
						}
					}
					want := 1
					if kind == AspectRetry {
						want = 2
					}
					if terminals != want {
						t.Fatalf("terminal event count=%d want=%d", terminals, want)
					}
				})
			}
		}
	}
}

// 模拟桥在确认分配/认回时提供的事实，既不改变 checkpointProbe 的持久预算，也不从
// Execution ID 推造持久身份。真实 Durable/退出进程验证在跨仓联调完成。
func aspectCheckpointHooks(p *checkpointProbe) CheckpointHooks {
	hooks := p.hooks()
	open := hooks.OpenAttempt
	var mu sync.Mutex
	seen := map[string]bool{}
	hooks.OpenAttempt = func(ctx context.Context, req CheckpointAttemptRequest) (CheckpointAttempt, error) {
		a, err := open(ctx, req)
		mu.Lock()
		resumed := seen[a.Scope]
		seen[a.Scope] = true
		mu.Unlock()
		a.Resumed = &resumed
		return a, err
	}
	return hooks
}

func TestAspectObservationCheckpointResumeAndCompleteRestore(t *testing.T) {
	ctx := checkpointTestContext(t)
	firstCtx, stopFirst := context.WithCancel(ctx)
	defer stopFirst()
	p := &checkpointProbe{}
	hooks := aspectCheckpointHooks(p)
	g := mustNew(t, ctx, "durable-retry")
	var calls atomic.Int32
	mustAdd(t, g, NewNode("n", "认回编号不冒充新重试", nil, nil, func(*RunCtx) error {
		switch calls.Add(1) {
		case 1:
			return errors.New("first failed")
		case 2:
			stopFirst()
			return context.Canceled
		default:
			return nil
		}
	}, Retry(3, 0)))
	first, second := &aspectTrace{}, &aspectTrace{}
	if err := g.Run(firstCtx, Inputs{}, WithExecCheckpoint("root", hooks), WithExecObserver(first.observer())); !errors.Is(err, context.Canceled) {
		t.Fatalf("first=%v", err)
	}
	if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks), WithExecObserver(second.observer())); err != nil {
		t.Fatalf("resume=%v", err)
	}
	var active, resumed AspectEvent
	for _, e := range first.snapshot() {
		if e.Phase == AspectAttemptStarted && e.Attempt == 2 {
			active = e
		}
	}
	for _, e := range second.snapshot() {
		if e.Phase == AspectAttemptStarted {
			if resumed.Scope != "" {
				t.Fatal("resume allocated more than one attempt")
			}
			resumed = e
		}
		if e.Phase == AspectRetrying {
			t.Fatalf("resume invented a retry: %+v", e)
		}
	}
	if active.Origin != AttemptNew || resumed.Origin != AttemptResumed || resumed.Attempt != 2 ||
		active.Scope == "" || resumed.Scope != active.Scope || resumed.ParentScope == "" ||
		resumed.ParentScope != active.ParentScope || resumed.Site.Execution == active.Site.Execution {
		t.Fatalf("active=%+v resumed=%+v", active, resumed)
	}
	if len(p.failures) != 1 || len(p.commits) != 1 || calls.Load() != 3 {
		t.Fatalf("failures=%v commits=%d calls=%d", p.failures, len(p.commits), calls.Load())
	}
	trace := &aspectTrace{}
	e, err := g.NewExecution(ctx, Inputs{}, WithExecCheckpoint("root", hooks), WithExecObserver(trace.observer()))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RestoreNodes([]NodeCheckpoint{p.commits[0].Node}); err != nil {
		t.Fatal(err)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
	if len(trace.snapshot()) != 0 || calls.Load() != 3 || len(p.commits) != 1 {
		t.Fatalf("complete restore fabricated history: %+v calls=%d commits=%d", trace.snapshot(), calls.Load(), len(p.commits))
	}
}

func TestAspectObservationCheckpointCursorFacts(t *testing.T) {
	for _, mode := range []string{"new", "legacy", "skip", "reliable", "exhausted", "panic"} {
		t.Run(mode, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			p := &checkpointProbe{}
			hooks := aspectCheckpointHooks(p)
			if mode == "legacy" {
				hooks = p.hooks()
			}
			cause := errors.New("business")
			if mode == "reliable" {
				hooks.FailAttempt = func(context.Context, string) error { return NoValue() }
			}
			if mode == "exhausted" {
				open := hooks.OpenAttempt
				hooks.OpenAttempt = func(ctx context.Context, req CheckpointAttemptRequest) (CheckpointAttempt, error) {
					if req.Boundary != "" {
						return CheckpointAttempt{Scope: "spent", Index: req.Limit, Exhausted: true}, nil
					}
					return open(ctx, req)
				}
			}
			trace := &aspectTrace{}
			g := mustNew(t, ctx, "checkpoint-facts")
			calls := 0
			mustAdd(t, g, NewNode("n", "游标事实", nil, nil, func(*RunCtx) error {
				calls++
				if mode == "panic" {
					panic(NoValue())
				}
				if mode == "skip" {
					return NoValue()
				}
				if calls == 1 {
					return cause
				}
				return nil
			}, Retry(2, time.Millisecond)))
			err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", hooks), WithExecObserver(trace.observer()))
			failed := mode == "reliable" || mode == "exhausted" || mode == "panic"
			if (err != nil) != failed {
				t.Fatalf("err=%v", err)
			}
			wantCalls := 2
			if mode == "skip" || mode == "reliable" {
				wantCalls = 1
			}
			if mode == "exhausted" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("calls=%d want=%d", calls, wantCalls)
			}
			starts, retries := 0, 0
			for _, e := range trace.snapshot() {
				if e.Phase == AspectRetrying {
					retries++
				}
				if e.Phase == AspectAttemptStarted {
					starts++
					wantOrigin := AttemptNew
					if mode == "legacy" {
						wantOrigin = AttemptUnknown
					}
					if e.Attempt != starts || e.Scope == "" || e.ParentScope == "" || e.Origin != wantOrigin {
						t.Fatalf("cursor=%+v", e)
					}
				}
				if e.Phase == AspectAttemptFinished && mode == "reliable" && (e.Reason != NodeFailed || !isCheckpointFailure(e.Err)) {
					t.Fatalf("reliable error was downgraded: %+v", e)
				}
				if e.Phase == AspectAttemptFinished && mode == "skip" && (e.Reason != NodeSkipped || e.Err != nil) {
					t.Fatalf("skip=%+v", e)
				}
			}
			if starts != wantCalls || retries != max(0, wantCalls-1) {
				t.Fatalf("starts=%d retries=%d", starts, retries)
			}
			if mode == "exhausted" && !reflect.DeepEqual(aspectPhases(trace.snapshot()), []AspectPhase{AspectStarted, AspectFinished}) {
				t.Fatalf("exhausted history=%v", aspectPhases(trace.snapshot()))
			}
		})
	}
}

func TestAspectObservationCheckpointNestedScopes(t *testing.T) {
	ctx := checkpointTestContext(t)
	p := &checkpointProbe{}
	trace := &aspectTrace{}
	g := mustNew(t, ctx, "nested-checkpoint")
	calls := 0
	mustAdd(t, g, NewNode("n", "局部边界与持久游标分开", nil, nil, func(*RunCtx) error {
		calls++
		if calls < 3 {
			return fmt.Errorf("failure %d", calls)
		}
		return nil
	}, Retry(2, 0), Retry(2, 0)))
	if err := g.Run(ctx, Inputs{}, WithExecCheckpoint("root", aspectCheckpointHooks(p)), WithExecObserver(trace.observer())); err != nil {
		t.Fatal(err)
	}
	outerScopes := map[int]string{}
	for _, e := range trace.snapshot() {
		if e.Phase == AspectAttemptStarted && e.Boundary == 1 {
			outerScopes[e.Attempt] = e.Scope
		}
		if e.Phase == AspectAttemptStarted && e.Boundary != 1 {
			if e.ParentBoundary != 1 || e.ParentScope != outerScopes[e.ParentAttempt] || e.Scope == e.ParentScope {
				t.Fatalf("nested association=%+v outer=%v", e, outerScopes)
			}
		}
	}
	if len(outerScopes) != 2 || outerScopes[1] == outerScopes[2] {
		t.Fatalf("outer=%v", outerScopes)
	}
}
