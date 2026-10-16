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
