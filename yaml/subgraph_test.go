package yaml_test

import (
	"strings"
	"sync"
	"testing"
	"time"

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
	}

	reg := pulse.NewRegistry()
	for _, k := range []string{"c.in", "c.cin", "c.cout", "c.side", "c.both", "c.nope", "c.mid_in", "c.mid_out", "c.out"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](k))
	}
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
