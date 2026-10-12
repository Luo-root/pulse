package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 静态组合 `Sub` 的契约用例：现成子图挂成一步。
//
// 覆盖四件事：跑得通（值桥回来）、**端口是唯一接线面**（装配期就拒非端口 / 方向反了）、
// 组合环装配期拒（同一子图挂两处仍是合法的）、以及**取消传得下去**——最后这条是票面
// 实测 3 的翻转：预建子图不转发取消的话，父图会睡满整段还报成功。
func TestStaticSub(t *testing.T) {
	ctx := context.Background()

	// childWithPorts 造一张声明了入口 / 出口的子图：入口 aIn → 出口 aOut。
	childWithPorts := func(t *testing.T, id string) (*Graph, [2]Key[string]) {
		t.Helper()
		aIn := NewKey[string](id + ".in")
		aOut := NewKey[string](id + ".out")
		g := mustNew(t, ctx, id)
		if err := g.DeclareInput(NewPort(aIn)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := g.DeclareOutput(NewPort(aOut)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		mustAdd(t, g, NewNode("step", "", Requires(aIn), Provides(aOut), func(rc *RunCtx) error {
			v, err := Get(rc, aIn)
			if err != nil {
				return err
			}
			return Set(rc, aOut, "child("+v+")")
		}))
		return g, [2]Key[string]{aIn, aOut}
	}

	t.Run("跑得通：父侧输入进去，子图输出桥回来", func(t *testing.T) {
		in := NewKey[string]("sp.in")
		out := NewKey[string]("sp.out")
		child, ks := childWithPorts(t, "sp.child")

		d := mustNew(t, ctx, "sp.parent")
		if err := Sub(d, "mount", "挂现成子图", []SubBind{In(in, ks[0]), Out(ks[1], out)}, child); err != nil {
			t.Fatalf("Sub = %v", err)
		}
		if err := Seed(d, in, "host"); err != nil {
			t.Fatalf("Seed = %v", err)
		}
		if err := d.Run(); err != nil {
			t.Fatalf("Run = %v", err)
		}
		st, got := d.slotOf(out.asRef()).snapshot()
		if st != slotReady {
			t.Fatalf("父侧输出该就绪，got state=%v", st)
		}
		if got != "child(host)" {
			t.Fatalf("桥回的值 = %v, want %q", got, "child(host)")
		}
	})

	t.Run("端口是唯一接线面：接内部中间 Key 在装配期就拒", func(t *testing.T) {
		in := NewKey[string]("ip.in")
		out := NewKey[string]("ip.out")
		mid := NewKey[string]("ip.mid")
		child := mustNew(t, ctx, "ip.child")
		if err := child.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		// mid 是**内部中间 Key**：有节点 Provides，但没声明成出口
		mustAdd(t, child, NewNode("mk", "", Requires(in), Provides(mid), func(rc *RunCtx) error {
			v, err := Get(rc, in)
			if err != nil {
				return err
			}
			return Set(rc, mid, v)
		}))

		d := mustNew(t, ctx, "ip.parent")
		err := Sub(d, "mount", "", []SubBind{In(in, in), Out(mid, out)}, child)
		if err == nil {
			t.Fatal("接到子图内部的中间 Key 该在装配期就拒")
		}
		if !strings.Contains(err.Error(), "is not a output port") {
			t.Fatalf("报文要点明「它不是这张图的端口」，got %v", err)
		}
	})

	t.Run("端口方向反了也点出来", func(t *testing.T) {
		out := NewKey[string]("dp.out")
		child, ks := childWithPorts(t, "dp.child")
		d := mustNew(t, ctx, "dp.parent")
		// 把子图的**入口**当成出口绑
		err := Sub(d, "mount", "", []SubBind{Out(ks[0], out)}, child)
		if err == nil {
			t.Fatal("方向反了该被拒")
		}
		if !strings.Contains(err.Error(), "is a declared input of the child graph") {
			t.Fatalf("报文要点明方向反了，got %v", err)
		}
	})

	t.Run("组合环在装配期拒；同一子图挂两处仍合法", func(t *testing.T) {
		leaf, lks := childWithPorts(t, "cyc.leaf")
		in := NewKey[string]("cyc.in")
		out := NewKey[string]("cyc.out")

		mid := mustNew(t, ctx, "cyc.mid")
		if err := Sub(mid, "step", "", []SubBind{In(in, lks[0]), Out(lks[1], out)}, leaf); err != nil {
			t.Fatalf("第一层挂载 = %v", err)
		}
		// 再把 mid 挂回 leaf → 环
		err := Sub(leaf, "back", "", []SubBind{In(in, lks[0]), Out(lks[1], out)}, mid)
		if err == nil {
			t.Fatal("把父图挂进自己的子树该被拒")
		}
		if !strings.Contains(err.Error(), "subgraph cycle") {
			t.Fatalf("报文要点明是组合环，got %v", err)
		}

		// 同一张子图在**不同挂载位置**出现不算环（判据只看当前路径）
		other := mustNew(t, ctx, "cyc.other")
		if err := Sub(other, "m1", "", []SubBind{In(in, lks[0]), Out(lks[1], out)}, leaf); err != nil {
			t.Fatalf("同一子图挂第二处不该报环：%v", err)
		}
	})

	t.Run("同一张子图挂两处：装配期放过、跑的时候说清只能跑一处", func(t *testing.T) {
		in := NewKey[string]("tw.in")
		out1 := NewKey[string]("tw.out1")
		out2 := NewKey[string]("tw.out2")
		child, ks := childWithPorts(t, "tw.child")

		d := mustNew(t, ctx, "tw.parent")
		for i, o := range [2]Key[string]{out1, out2} {
			step := []string{"m1", "m2"}[i]
			if err := Sub(d, step, "", []SubBind{In(in, ks[0]), Out(ks[1], o)}, child); err != nil {
				t.Fatalf("装配第 %d 处 = %v", i+1, err)
			}
		}
		if err := Seed(d, in, "host"); err != nil {
			t.Fatalf("Seed = %v", err)
		}
		err := d.Run()
		if !errors.Is(err, ErrGraphStarted) {
			t.Fatalf("跑到第二处该报 ErrGraphStarted，got %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "can run at one mount point only") {
			t.Fatalf("报文要说清静态挂载只能跑一处、每个挂载点各跑一份要等执行树，got %v", err)
		}
	})

	t.Run("取消传得下去：预建子图不会把父图的取消吞掉", func(t *testing.T) {
		in := NewKey[string]("cxl.in")
		out := NewKey[string]("cxl.out")
		sawCancel := make(chan struct{})

		child := mustNew(t, context.Background(), "cxl.child") // 预建：ctx 与父图无关
		if err := child.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := child.DeclareOutput(NewPort(out)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		mustAdd(t, child, NewNode("slow", "", Requires(in), Provides(out), func(rc *RunCtx) error {
			select {
			case <-time.After(3 * time.Second): // 没看见取消才会走到这里
				return Set(rc, out, "slept")
			case <-rc.Context().Done():
				close(sawCancel)
				return rc.Context().Err()
			}
		}))

		runCtx, cancel := context.WithCancel(context.Background())
		d := mustNew(t, runCtx, "cxl.parent")
		if err := Sub(d, "mount", "", []SubBind{In(in, in), Out(out, out)}, child); err != nil {
			t.Fatalf("Sub = %v", err)
		}
		if err := Seed(d, in, "host"); err != nil {
			t.Fatalf("Seed = %v", err)
		}
		time.AfterFunc(20*time.Millisecond, cancel)

		err := d.Run()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("父图被取消时该报取消，got %v（旧行为是子图跑满、父图报成功）", err)
		}
		select {
		case <-sawCancel:
		default:
			t.Fatal("子图没看见父图的取消：转发那一步没生效")
		}
	})
}

// TestSubConcurrentOppositeMountsDoNotMakeCycle 钉的是装配期那条判据的**原子性**。
//
// 「沿子树查一遍（没有环）→ 把子图挂上」是两步，而两个 goroutine 对向挂载
// （A 里挂 B、同时 B 里挂 A）时，两边都可能先看过「没有环」再各挂一步——环就进去了。
// 环一旦进去，之后 `Topology()` / `Validate()` 沿挂载边递归不是报错而是**爆栈**（单线程）
// 或**互等对方的锁**（并发）。
//
// 组合环是**全局**不变式：A 挂 B 与 B 挂 A 是两个图各自动作的结果，单看任何一张图的锁都
// 拦不住，所以「查 + 挂」放在同一个全局临界区里（`Sub` 的 subMountMu）。
//
// 变异对照：去掉那把全局锁 → 本用例在头几十轮内必红（两个方向都挂上了）。
func TestSubConcurrentOppositeMountsDoNotMakeCycle(t *testing.T) {
	ctx := context.Background()
	// 一张最小可挂的子图：入口 → 出口，两端都是声明过的端口，所以 binds 一定核得过。
	mk := func(id string) (*Graph, Key[string], Key[string]) {
		t.Helper()
		in := NewKey[string](id + ".in")
		out := NewKey[string](id + ".out")
		g := mustNew(t, ctx, id)
		if err := g.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := g.DeclareOutput(NewPort(out)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		mustAdd(t, g, NewNode("mk", "", Requires(in), Provides(out), func(rc *RunCtx) error { return nil }))
		return g, in, out
	}

	for iter := 0; iter < 200; iter++ {
		a, ain, aout := mk(fmt.Sprintf("opp.a%d", iter))
		b, bin, bout := mk(fmt.Sprintf("opp.b%d", iter))
		// 挂载点的父侧那两端用**新**键：它们会进这一层节点的 Provides，撞上这一层已有的
		// 来源会被 Add 拒掉——那是另一条判据，与本用例要测的东西无关。
		ax := NewKey[string](fmt.Sprintf("opp.ax%d", iter))
		ay := NewKey[string](fmt.Sprintf("opp.ay%d", iter))
		bx := NewKey[string](fmt.Sprintf("opp.bx%d", iter))
		by := NewKey[string](fmt.Sprintf("opp.by%d", iter))

		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() { <-start; errs <- Sub(a, "toB", "", []SubBind{In(ax, bin), Out(bout, ay)}, b) }()
		go func() { <-start; errs <- Sub(b, "toA", "", []SubBind{In(bx, ain), Out(aout, by)}, a) }()
		close(start)
		e1, e2 := <-errs, <-errs

		switch {
		case e1 == nil && e2 == nil:
			t.Fatalf("第 %d 轮两个方向都挂上了：环进去了（A 的子树里有 B，B 的子树里有 A）", iter)
		case e1 != nil && e2 != nil:
			t.Fatalf("第 %d 轮两个方向都失败：%v / %v", iter, e1, e2)
		}
	}
}

// 透传端口：同一条子侧键**既是入口又是出口**（`Port` 的说明与出口判据都认这种形状）。
// 父图要「喂进去再取回来」就得写两条绑定（`In(父, k)` + `Out(k, 父)`），所以去重必须
// **看方向**：只拦「同一条进口绑两次」（那会把父侧的值静默吃掉），不拦「一条进口 + 一条出口」。
// 出口也不去重——同一条出口推给两个父侧键谁也不丢（同一个父侧键早在 `Add` 那层就被
// 「declares ... twice in Provides」拒了）。
func TestSubPassThroughPort(t *testing.T) {
	ctx := context.Background()

	// passThroughChild 造一张「入口 k → 出口 k」的子图：内部有个节点**读** k（否则这个
	// 挂载点没有意义），但**不写**它——写它就成了「内部有来源」，与父侧那次喂值撞来源。
	passThroughChild := func(t *testing.T, id string) (*Graph, Key[string]) {
		t.Helper()
		k := NewKey[string](id + ".k")
		seen := NewKey[string](id + ".seen")
		g := mustNew(t, ctx, id)
		if err := g.DeclareInput(NewPort(k)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := g.DeclareOutput(NewPort(k)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		mustAdd(t, g, NewNode("use", "", Requires(k), Provides(seen), func(rc *RunCtx) error {
			v, err := Get(rc, k)
			if err != nil {
				return err
			}
			return Set(rc, seen, "child("+v+")")
		}))
		return g, k
	}

	// sink 收一条父侧键的值（父侧那几条槽没有公开的读法，用消费节点收）。
	sink := func(t *testing.T, g *Graph, name string, k Key[string]) chan string {
		t.Helper()
		got := make(chan string, 1)
		mustAdd(t, g, NewNode(name, "", Requires(k), nil, func(rc *RunCtx) error {
			v, err := Get(rc, k)
			if err != nil {
				return err
			}
			select {
			case got <- v:
			default:
			}
			return nil
		}))
		return got
	}

	t.Run("透传：In(父,k) + Out(k,父) 都接得上，父侧拿回喂进去的值", func(t *testing.T) {
		child, k := passThroughChild(t, "ptf.child")
		in := NewKey[string]("ptf.in")
		out := NewKey[string]("ptf.out")
		root := mustNew(t, ctx, "ptf")
		got := sink(t, root, "sink", out)
		if err := Sub(root, "step", "", []SubBind{In(in, k), Out(k, out)}, child); err != nil {
			t.Fatalf("透传（一条 In + 一条 Out 打在同一条端口上）该被接受，got %v", err)
		}
		if err := Seed(root, in, "hello"); err != nil {
			t.Fatal(err)
		}
		if err := root.Validate(); err != nil {
			t.Fatalf("Validate = %v", err)
		}
		if err := root.Run(); err != nil {
			t.Fatalf("Run = %v", err)
		}
		select {
		case v := <-got:
			if v != "hello" {
				t.Fatalf("父侧拿到 %q，want %q（喂进去什么就该取回什么）", v, "hello")
			}
		default:
			t.Fatal("父侧那条出口没到达：透传没接上")
		}
	})

	t.Run("同一条出口推给两个父侧键：两边都拿到，谁都不丢", func(t *testing.T) {
		child, k := passThroughChild(t, "ptf2.child")
		in := NewKey[string]("ptf2.in")
		o1 := NewKey[string]("ptf2.o1")
		o2 := NewKey[string]("ptf2.o2")
		root := mustNew(t, ctx, "ptf2")
		got1 := sink(t, root, "sink1", o1)
		got2 := sink(t, root, "sink2", o2)
		if err := Sub(root, "step", "", []SubBind{In(in, k), Out(k, o1), Out(k, o2)}, child); err != nil {
			t.Fatalf("同一条出口推给两个父侧键该被接受，got %v", err)
		}
		if err := Seed(root, in, "v"); err != nil {
			t.Fatal(err)
		}
		if err := root.Run(); err != nil {
			t.Fatalf("Run = %v", err)
		}
		for i, ch := range []chan string{got1, got2} {
			select {
			case v := <-ch:
				if v != "v" {
					t.Fatalf("第 %d 个父侧键拿到 %q，want %q", i+1, v, "v")
				}
			default:
				t.Fatalf("第 %d 个父侧键没拿到值", i+1)
			}
		}
	})

	t.Run("同一条进口绑两次：装配期就拒（第二次种值会被幂等首写吃掉）", func(t *testing.T) {
		child, k := passThroughChild(t, "ptf3.child")
		in1 := NewKey[string]("ptf3.in1")
		in2 := NewKey[string]("ptf3.in2")
		root := mustNew(t, ctx, "ptf3")
		err := Sub(root, "step", "", []SubBind{In(in1, k), In(in2, k)}, child)
		if err == nil || !strings.Contains(err.Error(), `child inlet "ptf3.child.k" is bound twice`) {
			t.Fatalf("同一条进口绑两次该被拒，got %v", err)
		}
		if !strings.Contains(err.Error(), "silently ignored") {
			t.Fatalf("报文该点明「第二次的值会被静默忽略」，got %v", err)
		}
	})
}

// 同一条进口在**子图内部**有来源（某个节点 `Provides` 它）：父侧那次 `In` 种值会撞
// `ErrDuplicateSource`（一条键恰好一个来源）。与上一条同族——都是「父侧的 `In` 不可能
// 生效」，也都能在装配期读一遍声明就定掉，别留到这一步真跑起来。
func TestSubRejectsInletProvidedInside(t *testing.T) {
	ctx := context.Background()
	k := NewKey[string]("pin.child.k")
	child := mustNew(t, ctx, "pin.child")
	if err := child.DeclareInput(NewPort(k)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, child, NewNode("prod", "", nil, Provides(k), func(rc *RunCtx) error {
		return Set(rc, k, "inside")
	}))
	in := NewKey[string]("pin.in")
	root := mustNew(t, ctx, "pin")
	if err := Seed(root, in, "v"); err != nil {
		t.Fatal(err)
	}
	err := Sub(root, "step", "", []SubBind{In(in, k)}, child)
	if err == nil || !strings.Contains(err.Error(), "is provided inside the child by node \"prod\"") {
		t.Fatalf("Sub = %v，want 装配期就报「子图内部有人产出它」并点到那个节点", err)
	}
	if !strings.Contains(err.Error(), "already has a source") {
		t.Fatalf("报文该点明撞的是来源冲突（%s），got %v", ErrDuplicateSource, err)
	}
}

// `Retry` 装在挂载子图那一步上：**装配期认不出来，进 Run 的第一件事就拒**。
//
// 认不出来的原因是硬的：`Aspect` 是裸函数类型（冻结面：它是函数不是接口，贴不了标签），
// 而闭包没有可查的标识——实测同一份 `Retry` 字面量造出来的两个闭包，`reflect` 给出的
// 指针都不同（`Retry(1,0)` 与 `Retry(2,0)` 差 0x220）。所以判据放在 `Retry` 自己身上。
//
// 不拒的话会静默接受，然后第二次 attempt 报一句「子图已经在用」——那句话把第一次失败的
// 真正原因盖掉（`Retry` 返回的是**最后一次** attempt 的错误），病根再也看不到。
func TestRetryOnSubgraphStepIsRefusedAtRun(t *testing.T) {
	ctx := context.Background()
	k := NewKey[string]("rtry.child.k")
	child := mustNew(t, ctx, "rtry.child")
	if err := child.DeclareInput(NewPort(k)); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	mustAdd(t, child, NewNode("boom", "", Requires(k), nil, func(rc *RunCtx) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		return errors.New("REAL-CAUSE")
	}))

	in := NewKey[string]("rtry.in")
	for _, tc := range []struct {
		name    string
		aspects []Aspect
	}{
		{"Retry 单独", []Aspect{Retry(2, 0)}},
		{"Retry 与别的切面混着（不看位置）", []Aspect{Timeout(time.Second), Retry(2, 0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := mustNew(t, ctx, "rtry")
			if err := Seed(root, in, "v"); err != nil {
				t.Fatal(err)
			}
			// 装配期放行（认不出来）——契约是「进 Run 第一件事就拒」，不是「装不上」。
			if err := Sub(root, "step", "", []SubBind{In(in, k)}, child, tc.aspects...); err != nil {
				t.Fatalf("当前口径是运行期拒，装配期不该报错，got %v", err)
			}
			err := root.Run()
			if err == nil || !strings.Contains(err.Error(), "Retry is not supported on a graph step") {
				t.Fatalf("Run = %v，want 点名 Retry 的那句", err)
			}
			if !strings.Contains(err.Error(), "Move retry onto the node inside the child") {
				t.Fatalf("报文该给出能照着改的写法，got %v", err)
			}
			if strings.Contains(err.Error(), "REAL-CAUSE") {
				t.Fatalf("子图根本不该被启动（判据在认领与种值之前），却跑到了子图的失败：%v", err)
			}
			select {
			case <-entered:
				t.Fatal("子图被跑过了：拒 Retry 的判据该在它前面")
			default:
			}
		})
	}

	t.Run("Timeout 这类切面照旧装得上、跑得通（拒的是 Retry，不是「子图节点不能有切面」）", func(t *testing.T) {
		ok := NewKey[string]("rtry.ok.k")
		okChild := mustNew(t, ctx, "rtry.ok.child")
		if err := okChild.DeclareInput(NewPort(ok)); err != nil {
			t.Fatal(err)
		}
		mustAdd(t, okChild, NewNode("pass", "", Requires(ok), nil, func(rc *RunCtx) error { return nil }))
		root := mustNew(t, ctx, "rtry.ok")
		if err := Seed(root, in, "v"); err != nil {
			t.Fatal(err)
		}
		if err := Sub(root, "step", "", []SubBind{In(in, ok)}, okChild, Timeout(time.Second)); err != nil {
			t.Fatalf("Timeout 该被接受，got %v", err)
		}
		if err := root.Run(); err != nil {
			t.Fatalf("Run = %v", err)
		}
	})
}

// 用户**自己写的**重试切面认不出来（`Aspect` 是裸函数类型，闭包只有代码地址可认，见
// retryAspectPC），所以运行期还有一道重入判据：这一步第二次进来时点明「子图只跑一次」，
// 并把第一次 attempt 的失败原因一起报出来——别让认领失败那句盖掉病根。
func TestSubReentryGuarded(t *testing.T) {
	ctx := context.Background()
	k := NewKey[string]("reentry.child.k")
	child := mustNew(t, ctx, "reentry.child")
	// 透传端口（同一条键既是入口又是出口）：这样父侧两条绑定都接得上，用例只测重入。
	if err := child.DeclareInput(NewPort(k)); err != nil {
		t.Fatal(err)
	}
	if err := child.DeclareOutput(NewPort(k)); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, child, NewNode("boom", "", Requires(k), nil, func(rc *RunCtx) error {
		return errors.New("REAL-CAUSE")
	}))

	in := NewKey[string]("reentry.in")
	out := NewKey[string]("reentry.out")
	root := mustNew(t, ctx, "reentry")
	if err := Seed(root, in, "v"); err != nil {
		t.Fatal(err)
	}
	// 自己写的重试：与 Retry 同形（失败再来一次）——装配期认不出它，所以放行到运行期。
	retrying := func(rc *RunCtx, next func(*RunCtx) error) error {
		if err := next(rc); err != nil {
			return next(rc)
		}
		return nil
	}
	if err := Sub(root, "step", "", []SubBind{In(in, k), Out(k, out)}, child, retrying); err != nil {
		t.Fatalf("自己写的切面不该在装配期被拒，got %v", err)
	}
	err := root.Run()
	if err == nil || !strings.Contains(err.Error(), "entered a second time") {
		t.Fatalf("Run = %v，want 重入那句", err)
	}
	if strings.Contains(err.Error(), "already in use") {
		t.Fatalf("别让认领失败那句盖掉病根：%v", err)
	}
	if !strings.Contains(err.Error(), "REAL-CAUSE") {
		t.Fatalf("第一次 attempt 的失败原因该一起报出来：%v", err)
	}
}
