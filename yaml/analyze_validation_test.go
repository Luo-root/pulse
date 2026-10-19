package yaml_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

// 类型与工厂都有宿主登记时，结构性错误应由共用 assembler / 核心判据报同一句。
// 每条配置只放一个待测结构错误，避免把类型标签诊断的顺序混成图校验契约。
func TestAnalyzeAndLoadShareStructuralErrors(t *testing.T) {
	reg := pulse.NewRegistry()
	for _, name := range []string{"a", "b", "c", "typo"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](name))
	}
	reg.MustRegister("f", func(*pulse.RunCtx) error { return nil })
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"解码", "nodes: [", "decode:"},
		{"版本", "version: 99\nnodes: [{id: n, uses: f}]", "unsupported version"},
		{"空根", "nodes: []", "document has no nodes"},
		{"空子图", "graphs: {empty: {nodes: []}}\nnodes: [{id: n, uses: f}]", "has no nodes"},
		{"无 id", "nodes: [{uses: f}]", "missing id"},
		{"无形态", "nodes: [{id: n}]", "missing uses or graph"},
		{"形态互斥", "nodes: [{id: n, uses: f, graph: nope}]", "mutually exclusive"},
		{"挂载路径分隔符", "graphs: {child: {nodes: [{id: leaf, uses: f, requires: [{name: a, type: string}]}]}}\nnodes: [{id: a/b, graph: child, in: {a: b}}]", "must not contain '/'"},
		{"重复 id", "nodes: [{id: n, uses: f}, {id: n, uses: f}]", "duplicate node id"},
		{"工厂节点接线", "nodes: [{id: n, uses: f, in: {a: b}}]", "factory node declares its boundary"},
		{"未知子图", "nodes: [{id: n, graph: nope, in: {a: b}}]", "references unknown graph"},
		{"重复来源", `
nodes:
  - {id: n1, uses: f, provides: [{name: a, type: string}]}
  - {id: n2, uses: f, provides: [{name: a, type: string}]}
`, "key already has a source"},
		{"读写同键", `
nodes:
  - id: n
    uses: f
    requires: [{name: a, type: string}]
    provides: [{name: a, type: string}]
`, "both requires and provides"},
		{"入口与生产者冲突", `
seeds:
  - key: {name: a, type: string}
    from: {value: x}
nodes:
  - {id: n, uses: f, provides: [{name: a, type: string}]}
`, "key already has a source"},
		{"子图没有接线", `
graphs:
  child: {nodes: [{id: leaf, uses: f}]}
nodes: [{id: step, graph: child}]
`, "needs in/out"},
		{"子图另写边界", `
graphs:
  child: {nodes: [{id: leaf, uses: f, requires: [{name: a, type: string}]}]}
nodes:
  - id: step
    graph: child
    in: {a: b}
    requires: [{name: b, type: string}]
`, "graph node declares its boundary"},
		{"绑了不读的键", `
graphs:
  child: {nodes: [{id: leaf, uses: f, requires: [{name: a, type: string}]}]}
nodes: [{id: step, graph: child, in: {typo: b}}]
`, "no node of it requires"},
		{"父输入撞内部生产", `
graphs:
  child: {nodes: [{id: leaf, uses: f, provides: [{name: a, type: string}]}]}
nodes: [{id: step, graph: child, in: {a: b}, out: {a: c}}]
`, "provides \"a\" inside"},
		{"漏接必需输入", `
graphs:
  child:
    nodes:
      - {id: leaf, uses: f, requires: [{name: a, type: string}], provides: [{name: b, type: string}]}
nodes: [{id: step, graph: child, out: {b: c}}]
`, "mounted at \"step\""},
		{"子图重复 seed", `
graphs:
  child:
    seeds:
      - {key: {name: a, type: string}, from: {value: x}}
      - {key: {name: a, type: string}, skip: true}
    nodes: [{id: leaf, uses: f}]
nodes: [{id: root, uses: f}]
`, "seeded twice"},
		{"seed 与 in 重叠", `
graphs:
  child:
    seeds: [{key: {name: a, type: string}, from: {value: x}}]
    nodes: [{id: leaf, uses: f, requires: [{name: a, type: string}]}]
nodes: [{id: step, graph: child, in: {a: b}}]
`, "binding also feeds"},
		{"子图外部 seed", `
graphs:
  child:
    seeds: [{key: {name: a, type: string}, from: {kind: file, path: missing}}]
    nodes: [{id: leaf, uses: f}]
nodes: [{id: root, uses: f}]
`, "kind \"file\" is not supported"},
		{"子图依赖环", `
graphs:
  child:
    nodes:
      - {id: n1, uses: f, requires: [{name: a, type: string}], provides: [{name: b, type: string}]}
      - {id: n2, uses: f, requires: [{name: b, type: string}], provides: [{name: a, type: string}]}
nodes: [{id: step, graph: child, out: {b: c}}]
`, "dependency cycle"},
		{"未引用子图依赖环", `
graphs:
  child:
    nodes:
      - {id: n1, uses: f, requires: [{name: a, type: string}], provides: [{name: b, type: string}]}
      - {id: n2, uses: f, requires: [{name: b, type: string}], provides: [{name: a, type: string}]}
nodes: [{id: root, uses: f}]
`, "dependency cycle"},
		{"子图引用环", `
graphs:
  child: {nodes: [{id: step, graph: child, in: {a: b}}]}
nodes: [{id: root, uses: f}]
`, "graph reference cycle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := pulseyaml.LoadOptions{GraphID: "checks", MaxRunning: 2}
			_, ae := pulseyaml.Analyze([]byte(tc.doc), opts)
			_, _, le := pulseyaml.Load([]byte(tc.doc), reg, opts)
			if ae == nil || le == nil {
				t.Fatalf("两个入口都应拒绝：Analyze=%v Load=%v", ae, le)
			}
			if !strings.Contains(ae.Error(), tc.want) {
				t.Fatalf("应点明 %q：%v", tc.want, ae)
			}
			if ae.Error() != le.Error() {
				t.Fatalf("结构诊断应同源：\nAnalyze=%v\nLoad=%v", ae, le)
			}
		})
	}
}

func TestAnalyzeGraphIDRequired(t *testing.T) {
	data := []byte("nodes: [{id: n, uses: f}]")
	_, ae := pulseyaml.Analyze(data, pulseyaml.LoadOptions{})
	_, _, le := pulseyaml.Load(data, pulse.NewRegistry(), pulseyaml.LoadOptions{})
	if ae == nil || le == nil || ae.Error() != le.Error() || !strings.Contains(ae.Error(), "graph id is required") {
		t.Fatalf("图身份判据应一致：Analyze=%v Load=%v", ae, le)
	}
}

func TestAnalyzeKeepsRootProblems(t *testing.T) {
	const doc = `
graphs:
  unused:
    nodes:
      - {id: hungry, uses: f, requires: [{name: template.in, type: string}]}
nodes:
  - {id: hungry, uses: f, requires: [{name: missing, type: string}]}
  - {id: n1, uses: f, requires: [{name: a, type: string}], provides: [{name: b, type: string}]}
  - {id: n2, uses: f, requires: [{name: b, type: string}], provides: [{name: a, type: string}]}
`
	reg := pulse.NewRegistry()
	for _, name := range []string{"template.in", "missing", "a", "b"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](name))
	}
	reg.MustRegister("f", func(*pulse.RunCtx) error { return nil })
	opts := pulseyaml.LoadOptions{GraphID: "root"}
	top, err := pulseyaml.Analyze([]byte(doc), opts)
	if err != nil {
		t.Fatalf("根层静态毛病应写进 Problems：%v", err)
	}
	loaded, _, err := pulseyaml.Load([]byte(doc), reg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(top.Problems) != 2 || !strings.Contains(top.Problems[0].Error(), `requires "missing"`) || !strings.Contains(top.Problems[1].Error(), "dependency cycle") {
		t.Fatalf("应按来源 → 环返回根层毛病：%v", top.Problems)
	}
	if !reflect.DeepEqual(top, loaded.Topology()) {
		t.Fatalf("包括根层 Problems 的完整形状应一致：Analyze=%+v Load=%+v", top, loaded.Topology())
	}
}

func TestAnalyzeChecksDeclaredTypeLabels(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want []string
	}{
		{"同名标签冲突", `
seeds: [{key: {name: input, type: string}}]
nodes: [{id: n, uses: f, requires: [{name: input, type: int}]}]
`, []string{"input", "string", "int", "conflicting type tags"}},
		{"in 两端已知冲突", `
seeds: [{key: {name: parent, type: int}}]
graphs:
  child: {nodes: [{id: leaf, uses: f, requires: [{name: input, type: string}]}]}
nodes: [{id: step, graph: child, in: {input: parent}}]
`, []string{"step", "input", "parent", "string", "int", "does not match"}},
		{"out 两端已知冲突", `
graphs:
  child: {nodes: [{id: leaf, uses: f, provides: [{name: output, type: string}]}]}
nodes:
  - {id: step, graph: child, out: {output: parent}}
  - {id: sink, uses: f, requires: [{name: parent, type: int}]}
`, []string{"step", "output", "parent", "string", "int", "does not match"}},
		{"跨层传播后的冲突", `
seeds: [{key: {name: parent, type: int}}]
graphs:
  leaf: {nodes: [{id: work, uses: f, requires: [{name: input, type: string}]}]}
  mid: {nodes: [{id: inner, graph: leaf, in: {input: middle}}]}
nodes: [{id: step, graph: mid, in: {middle: parent}}]
`, []string{"inner", "input", "middle", "string", "int", "does not match"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pulseyaml.Analyze([]byte(tc.doc), pulseyaml.LoadOptions{GraphID: "types"})
			if err == nil {
				t.Fatal("已知类型标签矛盾应拒绝")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("诊断应点明 %q：%v", want, err)
				}
			}
		})
	}
}

// 预置的 In/Out 必须与 YAML 接线一样传播标签；宿主 Go 类型仍只是装配占位的来源。
func TestAnalyzeHostBindingTypeLabels(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, source := range []string{"same-label", "input-only", "output-only", "conflict"} {
			conflict := source == "conflict"
			name := "single"
			if nested {
				name = "nested"
			}
			name += "/" + source
			t.Run(name, func(t *testing.T) {
				child, in, out := analysisHostPassThrough(t, nested, "inside")
				left, right := pulse.NewKey[int]("left"), pulse.NewKey[int]("right")
				opts := pulseyaml.LoadOptions{GraphID: "host-labels", Graph: []pulse.Option{func(g *pulse.Graph) {
					if err := g.DeclareInput(pulse.NewPort(left)); err != nil {
						t.Fatal(err)
					}
					if err := pulse.Sub(g, "host", "", []pulse.SubBind{pulse.In(left, in), pulse.Out(out, right)}, child); err != nil {
						t.Fatal(err)
					}
				}}, ObserverFor: func(string) pulse.Observer { t.Error("analysis must not call ObserverFor"); return nil }}
				doc := "seeds: [{key: {name: left, type: example.A}}]\nnodes: [{id: yaml, uses: f, requires: [{name: right, type: example.A}]}]\n"
				switch source {
				case "input-only":
					doc = "seeds: [{key: {name: left, type: example.A}}]\nnodes: [{id: yaml, uses: f}]\n"
				case "output-only":
					doc = "nodes: [{id: yaml, uses: f, requires: [{name: right, type: example.A}]}]\n"
				case "conflict":
					doc = strings.Replace(doc, "name: right, type: example.A", "name: right, type: example.B", 1)
				}
				top, err := pulseyaml.Analyze([]byte(doc), opts)
				if conflict {
					if err == nil || top != nil {
						t.Fatalf("host pass-through must reject conflicting YAML labels: top=%+v err=%v", top, err)
					}
					for _, want := range []string{"host", "example.A", "example.B", "does not match"} {
						if !strings.Contains(err.Error(), want) {
							t.Fatalf("label conflict must identify %q: %v", want, err)
						}
					}
					return
				}
				if err != nil || len(top.Problems) != 0 {
					t.Fatalf("matching labels must assemble: top=%+v err=%v", top, err)
				}
				if top.Nodes[0].Requires[0].Type != "example.A" || top.Nodes[0].Provides[0].Type != "example.A" || top.Nodes[0].Sub == nil {
					t.Fatalf("labels must propagate in both directions through the host mount: %+v", top.Nodes[0])
				}
				for sub := top.Nodes[0].Sub; sub != nil; {
					for _, ports := range [][]pulse.KeyShape{sub.Graph.InPorts, sub.Graph.OutPorts} {
						if len(ports) != 1 || ports[0].Type != "example.A" {
							t.Fatalf("known label must reach every host pass-through port at %q: %+v", sub.Graph.Path, ports)
						}
					}
					if len(sub.Nodes) == 0 {
						break
					}
					sub = sub.Nodes[0].Sub
				}
			})
		}
	}
}

// 同一子图定义挂两处，内部同名键属于各自挂载点；Fixed 不连接父图同名键。
func TestAnalyzeHostBindingLabelScopes(t *testing.T) {
	for _, mode := range []string{"shared", "nested-shared", "fixed", "fixed-skip"} {
		t.Run(mode, func(t *testing.T) {
			keyName := "inside"
			fixed := strings.HasPrefix(mode, "fixed")
			if fixed {
				keyName = "left"
			}
			child, in, out := analysisHostPassThrough(t, mode == "nested-shared", keyName)
			doc := `
seeds:
  - {key: {name: left.a, type: example.A}}
  - {key: {name: left.b, type: example.B}}
nodes:
  - {id: yaml, uses: f, requires: [{name: right.a, type: example.A}, {name: right.b, type: example.B}]}
`
			opts := pulseyaml.LoadOptions{GraphID: "host-scopes", Graph: []pulse.Option{func(g *pulse.Graph) {
				if fixed {
					bind := pulse.Fix(in, 7)
					if mode == "fixed-skip" {
						bind = pulse.FixSkip(in)
					}
					if err := pulse.Sub(g, "host", "", []pulse.SubBind{bind, pulse.Out(out, pulse.NewKey[int]("right"))}, child); err != nil {
						t.Fatal(err)
					}
					return
				}
				for _, suffix := range []string{"a", "b"} {
					binds := []pulse.SubBind{pulse.In(pulse.NewKey[int]("left."+suffix), in), pulse.Out(out, pulse.NewKey[int]("right."+suffix))}
					if err := pulse.Sub(g, suffix, "", binds, child); err != nil {
						t.Fatal(err)
					}
				}
			}}}
			if fixed {
				doc = "seeds: [{key: {name: left, type: example.A}}]\nnodes: [{id: yaml, uses: f, requires: [{name: right, type: example.B}]}]\n"
			}
			top, err := pulseyaml.Analyze([]byte(doc), opts)
			if err != nil || len(top.Problems) != 0 {
				t.Fatalf("unrelated graph scopes must not merge same-name keys: top=%+v err=%v", top, err)
			}
			for i, want := range []string{"example.A", "example.B"} {
				if fixed {
					want = "example.B"
				}
				sub := top.Nodes[i].Sub
				if mode == "nested-shared" {
					sub = sub.Nodes[0].Sub
				}
				if sub.Graph.InPorts[0].Type != want || sub.Graph.OutPorts[0].Type != want {
					t.Fatalf("mount %q must have its own label %q: %+v", sub.Graph.Path, want, sub.Graph)
				}
				if fixed {
					if top.Graph.InPorts[0].Type != "example.A" {
						t.Fatal("fixed child input must not change the unrelated same-name root label")
					}
					break
				}
			}
		})
	}
}

func TestAnalyzeUnusedHostBindingLabels(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		name := "same-label"
		if conflict {
			name = "conflict"
		}
		t.Run(name, func(t *testing.T) {
			child, in, out := analysisHostPassThrough(t, false, "inside")
			left, right := pulse.NewKey[int]("left"), pulse.NewKey[int]("right")
			// 根身份与模板名相同也不能混用两棵校验树的标签。
			opts := pulseyaml.LoadOptions{GraphID: "unused", Graph: []pulse.Option{func(g *pulse.Graph) {
				if err := g.DeclareInput(pulse.NewPort(left)); err != nil {
					t.Fatal(err)
				}
				if err := pulse.Sub(g, "host", "", []pulse.SubBind{pulse.In(left, in), pulse.Out(out, right)}, child); err != nil {
					t.Fatal(err)
				}
			}}}
			doc := `
graphs:
  unused:
    seeds: [{key: {name: left, type: example.A}}]
    nodes: [{id: read, uses: f, requires: [{name: right, type: example.A}]}]
nodes: [{id: yaml, uses: f}]
`
			if conflict {
				doc = strings.Replace(doc, "name: right, type: example.A", "name: right, type: example.B", 1)
			}
			top, err := pulseyaml.Analyze([]byte(doc), opts)
			// left/right 在模板内没有绑定：根 Option 的宿主 Sub 不能泄入这里，
			// 因而两条不同标签也不冲突。真实根 Sub 标签检查由 TestAnalyzeHostBindingTypeLabels 钉住。
			if err != nil || len(top.Problems) != 0 {
				t.Fatalf("unused template must not inherit root host bindings: top=%+v err=%v", top, err)
			}
			if top.Nodes[0].Requires[0].Type != "" || top.Nodes[0].Provides[0].Type != "" || top.Nodes[0].Sub.Graph.InPorts[0].Type != "" {
				t.Fatalf("unused template labels must not leak into the unrelated root host graph: %+v", top)
			}
		})
	}
}

func analysisHostPassThrough(t *testing.T, nested bool, name string) (*pulse.Graph, pulse.Key[int], pulse.Key[int]) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	child, err := pulse.New("same-host-id", "")
	must(err)
	inside := pulse.NewKey[int](name)
	must(child.DeclareInput(pulse.NewPort(inside)))
	must(child.DeclareOutput(pulse.NewPort(inside)))
	if !nested {
		return child, inside, inside
	}
	mid, err := pulse.New("same-host-id", "")
	must(err)
	in, out := pulse.NewKey[int]("mid.in"), pulse.NewKey[int]("mid.out")
	must(mid.DeclareInput(pulse.NewPort(in)))
	must(mid.DeclareOutput(pulse.NewPort(out)))
	must(pulse.Sub(mid, "inner", "", []pulse.SubBind{pulse.In(in, inside), pulse.Out(inside, out)}, child))
	return mid, in, out
}

func TestAnalyzeRequiresExplicitTypeTags(t *testing.T) {
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("a"))
	reg.MustRegister("f", func(*pulse.RunCtx) error { return nil })
	for _, doc := range []string{
		"nodes: [{id: n, uses: f, requires: [{name: a}]}]",
		"nodes: [{id: n, uses: f, provides: [{name: a}]}]",
		"seeds: [{key: {name: a}}]\nnodes: [{id: n, uses: f}]",
		"graphs: {unused: {nodes: [{id: n, uses: f, requires: [{name: a}]}]}}\nnodes: [{id: n, uses: f}]",
		// 可从另一条声明推导也不补写显式 KeySpec 的 type；与 Load 一样拒绝。
		"seeds: [{key: {name: a, type: string}}]\nnodes: [{id: n, uses: f, requires: [{name: a}]}]",
	} {
		opts := pulseyaml.LoadOptions{GraphID: "missing-type"}
		_, ae := pulseyaml.Analyze([]byte(doc), opts)
		_, _, le := pulseyaml.Load([]byte(doc), reg, opts)
		if ae == nil || le == nil || !strings.Contains(ae.Error(), "missing type tag") || !strings.Contains(le.Error(), "missing type tag") {
			t.Fatalf("显式键不能省 type：Analyze=%v Load=%v", ae, le)
		}
		if strings.Contains(ae.Error(), "interface {}") {
			t.Fatalf("诊断不能拿占位类型声称文档类型：%v", ae)
		}
	}
}
