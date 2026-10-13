package yaml_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

// Analyze 只看固定输入的结构；null 与不能赋给 int 的字面量都不走 SeedByName 的
// 真实值转换。Load 仍在挂载点按宿主登记的 Go 类型检查它们。
func TestAnalyzeDoesNotCheckLiteralAssignability(t *testing.T) {
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[int]("fixed"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("output"))
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("parent"))
	reg.MustRegister("f", func(*pulse.RunCtx) error { return nil })
	for _, value := range []string{"null", "wrong", "[1, 2]", "{field: value}"} {
		t.Run(value, func(t *testing.T) {
			doc := `
graphs:
  child:
    seeds: [{key: {name: fixed, type: int}, from: {kind: literal, value: ` + value + `}}]
    nodes:
      - {id: work, uses: f, requires: [{name: fixed, type: int}], provides: [{name: output, type: string}]}
nodes: [{id: step, graph: child, out: {output: parent}}]
`
			opts := pulseyaml.LoadOptions{GraphID: "literal"}
			top, err := pulseyaml.Analyze([]byte(doc), opts)
			if err != nil {
				t.Fatalf("分析不应判断值能否赋给 int：%v", err)
			}
			child := top.Nodes[0].Sub
			if len(child.Graph.InPorts) != 1 || child.Graph.InPorts[0].Type != "int" {
				t.Fatalf("仍应保留固定输入的声明：%+v", child.Graph)
			}
			binds := top.Nodes[0].Binds
			if len(binds) != 2 || !binds[1].Fixed || !binds[1].In || binds[1].Child != "fixed" {
				t.Fatalf("固定输入仍是挂载点的 Fix：%+v", binds)
			}
			if _, _, err := pulseyaml.Load([]byte(doc), reg, opts); err == nil || (!strings.Contains(err.Error(), "nil") && !strings.Contains(err.Error(), "not assignable")) {
				t.Fatalf("Load 应继续按真实类型拒绝坏值：%v", err)
			}
		})
	}
}

func TestAnalyzeDoesNotResolveRootSeeds(t *testing.T) {
	const doc = `
observer: unconfigured
seeds:
  - key: {name: env, type: string}
    from: {kind: env, env: PULSE_ANALYZE_UNSET}
  - key: {name: file, type: string}
    from: {kind: file, path: missing-file}
  - key: {name: caller, type: string}
    from: {kind: context, key: absent}
  - key: {name: literal, type: string}
    from: {kind: literal, value: null}
nodes:
  - id: n
    uses: not-installed
    requires:
      - {name: env, type: string}
      - {name: file, type: string}
      - {name: caller, type: string}
      - {name: literal, type: string}
`
	top, err := pulseyaml.Analyze([]byte(doc), pulseyaml.LoadOptions{
		GraphID: "seeds",
		ObserverFor: func(string) pulse.Observer {
			t.Error("Analyze 不应调用 ObserverFor")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("分析不取 env/file/context，也不判断 literal 值：%v", err)
	}
	if len(top.Problems) != 0 || len(top.Graph.InPorts) != 4 {
		t.Fatalf("seed 是静态入口，不是此次已取得的值：%+v", top)
	}
	for _, k := range top.Nodes[0].Requires {
		if k.From.Kind != pulse.SourceInput {
			t.Fatalf("seed 声明应成为静态来源 input：%+v", k)
		}
	}
}

// 先从同一份计划取得两次输入，再运行：第二次 Inputs 不能覆盖第一次的值。
// 这里钉住运行输入容器与字符串值的隔离，不承诺替宿主深拷贝指针 / map 的对象内容。
func TestAnalyzeSeedPlanInputsStayIsolated(t *testing.T) {
	const doc = `
seeds:
  - key: {name: input, type: string}
    from: {kind: context, key: caller}
nodes:
  - id: read
    uses: read
    requires: [{name: input, type: string}]
`
	reg := pulse.NewRegistry()
	key := pulse.NewKey[string]("input")
	pulse.MustRegisterKey(reg, key)
	var seen []string
	reg.MustRegister("read", func(rc *pulse.RunCtx) error {
		v, err := pulse.Get(rc, key)
		if err == nil {
			seen = append(seen, v)
		}
		return err
	})
	opts := pulseyaml.LoadOptions{GraphID: "isolation"}
	before, err := pulseyaml.Analyze([]byte(doc), opts)
	if err != nil {
		t.Fatal(err)
	}
	g, plan, err := pulseyaml.Load([]byte(doc), reg, opts)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	get := func(value string) pulse.Inputs {
		t.Helper()
		in, err := plan.Inputs(func(from pulseyaml.SeedFrom) (any, error) {
			calls++
			if from.Kind != "context" || from.Key != "caller" {
				t.Fatalf("意外的输入计划：%+v", from)
			}
			return value, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	first, second := get("first"), get("second")
	if calls != 2 {
		t.Fatalf("每次 Inputs 应独立取值：resolve 调用数=%d", calls)
	}
	for _, in := range []pulse.Inputs{first, second, first} {
		if err := g.Run(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(seen, []string{"first", "second", "first"}) {
		t.Fatalf("两份输入应独立，复用第一份也仍是 first：%v", seen)
	}
	if !reflect.DeepEqual(before, g.Topology()) {
		t.Fatal("Inputs / 运行不应改变静态拓扑")
	}
}
