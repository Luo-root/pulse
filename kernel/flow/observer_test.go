package flow

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

func (r *recordingObserver) OnNodeWaiting(id string) {
	r.mu.Lock()
	r.log = append(r.log, "W:"+id)
	r.mu.Unlock()
}
func (r *recordingObserver) OnNodeRunning(id string) {
	r.mu.Lock()
	r.log = append(r.log, "R:"+id)
	r.mu.Unlock()
}
func (r *recordingObserver) OnNodeFinished(id string, reason NodeFinishReason, err error) {
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
	g := New(context.Background(), WithObserver(obs))
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
	g := New(context.Background(), WithObserver(obs))
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
	g := New(context.Background(), WithObserver(obs))
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
		Waiting: func(string) { panic("observer boom") },
		Running: func(string) {},
		Finished: func(string, NodeFinishReason, error) {
			panic("finished boom")
		},
	}
	out := NewKey[int]("obs.panic.out")
	g := New(context.Background(), WithObserver(boom))
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
	g := New(context.Background(), WithObserver(obs), WithAspects(Timeout(30*time.Millisecond)))
	// 不 Seed in → WaitAll 阻塞直到超时
	if err := g.Add(NewNode("blocked", Requires(in), Provides(out), func(rc *RunCtx) error {
		t.Fatal("should not run")
		return nil
	})); err != nil {
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
}
