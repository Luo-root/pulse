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

var (
	kA = NewKey[string]("a")
	kB = NewKey[string]("b")
	kC = NewKey[string]("c")
	kL = NewKey[string]("left")
	kR = NewKey[string]("right")
)

// mustNew 构造测试图：graphID 必填校验通过后返回图实例。
func mustNew(t *testing.T, ctx context.Context, graphID string, opts ...Option) *Graph {
	t.Helper()
	g, err := New(ctx, graphID, opts...)
	if err != nil {
		t.Fatalf("New(%q): %v", graphID, err)
	}
	return g
}

func TestLinear(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("n1", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "hello")
	}))
	mustAdd(t, g, NewNode("n2", Requires(kA), Provides(kB), func(rc *RunCtx) error {
		v, err := Get(rc, kA)
		if err != nil {
			return err
		}
		// 二次 Set 忽略
		if err := Set(rc, kB, v+"!"); err != nil {
			return err
		}
		return Set(rc, kB, "ignored")
	}))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	rc := inspect(g)
	v, ok, skipped, err := TryGet(rc, kB)
	if err != nil || !ok || skipped || v != "hello!" {
		t.Fatalf("got %q ok=%v skip=%v err=%v", v, ok, skipped, err)
	}
}

func TestFanIn(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	var order atomic.Int32
	mustAdd(t, g, NewNode("a", nil, Provides(kA), func(rc *RunCtx) error {
		order.Add(1)
		return Set(rc, kA, "A")
	}))
	mustAdd(t, g, NewNode("b", nil, Provides(kB), func(rc *RunCtx) error {
		order.Add(1)
		return Set(rc, kB, "B")
	}))
	mustAdd(t, g, NewNode("c", Deps(Requires(kA), Requires(kB)), Provides(kC), func(rc *RunCtx) error {
		if order.Load() != 2 {
			t.Fatalf("fan-in ran before both parents, order=%d", order.Load())
		}
		a, _ := Get(rc, kA)
		b, _ := Get(rc, kB)
		return Set(rc, kC, a+b)
	}))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	rc := inspect(g)
	v, ok, _, _ := TryGet(rc, kC)
	if !ok || v != "AB" {
		t.Fatalf("fan-in result = %q", v)
	}
}

func TestBranchSkip(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	var leftRan, rightRan atomic.Bool
	mustAdd(t, g, NewNode("split", nil, Deps(Provides(kL), Provides(kR)), func(rc *RunCtx) error {
		if err := Set(rc, kL, "go-left"); err != nil {
			return err
		}
		return Skip(rc, kR)
	}))
	mustAdd(t, g, NewNode("left", Requires(kL), Provides(kA), func(rc *RunCtx) error {
		leftRan.Store(true)
		v, err := Get(rc, kL)
		if err != nil {
			return err
		}
		return Set(rc, kA, v)
	}))
	mustAdd(t, g, NewNode("right", Requires(kR), Provides(kB), func(rc *RunCtx) error {
		rightRan.Store(true)
		return Set(rc, kB, "should-not")
	}))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if !leftRan.Load() || rightRan.Load() {
		t.Fatalf("left=%v right=%v", leftRan.Load(), rightRan.Load())
	}
	rc := inspect(g)
	if _, ok, skipped, _ := TryGet(rc, kB); ok || !skipped {
		t.Fatal("right output should be skipped")
	}
}

func TestCascadeSkip(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	var bRan, cRan atomic.Bool
	mustAdd(t, g, NewNode("a", nil, Provides(kA), func(rc *RunCtx) error {
		return Skip(rc, kA)
	}))
	mustAdd(t, g, NewNode("b", Requires(kA), Provides(kB), func(rc *RunCtx) error {
		bRan.Store(true)
		return Set(rc, kB, "x")
	}))
	mustAdd(t, g, NewNode("c", Requires(kB), Provides(kC), func(rc *RunCtx) error {
		cRan.Store(true)
		return Set(rc, kC, "y")
	}))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if bRan.Load() || cRan.Load() {
		t.Fatal("cascade should not run B or C")
	}
}

// TestPartialArrivalRunsNode 到几个收几个：多路输入里只要有一路真的到了值，
// 节点就带着到了的那些进入 Run。上游某一路没有值，不该让手里还有数据的下游
// 跟着停，更不该让已经到达的值作废。
func TestPartialArrivalRunsNode(t *testing.T) {
	c1 := NewKey[string]("partial.c1")
	c2 := NewKey[string]("partial.c2")
	c3 := NewKey[string]("partial.c3")
	joined := NewKey[string]("partial.joined")

	var joinReason NodeFinishReason
	obs := ObserverFunc{Finished: func(_, nodeID string, r NodeFinishReason, _ error) {
		if nodeID == "join" {
			joinReason = r
		}
	}}

	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	// 三个候选：两个有产出，第三个这轮没产出——这是合法的跳过，不是失败。
	for i, k := range []Key[string]{c1, c2, c3} {
		k, want := k, fmt.Sprintf("v%d", i+1)
		mustAdd(t, g, NewNode(fmt.Sprintf("cand%d", i+1), nil, Provides(k), func(rc *RunCtx) error {
			if i == 2 {
				return Skip(rc, k)
			}
			return Set(rc, k, want)
		}))
	}
	// 汇聚节点要求全部三路：按老行为它会因 cand3 的跳过而整个不执行。
	mustAdd(t, g, NewNode("join",
		Deps(Requires(c1), Requires(c2), Requires(c3)), Provides(joined),
		func(rc *RunCtx) error {
			var got []string
			for _, k := range []Key[string]{c1, c2, c3} {
				v, ok, skipped, err := TryGet(rc, k)
				if err != nil {
					return err
				}
				switch {
				case ok:
					got = append(got, v)
				case skipped: // 这一路没有值，跳过即可
				default:
					return fmt.Errorf("输入既未就绪也未跳过——门不该把它放进来")
				}
			}
			return Set(rc, joined, strings.Join(got, "+"))
		}))

	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	v, ok, skipped, err := TryGet(inspect(g), joined)
	if err != nil || !ok || skipped || v != "v1+v2" {
		t.Fatalf("join 输出 = %q ok=%v skipped=%v err=%v，want \"v1+v2\" 就绪", v, ok, skipped, err)
	}
	if joinReason != NodeCompleted {
		t.Fatalf("join 终态 = %q, want completed（它进入了 Run）", joinReason)
	}
}

// TestAllInputsSkippedSkipsNode 一条值都没到，才是本节点自己跳过：
// 全部 Requires 都以跳过到达时 Run 不进入、全部输出跳过——纯分支的下游
// 仍按跳过收尾（与 TestPartialArrivalRunsNode 是同一条判据的两侧）。
func TestAllInputsSkippedSkipsNode(t *testing.T) {
	a := NewKey[string]("allskip.a")
	b := NewKey[string]("allskip.b")
	out := NewKey[string]("allskip.out")

	var ran atomic.Bool
	var reason NodeFinishReason
	obs := ObserverFunc{Finished: func(_, nodeID string, r NodeFinishReason, _ error) {
		if nodeID == "sink" {
			reason = r
		}
	}}

	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	if err := SkipSeed(g, a); err != nil {
		t.Fatal(err)
	}
	if err := SkipSeed(g, b); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("sink", Deps(Requires(a), Requires(b)), Provides(out), func(rc *RunCtx) error {
		ran.Store(true)
		return Set(rc, out, "should-not")
	}))

	if err := g.Run(); err != nil {
		t.Fatalf("全跳过是合法结果：%v", err)
	}
	if ran.Load() {
		t.Fatal("两路输入一条值都没到，节点不该进入 Run")
	}
	if reason != NodeSkipped {
		t.Fatalf("sink 终态 = %q, want skipped", reason)
	}
	if _, ok, skipped, err := TryGet(inspect(g), out); err != nil || ok || !skipped {
		t.Fatalf("out = ok=%v skipped=%v err=%v, want skipped", ok, skipped, err)
	}
}

// TestPartialArrivalGetSkippedInput 在「到几个收几个」下，Run 里 Get 一条没值的
// 输入是**正常路径**：返回 *SkipError（带 Key 名，errors.Is(err, ErrSkipped)
// 成立），既不阻塞到 ctx 取消，也不是节点失败——节点照常写自己的输出，
// 终态是 completed。
func TestPartialArrivalGetSkippedInput(t *testing.T) {
	have := NewKey[string]("get.have")
	miss := NewKey[string]("get.miss")
	out := NewKey[string]("get.out")

	var gotErr error
	var outReason NodeFinishReason
	obs := ObserverFunc{Finished: func(_, nodeID string, r NodeFinishReason, _ error) {
		if nodeID == "reader" {
			outReason = r
		}
	}}

	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	mustAdd(t, g, NewNode("src", nil, Provides(have), func(rc *RunCtx) error {
		return Set(rc, have, "v")
	}))
	mustAdd(t, g, NewNode("gone", nil, Provides(miss), func(rc *RunCtx) error {
		return Skip(rc, miss)
	}))
	mustAdd(t, g, NewNode("reader", Deps(Requires(have), Requires(miss)), Provides(out),
		func(rc *RunCtx) error {
			if _, err := Get(rc, miss); err != nil {
				gotErr = err
			}
			v, err := Get(rc, have)
			if err != nil {
				return err
			}
			return Set(rc, out, v)
		}))

	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(gotErr, ErrSkipped) {
		t.Fatalf("Get 一条跳过的输入 = %v, want ErrSkipped", gotErr)
	}
	var se *SkipError
	if !errors.As(gotErr, &se) || len(se.Keys) != 1 || se.Keys[0] != "get.miss" {
		t.Fatalf("SkipError.Keys = %v, want [get.miss]", se)
	}
	if outReason != NodeCompleted {
		t.Fatalf("reader 终态 = %q, want completed（少一路输入也该照做）", outReason)
	}
	if v, ok, skipped, err := TryGet(inspect(g), out); err != nil || !ok || skipped || v != "v" {
		t.Fatalf("out = %q ok=%v skipped=%v err=%v, want \"v\" 就绪", v, ok, skipped, err)
	}
}

// TestWaitAllStrictFanIn WaitAll 的返回值是**显式的 fan-in 策略声明**：
// 节点把它直接 return 出去 = 「缺一条就别跑我」。引擎让本节点以「跳过」收尾
// ——不是失败（Run 返回 nil），Retry 也不重试。本用例的节点体没写过任何
// 输出，所以 Provide 全部以跳过到达（写过的那部分不回滚，见
// TestWaitAllStrictKeepsPublishedOutput）。
func TestWaitAllStrictFanIn(t *testing.T) {
	a := NewKey[string]("strict.a")
	b := NewKey[string]("strict.b")
	out := NewKey[string]("strict.out")

	var runs atomic.Int32
	var reason NodeFinishReason
	obs := ObserverFunc{Finished: func(_, nodeID string, r NodeFinishReason, _ error) {
		if nodeID == "strict" {
			reason = r
		}
	}}
	req := Deps(Requires(a), Requires(b))

	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	if err := SkipSeed(g, b); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("src", nil, Provides(a), func(rc *RunCtx) error {
		return Set(rc, a, "v")
	}))
	mustAdd(t, g, NewNode("strict", req, Provides(out), func(rc *RunCtx) error {
		runs.Add(1)
		if err := WaitAll(rc, req...); err != nil {
			return err // 显式声明：缺一条就别跑我
		}
		return Set(rc, out, "v")
	}, Retry(3, 0)))

	if err := g.Run(); err != nil {
		t.Fatalf("跳过不是失败：%v", err)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("节点体跑了 %d 次，want 1（跳到收尾，Retry 不得重试）", got)
	}
	if reason != NodeSkipped {
		t.Fatalf("strict 终态 = %q, want skipped", reason)
	}
	if _, ok, skipped, err := TryGet(inspect(g), out); err != nil || ok || !skipped {
		t.Fatalf("out = ok=%v skipped=%v err=%v, want skipped（收尾为跳过时，没写过的输出跟着跳过）", ok, skipped, err)
	}
}

// TestWaitAllStrictKeepsPublishedOutput 钉住「以跳过收尾」的输出边界：
// 节点体已经 Set 过的槽**不回滚**——只有还没写的 Provide 会被跳过。这与
// 一次性槽位契约（到达即发布）一致，所以 godoc / 文档不能承诺「全部输出跳过」。
func TestWaitAllStrictKeepsPublishedOutput(t *testing.T) {
	a := NewKey[string]("pubstrict.a")
	b := NewKey[string]("pubstrict.b")
	published := NewKey[string]("pubstrict.published")
	unwritten := NewKey[string]("pubstrict.unwritten")

	var reason NodeFinishReason
	obs := ObserverFunc{Finished: func(_, nodeID string, r NodeFinishReason, _ error) {
		if nodeID == "strict" {
			reason = r
		}
	}}
	req := Deps(Requires(a), Requires(b))

	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	if err := SkipSeed(g, b); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("src", nil, Provides(a), func(rc *RunCtx) error {
		return Set(rc, a, "v")
	}))
	mustAdd(t, g, NewNode("strict", req, Deps(Provides(published), Provides(unwritten)),
		func(rc *RunCtx) error {
			if err := Set(rc, published, "kept"); err != nil {
				return err
			}
			return WaitAll(rc, req...) // 缺 b：以跳过收尾，但已发布的那条留下
		}))

	if err := g.Run(); err != nil {
		t.Fatalf("跳过不是失败：%v", err)
	}
	if reason != NodeSkipped {
		t.Fatalf("strict 终态 = %q, want skipped", reason)
	}
	// 先 Set 过的那条：不回滚
	if v, ok, skipped, err := TryGet(inspect(g), published); err != nil || !ok || skipped || v != "kept" {
		t.Fatalf("published = %q ok=%v skipped=%v err=%v，want \"kept\" 就绪（已发布的槽不回滚）", v, ok, skipped, err)
	}
	// 没写过的那条：跟着跳过
	if _, ok, skipped, err := TryGet(inspect(g), unwritten); err != nil || ok || !skipped {
		t.Fatalf("unwritten = ok=%v skipped=%v err=%v, want skipped", ok, skipped, err)
	}
}

func TestDuplicateProviderRejected(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("a", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "1")
	}))
	err := g.Add(NewNode("b", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "2")
	}))
	if err == nil {
		t.Fatal("expected duplicate provider error")
	}
}

func TestSelfEdgeRejected(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	err := g.Add(NewNode("loop", Requires(kA), Provides(kA), func(rc *RunCtx) error {
		return nil
	}))
	if err == nil {
		t.Fatal("expected self-edge error")
	}
}

func TestSeedAndNodeProviderConflict(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	if err := Seed(g, kA, "in"); err != nil {
		t.Fatal(err)
	}
	err := g.Add(NewNode("load", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "from-node")
	}))
	if !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("Seed then Add: want ErrDuplicateSource, got %v", err)
	}

	g2 := mustNew(t, context.Background(), "test")
	mustAdd(t, g2, NewNode("load", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "from-node")
	}))
	err = Seed(g2, kA, "in")
	if !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("Add then Seed: want ErrDuplicateSource, got %v", err)
	}
}

func TestSkipSeedAndNodeProviderConflict(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	if err := SkipSeed(g, kA); err != nil {
		t.Fatal(err)
	}
	err := g.Add(NewNode("load", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "from-node")
	}))
	if !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("SkipSeed then Add: want ErrDuplicateSource, got %v", err)
	}

	g2 := mustNew(t, context.Background(), "test")
	mustAdd(t, g2, NewNode("load", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "from-node")
	}))
	err = SkipSeed(g2, kA)
	if !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("Add then SkipSeed: want ErrDuplicateSource, got %v", err)
	}
}

func TestRepeatedSeedSemantics(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	if err := Seed(g, kA, "first"); err != nil {
		t.Fatal(err)
	}
	if err := Seed(g, kA, "ignored"); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	v, ok, skipped, err := TryGet(inspect(g), kA)
	if err != nil || !ok || skipped || v != "first" {
		t.Fatalf("got %q ok=%v skipped=%v err=%v", v, ok, skipped, err)
	}

	g = mustNew(t, context.Background(), "test")
	if err := SkipSeed(g, kA); err != nil {
		t.Fatal(err)
	}
	if err := SkipSeed(g, kA); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	_, ok, skipped, err = TryGet(inspect(g), kA)
	if err != nil || ok || !skipped {
		t.Fatalf("ok=%v skipped=%v err=%v", ok, skipped, err)
	}

	g = mustNew(t, context.Background(), "test")
	if err := Seed(g, kA, "value"); err != nil {
		t.Fatal(err)
	}
	if err := SkipSeed(g, kA); !errors.Is(err, ErrConflict) {
		t.Fatalf("Seed then SkipSeed: want ErrConflict, got %v", err)
	}
}

func TestEmptyGraphStartWait(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
}

// 顺序多次 next 合法（Retry 依赖）；第二次成功返回即可。
func TestAspectSequentialNextAllowed(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	var runs atomic.Int32
	aspect := Aspect(func(rc *RunCtx, next func(*RunCtx) error) error {
		if err := next(rc); err != nil {
			return err
		}
		// 顺序再调一次：应成功（第二次 core 因槽位已写而 Set 静默）。
		return next(rc)
	})
	mustAdd(t, g, NewNode("n", nil, Provides(kA), func(rc *RunCtx) error {
		runs.Add(1)
		return Set(rc, kA, "value")
	}, aspect))
	if err := g.Run(); err != nil {
		t.Fatalf("sequential next must be allowed, got %v", err)
	}
	if got := runs.Load(); got != 2 {
		t.Fatalf("node ran %d times, want 2", got)
	}
}

func TestAspectConcurrentNextCalledOnce(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	var runs atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	aspect := Aspect(func(rc *RunCtx, next func(*RunCtx) error) error {
		errCh := make(chan error, 1)
		go func() { errCh <- next(rc) }()
		<-started // 第一个已进入 core 并持有 depth
		err2 := next(rc)
		close(release)
		err1 := <-errCh
		if !errors.Is(err2, ErrNextCalledTwice) {
			return fmt.Errorf("overlapping next want ErrNextCalledTwice, got %v (first=%v)", err2, err1)
		}
		return ErrNextCalledTwice
	})
	mustAdd(t, g, NewNode("n", nil, Provides(kA), func(rc *RunCtx) error {
		started <- struct{}{}
		<-release
		runs.Add(1)
		return Set(rc, kA, "value")
	}, aspect))
	if err := g.Run(); !errors.Is(err, ErrNextCalledTwice) {
		t.Fatalf("want ErrNextCalledTwice, got %v", err)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("node ran %d times, want 1", got)
	}
}

// 跳过不是失败：输入一条值都没到时 Retry 必须立即返回，不得重跑整段
// 「等输入 + 执行」——否则白等 (attempts-1) × delay，内层切面
// （观测/埋点/记账）也被重复执行。
func TestRetryDoesNotRerunSkippedNode(t *testing.T) {
	in := NewKey[string]("retry.skip.in")
	out := NewKey[string]("retry.skip.out")
	const delay = 300 * time.Millisecond

	g := mustNew(t, context.Background(), "test", WithAspects(Retry(3, delay)))
	if err := SkipSeed(g, in); err != nil {
		t.Fatal(err)
	}

	var runs, inner atomic.Int32
	mustAdd(t, g, NewNode("down", Requires(in), Provides(out), func(rc *RunCtx) error {
		runs.Add(1) // 断言在主 goroutine 上做（节点跑在自己的 goroutine 里）
		return nil
	}, Aspect(func(rc *RunCtx, next func(*RunCtx) error) error {
		inner.Add(1)
		return next(rc)
	})))

	start := time.Now()
	if err := g.Run(); err != nil {
		t.Fatalf("a skip is not a failure: %v", err)
	}
	elapsed := time.Since(start)

	if got := inner.Load(); got != 1 {
		t.Fatalf("inner aspect ran %d times, want 1 (a skipped input must not be retried)", got)
	}
	if got := runs.Load(); got != 0 {
		t.Fatalf("node Run ran %d times, want 0 when its input is skipped", got)
	}
	if elapsed >= delay {
		t.Fatalf("skip path took %v, want no retry delay (Retry must not add attempts-1 waits)", elapsed)
	}
	rc := inspect(g)
	if _, ok, skipped, err := TryGet(rc, out); err != nil || ok || !skipped {
		t.Fatalf("out = ok=%v skipped=%v err=%v, want skipped", ok, skipped, err)
	}
}

func TestEmptyNodeIDRejected(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	err := g.Add(NewNode("", nil, Provides(kA), func(rc *RunCtx) error { return nil }))
	if err == nil {
		t.Fatal("expected empty id error")
	}
}

func TestDuplicateRequiresRejected(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	err := g.Add(NewNode("n", Deps(Requires(kA), Requires(kA)), Provides(kB), func(rc *RunCtx) error { return nil }))
	if err == nil {
		t.Fatal("expected duplicate requires error")
	}
}

func TestDuplicateProvidesRejected(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	err := g.Add(NewNode("n", nil, Deps(Provides(kA), Provides(kA)), func(rc *RunCtx) error { return nil }))
	if err == nil {
		t.Fatal("expected duplicate provides error")
	}
}

func TestDuplicateNodeIDRejected(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("n", nil, Provides(kA), func(rc *RunCtx) error { return Skip(rc, kA) }))
	err := g.Add(NewNode("n", nil, Provides(kB), func(rc *RunCtx) error { return Skip(rc, kB) }))
	if err == nil {
		t.Fatal("expected duplicate id error")
	}
}

func TestNodeErrorCancels(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	boom := errors.New("boom")
	var bRan atomic.Bool
	mustAdd(t, g, NewNode("a", nil, Provides(kA), func(rc *RunCtx) error {
		return boom
	}))
	mustAdd(t, g, NewNode("b", Requires(kA), Provides(kB), func(rc *RunCtx) error {
		bRan.Store(true)
		return Set(rc, kB, "x")
	}))
	err := g.Run()
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
	if bRan.Load() {
		t.Fatal("downstream should not run after failure")
	}
}

func TestTimeoutInterruptsWait(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	// kA 必须有来源（Start 的来源校验），但它迟迟不写：slow 会真的**在等**，
	// 这条用例测的就是超时能不能打断等待段。
	mustAdd(t, g, NewNode("late", nil, Provides(kA), func(rc *RunCtx) error {
		<-rc.Context().Done()
		return rc.Context().Err()
	}))
	mustAdd(t, g, NewNode("slow", Requires(kA), Provides(kB), func(rc *RunCtx) error {
		_, err := Get(rc, kA)
		return err
	}, Timeout(30*time.Millisecond)))
	err := g.Run()
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Run err = %v, want timeout", err)
	}
}

// TestStartRejectsUnsourcedRequires 装配期能静态判定的死图必须在 Start 就被拒：
// 「等一个永远不会被写入的槽」今天只表现为挂死（有 deadline 时是一句看不出病因的
// 超时，没有时进程会被 runtime 判为 fatal deadlock），而启动那一刻引擎就知道是
// 哪个节点的哪个 Key 没有来源。
func TestStartRejectsUnsourcedRequires(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("waiter", Requires(kA), nil, func(rc *RunCtx) error { return nil }))

	err := g.Start()
	if err == nil {
		t.Fatal("Start 放行了一张不可能跑完的图")
	}
	if !strings.Contains(err.Error(), `"waiter"`) || !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("错误里要能读到节点 id 与 Key 名，got %v", err)
	}
	// 校验不过时图**仍未启动**：补上来源后可以重新 Start 并跑完。
	if werr := g.Wait(); !errors.Is(werr, ErrGraphNotStarted) {
		t.Fatalf("Start 失败后图不该是已启动状态：Wait() = %v", werr)
	}
	mustAdd(t, g, NewNode("producer", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "v")
	}))
	if err := g.Run(); err != nil {
		t.Fatalf("补上来源后应能跑完：%v", err)
	}
}

// TestStartAcceptsSeededAndProvidedRequires 两种合法来源都不能被来源校验误伤：
// 外部 Seed/SkipSeed，以及另一个节点的 Provides（含隔一层的链路）。
func TestStartAcceptsSeededAndProvidedRequires(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	if err := Seed(g, kA, "seeded"); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("first", Requires(kA), Provides(kB), func(rc *RunCtx) error {
		v, err := Get(rc, kA)
		if err != nil {
			return err
		}
		return Set(rc, kB, v+"!")
	}))
	mustAdd(t, g, NewNode("second", Requires(kB), nil, func(rc *RunCtx) error {
		v, err := Get(rc, kB)
		if err != nil {
			return err
		}
		if v != "seeded!" {
			t.Fatalf("v = %q, want seeded!", v)
		}
		return nil
	}))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	// SkipSeed 也是来源：唯一输入没值（到几个收几个，一条都没有）→ 不进入 Run，
	// 整图无错返回。
	g2 := mustNew(t, context.Background(), "test")
	if err := SkipSeed(g2, kA); err != nil {
		t.Fatal(err)
	}
	var ran atomic.Bool
	mustAdd(t, g2, NewNode("skipped-input", Requires(kA), nil, func(rc *RunCtx) error {
		ran.Store(true)
		return nil
	}))
	if err := g2.Run(); err != nil {
		t.Fatal(err)
	}
	if ran.Load() {
		t.Fatal("唯一输入一条值都没到，不该进入 Run")
	}
}

// TestParentDeadlineIsCanceled 父 ctx 的截止时间到期与主动取消同属「这一轮被从
// 外面拆了」：等待中的节点终态是 canceled，不是 failed——后者会把它报成一个并
// 不存在的节点缺陷，宿主按 reason 分流时就会指错方向。
func TestParentDeadlineIsCanceled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()

	var mu sync.Mutex
	reasons := map[string]NodeFinishReason{}
	obs := ObserverFunc{Finished: func(_, nodeID string, r NodeFinishReason, _ error) {
		mu.Lock()
		reasons[nodeID] = r
		mu.Unlock()
	}}
	g := mustNew(t, ctx, "test", WithObserver(obs))
	// producer 有来源关系但从不写入：waiter 一直在等，直到父 ctx 到期。
	mustAdd(t, g, NewNode("producer", nil, Provides(kA), func(rc *RunCtx) error {
		<-rc.Context().Done()
		return rc.Context().Err()
	}))
	var waiterRan atomic.Bool
	mustAdd(t, g, NewNode("waiter", Requires(kA), nil, func(rc *RunCtx) error {
		waiterRan.Store(true)
		return nil
	}))

	err := g.Run()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run err = %v, want DeadlineExceeded", err)
	}
	if waiterRan.Load() {
		t.Fatal("waiter 不该进入 Run：它的输入从未被写入")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range []string{"producer", "waiter"} {
		if reasons[id] != NodeCanceled {
			t.Fatalf("%s 终态 = %q, want canceled", id, reasons[id])
		}
	}
}

func TestRecoveryPanic(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("p", nil, Provides(kA), func(rc *RunCtx) error {
		panic("explode")
	}))
	err := g.Run()
	if err == nil || err.Error() == "" {
		t.Fatalf("want panic error, got %v", err)
	}
}

func TestMaxRunningSerializes(t *testing.T) {
	g := mustNew(t, context.Background(), "test", WithMaxRunning(1))
	var current, max atomic.Int32
	run := func(rc *RunCtx) error {
		n := current.Add(1)
		for {
			old := max.Load()
			if n <= old || max.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		current.Add(-1)
		// 无 Provides：漏写自动 skip，无所谓
		return nil
	}
	mustAdd(t, g, NewNode("x", nil, nil, run))
	mustAdd(t, g, NewNode("y", nil, nil, run))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if max.Load() != 1 {
		t.Fatalf("max concurrent Run = %d, want 1", max.Load())
	}
}

// TestCancelInterruptsQueuedNode ctx 取消要能打断「等名额」：在 acquire 里
// 排队的节点不进入 Run，终态 canceled——与「等数据」一致（New 的 godoc
// 承诺「ctx 取消会打断所有等待」）。
//
// 顺序必须确定，不能让「谁先抢到名额」交给调度器：a 先 Set 出 k，b 才能
// 通过 WaitAll 走到 acquire。于是 a 必然先占住名额，b 必然排在后面——b 想
// 进 Run 只能等 a 释放名额，而 a 只在 cancel 之后才被放行。sleep 只是让
// 「b 已在 acquire 里」成为常态路径，删掉它结论仍成立（cancel 早于 acquire
// 时，select 只有 ctx 分支可走，同样 canceled）。
func TestCancelInterruptsQueuedNode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var bReason NodeFinishReason
	bDone := make(chan struct{})
	obs := ObserverFunc{Finished: func(_, nodeID string, reason NodeFinishReason, _ error) {
		if nodeID == "b" {
			bReason = reason
			close(bDone)
		}
	}}
	g := mustNew(t, ctx, "test", WithMaxRunning(1), WithObserver(obs))

	k := NewKey[string]("queue.k")
	kReady := make(chan struct{})
	releaseA := make(chan struct{})
	mustAdd(t, g, NewNode("a", nil, Provides(k), func(rc *RunCtx) error {
		if err := Set(rc, k, "v"); err != nil {
			return err
		}
		close(kReady)
		<-releaseA // 占住唯一名额，直到 b 已离队
		return nil
	}))

	var entered atomic.Int32
	mustAdd(t, g, NewNode("b", Requires(k), nil, func(rc *RunCtx) error {
		entered.Add(1)
		return nil
	}))

	go func() {
		<-kReady
		time.Sleep(20 * time.Millisecond) // 让 b 走到 acquire（非必要：见上方注释）
		cancel()
		select {
		case <-bDone:
			// 期望路径：b 立刻以 canceled 离队。
		case <-time.After(2 * time.Second):
			// 旧行为：b 仍卡在 acquire 里，放行 a 让断言报告原因而不是挂死。
		}
		close(releaseA)
	}()

	err := g.Run()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
	if n := entered.Load(); n != 0 {
		t.Fatalf("取消后排队节点仍进入 Run %d 次，want 0", n)
	}
	select {
	case <-bDone:
		if bReason != NodeCanceled {
			t.Fatalf("b 终态 = %q, want canceled", bReason)
		}
	default:
		t.Fatal("b 没有终态回调")
	}
}

func TestKeyTypeConflict(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("a", nil, Provides(NewKey[string]("dup")), func(rc *RunCtx) error { return nil }))
	err := g.Add(NewNode("b", nil, Provides(NewKey[int]("dup")), func(rc *RunCtx) error { return nil }))
	if err == nil {
		t.Fatal("expected type conflict")
	}
}

func TestUndeclaredWrite(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("a", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kB, "nope")
	}))
	if err := g.Run(); err == nil {
		t.Fatal("expected undeclared write error")
	}
}

func TestSetSkipConflict(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("a", nil, Provides(kA), func(rc *RunCtx) error {
		if err := Set(rc, kA, "v"); err != nil {
			return err
		}
		return Skip(rc, kA)
	}))
	if err := g.Run(); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}

func mustAdd(t *testing.T, g *Graph, n *Node) {
	t.Helper()
	if err := g.Add(n); err != nil {
		t.Fatal(err)
	}
}

// TestTimeoutWaitsForInnerRun 超时到期后必须等内层收干净再返回：
// 切面共享 `wrote`，提前返回会让收尾路径与还在跑的 Run 同时碰同一批槽
// （数据竞态），也会让 Run 的「阻塞到全部终止」失真。
func TestTimeoutWaitsForInnerRun(t *testing.T) {
	out := NewKey[string]("to.out")
	var bodyDone atomic.Bool

	g := mustNew(t, context.Background(), "test", WithAspects(Timeout(20*time.Millisecond)))
	mustAdd(t, g, NewNode("slow", nil, Provides(out), func(rc *RunCtx) error {
		time.Sleep(120 * time.Millisecond)
		bodyDone.Store(true)
		return Set(rc, out, "late")
	}))

	start := time.Now()
	err := g.Run()
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Run err = %v, want timeout", err)
	}
	if !bodyDone.Load() {
		t.Fatalf("Run 在 %v 就返回了，节点体还没退出（超时提前返回）", elapsed.Round(time.Millisecond))
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("Run 耗时 %v，短于节点体自身耗时——没等内层", elapsed.Round(time.Millisecond))
	}
}

// TestAcquireReturnsSlotWhenCanceled 「名额空着」与「ctx 已取消」同时成立时，
// acquire 必须以取消为准并把名额放回去。这是 `select` 唯一会漏的形态：
// 两个分支都就绪时它随机挑（若 goroutine 已经阻塞在 select 里，runtime 会把它
// 提交给先就绪的那个分支，那种情况反而是确定的——所以直接打 acquire 本身，
// 不靠调度碰运气）。run it with -count 才说明问题：漏了复查约有一半概率失败。
func TestAcquireReturnsSlotWhenCanceled(t *testing.T) {
	g := mustNew(t, context.Background(), "test", WithMaxRunning(1))
	ctx, cancel := context.WithCancel(g.ctx)
	cancel() // 已取消；同时名额是空的

	if err := g.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire = %v, want context.Canceled", err)
	}
	if n := len(g.sem); n != 0 {
		t.Fatalf("取消后名额没放回去：len(sem) = %d", n)
	}
	// 名额确实还能被正常用掉
	if err := g.acquire(g.ctx); err != nil {
		t.Fatalf("放回的名额不可用：%v", err)
	}
	g.release()
}

// TestAddIsAtomic Add 失败必须让图保持调用前的样子：失败那次声明的
// Provides 不能占住 producer，否则真正提供它的节点此后一直吃
// ErrDuplicateSource，等它的节点永远停在 pending。
func TestAddIsAtomic(t *testing.T) {
	a := NewKey[string]("atomic.a")
	b := NewKey[string]("atomic.b")

	g := mustNew(t, context.Background(), "test")
	if err := Seed(g, b, "seed-b"); err != nil { // b 已被 seed 占住
		t.Fatal(err)
	}
	// a 先被登记/claim，b 才冲突 → 失败的 Add 不能留下 a 的占位
	if err := g.Add(NewNode("bad", nil, Deps(Provides(a), Provides(b)), func(rc *RunCtx) error { return nil })); err == nil {
		t.Fatal("expect duplicate source error")
	}
	if err := g.Add(NewNode("good", nil, Provides(a), func(rc *RunCtx) error { return Set(rc, a, "x") })); err != nil {
		t.Fatalf("失败 Add 留下了 a 的 producer 占位：%v", err)
	}

	var got atomic.Bool
	mustAdd(t, g, NewNode("down", Requires(a), nil, func(rc *RunCtx) error {
		v, err := Get(rc, a)
		if err != nil {
			return err
		}
		got.Store(v == "x")
		return nil
	}))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if !got.Load() {
		t.Fatal("依赖 a 的节点没有拿到值")
	}
}

// TestSeedFailureLeavesNoTrace 与 Add 同一条纪律：校验不过的 Seed 既不能
// 占住来源，也不能把值写进槽位（判据与 Add 共享 check / sourceConflict）。
func TestSeedFailureLeavesNoTrace(t *testing.T) {
	shared := NewKey[string]("seed.trace")
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("owner", nil, Provides(shared), func(rc *RunCtx) error { return nil }))

	// 同名不同类型：类型校验失败，不能把 name 的类型改成 int
	if err := Seed(g, NewKey[int]("seed.trace"), 1); err == nil {
		t.Fatal("expect key type conflict")
	}
	// 同名同类型：来源已归 owner，报 ErrDuplicateSource，且值不得写入
	if err := Seed(g, shared, "v"); !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("want ErrDuplicateSource, got %v", err)
	}
	if st, _ := g.slotOf(shared.asRef()).snapshot(); st != slotPending {
		t.Fatalf("失败的 Seed 写进了槽位：state = %v, want slotPending", st)
	}
}

// TestWaitReasonStableAfterFailure 首错取消后，下游的终态必须是 canceled：
// 它没有执行是因为整图被取消，而不是因为恰好先看见了那条「解阻塞用的跳过」。
func TestWaitReasonStableAfterFailure(t *testing.T) {
	const rounds = 50
	for i := 0; i < rounds; i++ {
		k := NewKey[string]("wait.k")
		var reason NodeFinishReason
		obs := ObserverFunc{Finished: func(_, nodeID string, r NodeFinishReason, _ error) {
			if nodeID == "waiter" {
				reason = r
			}
		}}
		g := mustNew(t, context.Background(), "test", WithObserver(obs))
		mustAdd(t, g, NewNode("boom", nil, Provides(k), func(rc *RunCtx) error { return errors.New("boom") }))
		mustAdd(t, g, NewNode("waiter", Requires(k), nil, func(rc *RunCtx) error {
			t.Fatal("waiter 不该进入 Run")
			return nil
		}))
		if err := g.Run(); err == nil {
			t.Fatal("want boom")
		}
		if reason != NodeCanceled {
			t.Fatalf("第 %d 轮：waiter 终态 = %q, want canceled", i, reason)
		}
	}
}

// TestRetryDoesNotRollbackPublishedSlots 钉住契约：失败 attempt 已发布的槽
// 不回滚、下游据此已被唤醒，后续 attempt 的 Set 被静默忽略（幂等首写）。
// 所以「重试安全」的前提是失败前没有写过 Provide，也没有不可重入副作用。
//
// 同步点：第一次 attempt Set 之后等下游真的跑起来再失败——否则「下游是否
// 已被唤醒」取决于它有没有抢在取消之前进入等待，断言会变成掷硬币。
func TestRetryDoesNotRollbackPublishedSlots(t *testing.T) {
	out := NewKey[string]("retry.pub.out")
	var attempts, downstreamRuns atomic.Int32
	downstreamDone := make(chan struct{})

	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("flaky", nil, Provides(out),
		func(rc *RunCtx) error {
			n := attempts.Add(1)
			if err := Set(rc, out, fmt.Sprintf("attempt-%d", n)); err != nil {
				t.Errorf("attempt-%d Set err = %v（首个 Set 必须成功，其余幂等忽略）", n, err)
			}
			if n == 1 {
				select {
				case <-downstreamDone:
				case <-time.After(2 * time.Second):
					t.Error("下游没有被第一次 attempt 的 Set 唤醒")
				}
			}
			return errors.New("总是失败")
		},
		Retry(3, 0),
	))
	mustAdd(t, g, NewNode("downstream", Requires(out), nil, func(rc *RunCtx) error {
		downstreamRuns.Add(1)
		close(downstreamDone)
		return nil
	}))

	if err := g.Run(); err == nil {
		t.Fatal("want flaky error")
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
	if downstreamRuns.Load() != 1 {
		t.Fatalf("downstream 进入 Run %d 次，want 1（第一次 attempt 的 Set 就该唤醒它）", downstreamRuns.Load())
	}
	if st, v := g.slotOf(out.asRef()).snapshot(); st != slotReady || v != "attempt-1" {
		t.Fatalf("槽位终态 = (%v, %v)，want (slotReady, attempt-1)——已发布的槽不回滚", st, v)
	}
}

// TestWaitReleasesGraphCtx 跑完之后图自己的 ctx 必须被取消：它是 New 从父
// ctx 派生的子 ctx，不 cancel 就一直挂在父 ctx 的 children 上；同时
// Err() 不能把这次收尾的 cancel 读成运行结果。
func TestWaitReleasesGraphCtx(t *testing.T) {
	out := NewKey[string]("ctx.out")
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("n", nil, Provides(out), func(rc *RunCtx) error { return Set(rc, out, "v") }))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if g.ctx.Err() == nil {
		t.Fatal("Run 返回后 g.ctx 仍未取消：子 ctx 一直挂在父 ctx 上")
	}
	if err := g.Err(); err != nil {
		t.Fatalf("干净跑完的 Err() = %v, want nil（收尾 cancel 不算结果）", err)
	}

	// 父 ctx 长生命周期：跑完后子 ctx 已释放
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()
	out2 := NewKey[string]("ctx.out2")
	g2 := mustNew(t, parent, "test")
	mustAdd(t, g2, NewNode("n", nil, Provides(out2), func(rc *RunCtx) error { return nil }))
	if err := g2.Run(); err != nil {
		t.Fatal(err)
	}
	if g2.ctx.Err() == nil {
		t.Fatal("父 ctx 未取消时，子 ctx 应已随 Wait 收尾")
	}
	if err := g2.Err(); err != nil {
		t.Fatalf("g2.Err() = %v, want nil", err)
	}
}

// inspect 构造一个能读图上任意已登记 Key 的 RunCtx（仅测试用）。
func inspect(g *Graph) *RunCtx {
	n := &Node{id: "inspect"}
	for name := range g.slots {
		ref := keyRef{name: name, typ: g.keys.seen[name]}
		n.requires = append(n.requires, ref)
	}
	return newRunCtx(g, n, context.Background())
}

func TestGraphIDRequired(t *testing.T) {
	if _, err := New(context.Background(), ""); err == nil {
		t.Fatal("empty graph id must be rejected")
	}
}
