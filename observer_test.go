package pulse

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordingObserver struct {
	mu   sync.Mutex
	log  []string
	errs []error
}

func (r *recordingObserver) OnNodeWaiting(graphID, id string) {
	r.mu.Lock()
	r.log = append(r.log, "W:"+id)
	r.mu.Unlock()
}
func (r *recordingObserver) OnNodeRunning(graphID, id string) {
	r.mu.Lock()
	r.log = append(r.log, "R:"+id)
	r.mu.Unlock()
}
func (r *recordingObserver) OnNodeFinished(graphID, id string, reason NodeFinishReason, err error) {
	r.mu.Lock()
	r.log = append(r.log, "F:"+id+":"+string(reason))
	r.errs = append(r.errs, err)
	r.mu.Unlock()
}

func (r *recordingObserver) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]string(nil), r.log...)
	return out
}

func countPref(log []string, prefix string) int {
	n := 0
	for _, s := range log {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

func TestObserverLinearWaitRunFinished(t *testing.T) {
	obs := &recordingObserver{}
	in := NewKey[string]("obs.in")
	out := NewKey[string]("obs.out")
	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	if err := Seed(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(NewNode("n", Requires(in), Provides(out), func(rc *RunCtx) error {
		v, err := Get(rc, in)
		if err != nil {
			return err
		}
		return Set(rc, out, v)
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	log := obs.snapshot()
	if countPref(log, "W:n") != 1 || countPref(log, "R:n") != 1 || countPref(log, "F:n:completed") != 1 {
		t.Fatalf("lifecycle = %v", log)
	}
	// 顺序：Waiting → Running → Finished
	wi, ri, fi := -1, -1, -1
	for i, s := range log {
		switch s {
		case "W:n":
			wi = i
		case "R:n":
			ri = i
		case "F:n:completed":
			fi = i
		}
	}
	if !(wi < ri && ri < fi) {
		t.Fatalf("order want W<R<F, got %v", log)
	}
}

func TestObserverSkipHasFinishedNoRunning(t *testing.T) {
	obs := &recordingObserver{}
	a := NewKey[string]("obs.a")
	b := NewKey[string]("obs.b")
	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	if err := SkipSeed(g, a); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(NewNode("down", Requires(a), Provides(b), func(rc *RunCtx) error {
		t.Fatal("Run must not execute when input skipped")
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	log := obs.snapshot()
	if countPref(log, "W:down") != 1 {
		t.Fatalf("want Waiting once, got %v", log)
	}
	if countPref(log, "R:down") != 0 {
		t.Fatalf("Skip path must not emit Running: %v", log)
	}
	if countPref(log, "F:down:skipped") != 1 {
		t.Fatalf("want Finished skipped, got %v", log)
	}
}

func TestObserverRetryEmitsOnce(t *testing.T) {
	obs := &recordingObserver{}
	out := NewKey[string]("obs.retry.out")
	var attempts atomic.Int32
	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	if err := g.Add(NewNode("flaky", nil, Provides(out), func(rc *RunCtx) error {
		if attempts.Add(1) < 3 {
			return errors.New("try again")
		}
		return Set(rc, out, "ok")
	}, Retry(5, time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
	log := obs.snapshot()
	if countPref(log, "W:flaky") != 1 || countPref(log, "R:flaky") != 1 || countPref(log, "F:flaky:completed") != 1 {
		t.Fatalf("Retry must not double-fire lifecycle: %v", log)
	}
}

func TestObserverPanicIsolated(t *testing.T) {
	boom := ObserverFunc{
		Waiting: func(_, _ string) { panic("observer boom") },
		Running: func(_, _ string) {},
		Finished: func(_, _ string, _ NodeFinishReason, _ error) {
			panic("finished boom")
		},
	}
	out := NewKey[int]("obs.panic.out")
	g := mustNew(t, context.Background(), "test", WithObserver(boom))
	if err := g.Add(NewNode("ok", nil, Provides(out), func(rc *RunCtx) error {
		return Set(rc, out, 1)
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatalf("observer panic must not fail graph: %v", err)
	}
}

func TestObserverTimeoutFinishedFailedNoRunning(t *testing.T) {
	obs := &recordingObserver{}
	in := NewKey[string]("obs.to.in")
	out := NewKey[string]("obs.to.out")
	// 全局切面在这里换个用法：`late` 是 in 的来源（Start 的来源校验要求有），
	// 但它迟迟不写——若把超时挂在全局，先超时的会是 late 自己，它一失败就取消
	// 整图，blocked 的等待段会以 canceled 收场，就测不到「等待段超时」这条路径。
	// 所以超时挂在 blocked 自己的切面上。
	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	if err := g.Add(NewNode("late", nil, Provides(in), func(rc *RunCtx) error {
		<-rc.Context().Done()
		return rc.Context().Err()
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(NewNode("blocked", Requires(in), Provides(out), func(rc *RunCtx) error {
		t.Fatal("should not run")
		return nil
	}, Timeout(30*time.Millisecond))); err != nil {
		t.Fatal(err)
	}
	err := g.Run()
	if err == nil {
		t.Fatal("want timeout error")
	}
	log := obs.snapshot()
	if countPref(log, "R:blocked") != 0 {
		t.Fatalf("timeout during wait must not emit Running: %v", log)
	}
	if countPref(log, "F:blocked:failed") != 1 {
		t.Fatalf("timeout Finished reason want failed, got %v", log)
	}
	// 生产者是被整图取消带走的，那一边才是 canceled——两条原因不能混为一谈。
	if countPref(log, "F:late:canceled") != 1 {
		t.Fatalf("producer Finished reason want canceled, got %v", log)
	}
}

// TestMultiObserverIsolatesPanickingMember 一个成员 panic 只该落在它自己那一格：
// 扇出的意义是「组合多个出口」（宿主自己的 observer + observe 的适配器），前面
// 坏掉一个就让后面的观察者收不到事件，是这类组合最坏的失效方式。
func TestMultiObserverIsolatesPanickingMember(t *testing.T) {
	panicker := ObserverFunc{
		Waiting:  func(_, _ string) { panic("observer boom") },
		Running:  func(_, _ string) { panic("observer boom") },
		Finished: func(_, _ string, _ NodeFinishReason, _ error) { panic("observer boom") },
	}
	recorder := &recordingObserver{}
	in := NewKey[string]("multi.in")
	out := NewKey[string]("multi.out")

	g := mustNew(t, context.Background(), "test", WithObserver(MultiObserver{panicker, recorder}))
	if err := Seed(g, in, "x"); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(NewNode("n", Requires(in), Provides(out), func(rc *RunCtx) error {
		v, err := Get(rc, in)
		if err != nil {
			return err
		}
		return Set(rc, out, v)
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatalf("member panic must not fail graph: %v", err)
	}
	log := recorder.snapshot()
	// 三条回调一条都不能少：panic 的邻居照样收全，而不是只收到 panic 之前那几条。
	if countPref(log, "W:n") != 1 || countPref(log, "R:n") != 1 || countPref(log, "F:n:completed") != 1 {
		t.Fatalf("recorder 收到的回调 = %v，want W/R/F 各一条", log)
	}
}
