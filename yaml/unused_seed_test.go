package yaml_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

// 未引用模板没有挂载接口，但 seed 与节点产出是已知的两个来源。
// literal / skip 都必须复用核心来源冲突，不能因为模板暂未挂载而漏检。
func TestTemplateSeedSourceConflict(t *testing.T) {
	for _, seed := range []struct {
		name string
		body string
	}{
		{"literal", "from: {kind: literal, value: seed}"},
		{"skip", "skip: true"},
	} {
		for _, site := range []struct {
			name string
			root string
			want string
		}{
			{"unused", "nodes: [{id: root, uses: f}]", `graph "template"`},
			{"referenced", "nodes: [{id: mount, graph: template, out: {k: out}}]", `node "mount"`},
		} {
			t.Run(seed.name+"/"+site.name, func(t *testing.T) {
				doc := fmt.Sprintf(`
graphs:
  template:
    seeds:
      - key: {name: k, type: string}
        %s
    nodes:
      - {id: producer, uses: f, provides: [{name: k, type: string}]}
%s
`, seed.body, site.root)
				for _, result := range templateValidationResults(t, doc) {
					t.Run(result.name, func(t *testing.T) {
						if !errors.Is(result.err, pulse.ErrDuplicateSource) {
							t.Fatalf("seed 与节点产出应包装 ErrDuplicateSource：%v", result.err)
						}
						for _, want := range []string{site.want, `"k"`, `node "producer"`} {
							if !strings.Contains(result.err.Error(), want) {
								t.Fatalf("来源冲突应点明 %q：%v", want, result.err)
							}
						}
					})
				}
			})
		}
	}
}

// 未引用时未知来源可能是将来的入口；引用后漏接才是真错。
// 其它已知静态错误仍由共用装配器和核心判据拒绝。
func TestTemplateSourceValidationBoundaries(t *testing.T) {
	const missingInput = `
graphs:
  template:
    nodes:
      - {id: work, uses: f, requires: [{name: missing, type: string}], provides: [{name: k, type: string}]}
`
	for _, tc := range []struct {
		name      string
		doc       string
		want      string
		duplicate bool
	}{
		{"unused-missing-source", missingInput + "nodes: [{id: root, uses: f}]", "", false},
		{"referenced-missing-source", missingInput + "nodes: [{id: mount, graph: template, out: {k: out}}]", `requires "missing" but nothing provides it`, false},
		{"unused-two-producers", `
graphs:
  template:
    nodes:
      - {id: one, uses: f, provides: [{name: k, type: string}]}
      - {id: two, uses: f, provides: [{name: k, type: string}]}
nodes: [{id: root, uses: f}]
`, `graph "template"`, true},
		{"unused-cycle", `
graphs:
  template:
    nodes:
      - {id: one, uses: f, requires: [{name: x, type: string}], provides: [{name: y, type: string}]}
      - {id: two, uses: f, requires: [{name: y, type: string}], provides: [{name: x, type: string}]}
nodes: [{id: root, uses: f}]
`, "dependency cycle", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, result := range templateValidationResults(t, tc.doc) {
				t.Run(result.name, func(t *testing.T) {
					if tc.want == "" {
						if result.err != nil {
							t.Fatalf("未知来源可能是模板入口，不应拒绝：%v", result.err)
						}
						return
					}
					if result.err == nil || !strings.Contains(result.err.Error(), tc.want) {
						t.Fatalf("应报告 %q：%v", tc.want, result.err)
					}
					if errors.Is(result.err, pulse.ErrDuplicateSource) != tc.duplicate {
						t.Fatalf("来源冲突分类不符：%v", result.err)
					}
					if tc.name == "referenced-missing-source" && !strings.Contains(result.err.Error(), `mounted at "mount"`) {
						t.Fatalf("漏接输入应点明挂载位置：%v", result.err)
					}
				})
			}
		})
	}
}

// seed 可正常提供一条入口，同时保留另一条未知外部输入；被引用时由该挂载点接线。
func TestTemplateSeedsWithExternalInputs(t *testing.T) {
	for _, seed := range []string{"from: {kind: literal, value: seed}", "skip: true"} {
		for _, root := range []string{
			"nodes: [{id: root, uses: f}]",
			"seeds: [{key: {name: host, type: string}, from: {kind: env, env: TEMPLATE_INPUT}}]\nnodes: [{id: mount, graph: template, in: {missing: host}, out: {out: result}}]",
		} {
			doc := fmt.Sprintf(`
graphs:
  template:
    seeds:
      - key: {name: k, type: string}
        %s
    nodes:
      - {id: work, uses: f, requires: [{name: k, type: string}, {name: missing, type: string}], provides: [{name: out, type: string}]}
%s
`, seed, root)
			for _, result := range templateValidationResults(t, doc) {
				if result.err != nil {
					t.Fatalf("%s 应接受合法 seed 与外部输入：%v\n%s", result.name, result.err, doc)
				}
			}
		}
	}
}

// Analyze 只判断声明，不能把 Go literal 赋值检查带进来；两种模板路径口径一致。
func TestTemplateSeedLiteralAssignability(t *testing.T) {
	for _, root := range []string{
		"nodes: [{id: root, uses: f}]",
		"nodes: [{id: mount, graph: template, out: {out: result}}]",
	} {
		doc := fmt.Sprintf(`
graphs:
  template:
    seeds:
      - key: {name: k, type: string}
        from: {kind: literal, value: {wrong: shape}}
    nodes:
      - {id: work, uses: f, requires: [{name: k, type: string}], provides: [{name: out, type: string}]}
%s
`, root)
		for _, result := range templateValidationResults(t, doc) {
			if result.name == "Analyze" {
				if result.err != nil {
					t.Fatalf("Analyze 不应判断 Go literal 能否赋值：%v", result.err)
				}
			} else if result.err == nil || errors.Is(result.err, pulse.ErrDuplicateSource) {
				t.Fatalf("Load 应拒绝不匹配的 literal，而非来源冲突：%v", result.err)
			}
		}
	}
}

func templateValidationResults(t *testing.T, doc string) []struct {
	name string
	err  error
} {
	t.Helper()
	reg := pulse.NewRegistry()
	for _, name := range []string{"k", "out", "missing", "x", "y", "host", "result"} {
		pulse.MustRegisterKey(reg, pulse.NewKey[string](name))
	}
	reg.MustRegister("f", func(*pulse.RunCtx) error {
		t.Error("静态校验不应执行工厂")
		return nil
	})
	opts := pulseyaml.LoadOptions{GraphID: "templates"}
	g, plan, loadErr := pulseyaml.Load([]byte(doc), reg, opts)
	if loadErr != nil && (g != nil || plan != nil) {
		t.Fatal("Load 失败不应返回部分图或 SeedPlan")
	}
	opts.ObserverFor = func(string) pulse.Observer {
		t.Error("Analyze 不应创建观察出口")
		return nil
	}
	top, analyzeErr := pulseyaml.Analyze([]byte(doc), opts)
	if analyzeErr != nil && top != nil {
		t.Fatal("Analyze 失败不应返回部分拓扑")
	}
	return []struct {
		name string
		err  error
	}{{"Load", loadErr}, {"Analyze", analyzeErr}}
}
