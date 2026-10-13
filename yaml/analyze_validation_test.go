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
