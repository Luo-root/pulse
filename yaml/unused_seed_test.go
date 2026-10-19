package yaml_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

// 未引用模板自身的 seed 与实际挂载必须共享 Fix/FixSkip 事实，不能只声明来源。
// unknown 仍可能得到值；skip + unknown 的 fan-in 也不能被当成全部跳过。
func TestTemplateSeedStreamFacts(t *testing.T) {
	for _, shape := range []string{"direct", "relay", "nested"} {
		for _, seed := range []string{"skip", "literal", "unknown", "skip-and-unknown"} {
			for _, unused := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/unused=%t", shape, seed, unused), func(t *testing.T) {
					reg := pulse.NewRegistry()
					for _, name := range []string{"flag", "routed", "out", "result", "future"} {
						pulse.MustRegisterKey(reg, pulse.NewKey[int](name))
					}
					pulse.MustRegisterKey(reg, pulse.NewKey[pulse.Stream[int]]("stream"))
					bodyCalls := 0
					reg.MustRegister("work", func(*pulse.RunCtx) error {
						bodyCalls++
						t.Error("已知 skip 的业务体不应运行")
						return nil
					})
					reg.MustRegister("root", func(*pulse.RunCtx) error { return nil })
					seeds := "    seeds: [{key: {name: flag, type: int}, skip: true}]\n"
					switch seed {
					case "literal":
						seeds = strings.Replace(seeds, "skip: true", "from: {value: 1}", 1)
					case "unknown":
						seeds = ""
					}
					writer := `{id: writer, uses: work, requires: [{name: flag, type: int}], provides: [{name: stream, type: "pulse.Stream[int]"}, {name: out, type: int}]}`
					if seed == "skip-and-unknown" {
						writer = strings.Replace(writer, "name: flag, type: int}", "name: flag, type: int}, {name: future, type: int}", 1)
					}
					nodes, extra := "      - "+writer+"\n", ""
					if shape == "relay" {
						// writer 在 relay 前声明，事实仍须沿普通值传播，不能依赖声明顺序。
						nodes = "      - " + strings.Replace(writer, "name: flag", "name: routed", 1) + "\n      - {id: relay, uses: work, requires: [{name: flag, type: int}], provides: [{name: routed, type: int}]}\n"
					}
					if shape == "nested" {
						nodes = "      - {id: inner, graph: leaf, in: {flag: flag}, out: {out: out}}\n"
						if seed == "skip-and-unknown" {
							nodes = strings.Replace(nodes, "flag: flag}", "flag: flag, future: future}", 1)
						}
						extra = "  leaf:\n    nodes:\n      - " + writer + "\n"
					}
					root := "nodes: [{id: mount, graph: template, out: {out: result}}]\n"
					if seed == "unknown" {
						root = strings.Replace(root, "out:", "in: {flag: flag}, out:", 1)
					}
					if seed == "skip-and-unknown" {
						root = strings.Replace(root, "out:", "in: {future: future}, out:", 1)
					}
					if unused {
						root = "nodes: [{id: root, uses: root}]\n"
					}
					doc := "graphs:\n" + extra + "  template:\n" + seeds + "    nodes:\n" + nodes + root
					observerCalls := 0
					opts := pulseyaml.LoadOptions{GraphID: "template-facts", ObserverFor: func(string) pulse.Observer { observerCalls++; return nil }}
					if _, err := pulseyaml.Analyze([]byte(doc), opts); err != nil || observerCalls != 0 {
						t.Fatalf("Analyze 不证明真实流约束，也不建观察出口：err=%v observers=%d", err, observerCalls)
					}
					g, plan, err := pulseyaml.Load([]byte(doc), reg, opts)
					if seed != "skip" {
						if err == nil || !strings.Contains(err.Error(), `provides "stream" but nothing consumes it`) || g != nil || plan != nil || observerCalls != 0 || bodyCalls != 0 {
							t.Fatalf("ready/unknown 仍可能产生流，必须拒绝且无副作用：err=%v observers=%d bodies=%d", err, observerCalls, bodyCalls)
						}
						return
					}
					if err != nil {
						t.Fatalf("固定 skip 应与实际挂载口径一致：%v", err)
					}
					wantObservers := 0
					if !unused {
						wantObservers = 1
						if shape == "nested" {
							wantObservers++
						}
						if err := g.DeclareOutput(pulse.NewPort(pulse.NewKey[int]("result"))); err != nil {
							t.Fatal(err)
						}
					}
					if bodyCalls != 0 || observerCalls != wantObservers || len(g.Topology().Problems) != 0 || len(plan.Entries) != 0 {
						t.Fatalf("校验宿主不得泄入结果或调用出口：bodies=%d observers=%d plan=%+v", bodyCalls, observerCalls, plan)
					}
					// 未被读取的未来入口仍是可选，不需要为它编造输入值。
					if err := g.DeclareInput(pulse.NewPort(pulse.NewKey[int]("future"))); err != nil {
						t.Fatal(err)
					}
					if err := g.Validate(); err != nil {
						t.Fatal(err)
					}
					in, err := plan.Inputs(nil)
					if err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
					defer cancel()
					e, err := g.NewExecution(ctx, in)
					if err != nil {
						t.Fatal(err)
					}
					if err := e.Start(); err != nil {
						t.Fatal(err)
					}
					if err := e.Wait(); err != nil || bodyCalls != 0 {
						t.Fatalf("正常 skip 必须成功，不进入业务体：err=%v bodies=%d", err, bodyCalls)
					}
					if !unused {
						if _, state, err := pulse.ReadOutput(e, pulse.NewKey[int]("result")); err != nil || state != pulse.SlotStateSkipped {
							t.Fatalf("出口应正常 skipped：state=%s err=%v", state, err)
						}
					}
				})
			}
		}
	}
}

// 节点切面阻断 relay 的输出 skip 推断；不能在 YAML 自己传播 seed。
func TestTemplateSeedStreamRelayAspect(t *testing.T) {
	for _, aspect := range []string{"timeout: 1s", "retry: {attempts: 1}", "timeout: -1s"} {
		t.Run(aspect, func(t *testing.T) {
			reg := pulse.NewRegistry()
			for _, name := range []string{"flag", "routed"} {
				pulse.MustRegisterKey(reg, pulse.NewKey[int](name))
			}
			pulse.MustRegisterKey(reg, pulse.NewKey[pulse.Stream[int]]("stream"))
			reg.MustRegister("work", func(*pulse.RunCtx) error { t.Error("装配不能执行业务体"); return nil })
			doc := fmt.Sprintf(`
graphs:
  template:
    seeds: [{key: {name: flag, type: int}, skip: true}]
    nodes:
      - {id: writer, uses: work, requires: [{name: routed, type: int}], provides: [{name: stream, type: "pulse.Stream[int]"}]}
      - {id: relay, uses: work, requires: [{name: flag, type: int}], provides: [{name: routed, type: int}], %s}
nodes: [{id: root, uses: work}]
`, aspect)
			_, _, err := pulseyaml.Load([]byte(doc), reg, pulseyaml.LoadOptions{GraphID: "relay-aspect"})
			if aspect == "timeout: -1s" {
				if err != nil {
					t.Fatalf("负 timeout 不安装切面，应保留 skip 传播：%v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "nothing consumes it") {
				t.Fatalf("有效 relay 切面的输出仍未知，writer 不能当 skipped：%v", err)
			}
		})
	}
}

// 未知流入口通过临时宿主别名进入模板，分流诊断可能归到宿主，不能只取子层 Problems。
func TestTemplateSeedKeepsUnknownStreamReaders(t *testing.T) {
	for _, seeded := range []bool{false, true} {
		t.Run(fmt.Sprintf("seeded=%t", seeded), func(t *testing.T) {
			reg := pulse.NewRegistry()
			pulse.MustRegisterKey(reg, pulse.NewKey[int]("flag"))
			pulse.MustRegisterKey(reg, pulse.NewKey[pulse.Stream[int]]("stream"))
			reg.MustRegister("f", func(*pulse.RunCtx) error { t.Error("装配不能执行工厂"); return nil })
			seeds := ""
			if seeded {
				seeds = "    seeds: [{key: {name: flag, type: int}, skip: true}]\n"
			}
			doc := "graphs:\n  template:\n" + seeds + `    nodes:
      - {id: left, uses: f, requires: [{name: stream, type: "pulse.Stream[int]"}]}
      - {id: right, uses: f, requires: [{name: stream, type: "pulse.Stream[int]"}]}
nodes: [{id: root, uses: f}]
`
			g, plan, err := pulseyaml.Load([]byte(doc), reg, pulseyaml.LoadOptions{GraphID: "unknown-stream"})
			if err == nil || !strings.Contains(err.Error(), `stream output "stream" is read by 2 nodes`) || g != nil || plan != nil {
				t.Fatalf("未知输入不等于无流；未声明竞争组的两个读者仍须拒绝：%v", err)
			}
		})
	}
}

// 固定 seed 新增的校验宿主不能重放根 Option、带入宿主结构或封口共享 child。
func TestTemplateSeedHostIsolation(t *testing.T) {
	for _, entry := range []string{"Load", "Analyze"} {
		for _, templates := range []int{0, 1, 2, 3} {
			t.Run(fmt.Sprintf("%s/templates=%d", entry, templates), func(t *testing.T) {
				run := func(*pulse.RunCtx) error { t.Error("装配不能执行宿主或工厂"); return nil }
				child, err := pulse.New("shared-child", "")
				if err != nil {
					t.Fatal(err)
				}
				// 宿主独有键不登记在 YAML Registry，且与模板的同名节点隔离。
				hostOnly := pulse.NewKey[int]("host.only")
				if err := child.DeclareInput(pulse.NewPort(hostOnly)); err != nil {
					t.Fatal(err)
				}
				reg := pulse.NewRegistry()
				pulse.MustRegisterKey(reg, pulse.NewKey[int]("flag"))
				reg.MustRegister("f", run)
				calls, observerCalls := 0, 0
				opts := pulseyaml.LoadOptions{GraphID: "seed-isolation", Graph: []pulse.Option{func(g *pulse.Graph) {
					calls++
					if err := pulse.Sub(g, "host", "", []pulse.SubBind{pulse.Fix(hostOnly, 1)}, child); err != nil {
						t.Fatal(err)
					}
				}}, ObserverFor: func(string) pulse.Observer { observerCalls++; return nil }}
				doc := "nodes: [{id: root, uses: f}]\n"
				if templates != 0 {
					doc += "graphs:\n"
					for i := 0; i < templates; i++ {
						nodes := "[{id: host, uses: f, requires: [{name: flag, type: int}]}]"
						if i == 2 {
							nodes = "[{id: twice, uses: f}, {id: twice, uses: f}]"
						}
						doc += fmt.Sprintf("  template%d: {seeds: [{key: {name: flag, type: int}, skip: true}], nodes: %s}\n", i, nodes)
					}
				}
				var g *pulse.Graph
				var plan *pulseyaml.SeedPlan
				var top *pulse.Topology
				if entry == "Load" {
					g, plan, err = pulseyaml.Load([]byte(doc), reg, opts)
					if err == nil {
						top = g.Topology()
					}
				} else {
					top, err = pulseyaml.Analyze([]byte(doc), opts)
				}
				wantCalls := 2 // 校验根 + 真实根，与模板数量无关。
				if templates == 3 {
					wantCalls = 1
					if err == nil || !strings.Contains(err.Error(), "duplicate node id") || g != nil || plan != nil || top != nil {
						t.Fatalf("后续坏模板应拒绝且无部分产物：err=%v", err)
					}
				} else if err != nil || top == nil || len(top.Nodes) != 2 || top.Nodes[0].ID != "host" || top.Nodes[0].Sub == nil || top.Nodes[1].ID != "root" {
					t.Fatalf("校验宿主或模板结构不能泄入返回根图：top=%+v err=%v", top, err)
				}
				if calls != wantCalls || observerCalls != 0 {
					t.Fatalf("不得重放 Option 或为模板建出口：options=%d want=%d observers=%d", calls, wantCalls, observerCalls)
				}
				if err := child.Add(pulse.NewNode("after-load", "", nil, nil, run)); err != nil {
					t.Fatalf("无论成功或后续模板失败，共享 child 均不能被封口：%v", err)
				}
			})
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
