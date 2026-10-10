package yaml_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/observe"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

// #295：YAML 里「一步 = 一张子图」。这一层只把引擎已有的能力（pulse.Sub）
// 写出来——拓扑归 YAML，Run 还是归注册的工厂。
const subDoc = `
version: 1
graphs:
  enrich:
    nodes:
      - id: work
        uses: sg.work
        requires: [{name: sg.topic, type: string}]
        provides: [{name: sg.summary, type: string}]
seeds:
  - key: {name: sg.input, type: string}
    from: {kind: literal, value: "slot contract"}
nodes:
  - id: step1
    graph: enrich
    in:  {sg.topic: sg.input}
    out: {sg.summary: sg.result}
  - id: sink
    uses: sg.sink
    requires: [{name: sg.result, type: string}]
`

// subReg 建一张登记表：四个键 + 两个工厂（`sg.work` 读 sg.topic 写 sg.summary；
// `sg.sink` 把父图最后那条值捞进 sink）。
func subReg(t *testing.T, sink *string) *pulse.Registry {
	t.Helper()
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.topic"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.summary"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.input"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.result"))
	reg.MustRegister("sg.work", func(rc *pulse.RunCtx) error {
		v, err := pulse.Get(rc, pulse.NewKey[string]("sg.topic"))
		if err != nil {
			return err
		}
		return pulse.Set(rc, pulse.NewKey[string]("sg.summary"), "about "+v)
	})
	reg.MustRegister("sg.sink", func(rc *pulse.RunCtx) error {
		v, err := pulse.Get(rc, pulse.NewKey[string]("sg.result"))
		if err != nil {
			return err
		}
		*sink = v
		return nil
	})
	return reg
}

// mustObs 按层建一条观测出口（path 为空 = 这一层是根）。
func mustObs(t *testing.T, sink *observe.MemorySink, path string) pulse.Observer {
	t.Helper()
	o, err := observe.NewRecordObserver(observe.ObserveConfig{
		Sink: sink, HostID: "h", TraceID: "tr", Path: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// 端到端：YAML 装「父图里一个节点 = 一张子图」，值桥得回来，观测里子图记录
// 带 pulse.path（出口按层建，见 LoadOptions.ObserverFor）。
func TestLoadSubgraphEndToEnd(t *testing.T) {
	sink := &observe.MemorySink{}
	var got string
	reg := subReg(t, &got)

	g, plan, err := pulseyaml.Load([]byte(subDoc), reg, pulseyaml.LoadOptions{
		GraphID: "P",
		// 根图那条出口自己建（Path 留空 = 根这一层不写 pulse.path）。
		Graph: []pulse.Option{pulse.WithObserver(mustObs(t, sink, ""))},
		ObserverFor: func(path string) pulse.Observer {
			return mustObs(t, sink, path)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(g, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if got != "about slot contract" {
		t.Fatalf("子图产物没桥回父图：%q", got)
	}

	// 观测：子图那几条带 path=step1（图归因是它自己的 graph id），父图自己的不带。
	child, parent, childNodeRows := 0, 0, 0
	for _, rec := range sink.Snapshot() {
		gid, _ := observe.Get[string](rec.Attrs, pulse.AttrGraph)
		p, hasPath := observe.Get[string](rec.Attrs, pulse.AttrPath)
		switch gid {
		case "P/step1":
			child++
			if !hasPath || p != "step1" {
				t.Fatalf("子图记录应当带 path=step1，实得 (%q, %v)：%+v", p, hasPath, rec)
			}
			// 运行级那两条没有节点维度，节点记录才有。
			if n, ok := observe.Get[string](rec.Attrs, pulse.AttrNode); ok {
				childNodeRows++
				if n != "work" {
					t.Fatalf("子图的节点归因 = %q，want work", n)
				}
			}
		case "P":
			parent++
			if hasPath {
				t.Fatalf("父图自己的记录不该带 path：%+v", rec)
			}
		default:
			t.Fatalf("不该出现的图归因 %q", gid)
		}
	}
	if child != 4 { // 运行级两条 + work 节点两条
		t.Fatalf("子图记录数 = %d，want 4", child)
	}
	if childNodeRows != 2 {
		t.Fatalf("子图里带节点归因的记录数 = %d，want 2（work 的等待段与执行段）", childNodeRows)
	}
	if parent != 6 { // 运行级两条 + step1 / sink 各两条
		t.Fatalf("父图记录数 = %d，want 6", parent)
	}
}

// 类型不匹配：两端各按 {name, type} 查表，且用**同一个** type 记号——父侧那条
// 键登记成别的类型，装图期就报。
func TestLoadSubgraphTypeMismatch(t *testing.T) {
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.topic"))
	pulse.MustRegisterKey(reg, pulse.NewKey[int]("sg.count")) // 父侧那条是 int
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.summary"))
	reg.MustRegister("f", func(*pulse.RunCtx) error { return nil })

	doc := []byte(`
version: 1
graphs:
  g1:
    nodes:
      - id: n
        uses: f
        requires: [{name: sg.topic, type: string}]
        provides: [{name: sg.summary, type: string}]
nodes:
  - id: step1
    graph: g1
    in: {sg.topic: sg.count}
`)
	_, _, err := pulseyaml.Load(doc, reg, pulseyaml.LoadOptions{GraphID: "P"})
	if err == nil {
		t.Fatal("类型不匹配应当在 Load 期报错")
	}
	for _, want := range []string{"sg.count", "does not match registered"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误里要点名 %q：%v", want, err)
		}
	}
}

// 名字写错：引用一张不存在的子图 / 绑定一条子图没声明的键，两条都在 Load 期报。
func TestLoadSubgraphNamesAreChecked(t *testing.T) {
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.topic"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.summary"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.input"))
	reg.MustRegister("f", func(*pulse.RunCtx) error { return nil })

	_, _, err := pulseyaml.Load([]byte(`
version: 1
graphs:
  g1:
    nodes:
      - id: n
        uses: f
        requires: [{name: sg.topic, type: string}]
        provides: [{name: sg.summary, type: string}]
nodes:
  - id: step1
    graph: nope
    in: {sg.topic: sg.input}
`), reg, pulseyaml.LoadOptions{GraphID: "P"})
	if err == nil || !strings.Contains(err.Error(), `references unknown graph "nope"`) {
		t.Fatalf("want unknown graph，got %v", err)
	}

	_, _, err = pulseyaml.Load([]byte(`
version: 1
graphs:
  g1:
    nodes:
      - id: n
        uses: f
        requires: [{name: sg.topic, type: string}]
        provides: [{name: sg.summary, type: string}]
nodes:
  - id: step1
    graph: g1
    in: {sg.typo: sg.input}
`), reg, pulseyaml.LoadOptions{GraphID: "P"})
	if err == nil || !strings.Contains(err.Error(), `no node of it requires that key`) {
		t.Fatalf("want undeclared binding，got %v", err)
	}

	// 根**引用不到**的那张子图里写错名字也要报：装图期的校验是**全量**的，
	// 不是只走一遍可达路径（今天用不到、明天接上去才炸是最难查的一种）。
	_, _, err = pulseyaml.Load([]byte(`
version: 1
graphs:
  unused:
    nodes:
      - id: n
        graph: nope
        in: {sg.topic: sg.input}
  g1:
    nodes:
      - id: n
        uses: f
        requires: [{name: sg.topic, type: string}]
        provides: [{name: sg.summary, type: string}]
nodes:
  - id: step1
    graph: g1
    in: {sg.topic: sg.input}
    out: {sg.summary: sg.summary}
`), reg, pulseyaml.LoadOptions{GraphID: "P"})
	if err == nil || !strings.Contains(err.Error(), `references unknown graph "nope"`) {
		t.Fatalf("不可达的子图声明也要校验，got %v", err)
	}
}

// 第 2 层及以下写错的东西，**装图期**就要报完：落进图的只有根层那批，子图要等
// 运行到才装——只查根层的话，这些毛病会「装图全过、跑到一半才炸」，谁也引用不到
// 的 spec 更是永远不炸（它们本来就是最贵的两类）。
//
// 每一行都压在第 2 层（`mid`）或更深，根层那批另有用例。这里同时钉住两件事：
// 报错点在**哪张 spec**（不点名的话读的人还得自己找），以及引擎那边的装配期
// 检查（`Sub` / `Add`）也纳进来了——它们原来是等运行到才跑的。
func TestLoadChecksEveryGraphSpec(t *testing.T) {
	const head = `
version: 1
seeds:
  - key: {name: c.in, type: string}
    from: {kind: literal, value: "v"}
graphs:
`
	const tail = `
nodes:
  - id: step1
    graph: mid
    in:  {c.mid_in: c.in}
    out: {c.mid_out: c.out}
`

	cases := []struct {
		name string
		node string
		body string
		want string
	}{
		{
			name: "第 2 层的绑定指向子图里没人读的键",
			node: "toInner",
			body: `  mid:
    nodes:
      - id: toInner
        graph: inner
        in:  {c.nope: c.mid_in}
        out: {c.cout: c.mid_out}
  inner:
    nodes:
      - id: leaf
        uses: f
        requires: [{name: c.cin, type: string}]
        provides: [{name: c.cout, type: string}]
`,
			want: `graph "mid": node "toInner": graph "inner" reads "c.nope" in the binding`,
		},
		{
			name: "第 3 层的工厂名不存在",
			node: "leaf",
			body: `  mid:
    nodes:
      - id: toDeep
        graph: deep
        in:  {c.cin: c.mid_in}
        out: {c.cout: c.mid_out}
  deep:
    nodes:
      - id: leaf
        uses: does.not.exist
        requires: [{name: c.cin, type: string}]
        provides: [{name: c.cout, type: string}]
`,
			want: `graph "deep": node "leaf" uses unknown factory "does.not.exist"`,
		},
		{
			name: "谁也引用不到的 spec 里工厂名也不存在",
			node: "who",
			body: `  mid:
    nodes:
      - id: toInner
        graph: inner
        in:  {c.cin: c.mid_in}
        out: {c.cout: c.mid_out}
  inner:
    nodes:
      - id: leaf
        uses: f
        requires: [{name: c.cin, type: string}]
        provides: [{name: c.cout, type: string}]
  unused:
    nodes:
      - id: who
        uses: nope.not.registered
        requires: [{name: c.cin, type: string}]
        provides: [{name: c.cout, type: string}]
`,
			want: `graph "unused": node "who" uses unknown factory "nope.not.registered"`,
		},
		{
			name: "第 2 层两个节点写同一条键（引擎的来源冲突）",
			node: "also",
			body: `  mid:
    nodes:
      - id: toInner
        graph: inner
        in:  {c.cin: c.mid_in}
        out: {c.cout: c.mid_out}
      - id: also
        uses: f
        requires: [{name: c.mid_in, type: string}]
        provides: [{name: c.mid_out, type: string}]
  inner:
    nodes:
      - id: leaf
        uses: f
        requires: [{name: c.cin, type: string}]
        provides: [{name: c.cout, type: string}]
`,
			want: `graph "mid": node "also": pulse: key already has a source: "c.mid_out"`,
		},
		{
			name: "第 2 层两个节点同名（引擎的重复 id）",
			node: "dup",
			body: `  mid:
    nodes:
      - id: dup
        uses: f
        requires: [{name: c.mid_in, type: string}]
        provides: [{name: c.mid_out, type: string}]
      - id: dup
        uses: f
        requires: [{name: c.mid_in, type: string}]
        provides: [{name: c.mid_out, type: string}]
`,
			want: `graph "mid": node "dup": pulse: duplicate node id "dup"`,
		},
		{
			name: "第 2 层的子键既 in 又 out（引擎 Sub 的装配期检查）",
			node: "toBoth",
			body: `  mid:
    nodes:
      - id: toBoth
        graph: both
        in:  {c.both: c.mid_in}
        out: {c.both: c.mid_out}
  both:
    nodes:
      - id: reads
        uses: f
        requires: [{name: c.both, type: string}]
        provides: [{name: c.side, type: string}]
      - id: writes
        uses: f
        requires: [{name: c.side, type: string}]
        provides: [{name: c.both, type: string}]
`,
			want: `graph "mid": pulse: Sub "toBoth": child key "c.both" is bound twice`,
		},
		{
			name: "第 3 层的依赖成环（引擎 Start 的静态判据）",
			node: "toInner",
			body: `  mid:
    nodes:
      - id: toInner
        graph: inner
        in:  {c.cin: c.mid_in}
        out: {c.cout: c.mid_out}
  inner:
    nodes:
      - id: a
        uses: f
        requires: [{name: c.side, type: string}]
        provides: [{name: c.both, type: string}]
      - id: b
        uses: f
        requires: [{name: c.both, type: string}]
        provides: [{name: c.side, type: string}]
      - id: leaf
        uses: f
        requires: [{name: c.cin, type: string}]
        provides: [{name: c.cout, type: string}]
`,
			want: `graph "inner" (as referenced by node "toInner" of graph "mid"): pulse: dependency cycle: a -> b -> a`,
		},
		{
			name: "第 2 层有个节点读一条没人提供、也没人 seed 的键",
			node: "toInner",
			body: `  mid:
    nodes:
      - id: toInner
        graph: inner
        in:  {c.cin: c.mid_in}
        out: {c.cout: c.mid_out}
  inner:
    nodes:
      - id: leaf
        uses: f
        requires: [{name: c.cin, type: string}, {name: c.nope, type: string}]
        provides: [{name: c.cout, type: string}]
`,
			want: `graph "inner" (as referenced by node "toInner" of graph "mid"): pulse: node "leaf" requires "c.nope" but nothing provides or seeds it`,
		},
		{
			name: "in: 喂的那条子侧键，子图自己也提供（两个来源）",
			node: "toBoth2",
			body: `  mid:
    nodes:
      - id: toBoth2
        graph: both2
        in:  {c.both: c.mid_in}
        out: {c.side: c.mid_out}
  both2:
    nodes:
      - id: maker
        uses: f
        requires: []
        provides: [{name: c.both, type: string}]
      - id: user
        uses: f
        requires: [{name: c.both, type: string}]
        provides: [{name: c.side, type: string}]
`,
			want: `graph "mid": node "toBoth2": graph "both2" provides "c.both" inside`,
		},
		{
			name: "第 2 层的工厂节点写了 in/out（边界只有一处说法）",
			node: "confused",
			body: `  mid:
    nodes:
      - id: confused
        uses: f
        requires: [{name: c.mid_in, type: string}]
        provides: [{name: c.mid_out, type: string}]
        in:  {c.mid_in: c.in}
        out: {c.mid_out: c.out}
`,
			want: `graph "mid": node "confused": a factory node declares its boundary with requires/provides, not in/out`,
		},
		{
			name: "子图 seed 的键名没登记",
			node: "leaf",
			body: `  mid:
    nodes:
      - id: toInner
        graph: inner
        in:  {c.cin: c.mid_in}
        out: {c.cout: c.mid_out}
  inner:
    seeds:
      - key: {name: c.ghost, type: string}
        from: {kind: literal, value: "s"}
    nodes:
      - id: leaf
        uses: f
        requires: [{name: c.cin, type: string}]
        provides: [{name: c.cout, type: string}]
`,
			want: `graph "inner" seed "c.ghost": pulse: key "c.ghost" not registered`,
		},
		{
			name: "子图 seed 的类型记号写错",
			node: "leaf",
			body: `  mid:
    nodes:
      - id: toInner
        graph: inner
        in:  {c.cin: c.mid_in}
        out: {c.cout: c.mid_out}
  inner:
    seeds:
      - key: {name: c.num, type: string}
        from: {kind: literal, value: "s"}
    nodes:
      - id: leaf
        uses: f
        requires: [{name: c.cin, type: string}]
        provides: [{name: c.cout, type: string}]
`,
			want: `graph "inner" seed "c.num": pulse: key "c.num" type "string" does not match registered "int"`,
		},
		{
			name: "子图同一条键声明了两次 seed",
			node: "leaf",
			body: `  mid:
    nodes:
      - id: toInner
        graph: inner
        in:  {c.cin: c.mid_in}
        out: {c.cout: c.mid_out}
  inner:
    seeds:
      - key: {name: c.both, type: string}
        from: {kind: literal, value: "a"}
      - key: {name: c.both, type: string}
        from: {kind: literal, value: "b"}
    nodes:
      - id: leaf
        uses: f
        requires: [{name: c.cin, type: string}]
        provides: [{name: c.cout, type: string}]
`,
			want: `graph "inner": key "c.both" is seeded twice`,
		},
	}

	reg := pulse.NewRegistry()
	for _, k := range []string{"c.in", "c.cin", "c.cout", "c.side", "c.both", "c.nope", "c.mid_in", "c.mid_out", "c.out"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](k))
	}
	pulse.MustRegisterKey(reg, pulse.NewKey[int]("c.num"))
	reg.MustRegister("f", func(rc *pulse.RunCtx) error { return nil })

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := pulseyaml.Load([]byte(head+tc.body+tail), reg, pulseyaml.LoadOptions{GraphID: "P"})
			if err == nil {
				t.Fatal("装图期就该报，别留到运行期")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("报错没点到位置上：\n got %v\nwant 含 %q", err, tc.want)
			}
			// 节点 id 只该补一层：`addNodes` 与 `subBinds` 各补一次就成了
			// `node "x": node "x": …`（原来正是这么打的）。
			doubled := `node "` + tc.node + `": node "` + tc.node + `"`
			if strings.Contains(err.Error(), doubled) {
				t.Fatalf("节点 id 被补了两层：%v", err)
			}
		})
	}
}

// 引用环：a → b → a 装不出图，Load 期拦并给出具体路径（与引擎报依赖环同形）。
func TestLoadSubgraphCycle(t *testing.T) {
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.a"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.b"))
	_, _, err := pulseyaml.Load([]byte(`
version: 1
graphs:
  a:
    nodes:
      - id: toB
        graph: b
        in: {sg.a: sg.a}
        out: {sg.b: sg.b}
  b:
    nodes:
      - id: toA
        graph: a
        in: {sg.a: sg.a}
        out: {sg.b: sg.b}
nodes:
  - id: step1
    graph: a
    in: {sg.a: sg.a}
    out: {sg.b: sg.b}
`), reg, pulseyaml.LoadOptions{GraphID: "P"})
	if err == nil {
		t.Fatal("引用环应当在 Load 期报错")
	}
	if !strings.Contains(err.Error(), "graph reference cycle") || !strings.Contains(err.Error(), "a -> b -> a") {
		t.Fatalf("要报出具体那条环：%v", err)
	}
}

// 子图的 seed 只允许 literal：env / file / context 要靠宿主 IO，而 SeedPlan
// 是父图的产物——装图期就把话说清楚，别留到跑到一半。
func TestLoadSubgraphSeedKindRestricted(t *testing.T) {
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.topic"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.summary"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.result"))
	reg.MustRegister("f", func(*pulse.RunCtx) error { return nil })

	_, _, err := pulseyaml.Load([]byte(`
version: 1
graphs:
  g1:
    seeds:
      - key: {name: sg.topic, type: string}
        from: {kind: env, env: TOPIC}
    nodes:
      - id: n
        uses: f
        requires: [{name: sg.topic, type: string}]
        provides: [{name: sg.summary, type: string}]
nodes:
  - id: step1
    graph: g1
    out: {sg.summary: sg.result}
`), reg, pulseyaml.LoadOptions{GraphID: "P"})
	if err == nil || !strings.Contains(err.Error(), `kind "env" is not supported in a subgraph`) {
		t.Fatalf("want seed kind error，got %v", err)
	}
}

// 子图自己 seed 了某条键、父侧又用 in 喂它：两次种同一条槽，第二次会被幂等
// 首写静默忽略——装图期点名哪张图、哪个节点、哪条键。
func TestLoadSubgraphSeedVsBindConflict(t *testing.T) {
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.topic"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.summary"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.input"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.result"))
	reg.MustRegister("f", func(*pulse.RunCtx) error { return nil })

	_, _, err := pulseyaml.Load([]byte(`
version: 1
graphs:
  g1:
    seeds:
      - key: {name: sg.topic, type: string}
        from: {kind: literal, value: "默认值"}
    nodes:
      - id: n
        uses: f
        requires: [{name: sg.topic, type: string}]
        provides: [{name: sg.summary, type: string}]
nodes:
  - id: step1
    graph: g1
    in: {sg.topic: sg.input}
    out: {sg.summary: sg.result}
`), reg, pulseyaml.LoadOptions{GraphID: "P"})
	if err == nil || !strings.Contains(err.Error(), "silently ignored") {
		t.Fatalf("want seed/bind conflict，got %v", err)
	}
}

// 子图节点的边界由 in / out 声明，不能再用 requires / provides（重载会让
// 「这一步吃什么」有两处说法）。
func TestLoadSubgraphNodeRejectsRequires(t *testing.T) {
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.topic"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.summary"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.input"))
	reg.MustRegister("f", func(*pulse.RunCtx) error { return nil })

	_, _, err := pulseyaml.Load([]byte(`
version: 1
graphs:
  g1:
    nodes:
      - id: n
        uses: f
        provides: [{name: sg.summary, type: string}]
nodes:
  - id: step1
    graph: g1
    in: {sg.topic: sg.input}
    out: {sg.summary: sg.summary}
    requires: [{name: sg.input, type: string}]
`), reg, pulseyaml.LoadOptions{GraphID: "P"})
	if err == nil || !strings.Contains(err.Error(), "not requires/provides") {
		t.Fatalf("want in/out-only boundary error，got %v", err)
	}
}

// 同一张子图被引用两次 = 两个独立实例：图 id 与 path 都不一样，各自跑一遍。
func TestLoadSubgraphTwoInstances(t *testing.T) {
	sink := &observe.MemorySink{}
	// 两个实例会**并发**跑，所以这里得加锁：不加的话是这个用例自己制造一处
	// DATA RACE（CI 上真撞到过——`-race` 报的是本文件这行 append）。
	var gotMu sync.Mutex
	var got []string
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.topic"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.summary"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.input"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.result"))
	reg.MustRegister("sg.work", func(rc *pulse.RunCtx) error {
		gotMu.Lock()
		got = append(got, rc.NodeID())
		gotMu.Unlock()
		return pulse.Set(rc, pulse.NewKey[string]("sg.summary"), "x")
	})

	doc := []byte(`
version: 1
graphs:
  enrich:
    nodes:
      - id: work
        uses: sg.work
        requires: [{name: sg.topic, type: string}]
        provides: [{name: sg.summary, type: string}]
seeds:
  - key: {name: sg.input, type: string}
    from: {kind: literal, value: "v"}
nodes:
  - id: a
    graph: enrich
    in:  {sg.topic: sg.input}
    out: {sg.summary: sg.summary}
  - id: b
    graph: enrich
    in:  {sg.topic: sg.input}
    out: {sg.summary: sg.summary}
`)
	// 两个实例都写 sg.summary → 父侧那条键会被两个节点同时 Provides，引擎拒。
	// 所以这里各写各的：用两条不同的父侧键。
	doc = []byte(strings.Replace(string(doc),
		"  - id: b\n    graph: enrich\n    in:  {sg.topic: sg.input}\n    out: {sg.summary: sg.summary}",
		"  - id: b\n    graph: enrich\n    in:  {sg.topic: sg.input}\n    out: {sg.summary: sg.result}",
		1))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.result"))

	g, plan, err := pulseyaml.Load(doc, reg, pulseyaml.LoadOptions{
		GraphID:     "P",
		ObserverFor: func(path string) pulse.Observer { return mustObs(t, sink, path) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(g, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	gotMu.Lock()
	calls := append([]string(nil), got...)
	gotMu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("两个实例各跑一遍 work，实得 %d 次：%v", len(calls), calls)
	}
	seen := map[string]bool{}
	for _, rec := range sink.Snapshot() {
		if p, ok := observe.Get[string](rec.Attrs, pulse.AttrPath); ok {
			gid, _ := observe.Get[string](rec.Attrs, pulse.AttrGraph)
			seen[gid+"|"+p] = true
		}
	}
	for _, want := range []string{"P/a|a", "P/b|b"} {
		if !seen[want] {
			t.Fatalf("两个实例要靠 graph id + path 分得开，缺 %q（实得 %v）", want, seen)
		}
	}
}

// 子图里再挂子图：装图与路径都往下走一层。
func TestLoadSubgraphNested(t *testing.T) {
	var got string
	sink := &observe.MemorySink{}
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.topic"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.summary"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.input"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.result"))
	reg.MustRegister("sg.leaf", func(rc *pulse.RunCtx) error {
		v, err := pulse.Get(rc, pulse.NewKey[string]("sg.topic"))
		if err != nil {
			return err
		}
		return pulse.Set(rc, pulse.NewKey[string]("sg.summary"), v+"!")
	})
	reg.MustRegister("sg.sink", func(rc *pulse.RunCtx) error {
		v, err := pulse.Get(rc, pulse.NewKey[string]("sg.result"))
		if err != nil {
			return err
		}
		got = v
		return nil
	})

	g, plan, err := pulseyaml.Load([]byte(`
version: 1
graphs:
  inner:
    nodes:
      - id: leaf
        uses: sg.leaf
        requires: [{name: sg.topic, type: string}]
        provides: [{name: sg.summary, type: string}]
  outer:
    nodes:
      - id: mid
        graph: inner
        in:  {sg.topic: sg.topic}
        out: {sg.summary: sg.summary}
seeds:
  - key: {name: sg.input, type: string}
    from: {kind: literal, value: "叶子"}
nodes:
  - id: step1
    graph: outer
    in:  {sg.topic: sg.input}
    out: {sg.summary: sg.result}
  - id: sink
    uses: sg.sink
    requires: [{name: sg.result, type: string}]
`), reg, pulseyaml.LoadOptions{
		GraphID:     "P",
		ObserverFor: func(path string) pulse.Observer { return mustObs(t, sink, path) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(g, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if got != "叶子!" {
		t.Fatalf("两层之后桥回父侧的是 %q", got)
	}
	// 最里层那几条记录的 path 是两层接起来的。
	deep := 0
	for _, rec := range sink.Snapshot() {
		if p, ok := observe.Get[string](rec.Attrs, pulse.AttrPath); ok && p == "step1/mid" {
			deep++
		}
	}
	if deep != 4 { // 运行级两条 + leaf 节点两条
		t.Fatalf("最里层记录数 = %d，want 4（path=step1/mid）", deep)
	}
}

// 切面落在父侧那个节点上：`timeout` 就是给整张子图限时。
func TestLoadSubgraphTimeoutLimitsWholeChild(t *testing.T) {
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.topic"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.summary"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.input"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("sg.result"))
	reg.MustRegister("sg.slow", func(rc *pulse.RunCtx) error {
		select {
		case <-rc.Context().Done():
			return rc.Context().Err()
		case <-time.After(2 * time.Second):
			return nil
		}
	})

	g, plan, err := pulseyaml.Load([]byte(`
version: 1
graphs:
  slow:
    nodes:
      - id: wait
        uses: sg.slow
        requires: [{name: sg.topic, type: string}]
        provides: [{name: sg.summary, type: string}]
seeds:
  - key: {name: sg.input, type: string}
    from: {kind: literal, value: "v"}
nodes:
  - id: step1
    graph: slow
    timeout: 20ms
    in:  {sg.topic: sg.input}
    out: {sg.summary: sg.result}
`), reg, pulseyaml.LoadOptions{GraphID: "P"})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(g, nil); err != nil {
		t.Fatal(err)
	}
	err = g.Run()
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Run = %v，want 父侧节点的 timeout", err)
	}
	if !strings.Contains(err.Error(), "step1") {
		t.Fatalf("超时要点名是哪一步：%v", err)
	}
}

// 同一张 spec 被两处引用时，两边的 `in:` 可以不一样，而「子图里这条键有没有
// 来源」是**按引用点**成立的：有一处没喂，那张图跑到就一定会挂。所以校验也按
// 引用点各来一遍，而不是按 spec 取并集——并集会把「另一处喂了」记成这一处也喂了，
// 于是这张图要等真的跑到那一层才由引擎拒掉。
func TestLoadChecksEveryReferenceSite(t *testing.T) {
	reg := pulse.NewRegistry()
	for _, k := range []string{"rs.in", "rs.a2", "rs.b2", "rs.unused", "rs.cin", "rs.cout"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](k))
	}
	reg.MustRegister("f", func(*pulse.RunCtx) error { return nil })

	const head = `
version: 1
seeds:
  - key: {name: rs.in, type: string}
    from: {kind: literal, value: "v"}
graphs:
  inner:
    nodes:
      - id: leaf
        uses: f
        requires: [{name: rs.cin, type: string}]
        provides: [{name: rs.cout, type: string}]
  mid:
    nodes:
      - id: fed
        graph: inner
        in:  {rs.cin: rs.a2}
        out: {rs.cout: rs.b2}
  notmid:
    nodes:
      - id: hungry
        graph: inner
`
	const tail = `
nodes:
  - id: top
    graph: mid
    in:  {rs.a2: rs.in}
    out: {rs.b2: rs.b2}
  - id: top2
    graph: notmid
%s
`
	const top2NoIn = `    out: {rs.unused: rs.unused}`
	const top2Fed = `    in:  {rs.a2: rs.in}
    out: {rs.unused: rs.unused}`

	broken := head + "        out: {rs.cout: rs.unused}\n" + fmt.Sprintf(tail, top2NoIn)
	_, _, err := pulseyaml.Load([]byte(broken), reg, pulseyaml.LoadOptions{GraphID: "P"})
	if err == nil {
		t.Fatal("第二处引用没喂 rs.cin，装图期就该报——别等跑到那一层")
	}
	// 报错要说清是**哪一处**引用看到的：同一张图在两处看到的东西不一样
	if !strings.Contains(err.Error(), `graph "inner" (as referenced by node "hungry" of graph "notmid")`) {
		t.Fatalf("报错没点明引用点：%v", err)
	}
	if !strings.Contains(err.Error(), `requires "rs.cin" but nothing provides or seeds it`) {
		t.Fatalf("报错没点明是哪条键：%v", err)
	}

	// 对照组：两处都接上，整份文档就该全过——不能因为「有一处没喂」把两处都判死
	fixed := head + "        in:  {rs.cin: rs.a2}\n        out: {rs.cout: rs.unused}\n" + fmt.Sprintf(tail, top2Fed)
	if _, _, err := pulseyaml.Load([]byte(fixed), reg, pulseyaml.LoadOptions{GraphID: "P"}); err != nil {
		t.Fatalf("两处都接上后不该再报：%v", err)
	}
}

// 校验图不继承宿主的 ctx：那些图既不会跑、也没人会 cancel 它们，从宿主 ctx 派生
// 出来的子节点只会一直挂在宿主 ctx 上（热重载反复 Load = 越攒越多）。
//
// 直接可观测面就是宿主 ctx 的 children 表——标准库把派生出来的子节点挂在这张表
// 上，只有 cancel 或父 ctx 取消才摘掉。拿不到这张表（context 内部形状变了）就跳过：
// 这条断言盯的是「别往上面挂」，不是某种实现细节。
func TestLoadDoesNotRegisterCheckGraphsOnHostContext(t *testing.T) {
	const loads = 20
	host, cancel := context.WithCancel(context.Background())
	defer cancel()

	reg := pulse.NewRegistry()
	for _, k := range []string{"hc.in", "hc.a2", "hc.b2", "hc.cin", "hc.cout"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](k))
	}
	reg.MustRegister("f", func(*pulse.RunCtx) error { return nil })

	doc := []byte(`
version: 1
seeds:
  - key: {name: hc.in, type: string}
    from: {kind: literal, value: "v"}
graphs:
  inner:
    nodes:
      - id: leaf
        uses: f
        requires: [{name: hc.cin, type: string}]
        provides: [{name: hc.cout, type: string}]
  mid:
    nodes:
      - id: toInner
        graph: inner
        in:  {hc.cin: hc.a2}
        out: {hc.cout: hc.b2}
nodes:
  - id: step1
    graph: mid
    in:  {hc.a2: hc.in}
    out: {hc.b2: hc.b2}
`)
	before := ctxChildren(t, host)
	for i := 0; i < loads; i++ {
		if _, _, err := pulseyaml.Load(doc, reg, pulseyaml.LoadOptions{Context: host, GraphID: "HC"}); err != nil {
			t.Fatalf("第 %d 次 Load: %v", i, err)
		}
	}
	// 根图是宿主自己要的那张图，挂在宿主 ctx 上是对的（宿主跑完 Wait 就会释放），
	// 所以上限是「一次 Load 一个」；校验图（这份文档两张 spec）再挂上去就翻三倍。
	if got := ctxChildren(t, host) - before; got > loads {
		t.Fatalf("Load 往宿主 ctx 上挂了 %d 个子节点，最多只该有 %d（每张根图一个）", got, loads)
	}
}

// ctxChildren 读一个可取消 ctx 挂着的子节点数（context 包的内部表）。
func ctxChildren(t *testing.T, ctx context.Context) int {
	t.Helper()
	v := reflect.ValueOf(ctx)
	if v.Kind() != reflect.Ptr || v.Elem().Kind() != reflect.Struct {
		t.Skip("context 的内部形状变了：拿不到 children 表")
	}
	f := v.Elem().FieldByName("children")
	if !f.IsValid() || f.Kind() != reflect.Map || !f.CanAddr() {
		t.Skip("context 的内部形状变了：拿不到 children 表")
	}
	m := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
	if m.IsNil() {
		return 0
	}
	return m.Len()
}

// 装图**失败**的那次 `Load` 不该在宿主 ctx 上留下任何东西：返回值只有
// `(g, plan, err)`，失败时图不交出去，调用方连清理的把手都没有——所以
// 「所有可能失败的校验」必须排在「动宿主 ctx 建根图」之前。同一份坏文档
// 反复加载（热重载）时，留下的就是一堆没人认领的根图 ctx。
//
// 对照那一半同样重要：**成功**的 `Load` 照旧把根图挂在宿主 ctx 上（每张一个）
// ——根图是宿主自己要的图，取消必须从宿主传得进去。修「失败不留东西」不能
// 顺手把这条也掐掉（把根图也改成 `context.Background()` 就会：宿主取消再也
// 传不到正在跑的图上）。
func TestLoadFailureLeavesNothingOnHostContext(t *testing.T) {
	const loads = 20
	host, cancel := context.WithCancel(context.Background())
	defer cancel()

	reg := pulse.NewRegistry()
	for _, k := range []string{"fc.in", "fc.a2", "fc.b2", "fc.cin", "fc.cout", "fc.a", "fc.b"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](k))
	}
	reg.MustRegister("f", func(*pulse.RunCtx) error { return nil })

	const head = `
version: 1
seeds:
  - key: {name: fc.in, type: string}
    from: {kind: literal, value: "v"}
graphs:
  inner:
    nodes:
`
	// 坏：inner 里 a 与 b 互相依赖（成环）——装图期就该拒。
	const cycle = `      - id: a
        uses: f
        requires: [{name: fc.a, type: string}]
        provides: [{name: fc.b, type: string}]
      - id: b
        uses: f
        requires: [{name: fc.b, type: string}]
        provides: [{name: fc.a, type: string}]
      - id: leaf
        uses: f
        requires: [{name: fc.cin, type: string}]
        provides: [{name: fc.cout, type: string}]
`
	// 好：同一形状，去掉那个环。
	const noCycle = `      - id: leaf
        uses: f
        requires: [{name: fc.cin, type: string}]
        provides: [{name: fc.cout, type: string}]
`
	const mid = `  mid:
    nodes:
      - id: toInner
        graph: inner
        in:  {fc.cin: fc.a2}
        out: {fc.cout: fc.b2}
nodes:
  - id: step1
    graph: mid
    in:  {fc.a2: fc.in}
    out: {fc.b2: fc.b2}
`
	bad := []byte(head + cycle + mid)
	good := []byte(head + noCycle + mid)

	if _, _, err := pulseyaml.Load(bad, reg, pulseyaml.LoadOptions{Context: host, GraphID: "FC"}); err == nil {
		t.Fatal("这份文档本来就该装图失败（inner 里成环）")
	}
	before := ctxChildren(t, host)
	for i := 0; i < loads; i++ {
		if _, _, err := pulseyaml.Load(bad, reg, pulseyaml.LoadOptions{Context: host, GraphID: "FC"}); err == nil {
			t.Fatalf("第 %d 次 Load 竟然成功了", i)
		}
	}
	if got := ctxChildren(t, host) - before; got != 0 {
		t.Fatalf("失败的 Load 在宿主 ctx 上留下了 %d 个子节点，应该是 0", got)
	}

	beforeGood := ctxChildren(t, host)
	for i := 0; i < loads; i++ {
		if _, _, err := pulseyaml.Load(good, reg, pulseyaml.LoadOptions{Context: host, GraphID: "FC"}); err != nil {
			t.Fatalf("第 %d 次 Load: %v", i, err)
		}
	}
	if got := ctxChildren(t, host) - beforeGood; got != loads {
		t.Fatalf("成功的 Load 挂了 %d 个子节点，want %d（每张根图一个：根图必须继承宿主 ctx）", got, loads)
	}
}

// `skip: true` 的 seed 只把槽标成跳过，`from` 没有语义——装配期的 `seedOne`
// 与运行期的 `buildSub` 都是直接 `SkipSeed` 并忽略 `from`，顶层的
// `SeedPlan.Apply` 也一样。所以子图里 `skip: true` 带着 `env` / `file` /
// `context` 不该被拒；只有**要种值**的那条才必须给得出 `literal`。
func TestLoadSubgraphSkipSeedIgnoresFrom(t *testing.T) {
	reg := pulse.NewRegistry()
	for _, k := range []string{"sk.in", "sk.a2", "sk.b2", "sk.cin", "sk.cout", "sk.seed"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](k))
	}
	reg.MustRegister("f", func(*pulse.RunCtx) error { return nil })

	const head = `
version: 1
seeds:
  - key: {name: sk.in, type: string}
    from: {kind: literal, value: "v"}
graphs:
  inner:
    seeds:
      - {key: {name: sk.seed, type: string}, skip: true, from: {kind: env, env: "SOME_ENV"}}
    nodes:
      - id: leaf
        uses: f
        requires: [{name: sk.cin, type: string}]
        provides: [{name: sk.cout, type: string}]
  mid:
    nodes:
      - id: toInner
        graph: inner
        in:  {sk.cin: sk.a2}
        out: {sk.cout: sk.b2}
nodes:
  - id: step1
    graph: mid
    in:  {sk.a2: sk.in}
    out: {sk.b2: sk.b2}
`
	g, plan, err := pulseyaml.Load([]byte(head), reg, pulseyaml.LoadOptions{GraphID: "SK"})
	if err != nil {
		t.Fatalf("skip: true 的 seed 不该因为 from.kind 被拒：%v", err)
	}
	if err := plan.Apply(g, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatalf("Run = %v（跳过是到达，整轮该算成功）", err)
	}

	// 对照：同一条声明去掉 skip，`from` 就有了语义——仍然要在装图期拒。
	g2, _, err := pulseyaml.Load(
		[]byte(strings.Replace(head, "skip: true, ", "", 1)), reg, pulseyaml.LoadOptions{GraphID: "SK"})
	if err == nil {
		t.Fatal("要种值的那条只允许 literal，装图期就该拒")
	}
	if g2 != nil || !strings.Contains(err.Error(), `kind "env" is not supported in a subgraph`) {
		t.Fatalf("报错该点明是 kind 不受支持：%v", err)
	}
}
