package yaml_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

// 这一组用例钉住「子图在 Load 期装一次」这条路（#313 阶段 1）：
//
//   - 装配完**不运行**就能从根图一路 `.Sub.Sub…` 拿完整棵树，路径按挂载位置推导；
//   - 取消从挂载点传进子图（票面实测 3 那句「父图报成功」在这里翻过来）；
//   - 声明了 `retry:` 的子图节点在 `Load` 期就拒（静态子图只跑一次，第二次尝试起不来）；
//   - 只装不跑的校验树不建出口：宿主的 `ObserverFor` 不会因为多校验一遍而多收到回调；
//   - 根层自己的静态判据仍在 `Start` 那一刻报（它的外部输入是宿主在 Load 之后种的）。

// staticDoc 是「同一张 spec 挂两处、每处再嵌一层」的形状：根层 left / right 各挂一次
// mid，mid 里再挂一次 inner —— 一共四个挂载点（根两个、第二层两个）。
const staticDoc = `
version: 1
seeds:
  - key: {name: sv.in, type: string}
    from: {kind: literal, value: "v"}
graphs:
  inner:
    nodes:
      - id: leaf
        uses: sv.pass
        requires: [{name: sv.cin, type: string}]
        provides: [{name: sv.cout, type: string}]
  mid:
    nodes:
      - id: toInner
        graph: inner
        in:  {sv.cin: sv.a2}
        out: {sv.cout: sv.b2}
nodes:
  - id: left
    graph: mid
    in:  {sv.a2: sv.in}
    out: {sv.b2: sv.left}
  - id: right
    graph: mid
    in:  {sv.a2: sv.in}
    out: {sv.b2: sv.right}
  - id: sink
    uses: sv.sink
    requires: [{name: sv.left, type: string}, {name: sv.right, type: string}]
`

// staticReg 登记 staticDoc 用到的键与两个工厂（`sv.pass` 原样透传，`sv.sink` 把两个
// 挂载点的产物收进 got）。
func staticReg(t *testing.T, got *[]string) *pulse.Registry {
	t.Helper()
	reg := pulse.NewRegistry()
	for _, k := range []string{"sv.in", "sv.a2", "sv.cin", "sv.cout", "sv.b2", "sv.left", "sv.right"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](k))
	}
	reg.MustRegister("sv.pass", func(rc *pulse.RunCtx) error {
		v, err := pulse.Get(rc, pulse.NewKey[string]("sv.cin"))
		if err != nil {
			return err
		}
		return pulse.Set(rc, pulse.NewKey[string]("sv.cout"), v)
	})
	reg.MustRegister("sv.sink", func(rc *pulse.RunCtx) error {
		for _, name := range []string{"sv.left", "sv.right"} {
			v, err := pulse.Get(rc, pulse.NewKey[string](name))
			if err != nil {
				return err
			}
			*got = append(*got, v)
		}
		return nil
	})
	return reg
}

// 装配完**不运行**就看得见整棵树：`Topology()` 沿静态挂载边递归，路径与图身份都按
// 挂载位置推导；同一张 spec 挂两处是两张独立实例（各自一路 `.Sub`）。
//
// 这一条是「子图在 Load 期装一次」的直接验收：要是子图改回运行期才现建，它在
// `Topology()` 里就成了一个不透明的节点，这条必红。
func TestLoadSubtreeIsVisibleWithoutRunning(t *testing.T) {
	var got []string
	reg := staticReg(t, &got)
	g, plan, err := pulseyaml.Load([]byte(staticDoc), reg, pulseyaml.LoadOptions{GraphID: "S"})
	if err != nil {
		t.Fatal(err)
	}

	top := g.Topology()
	if top.Graph.ID != "S" || top.Graph.Path != "" {
		t.Fatalf("根层坐标 = (id=%q path=%q)，want (\"S\", \"\")", top.Graph.ID, top.Graph.Path)
	}
	if len(top.Nodes) != 3 {
		t.Fatalf("根层节点数 = %d，want 3（left / right / sink）", len(top.Nodes))
	}
	byID := map[string]pulse.NodeShape{}
	for _, n := range top.Nodes {
		byID[n.ID] = n
	}
	for _, id := range []string{"left", "right"} {
		n, ok := byID[id]
		if !ok {
			t.Fatalf("根层少了节点 %q", id)
		}
		if n.Sub == nil {
			t.Fatalf("节点 %q 该带着子图的静态形状：装图期就挂上了，不必等运行", id)
		}
		if n.Sub.Graph.ID != "S/"+id || n.Sub.Graph.Path != id {
			t.Fatalf("节点 %q 的子图坐标 = (id=%q path=%q)，want (%q, %q)",
				id, n.Sub.Graph.ID, n.Sub.Graph.Path, "S/"+id, id)
		}
		if len(n.Binds) != 2 {
			t.Fatalf("节点 %q 的接线 = %+v，want 两条（一条 in、一条 out）", id, n.Binds)
		}
		// 再深一层：mid 里的 toInner 挂着 inner。
		if len(n.Sub.Nodes) != 1 {
			t.Fatalf("节点 %q 的子图节点数 = %d，want 1（toInner）", id, len(n.Sub.Nodes))
		}
		inner := n.Sub.Nodes[0].Sub
		if inner == nil {
			t.Fatalf("节点 %q 的第二层该看到 inner", id)
		}
		if inner.Graph.Path != id+"/toInner" {
			t.Fatalf("最内层的挂载路径 = %q，want %q", inner.Graph.Path, id+"/toInner")
		}
		if len(inner.Nodes) != 1 || inner.Nodes[0].ID != "leaf" {
			t.Fatalf("最内层的节点 = %+v，want leaf", inner.Nodes)
		}
	}

	// 读拓扑是只读的：同一份形状逐字段相等（装配完读一遍不会给定义盖什么章）。
	before := g.Topology()
	_ = g.Topology()
	if after := g.Topology(); !reflect.DeepEqual(before, after) {
		t.Fatalf("读拓扑改动了定义：before=%+v after=%+v", before, after)
	}

	// 两处挂载各跑各的：同一张 spec 的两张实例都能跑，值都桥得回来。
	if err := plan.Apply(g, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"v", "v"}) {
		t.Fatalf("两个挂载点的产物 = %v，want 两条 v", got)
	}
}

// 取消从**挂载点**传进子图：父图 ctx 一取消，子图节点当场看见（不是跑满整段再把取消
// 吞掉、父图还报成功）。这是票面实测 3 的形状，走的是 Load 期建好的静态子图这条路。
func TestLoadCancelReachesChildGraph(t *testing.T) {
	reg := pulse.NewRegistry()
	for _, k := range []string{"cx.in", "cx.out"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](k))
	}
	entered := make(chan struct{})
	exited := make(chan struct{})
	var canceled atomic.Bool
	reg.MustRegister("cx.slow", func(rc *pulse.RunCtx) error {
		close(entered)
		defer close(exited)
		select {
		case <-time.After(3 * time.Second):
			return pulse.Set(rc, pulse.NewKey[string]("cx.out"), "slept")
		case <-rc.Context().Done():
			canceled.Store(true)
			return rc.Context().Err()
		}
	})

	const doc = `
version: 1
seeds:
  - key: {name: cx.in, type: string}
    from: {kind: literal, value: "v"}
graphs:
  slow:
    nodes:
      - id: sleep
        uses: cx.slow
        requires: [{name: cx.in, type: string}]
        provides: [{name: cx.out, type: string}]
nodes:
  - id: step
    graph: slow
    in:  {cx.in: cx.in}
    out: {cx.out: cx.out}
`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, plan, err := pulseyaml.Load([]byte(doc), reg, pulseyaml.LoadOptions{Context: ctx, GraphID: "CX"})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(g, nil); err != nil {
		t.Fatal(err)
	}

	go func() {
		<-entered // 等子图那个节点真的进了 Run 再取消
		cancel()
	}()
	start := time.Now()
	err = g.Run()
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("父 ctx 取消后 Run 该报取消，got %v（旧行为是子图跑满 3s、父图报成功）", err)
	}
	if elapsed > time.Second {
		t.Fatalf("取消没传进子图：Run 耗时 %v（子图节点在睡 3s）", elapsed)
	}
	select {
	case <-exited:
	default:
		t.Fatal("子图节点还没退出，Run 就返回了")
	}
	if !canceled.Load() {
		t.Fatal("子图节点不是被取消唤醒的")
	}
}

// 声明了 `retry:` 的子图节点在 `Load` 期就拒。
//
// 为什么不是「照旧能重试」：静态子图是一次性的，`Sub` 在运行期原子认领它，第一次尝试
// 就把它用掉了——第二次尝试只会拿到「这张图已经在用」，而那句还会盖掉第一次失败的
// 真正原因（`Retry` 返回的是**最后一次** attempt 的错误）。「每次尝试各装一张新图」
// 要等阶段 3 的事务性 `Retry`，所以这一阶段当场拒，并在报文里给出今天就能用的写法。
func TestLoadRetryGraphNodeRejected(t *testing.T) {
	reg := pulse.NewRegistry()
	for _, k := range []string{"rt.in", "rt.out"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](k))
	}
	reg.MustRegister("rt.flaky", func(rc *pulse.RunCtx) error {
		return pulse.Set(rc, pulse.NewKey[string]("rt.out"), "ok")
	})

	const doc = `
version: 1
seeds:
  - key: {name: rt.in, type: string}
    from: {kind: literal, value: "v"}
graphs:
  flaky:
    nodes:
      - id: boom
        uses: rt.flaky
        requires: [{name: rt.in, type: string}]
        provides: [{name: rt.out, type: string}]
nodes:
  - id: step
    graph: flaky
    retry: {attempts: 2, delay: 1ms}
    in:  {rt.in: rt.in}
    out: {rt.out: rt.out}
`
	_, _, err := pulseyaml.Load([]byte(doc), reg, pulseyaml.LoadOptions{GraphID: "RT"})
	if err == nil {
		t.Fatal("子图节点上的 retry 该在 Load 期被拒，不该装出一张图来")
	}
	for _, want := range []string{`node "step"`, "retry is not supported on a graph node", `graph "flaky"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("报文要能照着改（缺 %q）：%v", want, err)
		}
	}

	// 控制组：同一份文档**只去掉那一行**就装得起来——被拒的是那条声明，不是这张图。
	const withoutRetry = `
version: 1
seeds:
  - key: {name: rt.in, type: string}
    from: {kind: literal, value: "v"}
graphs:
  flaky:
    nodes:
      - id: boom
        uses: rt.flaky
        requires: [{name: rt.in, type: string}]
        provides: [{name: rt.out, type: string}]
nodes:
  - id: step
    graph: flaky
    in:  {rt.in: rt.in}
    out: {rt.out: rt.out}
`
	if _, _, err := pulseyaml.Load([]byte(withoutRetry), reg, pulseyaml.LoadOptions{GraphID: "RT"}); err != nil {
		t.Fatalf("去掉 retry 之后该装得起来：%v", err)
	}
}

// 嵌在 spec 里的那一层：报错要点明是**哪张 spec** 里的哪一格——同一份文档里多处
// 引用同一张 spec 时，只说节点 id 不够定位。
func TestLoadRetryGraphNodeReportsSpec(t *testing.T) {
	reg := pulse.NewRegistry()
	for _, k := range []string{"nr.in", "nr.out"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](k))
	}
	reg.MustRegister("nr.leaf", func(rc *pulse.RunCtx) error {
		return pulse.Set(rc, pulse.NewKey[string]("nr.out"), "ok")
	})

	const doc = `
version: 1
seeds:
  - key: {name: nr.in, type: string}
    from: {kind: literal, value: "v"}
graphs:
  leaf:
    nodes:
      - id: work
        uses: nr.leaf
        requires: [{name: nr.in, type: string}]
        provides: [{name: nr.out, type: string}]
  mid:
    nodes:
      - id: boom
        graph: leaf
        retry: {attempts: 2}
        in:  {nr.in: nr.in}
        out: {nr.out: nr.out}
nodes:
  - id: step
    graph: mid
    in:  {nr.in: nr.in}
    out: {nr.out: nr.out}
`
	_, _, err := pulseyaml.Load([]byte(doc), reg, pulseyaml.LoadOptions{GraphID: "NR"})
	if err == nil {
		t.Fatal("嵌在 spec 里的子图节点带 retry 也该被拒")
	}
	if !strings.Contains(err.Error(), `graph "mid": node "boom"`) {
		t.Fatalf("报文要点明是哪张 spec 里的哪一格：%v", err)
	}
}

// 只装不跑的**校验树**不建出口：`ObserverFor` 每个挂载点只调一次——「多校验一遍」不会
// 让宿主多收到回调（返回 nil = 这一层按继承走，正是宿主最省事的写法）。
func TestLoadBuildsOneObserverPerMountSite(t *testing.T) {
	var got []string
	reg := staticReg(t, &got)
	var calls atomic.Int32
	var paths []string
	_, _, err := pulseyaml.Load([]byte(staticDoc), reg, pulseyaml.LoadOptions{
		GraphID: "OB",
		ObserverFor: func(path string) pulse.Observer {
			calls.Add(1)
			paths = append(paths, path)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"left", "left/toInner", "right", "right/toInner"}
	sort.Strings(paths)
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("按层收到的路径 = %v，want %v", paths, want)
	}
	if n := calls.Load(); n != int32(len(want)) {
		t.Fatalf("ObserverFor 调了 %d 次，want %d（每个挂载点一次；校验树不建出口）", n, len(want))
	}
}

// 根层自己的静态判据仍在 `Start` 那一刻报：根层的外部输入是宿主在 `Load` 之后种进去的
// （`SeedPlan.Apply`），装图期看它当然是「没人提供、没人 seed」——在这一步校验会把
// 任何一份 `seeds:` 正常的文档都误报掉。
//
// 这条钉住的是**阶段 1 的边界**：子图那一层没有这个问题（它们的输入面就是引用点的
// `in:`，装配期已经声明成端口），所以子图在 `Load` 期查、根层在启动时查。「声明归端口、
// 值归运行输入」是阶段 2 随 `Execution` 落地的事，那天这条会翻过来。
func TestLoadLeavesRootLayerJudgmentToStart(t *testing.T) {
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("rl.missing"))
	reg.MustRegister("rl.work", func(rc *pulse.RunCtx) error { return nil })

	const doc = `
version: 1
nodes:
  - id: hungry
    uses: rl.work
    requires: [{name: rl.missing, type: string}]
`
	g, plan, err := pulseyaml.Load([]byte(doc), reg, pulseyaml.LoadOptions{GraphID: "RL"})
	if err != nil {
		t.Fatalf("根层的来源判据不在装图期报：%v", err)
	}
	if err := plan.Apply(g, nil); err != nil {
		t.Fatal(err)
	}
	err = g.Run()
	if err == nil || !strings.Contains(err.Error(), `requires "rl.missing" but nothing provides or seeds it`) {
		t.Fatalf("启动那一刻该报出来，got %v", err)
	}
}

// 谁也引用不到的 spec **没有挂载点**，所以**没有接口面**：它里面那些「没人提供」的键，
// 究竟是「等将来挂上去时由那一处的 `in:` 喂」还是「名字写错了」，装图期判不了——按闭合图
// 去校验会把前者一律报成后者，于是「模板库里有一张暂时还没接上」连 `Load` 都过不去。
//
// 所以未引用的 spec 只留**真判得了**的那几条：工厂名 / 空 nodes / 重复 id / 来源冲突 /
// 依赖成环 / 流式那批照旧，**来源那条不作数**（它读的键一律当入口声明进那张只装不跑的
// 校验图）。等它被引用时，接口面由**那一处**的 `in:` / `out:` 定，来源判据在那时按引用点
// 生效（见 TestLoadChecksEveryReferenceSite）。
func TestLoadUnreferencedSpecSkipsSourceCheck(t *testing.T) {
	reg := pulse.NewRegistry()
	for _, k := range []string{"ur.cin", "ur.cout", "ur.root", "cy.a_in", "cy.a_out"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](k))
	}
	reg.MustRegister("ur.f", func(*pulse.RunCtx) error { return nil })

	// 1) 未引用的 spec 里那条「没人喂」的键 = 这张 spec 的入口，装图期不该报
	const unused = `
version: 1
graphs:
  template:
    nodes:
      - id: leaf
        uses: ur.f
        requires: [{name: ur.cin, type: string}]
        provides: [{name: ur.cout, type: string}]
nodes:
  - id: root
    uses: ur.f
    provides: [{name: ur.root, type: string}]
`
	if _, _, err := pulseyaml.Load([]byte(unused), reg, pulseyaml.LoadOptions{GraphID: "UR"}); err != nil {
		t.Fatalf("未引用的 spec 没有挂载点、就没有接口面，那条键是它的入口而不是「忘了接线」：%v", err)
	}

	// 2) 真的被引用、而那一处没喂 → 照旧报（按引用点，报文点明挂载路径）
	const referenced = `
version: 1
graphs:
  template:
    nodes:
      - id: leaf
        uses: ur.f
        requires: [{name: ur.cin, type: string}]
        provides: [{name: ur.cout, type: string}]
nodes:
  - id: step
    graph: template
    out: {ur.cout: ur.root}
`
	_, _, err := pulseyaml.Load([]byte(referenced), reg, pulseyaml.LoadOptions{GraphID: "UR"})
	if err == nil || !strings.Contains(err.Error(), `requires "ur.cin" but nothing provides or seeds it`) {
		t.Fatalf("被引用时来源判据照旧按引用点生效，got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), `(mounted at "step")`) {
		t.Fatalf("报文要点明是哪一处引用看到的：%v", err)
	}

	// 3) 未引用的 spec 里**真判得了**的那几条照旧：依赖成环不因为「没人用」就不报
	const loop = `
version: 1
graphs:
  loop:
    nodes:
      - id: c1
        uses: ur.f
        requires: [{name: cy.a_in, type: string}]
        provides: [{name: cy.a_out, type: string}]
      - id: c2
        uses: ur.f
        requires: [{name: cy.a_out, type: string}]
        provides: [{name: cy.a_in, type: string}]
nodes:
  - id: root
    uses: ur.f
    provides: [{name: ur.root, type: string}]
`
	_, _, err = pulseyaml.Load([]byte(loop), reg, pulseyaml.LoadOptions{GraphID: "UR"})
	if err == nil || !strings.Contains(err.Error(), "dependency cycle") {
		t.Fatalf("未引用的 spec 里依赖成环照样要报，got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), `graph "loop"`) {
		t.Fatalf("报文要点明是哪张 spec：%v", err)
	}
}

// 透传端口：同一条子侧键同时挂在 `in:` 与 `out:`。它与引擎「同名入口 + 出口是透传」
// （见 problemsOutputsLocked）是同一条规则——那条键的值来自父侧，所以「内部得有人读它 /
// 有人产出它」两条都不适用；而**内部有人产出它**仍要拒（同一个槽两个来源，见上一条用例）。
//
// 改之前这里装不出来：引擎按「同一个子键绑了两次」拒，而那条判据不认透传。
func TestLoadPassThroughSubgraphPort(t *testing.T) {
	const doc = `
version: 1
seeds:
  - key: {name: pt.in, type: string}
    from: {kind: literal, value: "hello"}
graphs:
  wrap:
    nodes:
      - id: use
        uses: pt.use
        requires: [{name: pt.k, type: string}]
        provides: [{name: pt.seen, type: string}]
nodes:
  - id: step
    graph: wrap
    in:  {pt.k: pt.in}
    out: {pt.k: pt.out}
  - id: sink
    uses: pt.sink
    requires: [{name: pt.out, type: string}]
    provides: []
`
	reg := pulse.NewRegistry()
	for _, k := range []string{"pt.in", "pt.k", "pt.seen", "pt.out"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](k))
	}
	reg.MustRegister("pt.use", func(rc *pulse.RunCtx) error {
		v, err := pulse.Get(rc, pulse.NewKey[string]("pt.k"))
		if err != nil {
			return err
		}
		return pulse.Set(rc, pulse.NewKey[string]("pt.seen"), "child("+v+")")
	})
	var got []string
	reg.MustRegister("pt.sink", func(rc *pulse.RunCtx) error {
		v, err := pulse.Get(rc, pulse.NewKey[string]("pt.out"))
		if err != nil {
			return err
		}
		got = append(got, v)
		return nil
	})

	g, plan, err := pulseyaml.Load([]byte(doc), reg, pulseyaml.LoadOptions{GraphID: "PT"})
	if err != nil {
		t.Fatalf("透传该在 Load 期通过：%v", err)
	}
	if err := plan.Apply(g, nil); err != nil {
		t.Fatalf("Apply = %v", err)
	}
	if err := g.Run(); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if len(got) != 1 || got[0] != "hello" {
		t.Fatalf("父侧拿到 %v，want [hello]（喂进去什么就该取回什么）", got)
	}
}
