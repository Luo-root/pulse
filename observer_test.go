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
	// 图级那条的错误与归因单独留痕：errs 里混着节点级的错误，从里面分不出是谁的。
	gStartID  string
	gFinishID string
	gErr      error
}

func (r *recordingObserver) OnGraphStarted(graphID string) {
	r.mu.Lock()
	r.log = append(r.log, "GS")
	r.gStartID = graphID
	r.mu.Unlock()
}

func (r *recordingObserver) OnGraphFinished(graphID string, reason NodeFinishReason, err error) {
	r.mu.Lock()
	r.log = append(r.log, "GF:"+string(reason))
	r.gFinishID, r.gErr = graphID, err
	r.mu.Unlock()
}

// graphTail 读图级两条的留痕（started 的图 ID、finished 的图 ID 与错误）。
func (r *recordingObserver) graphTail() (startID, finishID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gStartID, r.gFinishID, r.gErr
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
		GraphStarted:  func(string) { panic("observer boom") },
		GraphFinished: func(string, NodeFinishReason, error) { panic("observer boom") },
		Waiting:       func(_, _ string) { panic("observer boom") },
		Running:       func(_, _ string) { panic("observer boom") },
		Finished:      func(_, _ string, _ NodeFinishReason, _ error) { panic("observer boom") },
	}
	recorder := &recordingObserver{}
	in := NewKey[string]("multi.in")
	out := NewKey[string]("multi.out")

	g := mustNew(t, context.Background(), "multi", WithObserver(MultiObserver{panicker, recorder}))
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
	// 五条回调一条都不能少：panic 的邻居照样收全，而不是只收到 panic 之前那几条。
	want := []string{"GS", "W:n", "R:n", "F:n:completed", "GF:completed"}
	if len(log) != len(want) {
		t.Fatalf("recorder 收到的回调 = %v，want %v", log, want)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("recorder 收到的回调 = %v，want %v", log, want)
		}
	}
}

// TestObserverGraphBrackets 图级两条把本轮的节点事件夹在中间：started 先于任何
// 节点事件、finished 晚于全部节点事件——这是「运行级」这个说法能给出的全部顺序
// 承诺，也是宿主敢按「第一/最后一条」切出一轮的依据。
func TestObserverGraphBrackets(t *testing.T) {
	obs := &recordingObserver{}
	in := NewKey[string]("obs.bk.in")
	out := NewKey[string]("obs.bk.out")
	g := mustNew(t, context.Background(), "bracket", WithObserver(obs))
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
	// 单节点图的整条观测是确定的：五条，且顺序固定。
	want := []string{"GS", "W:n", "R:n", "F:n:completed", "GF:completed"}
	log := obs.snapshot()
	if len(log) != len(want) {
		t.Fatalf("log = %v, want %v", log, want)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("log = %v, want %v", log, want)
		}
	}
	startID, finishID, err := obs.graphTail()
	if startID != "bracket" || finishID != "bracket" {
		t.Fatalf("graphID 归因漂移: started=%q finished=%q", startID, finishID)
	}
	if err != nil {
		t.Fatalf("completed 的 GraphFinished err = %v, want nil", err)
	}
}

// TestObserverGraphFinishedFailed 失败路径：finished 照发一次、reason = failed，
// 且 err **就是** Run 返回的那个——宿主不必再拿 Run 的返回值去和记录对账。
func TestObserverGraphFinishedFailed(t *testing.T) {
	obs := &recordingObserver{}
	out := NewKey[string]("obs.ff.out")
	boom := errors.New("node boom")
	g := mustNew(t, context.Background(), "failgraph", WithObserver(obs))
	if err := g.Add(NewNode("bad", nil, Provides(out), func(rc *RunCtx) error {
		return boom
	})); err != nil {
		t.Fatal(err)
	}
	err := g.Run()
	if !errors.Is(err, boom) {
		t.Fatalf("Run err = %v, want node error", err)
	}
	log := obs.snapshot()
	if len(log) == 0 || log[len(log)-1] != "GF:failed" {
		t.Fatalf("finished 未收在最后: %v", log)
	}
	if countPref(log, "GF:") != 1 {
		t.Fatalf("finished 次数 = %v", log)
	}
	if _, _, gerr := obs.graphTail(); !errors.Is(gerr, boom) {
		t.Fatalf("GraphFinished err = %v, want node error", gerr)
	}
}

// TestObserverGraphFinishedCanceled 取消路径：终态是 canceled 而不是 failed
// （宿主按 reason 分流，把一次没有 bug 的取消报成 failure 会指错方向）。
// 顺带钉住「只 Start 不 Wait 收不到 finished」——那正是 finished 的发出点。
func TestObserverGraphFinishedCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	obs := &recordingObserver{}
	out := NewKey[string]("obs.fc.out")
	g := mustNew(t, ctx, "cancelgraph", WithObserver(obs))
	if err := g.Add(NewNode("blocker", nil, Provides(out), func(rc *RunCtx) error {
		<-rc.Context().Done()
		return rc.Context().Err()
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	log := obs.snapshot()
	if len(log) == 0 || log[0] != "GS" {
		t.Fatalf("started 必须是第一条: %v", log)
	}
	if countPref(log, "GF:") != 0 {
		t.Fatalf("Wait 还没调用就收到了 finished: %v", log)
	}
	cancel()
	if err := g.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait err = %v, want context.Canceled", err)
	}
	log = obs.snapshot()
	if log[len(log)-1] != "GF:canceled" {
		t.Fatalf("图级终态 = %v, want 收在 GF:canceled", log)
	}
	if _, _, gerr := obs.graphTail(); !errors.Is(gerr, context.Canceled) {
		t.Fatalf("GraphFinished err = %v, want context.Canceled", gerr)
	}
}

// TestObserverGraphEventsOnce 重复 Start / 重复 Wait 都只发一次：started 属于
// 「这一轮开始」（重复 Start 是调用错误，不是新一轮），finished 属于「这一轮结果」
// （重复 Wait 读的是同一份结果）。
func TestObserverGraphEventsOnce(t *testing.T) {
	obs := &recordingObserver{}
	out := NewKey[string]("obs.once.out")
	g := mustNew(t, context.Background(), "once", WithObserver(obs))
	if err := g.Add(NewNode("n", nil, Provides(out), func(rc *RunCtx) error {
		return Set(rc, out, "ok")
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(); !errors.Is(err, ErrGraphStarted) {
		t.Fatalf("第二次 Start = %v, want ErrGraphStarted", err)
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("第二次 Wait = %v, want nil（同一轮结果）", err)
	}
	log := obs.snapshot()
	if countPref(log, "GS") != 1 || countPref(log, "GF:") != 1 {
		t.Fatalf("图级回调次数 = %v", log)
	}
}

// TestObserverNoGraphEventsOnRejectedStart 校验不过的图**仍未启动**，所以不该发出
// started：否则「一轮一次」就变成了「调用过 Start 一次」，宿主会为一次从未发生的
// 运行开一轮账；同理，没启动的图也等不到 finished（Wait 直接报 ErrGraphNotStarted）。
func TestObserverNoGraphEventsOnRejectedStart(t *testing.T) {
	obs := &recordingObserver{}
	missing := NewKey[string]("obs.missing")
	g := mustNew(t, context.Background(), "rejected", WithObserver(obs))
	if err := g.Add(NewNode("n", Requires(missing), nil, func(rc *RunCtx) error {
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(); err == nil {
		t.Fatal("want start validation error")
	}
	if log := obs.snapshot(); len(log) != 0 {
		t.Fatalf("校验失败也发了图级回调: %v", log)
	}
	if err := g.Wait(); !errors.Is(err, ErrGraphNotStarted) {
		t.Fatalf("Wait = %v, want ErrGraphNotStarted", err)
	}
	if log := obs.snapshot(); len(log) != 0 {
		t.Fatalf("未启动的图发了 finished: %v", log)
	}
}

// TestObserverEmptyGraphBrackets 空图也发这一对：它的两条启动校验同样齐（没有节点
// 就没有 Requires），运行结果是 completed——「这次运行什么都没干」也是一次运行。
func TestObserverEmptyGraphBrackets(t *testing.T) {
	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "empty", WithObserver(obs))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	log := obs.snapshot()
	if len(log) != 2 || log[0] != "GS" || log[1] != "GF:completed" {
		t.Fatalf("空图 log = %v, want [GS GF:completed]", log)
	}
}

// TestObserverFuncGraphFieldsFire 接线自检：ObserverFunc 的两个图级字段真的会被
// 引擎调用到。（字段是可选的，nil 就不发——TestObserverPanicIsolated 用的正是只填
// 节点三个的那种 ObserverFunc。）
func TestObserverFuncGraphFieldsFire(t *testing.T) {
	var mu sync.Mutex
	var started, finished []string
	var reason NodeFinishReason
	var gerr error
	obs := ObserverFunc{
		GraphStarted: func(graphID string) {
			mu.Lock()
			started = append(started, graphID)
			mu.Unlock()
		},
		GraphFinished: func(graphID string, r NodeFinishReason, err error) {
			mu.Lock()
			finished = append(finished, graphID)
			reason, gerr = r, err
			mu.Unlock()
		},
	}
	out := NewKey[string]("obs.of.out")
	g := mustNew(t, context.Background(), "fn", WithObserver(obs))
	if err := g.Add(NewNode("n", nil, Provides(out), func(rc *RunCtx) error {
		return Set(rc, out, "ok")
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(started) != 1 || started[0] != "fn" || len(finished) != 1 || finished[0] != "fn" {
		t.Fatalf("graph fields: started=%v finished=%v", started, finished)
	}
	if reason != NodeCompleted || gerr != nil {
		t.Fatalf("reason = %q err = %v", reason, gerr)
	}
}
