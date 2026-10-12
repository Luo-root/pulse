package pulse

import (
	"context"
	"errors"
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
