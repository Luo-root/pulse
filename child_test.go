package pulse

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件是快照里那条**子图活句柄**（#303 决策 I / #309 阶段二）：`NodeState.Child`
// 让「子图跑到哪了」也能问到底，而不是只有父图那一行。
//
// 三条边界是本文件的主线：
//
//  1. **有值的时刻是 `build` 返回之后**，不是父图 `Start` 之后（那一步没进 `Run`
//     时 `build` 一次都不被调用）；
//  2. **没跑到就没句柄**（`nil`），读它不 panic、不阻塞；
//  3. **认领即封条**：`Child` 是活对象，但拿到它只能读——`Add` / `Seed` / `Start`
//     从此一律 `ErrGraphStarted`（这是本票唯一一处改既有行为的地方）。

// subCase 装一张「父图 + 一步子图」的最小形状：父图一条种子进、一条结果从子图
// 桥回。leaf 是子图里那个节点。
//
// 四张键名刻意各不相同：父侧的入 / 出与子侧的入 / 出是**两张图**里的槽，
// 同名也合法，混在一个用例里读起来会分不清谁是谁。
type subCase struct {
	parent *Graph
	child  *Graph // build 交出来的那张（父侧那个节点只是引用了它）
	inKey  Key[string]
	outKey Key[string]
	stepID string
}

func newSubCase(t *testing.T, leaf func(*RunCtx) error, opts ...Option) *subCase {
	t.Helper()
	return newSubCaseOn(t, context.Background(), leaf, opts...)
}

// newSubCaseOn 与 newSubCase 同形，但父图建在给定的 ctx 上（取消那一路要它）。
//
// 返回指针而不是值：`build` 里把子图写进 `child` 字段的那个闭包捕获的是**这个**
// 结构，按值返回的话调用方拿到的是副本、那个字段永远是 nil。
func newSubCaseOn(t *testing.T, ctx context.Context, leaf func(*RunCtx) error, opts ...Option) *subCase {
	t.Helper()
	pin := NewKey[string]("c.pin")
	cout := NewKey[string]("c.cout")
	cin := NewKey[string]("c.cin")
	pout := NewKey[string]("c.pout")

	c := &subCase{inKey: pin, outKey: pout, stepID: "step"}
	g, err := New(ctx, "P", "父图", opts...)
	if err != nil {
		t.Fatal(err)
	}
	c.parent = g
	if err := Seed(g, pin, "父侧种子"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step", "跑一张子图", []SubBind{In(pin, cin), Out(cout, pout)},
		func(sc *SubCtx) (*Graph, error) {
			child, err := New(sc.Context(), sc.GraphID(), "子图")
			if err != nil {
				return nil, err
			}
			if err := child.Add(NewNode("work", "子图里那一步", Requires(cin), Provides(cout), leaf)); err != nil {
				return nil, err
			}
			c.child = child
			return child, nil
		}); err != nil {
		t.Fatal(err)
	}
	return c
}

// waitClose 等到 ch 关闭；用例挂死比失败难查得多，所以带上限。
func waitClose(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("等不到 %s", what)
	}
}

// childOf 非致命地取某一行的子图句柄——并发轮询那条路里不能调 t.Fatalf。
func childOf(s *Snapshot, id string) *Graph {
	for _, n := range s.Nodes {
		if n.ID == id {
			return n.Child
		}
	}
	return nil
}

// TestSnapshotChildLiveHandle：跑完一轮后，那一步的 `Child` 非 nil，且它就是
// `build` 交出来的那张图；`Snapshot()` 给出的 id 与 path 与 `runSub` 给 `SubCtx`
// 的一致；两层嵌套能一路走到底。
func TestSnapshotChildLiveHandle(t *testing.T) {
	pin := NewKey[string]("h.pin")
	cout := NewKey[string]("h.cout")
	cin := NewKey[string]("h.cin")
	pout := NewKey[string]("h.pout")

	var outer, inner *Graph
	var outerID, outerPath, innerID, innerPath string

	g := mustNew(t, context.Background(), "H")
	if err := Seed(g, pin, "叶子"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "outer", "外层的子图", []SubBind{In(pin, cin), Out(cout, pout)},
		func(sc *SubCtx) (*Graph, error) {
			outerID, outerPath = sc.GraphID(), sc.Path()
			child, err := New(sc.Context(), sc.GraphID(), "外层")
			if err != nil {
				return nil, err
			}
			outer = child
			if err := Sub(child, "inner", "内层的子图", []SubBind{In(cin, cin), Out(cout, cout)},
				func(sc *SubCtx) (*Graph, error) {
					innerID, innerPath = sc.GraphID(), sc.Path()
					grand, err := New(sc.Context(), sc.GraphID(), "内层")
					if err != nil {
						return nil, err
					}
					inner = grand
					return grand, grand.Add(NewNode("leaf", "真正干活的那个", Requires(cin), Provides(cout),
						func(rc *RunCtx) error {
							v, err := Get(rc, cin)
							if err != nil {
								return err
							}
							return Set(rc, cout, v+"→更深一层")
						}))
				}); err != nil {
				return nil, err
			}
			return child, nil
		}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	root := g.Snapshot()
	step := nodeStatus(t, root, "outer")
	if step.Status != NodeStatusCompleted {
		t.Fatalf("那一步状态 %q，期望 completed", step.Status)
	}
	if step.Child == nil {
		t.Fatal("子图节点跑完之后应当留着活句柄")
	}
	if step.Child != outer {
		t.Error("Child 拿到的不是 build 交出来的那张图")
	}
	if step.Child.ID() != outerID {
		t.Errorf("Child.ID() = %q，期望 %q（SubCtx.GraphID 给的那个）", step.Child.ID(), outerID)
	}
	outerSnap := step.Child.Snapshot()
	if outerSnap.Graph.Path != outerPath || outerPath != "outer" {
		t.Errorf("外层 path = %q，期望 %q（且合层级记号）", outerSnap.Graph.Path, outerPath)
	}
	if outerSnap.Graph.Status != GraphCompleted {
		t.Errorf("外层子图状态 %q，期望 completed", outerSnap.Graph.Status)
	}
	if got := nodeStatus(t, outerSnap, "inner"); got.Status != NodeStatusCompleted {
		t.Errorf("外层里的 inner 状态 %q，期望 completed", got.Status)
	}

	// 第二层：父快照 → 外层快照 → 内层快照，一路走到底。
	outerStep := nodeStatus(t, outerSnap, "inner")
	if outerStep.Child == nil {
		t.Fatal("第二层也应当留着活句柄")
	}
	if outerStep.Child != inner || outerStep.Child.ID() != innerID {
		t.Errorf("第二层句柄不对：%v / %q", outerStep.Child, innerID)
	}
	innerSnap := outerStep.Child.Snapshot()
	if innerSnap.Graph.Path != innerPath || innerPath != "outer/inner" {
		t.Errorf("内层 path = %q，期望 %q（且合层级记号）", innerSnap.Graph.Path, innerPath)
	}
	if len(innerSnap.Nodes) != 1 {
		t.Fatalf("内层应当只有 1 个节点，实际 %d 个", len(innerSnap.Nodes))
	}
	if got := innerSnap.Nodes[0]; got.ID != "leaf" || got.Status != NodeStatusCompleted {
		t.Errorf("内层叶子 %+v，期望 leaf / completed", got)
	}
	// 叶子那一步的输出在最内层那张图里是 ready——桥回父侧是父侧的事。
	if ks := keyState(t, innerSnap.Nodes[0].Outputs, cout.Name()); ks.State != SlotReady {
		t.Errorf("内层叶子的输出 %q，期望 ready", ks.State)
	}
}

// TestSnapshotChildNilWhenNotReached：三种「没跑到那一步」都必须是 nil——
// 输入全跳过（没进 `Run`）、被取消、`build` 自己报错——而且读它不 panic、不阻塞。
//
// 三条都顺带断言「`build` 一次都没被调用」：`Child` 为 nil 有两种成因
// （没进 `Run` / 进了但建图失败），只断言 nil 分不出是哪一种。
func TestSnapshotChildNilWhenNotReached(t *testing.T) {
	t.Run("all-inputs-skipped", func(t *testing.T) {
		pin := NewKey[string]("n1.pin")
		cout := NewKey[string]("n1.cout")
		cin := NewKey[string]("n1.cin")
		pout := NewKey[string]("n1.pout")
		var built atomic.Bool

		g := mustNew(t, context.Background(), "S1")
		// 那条输入以**跳过**到达：门判「一条值都没到」→ 这一步自己跳过。
		if err := SkipSeed(g, pin); err != nil {
			t.Fatal(err)
		}
		if err := Sub(g, "step", "", []SubBind{In(pin, cin), Out(cout, pout)},
			func(sc *SubCtx) (*Graph, error) {
				built.Store(true)
				child, err := New(sc.Context(), sc.GraphID(), "")
				if err != nil {
					return nil, err
				}
				return child, child.Add(NewNode("work", "", Requires(cin), Provides(cout),
					func(rc *RunCtx) error { return Set(rc, cout, "x") }))
			}); err != nil {
			t.Fatal(err)
		}
		if err := g.Run(); err != nil {
			t.Fatal(err)
		}
		step := nodeStatus(t, g.Snapshot(), "step")
		if step.Status != NodeStatusSkipped {
			t.Errorf("状态 %q，期望 skipped", step.Status)
		}
		if step.Child != nil {
			t.Errorf("那一步没进 Run，不该有子图句柄：%v", step.Child)
		}
		if built.Load() {
			t.Error("被判跳过的那一步不该调 build")
		}
	})

	t.Run("canceled-before-gate", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // 启动前就拆掉：节点到不了 Run
		pout := NewKey[string]("n2.pout")
		cout := NewKey[string]("n2.cout")
		var built atomic.Bool

		g := mustNew(t, ctx, "S2")
		if err := Sub(g, "step", "", []SubBind{Out(cout, pout)},
			func(sc *SubCtx) (*Graph, error) {
				built.Store(true)
				child, err := New(sc.Context(), sc.GraphID(), "")
				if err != nil {
					return nil, err
				}
				return child, child.Add(NewNode("work", "", nil, Provides(cout),
					func(rc *RunCtx) error { return Set(rc, cout, "x") }))
			}); err != nil {
			t.Fatal(err)
		}
		if err := g.Run(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v，期望 context.Canceled", err)
		}
		step := nodeStatus(t, g.Snapshot(), "step")
		if step.Status != NodeStatusCanceled {
			t.Errorf("状态 %q，期望 canceled", step.Status)
		}
		if step.Child != nil {
			t.Errorf("被取消的那一步不该有子图句柄：%v", step.Child)
		}
		if built.Load() {
			t.Error("到不了 Run 就不该调 build")
		}
	})

	t.Run("build-fails", func(t *testing.T) {
		boom := errors.New("build 挂了")
		pout := NewKey[string]("n3.pout")
		cout := NewKey[string]("n3.cout")

		g := mustNew(t, context.Background(), "S3")
		if err := Sub(g, "step", "", []SubBind{Out(cout, pout)},
			func(sc *SubCtx) (*Graph, error) { return nil, boom }); err != nil {
			t.Fatal(err)
		}
		if err := g.Run(); !errors.Is(err, boom) {
			t.Fatalf("Run = %v，期望带 boom", err)
		}
		step := nodeStatus(t, g.Snapshot(), "step")
		if step.Status != NodeStatusFailed || !errors.Is(step.Err, boom) {
			t.Errorf("状态 %q / %v，期望 failed / boom", step.Status, step.Err)
		}
		if step.Child != nil {
			t.Errorf("build 报错时不该有子图句柄：%v", step.Child)
		}
	})
}

// TestSnapshotChildAggregateStatus：子图的状态与那一步的状态对得上——
// 子图 completed → 父节点 completed；子图输出**全部**跳过 → 父节点 skipped；
// 子图 failed → 父节点 failed 且错误原样冒泡；子图 canceled → 父节点 canceled。
func TestSnapshotChildAggregateStatus(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		cout := NewKey[string]("c.cout")
		c := newSubCase(t, func(rc *RunCtx) error { return Set(rc, cout, "v") })
		if err := c.parent.Run(); err != nil {
			t.Fatal(err)
		}
		s := c.parent.Snapshot()
		step := nodeStatus(t, s, c.stepID)
		if step.Status != NodeStatusCompleted {
			t.Fatalf("状态 %q，期望 completed", step.Status)
		}
		if step.Child == nil || step.Child != c.child {
			t.Fatalf("跑完一轮后应当留着 build 交出来的那张图：%v", step.Child)
		}
		if got := step.Child.Snapshot().Graph.Status; got != GraphCompleted {
			t.Errorf("子图状态 %q，期望 completed", got)
		}
		if ks := keyState(t, step.Outputs, c.outKey.Name()); ks.State != SlotReady {
			t.Errorf("桥回父侧的输出 %q，期望 ready", ks.State)
		}
	})

	t.Run("child-outputs-all-skipped", func(t *testing.T) {
		cout := NewKey[string]("c.cout")
		c := newSubCase(t, func(rc *RunCtx) error { return Skip(rc, cout) })
		if err := c.parent.Run(); err != nil {
			t.Fatal(err)
		}
		s := c.parent.Snapshot()
		step := nodeStatus(t, s, c.stepID)
		if step.Status != NodeStatusSkipped {
			t.Fatalf("子图输出全跳过时那一步应当 skipped，实际 %q", step.Status)
		}
		if step.Child == nil {
			t.Fatal("跳过的只是输出槽，子图那张图照样该留着句柄")
		}
		// 子图那边是**正常跑完**的：Skip 的是输出槽，不是那个节点的状态。
		childSnap := step.Child.Snapshot()
		if childSnap.Graph.Status != GraphCompleted {
			t.Errorf("子图状态 %q，期望 completed", childSnap.Graph.Status)
		}
		work := nodeStatus(t, childSnap, "work")
		if work.Status != NodeStatusCompleted {
			t.Errorf("子图里那个节点状态 %q，期望 completed（Skip 的是输出槽）", work.Status)
		}
		if got := keyState(t, work.Outputs, cout.Name()); got.State != SlotSkipped {
			t.Errorf("子图那条输出槽 %q，期望 skipped", got.State)
		}
		// 父侧那条输出由引擎自动跳过。
		if got := keyState(t, step.Outputs, c.outKey.Name()); got.State != SlotSkipped {
			t.Errorf("父侧输出槽 %q，期望 skipped", got.State)
		}
	})

	t.Run("child-failed", func(t *testing.T) {
		boom := errors.New("子图里炸了")
		c := newSubCase(t, func(rc *RunCtx) error { return boom })
		if err := c.parent.Run(); !errors.Is(err, boom) {
			t.Fatalf("Run = %v，期望带 boom", err)
		}
		s := c.parent.Snapshot()
		if s.Graph.Status != GraphFailed {
			t.Errorf("父图状态 %q，期望 failed", s.Graph.Status)
		}
		step := nodeStatus(t, s, c.stepID)
		if step.Status != NodeStatusFailed || !errors.Is(step.Err, boom) {
			t.Fatalf("那一步 %q / %v，期望 failed / boom", step.Status, step.Err)
		}
		if step.Child == nil {
			t.Fatal("失败了也看得见那张装出来的子图")
		}
		childSnap := step.Child.Snapshot()
		if childSnap.Graph.Status != GraphFailed {
			t.Errorf("子图状态 %q，期望 failed", childSnap.Graph.Status)
		}
		if work := nodeStatus(t, childSnap, "work"); !errors.Is(work.Err, boom) {
			t.Errorf("子图里那个节点的错误 %v，期望带 boom", work.Err)
		}
	})

	t.Run("child-canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		entered := make(chan struct{})
		c := newSubCaseOn(t, ctx, func(rc *RunCtx) error {
			close(entered)
			// 等**自己那把** ctx：读它的 Err 与它的 Done 是同一个 ctx，
			// 不存在「外层关了、内层还没传播到」的窗口。
			<-rc.Context().Done()
			return rc.Context().Err()
		})
		if err := c.parent.Start(); err != nil {
			t.Fatal(err)
		}
		waitClose(t, entered, "子图里的节点进 Run")
		cancel()
		if err := c.parent.Wait(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait = %v，期望 context.Canceled", err)
		}
		s := c.parent.Snapshot()
		if s.Graph.Status != GraphCanceled {
			t.Errorf("父图状态 %q，期望 canceled", s.Graph.Status)
		}
		step := nodeStatus(t, s, c.stepID)
		if step.Status != NodeStatusCanceled {
			t.Fatalf("那一步状态 %q，期望 canceled", step.Status)
		}
		if step.Child == nil {
			t.Fatal("取消时子图已经装出来了，句柄应当在")
		}
		if got := step.Child.Snapshot().Graph.Status; got != GraphCanceled {
			t.Errorf("子图状态 %q，期望 canceled", got)
		}
	})
}

// TestSnapshotChildSealedByClaim：「交出来了、后面才失败」+ 认领即封条——
// 绑定报错时那一步是 failed、`Child` 非 nil、子图快照仍是 `pending`（装出来了、
// 没跑起来）；而这张子图从此只能读：`Add` / `Seed` / `SkipSeed` / `Start` 全被拒，
// `Snapshot` / `Topology` / `Validate` 照常可用。
//
// 为什么选「绑定报错」当封条的观测点：它让 `claimed == true` 而 `started == false`
// 同时成立（快照就报 `pending`），于是那三个拒绝只能来自**认领**这道闸——不会与
// 「已经启动过」混在一起，判据是确定的。
func TestSnapshotChildSealedByClaim(t *testing.T) {
	cin := NewKey[string]("seal.cin")
	cout := NewKey[string]("seal.cout")
	missing := NewKey[string]("seal.missing") // 子图**没声明**这条：绑定校验必须拒
	var child *Graph

	g := mustNew(t, context.Background(), "SEAL")
	if err := Sub(g, "step", "", []SubBind{In(cin, missing), Out(cout, cout)},
		func(sc *SubCtx) (*Graph, error) {
			c, err := New(sc.Context(), sc.GraphID(), "子图")
			if err != nil {
				return nil, err
			}
			// 子图那条输入由它**自己**喂：这样这张图自己是合法的（Validate 通过），
			// 与父侧那条接错的线分开——用例才钉得准「封条不碰只读入口」。
			if err := Seed(c, cin, "子图自己喂的"); err != nil {
				return nil, err
			}
			child = c
			return c, c.Add(NewNode("work", "", Requires(cin), Provides(cout),
				func(rc *RunCtx) error { return Set(rc, cout, "x") }))
		}); err != nil {
		t.Fatal(err)
	}
	if err := Seed(g, cin, "父侧种子"); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err == nil {
		t.Fatal("绑了一条子图不认识的键，应当报错")
	}

	step := nodeStatus(t, g.Snapshot(), "step")
	if step.Status != NodeStatusFailed {
		t.Fatalf("那一步状态 %q，期望 failed", step.Status)
	}
	if step.Child == nil || step.Child != child {
		t.Fatalf("绑定报错时也该留着那张装出来的子图：%v", step.Child)
	}
	if got := step.Child.Snapshot().Graph.Status; got != GraphPending {
		t.Fatalf("子图状态 %q，期望 pending（装出来了、没跑起来）", got)
	}

	// —— 认领即封条：三个写入口全拒，且拒的是同一个哨兵 ——
	if err := child.Add(NewNode("late", "", nil, nil, func(rc *RunCtx) error { return nil })); !errors.Is(err, ErrGraphStarted) {
		t.Errorf("认领之后 Add = %v，期望 ErrGraphStarted", err)
	}
	if err := Seed(child, cout, "late"); !errors.Is(err, ErrGraphStarted) {
		t.Errorf("认领之后 Seed = %v，期望 ErrGraphStarted", err)
	}
	if err := SkipSeed(child, cout); !errors.Is(err, ErrGraphStarted) {
		t.Errorf("认领之后 SkipSeed = %v，期望 ErrGraphStarted", err)
	}
	if err := child.Start(); !errors.Is(err, ErrGraphStarted) {
		t.Errorf("认领之后 Start = %v，期望 ErrGraphStarted", err)
	}
	if err := child.Run(); !errors.Is(err, ErrGraphStarted) {
		t.Errorf("认领之后 Run = %v，期望 ErrGraphStarted", err)
	}

	// —— 只读入口照常 ——
	if child.Topology() == nil {
		t.Error("认领之后 Topology 应当照常可用")
	}
	if err := child.Validate(); err != nil {
		t.Errorf("认领之后 Validate = %v，期望 nil（图本来就是合法的）", err)
	}
	if snap := child.Snapshot(); snap == nil || len(snap.Nodes) != 1 {
		t.Errorf("认领之后 Snapshot 应当照常可用，实际 %+v", snap)
	}
}

// TestSnapshotChildSealHoldsUnderConcurrency：`build` 里把子图指针交给另一个
// goroutine（宿主完全可以这么干），它在父侧绑定 / 种值 / 跑子图期间一直敲
// `Add` / `Seed` / `Start`——每一次都必须被拒，子图照常跑完。
//
// 与上一条的分工：那条钉「拒绝的判据是认领」，这条钉「并发下也封得住」，
// `-race` 干净是它的一半。
//
// 两条同步点让这个用例是**确定的**（不是「压一压看运气」）：
//   - 敲打那条 goroutine 等到子图里那个节点**进了 Run** 才开始计数——那一刻认领
//     早已发生（认领在 `build` 返回之后、跑子图之前），所以计进去的每一次都该被拒；
//     `build` 返回前不许敲，那时这张图还是普通的未认领图，敲成功才正常。
//   - 子图那个节点堵在门口直到第一轮敲完，所以「一次都没敲上」不可能发生。
func TestSnapshotChildSealHoldsUnderConcurrency(t *testing.T) {
	pin := NewKey[string]("sealc.pin")
	cout := NewKey[string]("sealc.cout")
	cin := NewKey[string]("sealc.cin")
	pout := NewKey[string]("sealc.pout")

	reached := make(chan struct{})    // 子图里那个节点进 Run
	firstRound := make(chan struct{}) // 敲打那条 goroutine 至少完成一轮
	release := make(chan struct{})    // 放子图那个节点走
	stop := make(chan struct{})
	var attempts, rejected atomic.Int64
	var wg sync.WaitGroup

	g := mustNew(t, context.Background(), "SEALC")
	if err := Seed(g, pin, "父侧种子"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step", "", []SubBind{In(pin, cin), Out(cout, pout)},
		func(sc *SubCtx) (*Graph, error) {
			c, err := New(sc.Context(), sc.GraphID(), "子图")
			if err != nil {
				return nil, err
			}
			if err := c.Add(NewNode("work", "", Requires(cin), Provides(cout),
				func(rc *RunCtx) error {
					close(reached)
					<-release
					v, err := Get(rc, cin)
					if err != nil {
						return err
					}
					return Set(rc, cout, v+"!")
				})); err != nil {
				return nil, err
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-reached // 认领早已发生：从这里起的每一次都该被拒
				for {
					select {
					case <-stop:
						return
					default:
					}
					if err := c.Add(NewNode("late", "", nil, nil,
						func(rc *RunCtx) error { return nil })); errors.Is(err, ErrGraphStarted) {
						rejected.Add(1)
					}
					if err := Seed(c, cout, "late"); errors.Is(err, ErrGraphStarted) {
						rejected.Add(1)
					}
					if err := c.Start(); errors.Is(err, ErrGraphStarted) {
						rejected.Add(1)
					}
					if attempts.Add(1) == 1 {
						close(firstRound)
					}
				}
			}()
			return c, nil
		}); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	waitClose(t, firstRound, "敲打那条 goroutine 的第一轮")
	close(release)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()

	if n := attempts.Load(); n == 0 {
		t.Fatal("并发那条 goroutine 一次都没敲上，用例没验到东西")
	} else if rejected.Load() != n*3 {
		t.Fatalf("%d 轮里只有 %d 次被拒（期望每一轮的三次都被拒）", n, rejected.Load())
	}

	step := nodeStatus(t, g.Snapshot(), "step")
	if step.Status != NodeStatusCompleted || step.Child == nil {
		t.Fatalf("被敲着的那张子图应当照常跑完：状态 %q / Child %v", step.Status, step.Child)
	}
	childSnap := step.Child.Snapshot()
	if childSnap.Graph.Status != GraphCompleted {
		t.Errorf("子图状态 %q，期望 completed", childSnap.Graph.Status)
	}
	if got := nodeStatus(t, childSnap, "work"); got.Status != NodeStatusCompleted {
		t.Errorf("子图里那个节点状态 %q，期望 completed", got.Status)
	}
}

// TestSnapshotChildWhileRunning：边跑边拍**父子两层**快照，`-race` 必须干净——
// 句柄是活对象，读它的那一刻子图可能正在变。
func TestSnapshotChildWhileRunning(t *testing.T) {
	pin := NewKey[string]("race.pin")
	cout := NewKey[string]("race.cout")
	cin := NewKey[string]("race.cin")
	pout := NewKey[string]("race.pout")

	gate := make(chan struct{})
	entered := make(chan struct{})

	g := mustNew(t, context.Background(), "RACE")
	if err := Seed(g, pin, "父侧种子"); err != nil {
		t.Fatal(err)
	}
	if err := Sub(g, "step", "", []SubBind{In(pin, cin), Out(cout, pout)},
		func(sc *SubCtx) (*Graph, error) {
			c, err := New(sc.Context(), sc.GraphID(), "子图")
			if err != nil {
				return nil, err
			}
			if err := c.Add(NewNode("work", "", Requires(cin), Provides(cout),
				func(rc *RunCtx) error {
					close(entered)
					<-gate
					return Set(rc, cout, "done")
				})); err != nil {
				return nil, err
			}
			return c, nil
		}); err != nil {
		t.Fatal(err)
	}
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	waitClose(t, entered, "子图里的节点进 Run")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// 活句柄那一路：子图自己的快照与拓扑（会与子图里的写并发）。
				if child := childOf(g.Snapshot(), "step"); child != nil {
					_ = child.Snapshot()
					_ = child.Topology()
				}
			}
		}()
	}
	close(gate)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()

	step := nodeStatus(t, g.Snapshot(), "step")
	if step.Child == nil {
		t.Fatal("跑完之后应当留着活句柄")
	}
	if got := step.Child.Snapshot().Graph.Status; got != GraphCompleted {
		t.Errorf("子图状态 %q，期望 completed", got)
	}
}
