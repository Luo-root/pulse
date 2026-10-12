package pulse

import (
	"context"
	"sync"
	"testing"
)

// 本文件是**测试专用**的脚手架：把「这一次运行的 ctx 与输入」按图暂存起来。
//
// 引擎侧的运行入口是 `g.Run(ctx, in)` / `g.NewExecution(ctx, in)`——ctx 与输入属于
// **一次运行**，不再是图上的字段。用例里上百处「先种几条值、再 `g.Run()`」如果逐处展开成
// `var in Inputs; Seed(&in, k, v); g.Run(ctx, in)`，每个用例要多几行样板，而它们要测的是
// **行为**、不是 API 的拼写；所以这里把这一次运行的 ctx 与输入暂存起来，用例照旧写它
// 要表达的时序。
//
// **这一层只活在测试包里**，不进公开面，也不是用户可以照抄的形状：文档、README 与站点里
// 一律写真实形状（`g.Run(ctx, in)`）。新用例也**优先直接写** `g.Run(ctx, in)`——这一层是
// 为了不让几百处既有调用点淹掉真正要改的地方，不是给新用例用的糖。

// pendingRun 是一张图「这一次运行」的暂存：ctx、输入，以及已经建出来的执行
// （`startOn` / `runOn` 各自记一份，好让 `waitOn` / `errOn` 找得到）。
type pendingRun struct {
	ctx context.Context
	in  Inputs
	e   *Execution
}

var (
	pendingMu   sync.Mutex
	pendingRuns = map[*Graph]*pendingRun{}
)

func pendingOf(g *Graph) *pendingRun {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	pr := pendingRuns[g]
	if pr == nil {
		pr = &pendingRun{}
		pendingRuns[g] = pr
	}
	return pr
}

// seedOn 给「这一次运行」种一条输入（引擎侧是 `Seed(&in, k, v)`）。
func seedOn[T any](g *Graph, k Key[T], v T) error {
	return Seed(&pendingOf(g).in, k, v)
}

// skipSeedOn 把「这一次运行」的某条输入标成跳过（引擎侧是 `SkipSeed(&in, k)`）。
func skipSeedOn[T any](g *Graph, k Key[T]) error {
	return SkipSeed(&pendingOf(g).in, k)
}

// runOn 用这张图暂存的 ctx 与输入跑一遍：等价于 `g.Run(ctx, in)`。
func runOn(g *Graph) error {
	pr := pendingOf(g)
	e, err := g.NewExecution(pr.runCtx(), pr.in)
	if err != nil {
		return err
	}
	pr.e = e
	if err := e.Start(); err != nil {
		return err
	}
	return e.Wait()
}

// startOn 等价于 `e, err := g.NewExecution(ctx, in); e.Start()`——执行本身也暂存下来，
// 好让 `waitOn` / `errOn` 找得到。
func startOn(g *Graph) error {
	pr := pendingOf(g)
	e, err := g.NewExecution(pr.runCtx(), pr.in)
	if err != nil {
		return err
	}
	pr.e = e
	return e.Start()
}

// runCtx 是这次运行用的 ctx：`mustNew` 记下的那个（用例从没给过就用 Background）。
func (pr *pendingRun) runCtx() context.Context {
	if pr.ctx == nil {
		return context.Background()
	}
	return pr.ctx
}

func waitOn(g *Graph) error {
	pr := pendingOf(g)
	if pr.e == nil {
		return ErrGraphNotStarted
	}
	return pr.e.Wait()
}

func errOn(g *Graph) error {
	pr := pendingOf(g)
	if pr.e == nil {
		return ErrGraphNotStarted
	}
	return pr.e.Err()
}

// execOn 取这张图**这一次运行**的执行——`runOn` / `startOn` 建出来的那一份。
// 要看这次运行的槽位 / ctx / 首错本身（而不是只看返回值）的用例用它。
func execOn(g *Graph) *Execution {
	pr := pendingOf(g)
	if pr.e == nil {
		panic("execOn: 这张图还没建过执行（试 runOn / startOn / mustExec）")
	}
	return pr.e
}

// slotOn 看这次运行里某条键的槽位终态（引擎侧是 `e.slotOf(ref).snapshot()`）。
func slotOn(g *Graph, ref keyRef) (slotState, any) {
	return execOn(g).slotOf(ref).snapshot()
}

// declareIn 声明这张图的**外部进口**（引擎侧是 `g.DeclareInput(NewPort(k), …)`）。
//
// 阶段 2 起「声明外部来源」与「写这一次的值」是两件事：值喂给没声明过的键是错误
// （那正是「你忘了接线」与「这本来就是外部输入」分开的地方），所以用到输入的用例
// 都要有这一行。写成薄封装只是省掉 `if err != nil` 那三行。
func declareIn(t *testing.T, g *Graph, ports ...Port) {
	t.Helper()
	if err := g.DeclareInput(ports...); err != nil {
		t.Fatalf("DeclareInput: %v", err)
	}
}

// declareOut 同上，出口。
func declareOut(t *testing.T, g *Graph, ports ...Port) {
	t.Helper()
	if err := g.DeclareOutput(ports...); err != nil {
		t.Fatalf("DeclareOutput: %v", err)
	}
}

// mustExec 建一份执行（要拿到 `*Execution` 本身、而不是只看错误的用例用它）。
func mustExec(t *testing.T, g *Graph, ctx context.Context, in Inputs) *Execution {
	t.Helper()
	e, err := g.NewExecution(ctx, in)
	if err != nil {
		t.Fatalf("NewExecution(%s): %v", g.ID(), err)
	}
	return e
}

// runWith 是「显式给 ctx 与输入、跑一遍」的薄封装，给不使用暂存的那批用例用。
func runWith(g *Graph, ctx context.Context, in Inputs) error { return g.Run(ctx, in) }
