package pulse

import (
	"context"
	"strings"
	"testing"
)

// 静态子树的**静态可见性**：装配完（不运行）就能递归看全树、按挂载点报毛病，
// 而且读这些入口不会改动任何定义。
//
// 这一组是票面验收里「A/B/C 各自构建、D 组合后**无运行**即可递归看全树并报全部静态错」
// 与「同一子图多处挂载不改定义」两条的落地用例。
func TestStaticSubtreeVisibility(t *testing.T) {
	ctx := context.Background()

	// leafWithPorts 造一张声明了入口 / 出口、且**有一个死节点**的子图（hungry 读的键
	// 没人提供、也没声明成入口）。
	leafWithPorts := func(t *testing.T, id string, dead bool) (*Graph, Key[string], Key[string]) {
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
		mustAdd(t, g, NewNode("step", "", Requires(in), Provides(out), func(rc *RunCtx) error {
			v, err := Get(rc, in)
			if err != nil {
				return err
			}
			return Set(rc, out, v)
		}))
		if dead {
			ghost := NewKey[string](id + ".ghost")
			mustAdd(t, g, NewNode("hungry", "", Requires(ghost), nil, func(rc *RunCtx) error { return nil }))
		}
		return g, in, out
	}

	t.Run("无运行即可递归看全树：节点、路径、绑定", func(t *testing.T) {
		leaf, lin, lout := leafWithPorts(t, "tv.leaf", false)
		dIn := NewKey[string]("tv.in")
		dOut := NewKey[string]("tv.out")
		d := mustNew(t, ctx, "tv.parent")
		if err := Sub(d, "step", "", []SubBind{In(dIn, lin), Out(lout, dOut)}, leaf); err != nil {
			t.Fatalf("Sub = %v", err)
		}

		top := d.Topology()
		if top.Graph.Path != "" {
			t.Fatalf("根图的 Path 该是空串，got %q", top.Graph.Path)
		}
		if len(top.Nodes) != 1 {
			t.Fatalf("父图该有一个节点，got %+v", top.Nodes)
		}
		sub := top.Nodes[0].Sub
		if sub == nil {
			t.Fatal("装配完之后 Nodes[0].Sub 该是子图的静态形状——不用先跑")
		}
		if sub.Graph.ID != "tv.leaf" || sub.Graph.Path != "step" {
			t.Fatalf("子图静态形状：id=%q path=%q，want id=tv.leaf path=step", sub.Graph.ID, sub.Graph.Path)
		}
		if len(sub.Nodes) != 1 || sub.Nodes[0].ID != "step" {
			t.Fatalf("子树的节点没递归下来：%+v", sub.Nodes)
		}
		binds := top.Nodes[0].Binds
		if len(binds) != 2 || binds[0].In != true || binds[1].In != false {
			t.Fatalf("父子接线不对：%+v", binds)
		}
		if binds[0].Parent != "tv.in" || binds[0].Child != "tv.leaf.in" {
			t.Fatalf("In 那头的两端不对：%+v", binds[0])
		}
		if binds[1].Parent != "tv.out" || binds[1].Child != "tv.leaf.out" {
			t.Fatalf("Out 那头的两端不对：%+v", binds[1])
		}
	})

	t.Run("两级嵌套的路径按挂载位置推出来", func(t *testing.T) {
		leaf, lin, lout := leafWithPorts(t, "n2.leaf", false)
		midIn := NewKey[string]("n2.mid.in")
		midOut := NewKey[string]("n2.mid.out")
		mid := mustNew(t, ctx, "n2.mid")
		// 中间层自己也是「被别人挂的子图」，所以它同样要把两头声明成端口
		if err := mid.DeclareInput(NewPort(midIn)); err != nil {
			t.Fatalf("mid.DeclareInput = %v", err)
		}
		if err := mid.DeclareOutput(NewPort(midOut)); err != nil {
			t.Fatalf("mid.DeclareOutput = %v", err)
		}
		if err := Sub(mid, "inner", "", []SubBind{In(midIn, lin), Out(lout, midOut)}, leaf); err != nil {
			t.Fatalf("内层挂载 = %v", err)
		}
		dIn := NewKey[string]("n2.in")
		dOut := NewKey[string]("n2.out")
		d := mustNew(t, ctx, "n2.parent")
		if err := d.DeclareInput(NewPort(dIn)); err != nil {
			t.Fatalf("parent.DeclareInput = %v", err)
		}
		if err := Sub(d, "outer", "", []SubBind{In(dIn, midIn), Out(midOut, dOut)}, mid); err != nil {
			t.Fatalf("外层挂载 = %v", err)
		}

		top := d.Topology()
		inner := top.Nodes[0].Sub
		if inner == nil || inner.Graph.Path != "outer" {
			t.Fatalf("中间层路径该是 outer，got %+v", inner.Graph)
		}
		leafTop := inner.Nodes[0].Sub
		if leafTop == nil || leafTop.Graph.Path != "outer/inner" {
			t.Fatalf("最内层路径该是 outer/inner，got %+v", leafTop.Graph)
		}
	})

	t.Run("无运行即可报子图的静态毛病，报文点明是哪一处挂载", func(t *testing.T) {
		leaf, lin, lout := leafWithPorts(t, "bad.leaf", true)
		dIn := NewKey[string]("bad.in")
		dOut := NewKey[string]("bad.out")
		d := mustNew(t, ctx, "bad.parent")
		// 父侧那条输入也要有来源（这里是「声明的入口」），否则先报的是父图自己的毛病
		if err := d.DeclareInput(NewPort(dIn)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := Sub(d, "mount", "", []SubBind{In(dIn, lin), Out(lout, dOut)}, leaf); err != nil {
			t.Fatalf("Sub = %v", err)
		}
		err := d.Validate()
		if err == nil {
			t.Fatal("子图里那个死节点该在父图上就被报出来——不必先跑")
		}
		if !strings.Contains(err.Error(), `mounted at "mount"`) {
			t.Fatalf("报文该点明是哪一处挂载，got %v", err)
		}
		if !strings.Contains(err.Error(), "bad.leaf.ghost") {
			t.Fatalf("报文该带着子图那条键的病根，got %v", err)
		}
		if got := len(d.Topology().Problems); got == 0 {
			t.Fatal("Topology().Problems 也该有这条")
		}
	})

	t.Run("同一子图挂两处：两处各一条，各自点明位置", func(t *testing.T) {
		leaf, lin, lout := leafWithPorts(t, "tw2.leaf", true)
		dIn := NewKey[string]("tw2.in")
		o1 := NewKey[string]("tw2.out1")
		o2 := NewKey[string]("tw2.out2")
		d := mustNew(t, ctx, "tw2.parent")
		if err := Sub(d, "m1", "", []SubBind{In(dIn, lin), Out(lout, o1)}, leaf); err != nil {
			t.Fatalf("m1 = %v", err)
		}
		if err := Sub(d, "m2", "", []SubBind{In(dIn, lin), Out(lout, o2)}, leaf); err != nil {
			t.Fatalf("m2 = %v", err)
		}

		probs := d.Topology().Problems
		var atM1, atM2 int
		for _, p := range probs {
			switch {
			case strings.Contains(p.Error(), `mounted at "m1"`):
				atM1++
			case strings.Contains(p.Error(), `mounted at "m2"`):
				atM2++
			}
		}
		if atM1 != 1 || atM2 != 1 {
			t.Fatalf("同一子图挂两处该各报一条（m1=%d m2=%d），got %d 条：%v", atM1, atM2, len(probs), probs)
		}
	})

	t.Run("读静态入口不改动定义", func(t *testing.T) {
		leaf, lin, lout := leafWithPorts(t, "ro.leaf", false)
		dIn := NewKey[string]("ro.in")
		dOut := NewKey[string]("ro.out")
		d := mustNew(t, ctx, "ro.parent")
		if err := Sub(d, "mount", "", []SubBind{In(dIn, lin), Out(lout, dOut)}, leaf); err != nil {
			t.Fatalf("Sub = %v", err)
		}
		before := leaf.Topology()
		_ = d.Topology()
		_ = d.Validate()
		after := leaf.Topology()
		// 子图自己那份视图里的 path 恒为空：路径是按**挂载位置**在每次视图里现推的。
		// 定义上已经没有可被盖写的 path 字段了（那正是「读一遍就盖章」那条旧路的根源），
		// 所以这里钉的是**语义**：从它自己看永远是根，从父图看才是挂载位置。
		if before.Graph.Path != "" || after.Graph.Path != "" {
			t.Fatalf("子图定义自己的 path 不该被读取动作盖章：before=%q after=%q",
				before.Graph.Path, after.Graph.Path)
		}
		if len(before.Nodes) != len(after.Nodes) || before.Graph.ID != after.Graph.ID {
			t.Fatalf("读一遍把定义改了：before=%+v after=%+v", before.Graph, after.Graph)
		}
		// 父图那条视图里，子图这一层按挂载位置推成 "mount"——两处视图互不干扰。
		if top := d.Topology(); top.Nodes[0].Sub == nil || top.Nodes[0].Sub.Graph.Path != "mount" {
			t.Fatalf("父图视图里的挂载路径 = %+v，want mount", top.Nodes[0].Sub)
		}
		if got := leaf.Topology().Graph.Path; got != "" {
			t.Fatalf("读过父图之后，子图自己那份的 path 变成了 %q——定义被改了", got)
		}
	})

	t.Run("观察者照旧继承给静态子图（adopt 不能少）", func(t *testing.T) {
		leaf, lin, lout := leafWithPorts(t, "obs.leaf", false)
		dIn := NewKey[string]("obs.in")
		dOut := NewKey[string]("obs.out")

		var seen []string
		obs := ObserverFunc{
			Running: func(graphID, nodeID string) { seen = append(seen, "run:"+nodeID) },
			Finished: func(graphID, nodeID string, reason NodeFinishReason, err error) {
				seen = append(seen, "fin:"+nodeID)
			},
		}
		d := mustNew(t, ctx, "obs.parent", WithObserver(obs))
		if err := Sub(d, "mount", "", []SubBind{In(dIn, lin), Out(lout, dOut)}, leaf); err != nil {
			t.Fatalf("Sub = %v", err)
		}
		if err := Seed(d, dIn, "v"); err != nil {
			t.Fatalf("Seed = %v", err)
		}
		if err := d.Run(); err != nil {
			t.Fatalf("Run = %v", err)
		}
		var sawChild bool
		for _, s := range seen {
			// 节点事件的回调只带 nodeID，所以认的是子图那个节点的 id（父图只有 mount 一步）
			if s == "run:step" || s == "fin:step" {
				sawChild = true
			}
		}
		if !sawChild {
			t.Fatalf("静态子图的节点事件没出现在父图的出口里（漏了 adopt）：%v", seen)
		}
	})
}
