package yaml_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// 宿主 Option 先装的 typed key 与文档同名时，Analyze 仍只判断标签与接线。
// Load 使用真实类型装配并运行，证明预置节点和端口确实供给了 YAML 的输入。
func TestAnalyzeTypedGraphOptions(t *testing.T) {
	const read = "nodes: [{id: read, uses: read, requires: [{name: value, type: int}]}]\n"
	const seed = "seeds: [{key: {name: value, type: int}, from: {kind: context, key: caller}}]\n"
	const emit = "nodes: [{id: emit, uses: emit, provides: [{name: value, type: int}]}]\n"
	const mounted = `
graphs:
  child:
    nodes: [{id: read, uses: read, requires: [{name: child.value, type: int}], provides: [{name: result, type: int}]}]
nodes: [{id: mount, graph: child, in: {child.value: value}, out: {result: result}}]
`
	for _, tc := range []struct {
		name      string
		doc       string
		wantHost  int32
		duplicate bool
	}{
		{"provider", read, 1, false},
		{"inlet", read, 0, false},
		{"host-sub", read, 1, false},
		{"mount-input", mounted, 1, false},
		{"consumer", emit, 1, false},
		{"outlet", emit, 0, false},
		{"unrelated", seed + read, 1, false},
		{"config-only", seed + read, 0, false},
		{"duplicate-root-seed", strings.Replace(seed, "]\n", ", {key: {name: value, type: int}, from: {value: 8}}]\n", 1) + read, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := pulse.NewKey[int]("value")
			var hostCalls, factoryCalls, observerCalls, aspectCalls atomic.Int32
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			check := func(rc *pulse.RunCtx, k pulse.Key[int]) error {
				v, err := pulse.Get(rc, k)
				if err == nil && v != 7 {
					return fmt.Errorf("%s = %d, want 7", k.Name(), v)
				}
				return err
			}
			host := pulse.Option(func(g *pulse.Graph) {
				switch tc.name {
				case "provider", "mount-input", "unrelated":
					key := value
					if tc.name == "unrelated" {
						key = pulse.NewKey[int]("host.only")
					}
					must(g.Add(pulse.NewNode("host", "host provider", nil, pulse.Provides(key), func(rc *pulse.RunCtx) error {
						hostCalls.Add(1)
						return pulse.Set(rc, key, 7)
					})))
				case "inlet", "duplicate-root-seed":
					must(g.DeclareInput(pulse.NewPort(value)))
				case "consumer":
					must(g.Add(pulse.NewNode("host", "host consumer", pulse.Requires(value), nil, func(rc *pulse.RunCtx) error {
						hostCalls.Add(1)
						return check(rc, value)
					})))
				case "outlet":
					must(g.DeclareOutput(pulse.NewPort(value)))
				case "host-sub":
					child, err := pulse.New("host-child", "host definition")
					must(err)
					in, out := pulse.NewKey[int]("host.in"), pulse.NewKey[int]("host.out")
					fixed, pass := pulse.NewKey[int]("host.fixed"), pulse.NewKey[int]("host.pass")
					parent, echo := pulse.NewKey[int]("host.parent"), pulse.NewKey[int]("host.echo")
					must(g.DeclareInput(pulse.NewPort(parent)))
					must(child.DeclareInput(pulse.NewPort(in), pulse.NewPort(fixed), pulse.NewPort(pass)))
					must(child.DeclareOutput(pulse.NewPort(out), pulse.NewPort(pass)))
					must(child.Add(pulse.NewNode("host-leaf", "host work", pulse.Requires(in, fixed), pulse.Provides(out), func(rc *pulse.RunCtx) error {
						hostCalls.Add(1)
						for _, key := range []pulse.Key[int]{in, fixed} {
							if err := check(rc, key); err != nil {
								return err
							}
						}
						return pulse.Set(rc, out, 7)
					})))
					must(pulse.Sub(g, "host", "host mount", []pulse.SubBind{
						pulse.In(parent, in), pulse.Fix(fixed, 7), pulse.Out(out, value), pulse.In(parent, pass), pulse.Out(pass, echo),
					}, child))
				}
			})
			obs := pulse.ObserverFunc{GraphStarted: func(pulse.Site) { observerCalls.Add(1) }}
			outletCalls := 0
			opts := pulseyaml.LoadOptions{
				GraphID: "typed-options", MaxRunning: 3,
				Graph: []pulse.Option{pulse.WithMaxRunning(2), pulse.WithObserver(obs), pulse.WithAspects(func(rc *pulse.RunCtx, next func(*pulse.RunCtx) error) error {
					aspectCalls.Add(1)
					return next(rc)
				}), host},
				ObserverFor: func(string) pulse.Observer { outletCalls++; return obs },
			}
			data := []byte("description: option graph\n" + tc.doc)
			top, err := pulseyaml.Analyze(data, opts)
			must(err)
			if len(top.Problems) != 0 || outletCalls != 0 || observerCalls.Load() != 0 || aspectCalls.Load() != 0 || hostCalls.Load() != 0 {
				t.Fatalf("Analyze must preserve sources without execution or observer side effects: top=%+v", top)
			}
			reg := pulse.NewRegistry()
			pulse.MustRegisterKey(reg, value)
			input := value
			if tc.name == "mount-input" {
				input = pulse.NewKey[int]("child.value")
				pulse.MustRegisterKey(reg, input)
				pulse.MustRegisterKey(reg, pulse.NewKey[int]("result"))
			}
			reg.MustRegister("read", func(rc *pulse.RunCtx) error {
				factoryCalls.Add(1)
				if err := check(rc, input); err != nil {
					return err
				}
				if tc.name == "mount-input" {
					return pulse.Set(rc, pulse.NewKey[int]("result"), 7)
				}
				return nil
			})
			reg.MustRegister("emit", func(rc *pulse.RunCtx) error { factoryCalls.Add(1); return pulse.Set(rc, value, 7) })
			g, plan, err := pulseyaml.Load(data, reg, opts)
			must(err)
			loaded := g.Topology()
			// 宿主键只接受 YAML 通过绑定传播的标签：host.out 连接到文档的 value；
			// 其余内部键仍未知，不能从宿主 Go 类型补标签。
			var clearHostTypes func(*pulse.Topology)
			clearHostTypes = func(top *pulse.Topology) {
				clear := func(keys []pulse.KeyShape) {
					for i := range keys {
						if strings.HasPrefix(keys[i].Name, "host.") && keys[i].Name != "host.out" {
							keys[i].Type = ""
						}
					}
				}
				clear(top.Graph.InPorts)
				clear(top.Graph.OutPorts)
				for _, n := range top.Nodes {
					clear(n.Requires)
					clear(n.Provides)
					if n.Sub != nil {
						clearHostTypes(n.Sub)
					}
				}
			}
			clearHostTypes(loaded)
			if !reflect.DeepEqual(top, loaded) {
				t.Fatalf("host declarations, ports, binds and source edges must match Load: Analyze=%+v Load=%+v", top, loaded)
			}
			wantOutlets := 0
			if tc.name == "mount-input" {
				wantOutlets = 1
			}
			if outletCalls != wantOutlets || observerCalls.Load() != 0 || aspectCalls.Load() != 0 || hostCalls.Load() != 0 || factoryCalls.Load() != 0 {
				t.Fatal("Load must only construct observers for YAML mounts, without executing bodies")
			}
			resolved := 0
			in, err := plan.Inputs(func(from pulseyaml.SeedFrom) (any, error) {
				resolved++
				if from.Kind != "context" || from.Key != "caller" {
					return nil, fmt.Errorf("unexpected seed: %+v", from)
				}
				return 7, nil
			})
			if tc.duplicate {
				if err == nil || !strings.Contains(err.Error(), "already set") || factoryCalls.Load() != 0 {
					t.Fatalf("duplicate root seeds must still fail only at Inputs: %v", err)
				}
				return
			}
			must(err)
			if tc.name == "inlet" {
				must(pulse.Seed(&in, value, 7))
			}
			if tc.name == "host-sub" {
				must(pulse.Seed(&in, pulse.NewKey[int]("host.parent"), 7))
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			must(g.Run(ctx, in))
			wantResolved := 0
			if tc.name == "unrelated" || tc.name == "config-only" {
				wantResolved = 1
			}
			if hostCalls.Load() != tc.wantHost || factoryCalls.Load() != 1 || resolved != wantResolved || observerCalls.Load() == 0 || aspectCalls.Load() == 0 {
				t.Fatalf("Load must run the actual typed graph with value 7: host=%d factory=%d resolver=%d observer=%d aspect=%d", hostCalls.Load(), factoryCalls.Load(), resolved, observerCalls.Load(), aspectCalls.Load())
			}
		})
	}
}

// 同名宿主 Go 类型不会充当文档标签；真正跨类型的 Load 与文档标签冲突仍拒绝。
func TestAnalyzeTypedGraphOptionsKeepTypeBoundaries(t *testing.T) {
	value := pulse.NewKey[int]("value")
	opts := pulseyaml.LoadOptions{GraphID: "type-boundary", Graph: []pulse.Option{func(g *pulse.Graph) {
		if err := g.Add(pulse.NewNode("host", "", nil, pulse.Provides(value), func(*pulse.RunCtx) error {
			t.Error("analysis must not run the host body")
			return nil
		})); err != nil {
			t.Fatal(err)
		}
	}}}
	data := []byte("nodes: [{id: read, uses: read, requires: [{name: value, type: string}]}]\n")
	top, err := pulseyaml.Analyze(data, opts)
	if err != nil || len(top.Problems) != 0 || top.Nodes[0].Provides[0].Type != "string" || top.Nodes[1].Requires[0].Type != "string" {
		t.Fatalf("only the YAML label is authoritative during Analyze: top=%+v err=%v", top, err)
	}
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("value"))
	reg.MustRegister("read", func(*pulse.RunCtx) error { t.Error("must not run real factory"); return nil })
	if g, plan, err := pulseyaml.Load(data, reg, opts); err == nil || !strings.Contains(err.Error(), "already registered as int, cannot reuse as string") || g != nil || plan != nil {
		t.Fatalf("Load must retain actual Go type rejection: graph=%v plan=%v err=%v", g, plan, err)
	}
	conflict := append([]byte("seeds: [{key: {name: value, type: int}}]\n"), data...)
	if top, err := pulseyaml.Analyze(conflict, opts); err == nil || !strings.Contains(err.Error(), "conflicting type tags") || top != nil {
		t.Fatalf("document label conflicts must still be rejected: top=%v err=%v", top, err)
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
