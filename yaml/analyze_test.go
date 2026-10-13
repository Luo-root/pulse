package yaml_test

import (
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

// 同一张 mid 挂两处，内层带固定值、固定跳过及仅在接线中出现的透传键。
// 原始输入的声明序刻意与名字排序不同，核对端口保留真正的声明序。
const analyzeTreeDoc = `
version: 1
description: 根说明
seeds:
  - key: {name: z.raw, type: string}
    from: {kind: context, key: input}
  - key: {name: a.raw, type: "[]string"}
    from: {kind: file, path: missing-file}
graphs:
  leaf:
    description: 末层说明
    seeds:
      - key: {name: z.skip, type: string}
        skip: true
        from: {kind: env, env: MISSING}
      - key: {name: m.fixed, type: int}
        from: {kind: literal, value: 7}
    nodes:
      - id: work
        description: 叶子工作
        uses: work
        requires:
          - {name: child.in, type: string}
          - {name: m.fixed, type: int}
          - {name: z.skip, type: string}
        provides:
          - {name: child.out, type: string}
          - {name: child.extra, type: "[]string"}
        timeout: 1s
        retry: {attempts: 2}
  mid:
    description: 中层说明
    nodes:
      - id: toLeaf
        description: 嵌套挂载
        graph: leaf
        in: {child.in: mid.in, pass: mid.pass}
        out: {child.out: mid.out, pass: mid.echo}
nodes:
  - id: left
    description: 左挂载
    graph: mid
    in: {mid.in: z.raw, mid.pass: a.raw}
    out: {mid.echo: root.pass1, mid.out: root.left}
    retry: {attempts: 2}
  - id: right
    description: 右挂载
    graph: mid
    in: {mid.in: z.raw, mid.pass: a.raw}
    out: {mid.echo: root.pass2, mid.out: root.right}
  - id: sink
    description: 汇总
    uses: sink
    requires:
      - {name: root.left, type: string}
      - {name: root.right, type: string}
      - {name: root.pass1, type: "[]string"}
      - {name: root.pass2, type: "[]string"}
`

// 已知标签的树与 Load / 手写 Go 图逐字段一致；比较包括图身份、挂载路径、描述、
// 端口、键来源、接线、边、度数、拓扑序和 Problems，不依赖运行结果补形状。
func TestAnalyzeMatchesLoadAndGoTopology(t *testing.T) {
	reg := pulse.NewRegistry()
	for _, name := range []string{"z.raw", "child.in", "child.out", "z.skip", "mid.in", "mid.out", "root.left", "root.right"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](name))
	}
	for _, name := range []string{"a.raw", "child.extra", "pass", "mid.pass", "mid.echo", "root.pass1", "root.pass2"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[[]string](name))
	}
	pulse.MustRegisterKey(reg, pulse.NewKey[int]("m.fixed"))
	neverRun := func(*pulse.RunCtx) error {
		t.Error("读取静态形状不应运行工厂")
		return nil
	}
	reg.MustRegister("work", neverRun)
	reg.MustRegister("sink", neverRun)

	var observerCalls, outletCalls, aspectCalls atomic.Int32
	obs := pulse.ObserverFunc{
		GraphStarted:  func(pulse.Site) { observerCalls.Add(1) },
		GraphFinished: func(pulse.Site, pulse.NodeFinishReason, error) { observerCalls.Add(1) },
		Waiting:       func(pulse.Site, string) { observerCalls.Add(1) },
		Running:       func(pulse.Site, string) { observerCalls.Add(1) },
		Finished:      func(pulse.Site, string, pulse.NodeFinishReason, error) { observerCalls.Add(1) },
	}
	aspect := func(rc *pulse.RunCtx, next func(*pulse.RunCtx) error) error {
		aspectCalls.Add(1)
		return next(rc)
	}
	opts := pulseyaml.LoadOptions{
		GraphID: "analysis", MaxRunning: 3,
		Graph: []pulse.Option{pulse.WithMaxRunning(1), pulse.WithObserver(obs), pulse.WithAspects(aspect)},
		ObserverFor: func(string) pulse.Observer {
			outletCalls.Add(1)
			return obs
		},
	}
	top, err := pulseyaml.Analyze([]byte(analyzeTreeDoc), opts)
	if err != nil {
		t.Fatal(err)
	}
	if outletCalls.Load() != 0 || observerCalls.Load() != 0 || aspectCalls.Load() != 0 {
		t.Fatalf("Analyze 触发了出口或运行回调：outlet=%d observer=%d aspect=%d", outletCalls.Load(), observerCalls.Load(), aspectCalls.Load())
	}
	loaded, _, err := pulseyaml.Load([]byte(analyzeTreeDoc), reg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(top, loaded.Topology()) {
		t.Fatalf("Analyze / Load 拓扑不一致：\nAnalyze=%+v\nLoad=%+v", top, loaded.Topology())
	}
	if outletCalls.Load() != 4 || observerCalls.Load() != 0 || aspectCalls.Load() != 0 {
		t.Fatalf("Load 应仅为四个挂载点建出口：outlet=%d observer=%d aspect=%d", outletCalls.Load(), observerCalls.Load(), aspectCalls.Load())
	}

	goGraph := analyzeGoTree(t, neverRun)
	if !reflect.DeepEqual(top, goGraph.Topology()) {
		t.Fatalf("Analyze / Go 拓扑不一致：\nAnalyze=%+v\nGo=%+v", top, goGraph.Topology())
	}
	if got := top.Graph.InPorts; len(got) != 2 || got[0].Name != "z.raw" || got[1].Name != "a.raw" {
		t.Fatalf("根入口应按 seeds 声明序：%+v", got)
	}
	leaf := top.Nodes[0].Sub.Nodes[0].Sub
	var ports []string
	for _, k := range leaf.Graph.InPorts {
		ports = append(ports, k.Name)
	}
	if !reflect.DeepEqual(ports, []string{"child.in", "pass", "m.fixed", "z.skip"}) {
		t.Fatalf("子入口应按接线后固定输入的实际声明序：%v", ports)
	}
	for _, k := range leaf.Graph.OutPorts {
		if k.Name == "pass" && k.Type != "[]string" {
			t.Fatalf("多层绑定的标签应传播到纯透传口：%+v", k)
		}
	}

	// 每次分析各装一份，修改返回值不应影响下一次结果。
	top.Graph.InPorts[0].Type = "changed"
	leaf.Nodes[0].Requires[0].Name = "changed"
	again, err := pulseyaml.Analyze([]byte(analyzeTreeDoc), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, loaded.Topology()) {
		t.Fatal("前一份分析结果污染了下一次结果")
	}
}

func analyzeGoTree(t *testing.T, run pulse.RunFunc) *pulse.Graph {
	t.Helper()
	newGraph := func(id, description string, opts ...pulse.Option) *pulse.Graph {
		g, err := pulse.New(id, description, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	raw := pulse.NewKey[string]("z.raw")
	list := pulse.NewKey[[]string]("a.raw")
	cin := pulse.NewKey[string]("child.in")
	cout := pulse.NewKey[string]("child.out")
	extra := pulse.NewKey[[]string]("child.extra")
	pass := pulse.NewKey[[]string]("pass")
	fixed := pulse.NewKey[int]("m.fixed")
	skip := pulse.NewKey[string]("z.skip")
	mi := pulse.NewKey[string]("mid.in")
	mo := pulse.NewKey[string]("mid.out")
	mp := pulse.NewKey[[]string]("mid.pass")
	me := pulse.NewKey[[]string]("mid.echo")
	left := pulse.NewKey[string]("root.left")
	right := pulse.NewKey[string]("root.right")
	p1 := pulse.NewKey[[]string]("root.pass1")
	p2 := pulse.NewKey[[]string]("root.pass2")
	root := newGraph("analysis", "根说明", pulse.WithMaxRunning(3))
	for i, site := range []string{"left", "right"} {
		mid := newGraph("analysis/"+site, "中层说明")
		leaf := newGraph("analysis/"+site+"/toLeaf", "末层说明")
		must(leaf.Add(pulse.NewNode("work", "叶子工作", pulse.Deps(pulse.Requires(cin), pulse.Requires(fixed), pulse.Requires(skip)), pulse.Deps(pulse.Provides(cout), pulse.Provides(extra)), run)))
		must(leaf.DeclareInput(pulse.NewPort(cin), pulse.NewPort(pass), pulse.NewPort(fixed), pulse.NewPort(skip)))
		must(leaf.DeclareOutput(pulse.NewPort(cout), pulse.NewPort(pass)))
		must(pulse.Sub(mid, "toLeaf", "嵌套挂载", []pulse.SubBind{
			pulse.In(mi, cin), pulse.In(mp, pass), pulse.Out(cout, mo), pulse.Out(pass, me),
			pulse.Fix(fixed, 7), pulse.FixSkip(skip),
		}, leaf))
		must(mid.DeclareInput(pulse.NewPort(mi), pulse.NewPort(mp)))
		must(mid.DeclareOutput(pulse.NewPort(me), pulse.NewPort(mo)))
		out, p, desc := left, p1, "左挂载"
		if i == 1 {
			out, p, desc = right, p2, "右挂载"
		}
		must(pulse.Sub(root, site, desc, []pulse.SubBind{
			pulse.In(raw, mi), pulse.In(list, mp), pulse.Out(me, p), pulse.Out(mo, out),
		}, mid))
	}
	must(root.Add(pulse.NewNode("sink", "汇总", pulse.Deps(pulse.Requires(left, right), pulse.Requires(p1, p2)), nil, run)))
	must(root.DeclareInput(pulse.NewPort(raw), pulse.NewPort(list)))
	return root
}

func TestAnalyzeAllowsUnregisteredFactory(t *testing.T) {
	data := []byte("nodes: [{id: future, uses: not-installed}]")
	opts := pulseyaml.LoadOptions{GraphID: "factory"}
	top, err := pulseyaml.Analyze(data, opts)
	if err != nil || top == nil || len(top.Nodes) != 1 {
		t.Fatalf("无注册表仍应分析工厂节点：top=%+v err=%v", top, err)
	}
	if _, _, err := pulseyaml.Load(data, pulse.NewRegistry(), opts); err == nil || !strings.Contains(err.Error(), "unknown factory") {
		t.Fatalf("Load 仍应校验工厂登记：%v", err)
	}
}

func TestAnalyzePreservesUnknownTypeLabels(t *testing.T) {
	const doc = `
seeds:
  - key: {name: input, type: example.Item}
    from: {kind: file, path: missing-file}
nodes:
  - id: n
    uses: not-installed
    requires: [{name: input, type: example.Item}]
    provides: [{name: output, type: "[]example.Item"}]
`
	top, err := pulseyaml.Analyze([]byte(doc), pulseyaml.LoadOptions{GraphID: "labels"})
	if err != nil {
		t.Fatal(err)
	}
	if top.Graph.InPorts[0].Type != "example.Item" || top.Nodes[0].Requires[0].Type != "example.Item" || top.Nodes[0].Provides[0].Type != "[]example.Item" {
		t.Fatalf("标签不能被占位 reflect 类型替换：%+v", top)
	}
}

func TestAnalyzeLeavesUninferableBindingTypesUnknown(t *testing.T) {
	const doc = `
graphs:
  wrap:
    nodes: [{id: noop, uses: f}]
nodes:
  - id: step
    graph: wrap
    in: {pass: external}
    out: {pass: output}
`
	opts := pulseyaml.LoadOptions{GraphID: "unknown"}
	top, err := pulseyaml.Analyze([]byte(doc), opts)
	if err != nil {
		t.Fatal(err)
	}
	n := top.Nodes[0]
	for _, keys := range [][]pulse.KeyShape{n.Requires, n.Provides, n.Sub.Graph.InPorts, n.Sub.Graph.OutPorts} {
		for _, k := range keys {
			if k.Type != "" {
				t.Fatalf("没有任何 typed KeySpec，键类型应未知：%+v", k)
			}
		}
	}
	reg := pulse.NewRegistry()
	for _, name := range []string{"pass", "external", "output"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](name))
	}
	reg.MustRegister("f", func(*pulse.RunCtx) error { return nil })
	g, _, err := pulseyaml.Load([]byte(doc), reg, opts)
	if err != nil {
		t.Fatal(err)
	}
	loaded := g.Topology()
	if loaded.Nodes[0].Sub.Graph.OutPorts[0].Type != "string" {
		t.Fatal("Load 应有宿主登记的实际类型")
	}
	// 此处只能比较除 Type 外的形状；Analyze 没有宿主表，无法声称知道 string。
	loaded.Nodes[0].Requires[0].Type = ""
	loaded.Nodes[0].Provides[0].Type = ""
	loaded.Nodes[0].Sub.Graph.InPorts[0].Type = ""
	loaded.Nodes[0].Sub.Graph.OutPorts[0].Type = ""
	if !reflect.DeepEqual(top, loaded) {
		t.Fatalf("未知 Type 以外的字段应一致：Analyze=%+v Load=%+v", top, loaded)
	}
}
