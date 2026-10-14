package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLinear(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("n1", "", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "hello")
	}))
	mustAdd(t, g, NewNode("n2", "", Requires(kA), Provides(kB), func(rc *RunCtx) error {
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
	if err := runOn(g); err != nil {
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
	mustAdd(t, g, NewNode("a", "", nil, Provides(kA), func(rc *RunCtx) error {
		order.Add(1)
		return Set(rc, kA, "A")
	}))
	mustAdd(t, g, NewNode("b", "", nil, Provides(kB), func(rc *RunCtx) error {
		order.Add(1)
		return Set(rc, kB, "B")
	}))
	mustAdd(t, g, NewNode("c", "", Deps(Requires(kA), Requires(kB)), Provides(kC), func(rc *RunCtx) error {
		if order.Load() != 2 {
			t.Fatalf("fan-in ran before both parents, order=%d", order.Load())
		}
		a, _ := Get(rc, kA)
		b, _ := Get(rc, kB)
		return Set(rc, kC, a+b)
	}))
	if err := runOn(g); err != nil {
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
	mustAdd(t, g, NewNode("split", "", nil, Deps(Provides(kL), Provides(kR)), func(rc *RunCtx) error {
		if err := Set(rc, kL, "go-left"); err != nil {
			return err
		}
		return Skip(rc, kR)
	}))
	mustAdd(t, g, NewNode("left", "", Requires(kL), Provides(kA), func(rc *RunCtx) error {
		leftRan.Store(true)
		v, err := Get(rc, kL)
		if err != nil {
			return err
		}
		return Set(rc, kA, v)
	}))
	mustAdd(t, g, NewNode("right", "", Requires(kR), Provides(kB), func(rc *RunCtx) error {
		rightRan.Store(true)
		return Set(rc, kB, "should-not")
	}))
	if err := runOn(g); err != nil {
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
	mustAdd(t, g, NewNode("a", "", nil, Provides(kA), func(rc *RunCtx) error {
		return Skip(rc, kA)
	}))
	mustAdd(t, g, NewNode("b", "", Requires(kA), Provides(kB), func(rc *RunCtx) error {
		bRan.Store(true)
		return Set(rc, kB, "x")
	}))
	mustAdd(t, g, NewNode("c", "", Requires(kB), Provides(kC), func(rc *RunCtx) error {
		cRan.Store(true)
		return Set(rc, kC, "y")
	}))
	if err := runOn(g); err != nil {
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
	obs := ObserverFunc{Finished: func(_ Site, nodeID string, r NodeFinishReason, _ error) {
		if nodeID == "join" {
			joinReason = r
		}
	}}

	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	// 三个候选：两个有产出，第三个这轮没产出——这是合法的跳过，不是失败。
	for i, k := range []Key[string]{c1, c2, c3} {
		k, want := k, fmt.Sprintf("v%d", i+1)
		mustAdd(t, g, NewNode(fmt.Sprintf("cand%d", i+1), "", nil, Provides(k), func(rc *RunCtx) error {
			if i == 2 {
				return Skip(rc, k)
			}
			return Set(rc, k, want)
		}))
	}
	// 汇聚节点要求全部三路：按老行为它会因 cand3 的跳过而整个不执行。
	mustAdd(t, g, NewNode("join", "",
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

	if err := runOn(g); err != nil {
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
	obs := ObserverFunc{Finished: func(_ Site, nodeID string, r NodeFinishReason, _ error) {
		if nodeID == "sink" {
			reason = r
		}
	}}

	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	declareIn(t, g, NewPort(a))
	if err := skipSeedOn(g, a); err != nil {
		t.Fatal(err)
	}
	declareIn(t, g, NewPort(b))
	if err := skipSeedOn(g, b); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("sink", "", Deps(Requires(a), Requires(b)), Provides(out), func(rc *RunCtx) error {
		ran.Store(true)
		return Set(rc, out, "should-not")
	}))

	if err := runOn(g); err != nil {
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
	obs := ObserverFunc{Finished: func(_ Site, nodeID string, r NodeFinishReason, _ error) {
		if nodeID == "reader" {
			outReason = r
		}
	}}

	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	mustAdd(t, g, NewNode("src", "", nil, Provides(have), func(rc *RunCtx) error {
		return Set(rc, have, "v")
	}))
	mustAdd(t, g, NewNode("gone", "", nil, Provides(miss), func(rc *RunCtx) error {
		return Skip(rc, miss)
	}))
	mustAdd(t, g, NewNode("reader", "", Deps(Requires(have), Requires(miss)), Provides(out),
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

	if err := runOn(g); err != nil {
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
	obs := ObserverFunc{Finished: func(_ Site, nodeID string, r NodeFinishReason, _ error) {
		if nodeID == "strict" {
			reason = r
		}
	}}
	req := Deps(Requires(a), Requires(b))

	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	declareIn(t, g, NewPort(b))
	if err := skipSeedOn(g, b); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("src", "", nil, Provides(a), func(rc *RunCtx) error {
		return Set(rc, a, "v")
	}))
	mustAdd(t, g, NewNode("strict", "", req, Provides(out), func(rc *RunCtx) error {
		runs.Add(1)
		if err := WaitAll(rc, req...); err != nil {
			return err // 显式声明：缺一条就别跑我
		}
		return Set(rc, out, "v")
	}, Retry(3, 0)))

	if err := runOn(g); err != nil {
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
	obs := ObserverFunc{Finished: func(_ Site, nodeID string, r NodeFinishReason, _ error) {
		if nodeID == "strict" {
			reason = r
		}
	}}
	req := Deps(Requires(a), Requires(b))

	g := mustNew(t, context.Background(), "test", WithObserver(obs))
	declareIn(t, g, NewPort(b))
	if err := skipSeedOn(g, b); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("src", "", nil, Provides(a), func(rc *RunCtx) error {
		return Set(rc, a, "v")
	}))
	mustAdd(t, g, NewNode("strict", "", req, Deps(Provides(published), Provides(unwritten)),
		func(rc *RunCtx) error {
			if err := Set(rc, published, "kept"); err != nil {
				return err
			}
			return WaitAll(rc, req...) // 缺 b：以跳过收尾，但已发布的那条留下
		}))

	if err := runOn(g); err != nil {
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
	mustAdd(t, g, NewNode("a", "", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "1")
	}))
	err := g.Add(NewNode("b", "", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "2")
	}))
	if err == nil {
		t.Fatal("expected duplicate provider error")
	}
}

func TestSelfEdgeRejected(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	err := g.Add(NewNode("loop", "", Requires(kA), Provides(kA), func(rc *RunCtx) error {
		return nil
	}))
	if err == nil {
		t.Fatal("expected self-edge error")
	}
}

// TestSeedAndNodeProviderConflict 一条键恰好一个来源：图里有人 `Provides` 它，
// 外面就不该再喂。
//
// 阶段 2 之后「喂值」不占来源（`Seed` 写的是**这一次运行**的 `Inputs`，不碰定义），
// 所以判据从「`Seed` 报错」搬到「建执行报错」——两个方向都钉住：先 `Add` 后喂，
// 建执行当场报；先喂后 `Add`，装上装得进（定义里本来就没有「外面喂过」这件事），
// 但拿那份输入去跑仍然报。判据一条没少，只是换了时刻。
//
// 这里**不**声明端口：那条键要是也声明成进口，装配期就先把「声明过的进口 + 节点产出」
// 拒掉了（见 TestInputPortAndProducerConflict）——那是另一条判据，会盖住要测的这条。
func TestSeedAndNodeProviderConflict(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("load", "", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "from-node")
	}))
	if err := seedOn(g, kA, "in"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.NewExecution(context.Background(), pendingOf(g).in); err == nil {
		t.Fatal("图内已有来源，外面喂进来的值必须被拒，而不是静默忽略")
	}

	// 反过来：先喂后装。喂值不占来源（`Seed` 不碰定义），所以装得进；但拿那份输入去跑仍然报。
	g2 := mustNew(t, context.Background(), "test")
	if err := seedOn(g2, kA, "in"); err != nil {
		t.Fatal(err)
	}
	err := g2.Add(NewNode("load", "", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "from-node")
	}))
	if err != nil {
		t.Fatalf("喂值不该占住来源（Seed 不碰定义）：%v", err)
	}
	if _, err := g2.NewExecution(context.Background(), pendingOf(g2).in); err == nil {
		t.Fatal("装完节点再拿那份输入跑，必须报")
	}
}

func TestSkipSeedAndNodeProviderConflict(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("load", "", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "from-node")
	}))
	if err := skipSeedOn(g, kA); err != nil {
		t.Fatal(err)
	}
	if _, err := g.NewExecution(context.Background(), pendingOf(g).in); err == nil {
		t.Fatal("图内已有来源，外面标跳过也必须被拒")
	}

	g2 := mustNew(t, context.Background(), "test")
	if err := skipSeedOn(g2, kA); err != nil {
		t.Fatal(err)
	}
	if err := g2.Add(NewNode("load", "", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "from-node")
	})); err != nil {
		t.Fatalf("标跳过不该占住来源（SkipSeed 不碰定义）：%v", err)
	}
	if _, err := g2.NewExecution(context.Background(), pendingOf(g2).in); err == nil {
		t.Fatal("装完节点再拿那份输入跑，必须报")
	}
}

// TestRepeatedWriteIntoOneInputs 一份 `Inputs` 里同一条键写两次是**错误**，
// 不是静默忽略。
//
// 旧入口把「重复种子」做成幂等首写（后一笔无声丢弃）——那正是最容易写成
// 「先填默认值、再拿真实值覆盖」而覆盖悄悄丢掉的形状（槽位的幂等首写是给
// `Set` 用的，不该顺手管到宿主给的输入上）。现在当场报，点名那条键。
func TestRepeatedWriteIntoOneInputs(t *testing.T) {
	var in Inputs
	if err := Seed(&in, kA, "first"); err != nil {
		t.Fatal(err)
	}
	if err := Seed(&in, kA, "ignored"); err == nil {
		t.Fatal("同一份输入里重复 Seed 必须报，不能静默丢后一笔")
	}
	if err := SkipSeed(&in, kA); err == nil {
		t.Fatal("先 Seed 后 SkipSeed 也必须报")
	}

	var in2 Inputs
	if err := SkipSeed(&in2, kA); err != nil {
		t.Fatal(err)
	}
	if err := SkipSeed(&in2, kA); err == nil {
		t.Fatal("重复 SkipSeed 必须报")
	}
	if err := Seed(&in2, kA, "late"); err == nil {
		t.Fatal("先 SkipSeed 后 Seed 也必须报")
	}
}

// TestStartRejectsDependencyCycle 环是启动那一刻就能静态判定的事：每个
// Requires 都有生产者（来源校验会放行），但没有任何节点能先进入 Run——门要等
// 全部输入到达，环里的槽谁也等不来谁。不拒掉的话，有 deadline 时是一句看不出
// 病因的超时，没有时进程会被 runtime 判为 fatal deadlock。
//
// 报错要指出环上的节点与 Key，且顺序稳定（按声明序取第一条出边）。
func TestStartRejectsDependencyCycle(t *testing.T) {
	x := NewKey[int]("x")
	y := NewKey[int]("y")

	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("A", "", Requires(y), Provides(x), func(rc *RunCtx) error { return nil }))
	mustAdd(t, g, NewNode("B", "", Requires(x), Provides(y), func(rc *RunCtx) error { return nil }))

	err := startOn(g)
	if err == nil {
		t.Fatal("含环的图必须在 Start 被拒，而不是留到运行时挂死")
	}
	const want = `pulse: dependency cycle: A -> B -> A (A requires "y", B requires "x")`
	if err.Error() != want {
		t.Fatalf("Start err =\n  %q\nwant\n  %q", err.Error(), want)
	}
	// 校验不过时图**仍未启动**：Wait 仍报 ErrGraphNotStarted，再 Start 仍报环
	if err := waitOn(g); !errors.Is(err, ErrGraphNotStarted) {
		t.Fatalf("Wait = %v, want ErrGraphNotStarted（失败的 Start 不该把图启动起来）", err)
	}
	if err := startOn(g); err == nil || errors.Is(err, ErrGraphStarted) {
		t.Fatalf("第二次 Start = %v, want 同一个环错误（不是 ErrGraphStarted）", err)
	}
}

// TestStartCycleIgnoresSeededEdges Seed/SkipSeed 的 Key 在 Start 之前就已经
// 到达，不构成依赖边：它既不该被算进环里，也不该掩盖真正的环。
func TestStartCycleIgnoresSeededEdges(t *testing.T) {
	seed := NewKey[int]("cyc.seed")
	x := NewKey[int]("cyc.x")
	y := NewKey[int]("cyc.y")
	z := NewKey[int]("cyc.z")

	g := mustNew(t, context.Background(), "test")
	declareIn(t, g, NewPort(seed))
	if err := seedOn(g, seed, 1); err != nil {
		t.Fatal(err)
	}
	// A/B/C 三节点环；A 另外吃一条 Seed（那条边不参与环）
	mustAdd(t, g, NewNode("A", "", Deps(Requires(x), Requires(seed)), Provides(y), func(rc *RunCtx) error { return nil }))
	mustAdd(t, g, NewNode("B", "", Requires(y), Provides(z), func(rc *RunCtx) error { return nil }))
	mustAdd(t, g, NewNode("C", "", Requires(z), Provides(x), func(rc *RunCtx) error { return nil }))

	err := startOn(g)
	if err == nil {
		t.Fatal("三节点环必须在 Start 被拒")
	}
	const want = `pulse: dependency cycle: A -> B -> C -> A (A requires "cyc.x", B requires "cyc.y", C requires "cyc.z")`
	if err.Error() != want {
		t.Fatalf("Start err =\n  %q\nwant\n  %q", err.Error(), want)
	}
}

// TestStartHandlesMultiKeyFromOneProvider 同一个上游提供多条 Key、下游全吃：
// 边与入度必须只记一次。多记一次不会报错、少记一次会把合法图误判成环——这条
// 钉住的是「不会被误判」。
func TestStartHandlesMultiKeyFromOneProvider(t *testing.T) {
	p := NewKey[int]("mk.p")
	q := NewKey[int]("mk.q")
	out := NewKey[int]("mk.out")

	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("up", "", nil, Deps(Provides(p), Provides(q)), func(rc *RunCtx) error {
		if err := Set(rc, p, 1); err != nil {
			return err
		}
		return Set(rc, q, 2)
	}))
	mustAdd(t, g, NewNode("down", "", Deps(Requires(p), Requires(q)), Provides(out), func(rc *RunCtx) error {
		a, err := Get(rc, p)
		if err != nil {
			return err
		}
		b, err := Get(rc, q)
		if err != nil {
			return err
		}
		return Set(rc, out, a+b)
	}))

	if err := runOn(g); err != nil {
		t.Fatalf("合法图被误判成环：%v", err)
	}
}

// TestStartAcceptsDiamondWithSeed 无环图不受新判据影响：Seed 起点 + 菱形
// （一个上游扇出两条、再汇聚到一个下游）照常启动并跑完。
func TestStartAcceptsDiamondWithSeed(t *testing.T) {
	seed := NewKey[int]("dia.seed")
	left := NewKey[int]("dia.left")
	right := NewKey[int]("dia.right")
	out := NewKey[int]("dia.out")

	g := mustNew(t, context.Background(), "test")
	declareIn(t, g, NewPort(seed))
	if err := seedOn(g, seed, 1); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("L", "", Requires(seed), Provides(left), func(rc *RunCtx) error {
		v, err := Get(rc, seed)
		if err != nil {
			return err
		}
		return Set(rc, left, v+1)
	}))
	mustAdd(t, g, NewNode("R", "", Requires(seed), Provides(right), func(rc *RunCtx) error {
		v, err := Get(rc, seed)
		if err != nil {
			return err
		}
		return Set(rc, right, v+2)
	}))
	mustAdd(t, g, NewNode("D", "", Deps(Requires(left), Requires(right)), Provides(out), func(rc *RunCtx) error {
		a, err := Get(rc, left)
		if err != nil {
			return err
		}
		b, err := Get(rc, right)
		if err != nil {
			return err
		}
		return Set(rc, out, a+b)
	}))

	if err := runOn(g); err != nil {
		t.Fatalf("无环图被新判据误伤：%v", err)
	}
	if v, ok, skipped, err := TryGet(inspect(g), out); err != nil || !ok || skipped || v != 5 {
		t.Fatalf("out = %d ok=%v skipped=%v err=%v, want 5", v, ok, skipped, err)
	}
}

func TestEmptyNodeIDRejected(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	err := g.Add(NewNode("", "", nil, Provides(kA), func(rc *RunCtx) error { return nil }))
	if err == nil {
		t.Fatal("expected empty id error")
	}
}

func TestDuplicateRequiresRejected(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	err := g.Add(NewNode("n", "", Deps(Requires(kA), Requires(kA)), Provides(kB), func(rc *RunCtx) error { return nil }))
	if err == nil {
		t.Fatal("expected duplicate requires error")
	}
}

func TestDuplicateProvidesRejected(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	err := g.Add(NewNode("n", "", nil, Deps(Provides(kA), Provides(kA)), func(rc *RunCtx) error { return nil }))
	if err == nil {
		t.Fatal("expected duplicate provides error")
	}
}

func TestDuplicateNodeIDRejected(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("n", "", nil, Provides(kA), func(rc *RunCtx) error { return Skip(rc, kA) }))
	err := g.Add(NewNode("n", "", nil, Provides(kB), func(rc *RunCtx) error { return Skip(rc, kB) }))
	if err == nil {
		t.Fatal("expected duplicate id error")
	}
}

// TestStartRejectsUnsourcedRequires 装配期能静态判定的死图必须在 Start 就被拒：
// 「等一个永远不会被写入的槽」今天只表现为挂死（有 deadline 时是一句看不出病因的
// 超时，没有时进程会被 runtime 判为 fatal deadlock），而启动那一刻引擎就知道是
// 哪个节点的哪个 Key 没有来源。
func TestStartRejectsUnsourcedRequires(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("waiter", "", Requires(kA), nil, func(rc *RunCtx) error { return nil }))

	err := startOn(g)
	if err == nil {
		t.Fatal("Start 放行了一张不可能跑完的图")
	}
	if !strings.Contains(err.Error(), `"waiter"`) || !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("错误里要能读到节点 id 与 Key 名，got %v", err)
	}
	// 校验不过时图**仍未启动**：补上来源后可以重新 Start 并跑完。
	if werr := waitOn(g); !errors.Is(werr, ErrGraphNotStarted) {
		t.Fatalf("Start 失败后图不该是已启动状态：Wait() = %v", werr)
	}
	mustAdd(t, g, NewNode("producer", "", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kA, "v")
	}))
	if err := runOn(g); err != nil {
		t.Fatalf("补上来源后应能跑完：%v", err)
	}
}

// TestStartAcceptsSeededAndProvidedRequires 两种合法来源都不能被来源校验误伤：
// 声明过的入口（`DeclareInput`）喂进来的，以及另一个节点的 Provides（含隔一层的链路）。
func TestStartAcceptsSeededAndProvidedRequires(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	declareIn(t, g, NewPort(kA))
	if err := seedOn(g, kA, "seeded"); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("first", "", Requires(kA), Provides(kB), func(rc *RunCtx) error {
		v, err := Get(rc, kA)
		if err != nil {
			return err
		}
		return Set(rc, kB, v+"!")
	}))
	mustAdd(t, g, NewNode("second", "", Requires(kB), nil, func(rc *RunCtx) error {
		v, err := Get(rc, kB)
		if err != nil {
			return err
		}
		if v != "seeded!" {
			t.Fatalf("v = %q, want seeded!", v)
		}
		return nil
	}))
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}

	// SkipSeed 也是来源：唯一输入没值（到几个收几个，一条都没有）→ 不进入 Run，
	// 整图无错返回。
	g2 := mustNew(t, context.Background(), "test")
	declareIn(t, g2, NewPort(kA))
	if err := skipSeedOn(g2, kA); err != nil {
		t.Fatal(err)
	}
	var ran atomic.Bool
	mustAdd(t, g2, NewNode("skipped-input", "", Requires(kA), nil, func(rc *RunCtx) error {
		ran.Store(true)
		return nil
	}))
	if err := runOn(g2); err != nil {
		t.Fatal(err)
	}
	if ran.Load() {
		t.Fatal("唯一输入一条值都没到，不该进入 Run")
	}
}

func TestKeyTypeConflict(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("a", "", nil, Provides(NewKey[string]("dup")), func(rc *RunCtx) error { return nil }))
	err := g.Add(NewNode("b", "", nil, Provides(NewKey[int]("dup")), func(rc *RunCtx) error { return nil }))
	if err == nil {
		t.Fatal("expected type conflict")
	}
}

func TestUndeclaredWrite(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("a", "", nil, Provides(kA), func(rc *RunCtx) error {
		return Set(rc, kB, "nope")
	}))
	if err := runOn(g); err == nil {
		t.Fatal("expected undeclared write error")
	}
}

func TestSetSkipConflict(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("a", "", nil, Provides(kA), func(rc *RunCtx) error {
		if err := Set(rc, kA, "v"); err != nil {
			return err
		}
		return Skip(rc, kA)
	}))
	if err := runOn(g); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
}

// TestAddIsAtomic Add 失败必须让图保持调用前的样子：失败那次声明的
// Provides 不能占住 producer，否则真正提供它的节点此后一直吃
// ErrDuplicateSource，等它的节点永远停在 pending。
func TestAddIsAtomic(t *testing.T) {
	a := NewKey[string]("atomic.a")
	b := NewKey[string]("atomic.b")

	g := mustNew(t, context.Background(), "test")
	// b 已被另一个节点占住（阶段 2 起「来源」只可能是节点：喂值不占来源）。
	mustAdd(t, g, NewNode("owner", "", nil, Provides(b), func(rc *RunCtx) error {
		return Set(rc, b, "owned")
	}))
	// a 先被登记/claim，b 才冲突 → 失败的 Add 不能留下 a 的占位
	if err := g.Add(NewNode("bad", "", nil, Deps(Provides(a), Provides(b)), func(rc *RunCtx) error { return nil })); err == nil {
		t.Fatal("expect duplicate source error")
	}
	if err := g.Add(NewNode("good", "", nil, Provides(a), func(rc *RunCtx) error { return Set(rc, a, "x") })); err != nil {
		t.Fatalf("失败 Add 留下了 a 的 producer 占位：%v", err)
	}

	var got atomic.Bool
	mustAdd(t, g, NewNode("down", "", Requires(a), nil, func(rc *RunCtx) error {
		v, err := Get(rc, a)
		if err != nil {
			return err
		}
		got.Store(v == "x")
		return nil
	}))
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	if !got.Load() {
		t.Fatal("依赖 a 的节点没有拿到值")
	}
}

// TestSeedFailureLeavesNoTrace 与 Add 同一条纪律：校验不过的输入既不占来源、
// 也不落槽位。
//
// 阶段 2 之后 `Seed` 只写**这一次运行**的 `Inputs`（它见不到图），所以这两条判据从
// 「`Seed` 报错」搬到「`NewExecution` 报错」——纪律不变：报错的那一次一条槽都没写、
// 定义也没被写脏，补一份正当的输入照样跑。
func TestSeedFailureLeavesNoTrace(t *testing.T) {
	shared := NewKey[string]("seed.trace")
	g := mustNew(t, context.Background(), "test")
	mustAdd(t, g, NewNode("owner", "", nil, Provides(shared), func(rc *RunCtx) error {
		return Set(rc, shared, "from-node")
	}))

	// 同名不同类型：类型校验失败，不能把 name 的类型改成 int。
	var wrongType Inputs
	if err := Seed(&wrongType, NewKey[int]("seed.trace"), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := g.NewExecution(context.Background(), wrongType); err == nil {
		t.Fatal("expect key type conflict")
	}
	// 同名同类型：来源在**图内**，外面喂进来的值会被静默忽略——创建期就报。
	var fedInside Inputs
	if err := Seed(&fedInside, shared, "from-host"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.NewExecution(context.Background(), fedInside); err == nil {
		t.Fatal("expect source conflict")
	}

	// 两次失败都不留痕迹：定义没被写脏，跑出来的还是节点自己那条值。
	if err := g.Validate(); err != nil {
		t.Fatalf("失败的喂值写脏了定义：%v", err)
	}
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	if st, v := slotOn(g, shared.asRef()); st != slotReady || v != "from-node" {
		t.Fatalf("槽位 = (%v, %v)，want (ready, from-node)：失败的喂值落了槽", st, v)
	}
}

func TestGraphIDRequired(t *testing.T) {
	if _, err := New("", ""); err == nil {
		t.Fatal("empty graph id must be rejected")
	}
}

// TestNewNodeCopiesDeclarations 节点的声明一旦交进 `NewNode`，调用方手里那份切片就不该再能
// 改动它：`Add` 已经把依赖形状（来源表、`Requires` / `Provides`）登记进图，之后再改底层数组
// 等于绕过装配锁与封口去改定义——跑起来的图与校验 / 拓扑看到的图成了两张。
func TestNewNodeCopiesDeclarations(t *testing.T) {
	a := NewKey[string]("nc.a")
	b := NewKey[string]("nc.b")
	out := NewKey[string]("nc.out")
	hijack := NewKey[string]("nc.hijack")
	req := Requires(a)
	prov := Provides(out)
	aspects := []Aspect{func(rc *RunCtx, next func(*RunCtx) error) error { return next(rc) }}

	g := mustNew(t, context.Background(), "nc")
	declareIn(t, g, NewPort(a))
	mustAdd(t, g, NewNode("n", "", req, prov, func(rc *RunCtx) error {
		v, err := Get(rc, a)
		if err != nil {
			return err
		}
		return Set(rc, out, v)
	}, aspects...))

	// 调用方改那三份切片：图里那条声明不该跟着变。
	req[0] = Requires(b)[0]
	prov[0] = Provides(hijack)[0]
	aspects[0] = func(rc *RunCtx, next func(*RunCtx) error) error { return errors.New("hijacked") }

	top := g.Topology()
	if got := top.Nodes[0].Requires[0].Name; got != a.Name() {
		t.Fatalf("Requires = %q, want %q：NewNode 该复制声明", got, a.Name())
	}
	if got := top.Nodes[0].Provides[0].Name; got != out.Name() {
		t.Fatalf("Provides = %q, want %q：NewNode 该复制声明", got, out.Name())
	}
	// 行为判据：依赖还是 a 那条值、切面也没被换掉。
	var in Inputs
	if err := Seed(&in, a, "v"); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(context.Background(), in); err != nil {
		t.Fatalf("跑 = %v，want nil：声明被调用方那份切片改掉了", err)
	}
}

// 描述是**构造参数**、不是 Option：#303 阶段一的三处（图 / 节点 / `Sub` 那一步）
// 都能读回来，空串合法，且入图后不再变（只由构造函数写入，没有 setter）。
func TestDescriptionRoundTrip(t *testing.T) {
	g, err := New("P", "主流程")
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Description(); got != "主流程" {
		t.Fatalf("Graph.Description() = %q, want 主流程", got)
	}

	n := NewNode("load", "读入库", nil, nil, func(*RunCtx) error { return nil })
	if got := n.Description(); got != "读入库" {
		t.Fatalf("Node.Description() = %q, want 读入库", got)
	}

	// `Sub` 那一步的描述落在**父侧那个节点**上（子图自己的描述走它自己的 `New`）。
	in := NewKey[string]("desc.in")
	cin := NewKey[string]("desc.cin")
	if err := Seed(&pendingOf(g).in, in, "v"); err != nil {
		t.Fatal(err)
	}
	inner, err := New("P/step", "内层子图")
	if err != nil {
		t.Fatal(err)
	}
	if err := inner.DeclareInput(NewPort(cin)); err != nil {
		t.Fatal(err)
	}
	err = Sub(g, "step", "跑一张子图", []SubBind{In(in, cin)}, inner)
	if err != nil {
		t.Fatal(err)
	}
	var step *Node
	for _, cand := range g.nodes {
		if cand.id == "step" {
			step = cand
		}
	}
	if step == nil {
		t.Fatal("没找到 Sub 那一步")
	}
	if got := step.Description(); got != "跑一张子图" {
		t.Fatalf("Sub 那一步的描述 = %q, want 跑一张子图", got)
	}

	// 空串 = 没写，合法。
	empty, err := New("Q", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := empty.Description(); got != "" {
		t.Fatalf("没写描述时应该是 \"\"，got %q", got)
	}
}
