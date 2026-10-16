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
