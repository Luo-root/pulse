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

// 本文件是阶段 2（`Graph` = 定义、`Execution` = 一次运行）的**验收用例**：
// 票面 `#315` 的行为清单逐条落地，而且**照真实形状写**（`g.Run(ctx, in)` 那一套），
// 不用 testrun_test.go 的脚手架——脚手架是为了不让几百处既有调用点淹掉要改的地方，
// 新用例该长成用户照着抄的样子。

// TestDefinitionRunsManyTimes 同一张定义顺序跑两次：两次各自正确、第二次不受第一次影响。
//
// 旧模型里这是做不到的（`Start` 成功即认领、槽位不重开）；现在「一次运行」的所有状态
// （槽位 / 首错 / 取消域 / 每节点进度）都长在 `Execution` 上。
func TestDefinitionRunsManyTimes(t *testing.T) {
	ctx := context.Background()
	in := NewKey[string]("seq.in")
	out := NewKey[string]("seq.out")
	g := mustNew(t, ctx, "seq")
	declareIn(t, g, NewPort(in))
	mustAdd(t, g, NewNode("echo", "", Requires(in), Provides(out), func(rc *RunCtx) error {
		v, err := Get(rc, in)
		if err != nil {
			return err
		}
		return Set(rc, out, v+"!")
	}))

	for _, v := range []string{"first", "second", "third"} {
		var inputs Inputs
		if err := Seed(&inputs, in, v); err != nil {
			t.Fatal(err)
		}
		e, err := g.NewExecution(ctx, inputs)
		if err != nil {
			t.Fatalf("第 %s 轮 NewExecution: %v", v, err)
		}
		if err := e.Start(); err != nil {
			t.Fatalf("第 %s 轮 Start: %v", v, err)
		}
		if err := e.Wait(); err != nil {
			t.Fatalf("第 %s 轮 Wait: %v", v, err)
		}
		// 每一轮读的都是**自己那一份**执行里的槽位。
		if st, got := e.slotOf(out.asRef()).snapshot(); st != slotReady || got != v+"!" {
			t.Fatalf("第 %s 轮 out = (%v, %v)", v, st, got)
		}
	}
}

// TestDefinitionRunsConcurrently 同一张定义并发跑两份执行：互不污染（`-race` 是判据）。
//
// 两次的值不同、跑的次数不同（一边等一个信号），判据是「各读到各的那一份」——
// 只判「都跑完了」兜不住「其实共用了一份槽位表」。
func TestDefinitionRunsConcurrently(t *testing.T) {
	ctx := context.Background()
	in := NewKey[string]("conc.in")
	out := NewKey[string]("conc.out")
	g := mustNew(t, ctx, "conc")
	declareIn(t, g, NewPort(in))
	release := make(chan struct{})
	mustAdd(t, g, NewNode("slow", "", Requires(in), Provides(out), func(rc *RunCtx) error {
		v, err := Get(rc, in)
		if err != nil {
			return err
		}
		if v == "blocked" {
			select {
			case <-release:
			case <-rc.Context().Done():
				return rc.Context().Err()
			}
		}
		return Set(rc, out, v+"!")
	}))

	var slowInputs, fastInputs Inputs
	if err := Seed(&slowInputs, in, "blocked"); err != nil {
		t.Fatal(err)
	}
	if err := Seed(&fastInputs, in, "quick"); err != nil {
		t.Fatal(err)
	}
	slow, err := g.NewExecution(ctx, slowInputs)
	if err != nil {
		t.Fatal(err)
	}
	fast, err := g.NewExecution(ctx, fastInputs)
	if err != nil {
		t.Fatal(err)
	}
	if err := slow.Start(); err != nil {
		t.Fatal(err)
	}
	if err := fast.Start(); err != nil {
		t.Fatal(err)
	}
	if err := fast.Wait(); err != nil {
		t.Fatalf("快的那一份该干净跑完：%v", err)
	}
	if _, got := fast.slotOf(out.asRef()).snapshot(); got != "quick!" {
		t.Fatalf("快的这一份 out = %v，want quick!", got)
	}
	// 慢的那一份还在等：它的槽位**不许**被另一份写成 quick!。
	if st, got := slow.slotOf(out.asRef()).snapshot(); st == slotReady {
		t.Fatalf("慢的那一份被别人写了：(%v, %v)", st, got)
	}
	close(release)
	if err := slow.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, got := slow.slotOf(out.asRef()).snapshot(); got != "blocked!" {
		t.Fatalf("慢的这一份 out = %v，want blocked!", got)
	}
	if slow.ID() == fast.ID() {
		t.Fatalf("两份执行的身份必须不同，都是 %q", slow.ID())
	}
}

// TestCancelAffectsOnlyOneRun 取消只影响**这一次**：被取消的那一份报取消，
// 同一张定义换一份执行照旧跑得成（取消没有落到定义上，也没有落到别的运行上）。
func TestCancelAffectsOnlyOneRun(t *testing.T) {
	in := NewKey[string]("cxl2.in")
	out := NewKey[string]("cxl2.out")
	g := mustNew(t, context.Background(), "cxl2")
	declareIn(t, g, NewPort(in))
	mustAdd(t, g, NewNode("wait", "", Requires(in), Provides(out), func(rc *RunCtx) error {
		v, err := Get(rc, in)
		if err != nil {
			return err
		}
		if v == "wait" {
			<-rc.Context().Done()
			return rc.Context().Err()
		}
		return Set(rc, out, v)
	}))

	runCtx, cancel := context.WithCancel(context.Background())
	var waitInputs Inputs
	if err := Seed(&waitInputs, in, "wait"); err != nil {
		t.Fatal(err)
	}
	waiter, err := g.NewExecution(runCtx, waitInputs)
	if err != nil {
		t.Fatal(err)
	}
	if err := waiter.Start(); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := waiter.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("被取消的那一份 Wait = %v，want context.Canceled", err)
	}

	// 同一张定义、换个 ctx 再跑一次：成功（定义没被上一次的取消写脏，也没有「跑过就废」）。
	var okInputs Inputs
	if err := Seed(&okInputs, in, "go"); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(context.Background(), okInputs); err != nil {
		t.Fatalf("取消之后同一张定义该还能跑：%v", err)
	}
	// 已经跑过的那一份执行不给第二次机会（一次运行就是一次）。
	again, err := g.NewExecution(context.Background(), okInputs)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Start(); err != nil {
		t.Fatal(err)
	}
	if err := again.Start(); !errors.Is(err, ErrGraphStarted) {
		t.Fatalf("同一份执行二次 Start = %v，want ErrGraphStarted", err)
	}
	if err := again.Wait(); err != nil {
		t.Fatal(err)
	}
}

// TestExecutionRejectsMissingInput 输入不齐在**创建那一刻**报，点名是哪条键——
// 而不是等运行期挂死到 ctx 到期（旧模型就是那样）。
//
// 判据是「有人 `Requires` 它」，不是「声明过的进口都要喂」：一条声明了却没人用的进口
// （留作公共接口）不该逼调用方编个值出来。
func TestExecutionRejectsMissingInput(t *testing.T) {
	ctx := context.Background()
	needed := NewKey[string]("mi.needed")
	unused := NewKey[string]("mi.unused")
	out := NewKey[string]("mi.out")
	g := mustNew(t, ctx, "mi")
	declareIn(t, g, NewPort(needed), NewPort(unused))
	mustAdd(t, g, NewNode("n", "", Requires(needed), Provides(out), func(rc *RunCtx) error {
		v, err := Get(rc, needed)
		if err != nil {
			return err
		}
		return Set(rc, out, v)
	}))

	_, err := g.NewExecution(ctx, Inputs{})
	if err == nil {
		t.Fatal("声明过的进口没人喂，建执行就该报")
	}
	for _, want := range []string{"has no value for this run", needed.Name(), `"n"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("报文该含 %q，got %v", want, err)
		}
	}
	// 非空对照：没人 Requires 的那条进口不喂也照样能建（公共接口留白是合法的）。
	var in Inputs
	if err := Seed(&in, needed, "v"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.NewExecution(ctx, in); err != nil {
		t.Fatalf("没人读的进口不喂不该挡住建执行：%v", err)
	}
}

// TestExecutionRejectsUndeclaredFeed 喂一条**没 `DeclareInput` 过**的键 → 创建期报。
//
// 旧模型里 `Seed` 顺带承担了「声明外部来源」，所以「你忘了接线」与「这本来就是外部输入」
// 分不开；现在声明归 `DeclareInput`、值归 `Inputs`，于是喂错键当场就是一句错。
func TestExecutionRejectsUndeclaredFeed(t *testing.T) {
	ctx := context.Background()
	out := NewKey[string]("uf.out")
	g := mustNew(t, ctx, "uf")
	mustAdd(t, g, NewNode("src", "", nil, Provides(out), func(rc *RunCtx) error {
		return Set(rc, out, "v")
	}))

	var in Inputs
	if err := Seed(&in, NewKey[string]("uf.ghost"), "v"); err != nil {
		t.Fatal(err)
	}
	_, err := g.NewExecution(ctx, in)
	if err == nil {
		t.Fatal("喂没声明的键该报")
	}
	for _, want := range []string{"not a declared input", "uf.ghost", "DeclareInput"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("报文该含 %q，got %v", want, err)
		}
	}
}

// TestRunLeavesDefinitionUntouched 运行**不写脏定义**：跑完子图的 `Topology()` /
// `Validate()` 与跑之前逐字一致，而且它还能挂到别处再跑一遍（阶段 1 的「封口 ≠ 用过」）。
func TestRunLeavesDefinitionUntouched(t *testing.T) {
	ctx := context.Background()
	cin := NewKey[string]("du.cin")
	cout := NewKey[string]("du.cout")
	child := mustNew(t, ctx, "du.child")
	if err := child.DeclareInput(NewPort(cin)); err != nil {
		t.Fatal(err)
	}
	if err := child.DeclareOutput(NewPort(cout)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, child, NewNode("n", "", Requires(cin), Provides(cout), func(rc *RunCtx) error {
		v, err := Get(rc, cin)
		if err != nil {
			return err
		}
		return Set(rc, cout, v+"|child")
	}))
	before := fmt.Sprintf("%+v", child.Topology())

	in := NewKey[string]("du.in")
	out1 := NewKey[string]("du.out1")
	out2 := NewKey[string]("du.out2")
	g := mustNew(t, ctx, "du.parent")
	declareIn(t, g, NewPort(in))
	if err := Sub(g, "one", "", []SubBind{In(in, cin), Out(cout, out1)}, child); err != nil {
		t.Fatal(err)
	}
	var inputs Inputs
	if err := Seed(&inputs, in, "a"); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(ctx, inputs); err != nil {
		t.Fatal(err)
	}
	if after := fmt.Sprintf("%+v", child.Topology()); after != before {
		t.Fatalf("跑完之后子图定义变了：\nbefore %s\nafter  %s", before, after)
	}
	if err := child.Validate(); err != nil {
		t.Fatalf("跑完之后子图自己的 Validate 该照旧通过：%v", err)
	}

	// 还能挂到别处跑：同一张子图、另一个父图、另一个输出键。
	other := mustNew(t, ctx, "du.other")
	declareIn(t, other, NewPort(in))
	if err := Sub(other, "two", "", []SubBind{In(in, cin), Out(cout, out2)}, child); err != nil {
		t.Fatalf("跑过的子图该还能挂到别处：%v", err)
	}
	var inputs2 Inputs
	if err := Seed(&inputs2, in, "b"); err != nil {
		t.Fatal(err)
	}
	if err := other.Run(ctx, inputs2); err != nil {
		t.Fatal(err)
	}
	top := other.Topology()
	if got := top.Nodes[0].Sub; got == nil || got.Graph.Path != "two" {
		t.Fatalf("挂载位置该推导成 two，got %+v", got)
	}
}

// TestExecOptionPrecedence `ExecOption` 是**本次运行**的覆盖：优先级
// 本次运行 > 定义声明；`WithExecAspects` 是**追加**不是替换（定义声明的那批仍在）。
func TestExecOptionPrecedence(t *testing.T) {
	ctx := context.Background()
	out := NewKey[string]("eo.out")
	defObs, execObs := &recordingObserver{}, &recordingObserver{}
	defRan, execRan := new(atomic.Int32), new(atomic.Int32)
	defAspect := func(rc *RunCtx, next func(*RunCtx) error) error {
		defRan.Add(1)
		return next(rc)
	}
	execAspect := func(rc *RunCtx, next func(*RunCtx) error) error {
		execRan.Add(1)
		return next(rc)
	}
	g := mustNew(t, ctx, "eo", WithObserver(defObs), WithAspects(defAspect))
	mustAdd(t, g, NewNode("n", "", nil, Provides(out), func(rc *RunCtx) error {
		return Set(rc, out, "v")
	}))

	// 定义声明的观察者 + 定义声明的切面：这一次运行照样看得见。
	if err := g.Run(ctx, Inputs{}); err != nil {
		t.Fatal(err)
	}
	if len(defObs.snapshot()) == 0 {
		t.Fatal("没传 ExecOption 时该用定义声明的观察者")
	}
	if defRan.Load() != 1 {
		t.Fatalf("定义声明的切面跑了 %d 次，want 1", defRan.Load())
	}

	// 传了 ExecOption：观察者换成本次的（定义那份这一次收不到），切面**追加**上去。
	if err := g.Run(ctx, Inputs{}, WithExecObserver(execObs), WithExecAspects(execAspect)); err != nil {
		t.Fatal(err)
	}
	if got := countPref(execObs.snapshot(), "GS"); got != 1 {
		t.Fatalf("本次运行的观察者该收到 GS 一次，got %d（%v）", got, execObs.snapshot())
	}
	// 第一轮那一整轮（GS + W/R/F + GF = 5 条）仍在，而第二轮它一条都没收到。
	if got := len(defObs.snapshot()); got != 5 {
		t.Fatalf("第二轮不该走到定义声明的观察者：它该还是第一轮那 5 条，got %d（%v）",
			got, defObs.snapshot())
	}
	if defRan.Load() != 2 || execRan.Load() != 1 {
		t.Fatalf("切面该是**追加**：定义那份共 2 次、本次那份 1 次，got %d / %d",
			defRan.Load(), execRan.Load())
	}
}

// TestExecMaxRunningOverridesDefinition 名额同理：定义声明 1、本次运行给 4 →
// 两个节点可以同时在 Run 里（本次运行的声明压过定义）。
func TestExecMaxRunningOverridesDefinition(t *testing.T) {
	ctx := context.Background()
	a, b := NewKey[string]("em.a"), NewKey[string]("em.b")
	g := mustNew(t, ctx, "em", WithMaxRunning(1))

	var cur, peak atomic.Int32
	var mu sync.Mutex
	body := func(rc *RunCtx, out Key[string], v string) error {
		n := cur.Add(1)
		mu.Lock()
		if n > peak.Load() {
			peak.Store(n)
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		cur.Add(-1)
		return Set(rc, out, v)
	}
	mustAdd(t, g, NewNode("a", "", nil, Provides(a), func(rc *RunCtx) error { return body(rc, a, "a") }))
	mustAdd(t, g, NewNode("b", "", nil, Provides(b), func(rc *RunCtx) error { return body(rc, b, "b") }))

	if err := g.Run(ctx, Inputs{}); err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got != 1 {
		t.Fatalf("定义声明 1 个名额时峰值 = %d，want 1", got)
	}
	peak.Store(0)
	if err := g.Run(ctx, Inputs{}, WithExecMaxRunning(4)); err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got != 2 {
		t.Fatalf("本次运行给 4 个名额时峰值 = %d，want 2（两个节点同时在跑）", got)
	}
}

// TestSitePathMatchesTopology 身份视图两处一致（第三处 `Snapshot()` 是阶段 4）：
// 同一张图的同一个位置，`Topology().Graph.Path` 与观测里的 `Site.Path` 逐字相同。
func TestSitePathMatchesTopology(t *testing.T) {
	ctx := context.Background()
	cin := NewKey[string]("sp2.cin")
	cout := NewKey[string]("sp2.cout")
	in := NewKey[string]("sp2.in")
	out := NewKey[string]("sp2.out")

	child := mustNew(t, ctx, "sp2.child")
	if err := child.DeclareInput(NewPort(cin)); err != nil {
		t.Fatal(err)
	}
	if err := child.DeclareOutput(NewPort(cout)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, child, NewNode("inner", "", Requires(cin), Provides(cout), func(rc *RunCtx) error {
		return Set(rc, cout, "v")
	}))

	obs := &recordingObserver{}
	g := mustNew(t, ctx, "sp2.parent", WithObserver(obs))
	declareIn(t, g, NewPort(in))
	if err := Sub(g, "step", "", []SubBind{In(in, cin), Out(cout, out)}, child); err != nil {
		t.Fatal(err)
	}
	var inputs Inputs
	if err := Seed(&inputs, in, "v"); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(ctx, inputs); err != nil {
		t.Fatal(err)
	}

	top := g.Topology()
	sub := top.Nodes[0].Sub
	if sub == nil {
		t.Fatal("挂载之后 Nodes[0].Sub 该是子图定义")
	}
	path := sub.Graph.Path
	if path != "step" {
		t.Fatalf("静态形状里的 path = %q，want step", path)
	}
	// 观测里的同一个位置：子执行的 `Site.Path` 与它逐字相同，且根执行是空串。
	if !obs.pathSeen(path) {
		t.Fatalf("观测里没有 path=%q 的图级事件：%+v", path, obs.sites)
	}
	if !obs.pathSeen("") {
		t.Fatalf("根执行的位置该是空串：%+v", obs.sites)
	}
	// 身份三件套：定义 id（挂载不改写它）、本次运行 id（唯一）、位置。
	for _, s := range obs.sites {
		if s.Graph == "" || s.Execution == "" {
			t.Fatalf("身份缺字段：%+v", s)
		}
	}
}

// TestSubStepRunsInEveryExecution 挂子图那一步的「只进一次」管的是**这一次运行**，
// 不是「这张定义跑过没有」。
//
// 判据是两次都拿到各自正确的结果：顺序两遍、并发四份。旧形状把 `entered` / `firstErr`
// 捕获在静态 Node 的体闭包里——那份闭包随定义建一次、每次都跑它，于是「这张定义跑过
// 一遍」被记成了「这一步跑过了」：第二遍与并发的第二份都会被误报成重入（而它本来正是
// 要挡住「一次运行里第二次进入这一步」）。
func TestSubStepRunsInEveryExecution(t *testing.T) {
	ctx := context.Background()
	cin := NewKey[string]("se.cin")
	cout := NewKey[string]("se.cout")
	in := NewKey[string]("se.in")
	out := NewKey[string]("se.out")

	child := mustNew(t, ctx, "se.child")
	declareIn(t, child, NewPort(cin))
	declareOut(t, child, NewPort(cout))
	mustAdd(t, child, NewNode("inner", "", Requires(cin), Provides(cout), func(rc *RunCtx) error {
		v, err := Get(rc, cin)
		if err != nil {
			return err
		}
		return Set(rc, cout, v+"|child")
	}))

	g := mustNew(t, ctx, "se.parent")
	declareIn(t, g, NewPort(in))
	if err := Sub(g, "step", "", []SubBind{In(in, cin), Out(cout, out)}, child); err != nil {
		t.Fatal(err)
	}

	// 顺序两遍。
	for i := 0; i < 2; i++ {
		sent, want := fmt.Sprintf("v%d", i), fmt.Sprintf("v%d|child", i)
		var inputs Inputs
		if err := Seed(&inputs, in, sent); err != nil {
			t.Fatal(err)
		}
		e, err := g.NewExecution(ctx, inputs)
		if err != nil {
			t.Fatalf("第 %d 轮 NewExecution: %v", i, err)
		}
		if err := e.Start(); err != nil {
			t.Fatalf("第 %d 轮 Start: %v", i, err)
		}
		if err := e.Wait(); err != nil {
			t.Fatalf("第 %d 轮 Wait: %v", i, err)
		}
		if st, got := e.slotOf(out.asRef()).snapshot(); st != slotReady || got != want {
			t.Fatalf("第 %d 轮 out = (%v, %v), want (ready, %s)", i, st, got, want)
		}
	}

	// 并发四份：各建各的子执行，互不干扰（`-race` 是另一半判据）。
	const n = 4
	errs := make([]error, n)
	vals := make([]any, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			var inputs Inputs
			if err := Seed(&inputs, in, fmt.Sprintf("c%d", i)); err != nil {
				errs[i] = err
				return
			}
			e, err := g.NewExecution(ctx, inputs)
			if err != nil {
				errs[i] = err
				return
			}
			if err := e.Start(); err != nil {
				errs[i] = err
				return
			}
			if err := e.Wait(); err != nil {
				errs[i] = err
				return
			}
			_, vals[i] = e.slotOf(out.asRef()).snapshot()
		}()
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("并发第 %d 份：%v", i, errs[i])
		}
		if want := fmt.Sprintf("c%d|child", i); vals[i] != want {
			t.Fatalf("并发第 %d 份 out = %v, want %s", i, vals[i], want)
		}
	}
}

// TestStartRejectsLateRequiredInput 执行建好之后、启动之前，定义又长出一个「要这条进口」的
// 节点：那份执行手里没有这条输入，`Start` 必须拒。
//
// 判据与创建那一刻是同一条（「这一次运行里，被 `Requires` 的进口都要有交代」），只是判两次：
// 创建时判一次是**早反馈**，`Start` 再判一次是因为它接着就要封口——「这次运行要用的定义」
// 正是被它封上的那一张，两者必须是同一个版本。少了后一次，定义就能在这两刻之间长出新要求，
// 而这份执行只会等一个永远不来的槽（等到 ctx 到期，报一句看不出病因的超时）。
func TestStartRejectsLateRequiredInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	late := NewKey[string]("lr.late")
	out := NewKey[string]("lr.out")
	g := mustNew(t, ctx, "lr")
	declareIn(t, g, NewPort(late))

	// 声明了却没人 Requires：空输入也建得起执行（公共接口留白是合法的）。
	e, err := g.NewExecution(ctx, Inputs{})
	if err != nil {
		t.Fatalf("没人读的进口不该挡住建执行：%v", err)
	}

	// 建完执行之后，定义长出「要这条进口」的节点。
	mustAdd(t, g, NewNode("n", "", Requires(late), Provides(out), func(rc *RunCtx) error {
		v, err := Get(rc, late)
		if err != nil {
			return err
		}
		return Set(rc, out, v)
	}))

	err = e.Start()
	if err == nil {
		t.Fatal("Start 该拒：这份执行没有 late 的值，跑起来只会等到 ctx 到期")
	}
	for _, want := range []string{"has no value for this run", late.Name(), `"n"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("报文该含 %q，got %v", want, err)
		}
	}
	// 被拒的启动不算启动过（与静态校验被拒同一条线性化规则）。
	if err := e.Wait(); !errors.Is(err, ErrGraphNotStarted) {
		t.Fatalf("被拒之后 Wait = %v, want ErrGraphNotStarted", err)
	}

	// 一层都没封：补上输入、换一份执行照样跑得通。
	var in Inputs
	if err := Seed(&in, late, "v"); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(ctx, in); err != nil {
		t.Fatalf("补上输入之后该跑得通：%v", err)
	}
}
