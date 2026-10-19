package yaml_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

// 根 seed 与普通节点或挂载节点的产出冲突，都应先拒绝，再决定观察出口。
func TestRootSeedConflictPrecedesObservers(t *testing.T) {
	for _, seed := range []struct {
		name string
		body string
	}{
		{"literal", "from: {kind: literal, value: v}"},
		{"skip", "skip: true"},
		{"custom", "from: {kind: plugin-x, value: {selector: item-id}}"},
	} {
		for _, source := range []struct {
			name string
			key  string
			node string
		}{
			{"ordinary", "sv.in", "producer"},
			{"mount-output", "sv.left", "left"},
		} {
			t.Run(seed.name+"/"+source.name, func(t *testing.T) {
				var got []string
				reg := staticReg(t, &got)
				reg.MustRegister("root-source", func(*pulse.RunCtx) error {
					t.Error("装配不应执行工厂")
					return nil
				})
				doc := strings.Replace(staticDoc, "from: {kind: literal, value: \"v\"}", seed.body, 1)
				doc = strings.Replace(doc, "key: {name: sv.in,", "key: {name: "+source.key+",", 1)
				if source.name == "ordinary" {
					doc += "  - {id: producer, uses: root-source, provides: [{name: sv.in, type: string}]}\n"
				}
				calls := 0
				opts := pulseyaml.LoadOptions{
					GraphID: "root-conflict",
					ObserverFor: func(string) pulse.Observer {
						calls++
						return nil
					},
				}
				g, plan, le := pulseyaml.Load([]byte(doc), reg, opts)
				top, ae := pulseyaml.Analyze([]byte(doc), opts)
				if !errors.Is(le, pulse.ErrDuplicateSource) || !errors.Is(ae, pulse.ErrDuplicateSource) {
					t.Fatalf("两个入口都应包装 ErrDuplicateSource：Load=%v Analyze=%v", le, ae)
				}
				if le.Error() != ae.Error() || !strings.HasPrefix(le.Error(), "pulse/yaml: seeds:") || !strings.Contains(le.Error(), `"`+source.key+`"`) || !strings.Contains(le.Error(), `node "`+source.node+`"`) {
					t.Fatalf("根 seed 诊断应同源并点名键与生产者：Load=%v Analyze=%v", le, ae)
				}
				if calls != 0 || g != nil || plan != nil || top != nil {
					t.Fatalf("预检失败应无部分产物且不调用 ObserverFor：calls=%d graph=%v plan=%v top=%v", calls, g, plan, top)
				}
			})
		}
	}
}

// 重复根 seed 仍归这次 Inputs 判断，不因未引用模板的固定绑定预检提前拒绝。
func TestRootDuplicateSeedRemainsInputsCheck(t *testing.T) {
	for _, skip := range []bool{false, true} {
		t.Run(fmt.Sprintf("skip=%t", skip), func(t *testing.T) {
			reg := pulse.NewRegistry()
			pulse.MustRegisterKey(reg, pulse.NewKey[int]("flag"))
			reg.MustRegister("f", func(*pulse.RunCtx) error { t.Error("装配不应运行工厂"); return nil })
			seed := "from: {value: 1}"
			if skip {
				seed = "skip: true"
			}
			doc := fmt.Sprintf(`
seeds:
  - {key: {name: flag, type: int}, %s}
  - {key: {name: flag, type: int}, %s}
graphs:
  unused:
    seeds: [{key: {name: flag, type: int}, skip: true}]
    nodes: [{id: work, uses: f, requires: [{name: flag, type: int}]}]
nodes: [{id: root, uses: f}]
`, seed, seed)
			opts := pulseyaml.LoadOptions{GraphID: "root-duplicate"}
			_, plan, err := pulseyaml.Load([]byte(doc), reg, opts)
			if err != nil || len(plan.Entries) != 2 {
				t.Fatalf("Load 保留两条根计划，不提前判本轮重复输入：plan=%+v err=%v", plan, err)
			}
			if _, err := pulseyaml.Analyze([]byte(doc), opts); err != nil {
				t.Fatalf("Analyze 不应提前判本轮输入：%v", err)
			}
			if _, err := plan.Inputs(nil); err == nil || !strings.Contains(err.Error(), "already set in this Inputs") {
				t.Fatalf("Inputs 必须拒绝同键第二次写入：%v", err)
			}
		})
	}
}

// 同一 spec 的两处挂载及其嵌套各建一次出口；未引用模板仍可保留未知外部来源。
func TestRootSeedPrecheckPreservesValidMounts(t *testing.T) {
	for _, seed := range []struct {
		name string
		body string
	}{
		{"literal", "from: {kind: literal, value: v}"},
		{"skip", "skip: true"},
		{"custom", "from: {kind: plugin-x, value: {selector: item-id}}"},
	} {
		t.Run(seed.name, func(t *testing.T) {
			var got []string
			reg := staticReg(t, &got)
			pulse.MustRegisterKey(reg, pulse.NewKey[string]("future.in"))
			doc := strings.Replace(staticDoc, "from: {kind: literal, value: \"v\"}", seed.body, 1)
			doc = strings.Replace(doc, "graphs:\n", "graphs:\n  unused:\n    nodes:\n      - {id: template, uses: sv.pass, requires: [{name: future.in, type: string}]}\n", 1)
			paths := map[string]int{}
			opts := pulseyaml.LoadOptions{
				GraphID: "valid-mounts",
				ObserverFor: func(path string) pulse.Observer {
					paths[path]++
					return nil
				},
			}
			g, plan, err := pulseyaml.Load([]byte(doc), reg, opts)
			if err != nil {
				t.Fatal(err)
			}
			wantPaths := map[string]int{"left": 1, "left/toInner": 1, "right": 1, "right/toInner": 1}
			if !reflect.DeepEqual(paths, wantPaths) || len(got) != 0 {
				t.Fatalf("Load 应只按挂载位置建出口，不运行工厂：paths=%v got=%v", paths, got)
			}
			top, err := pulseyaml.Analyze([]byte(doc), opts)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(paths, wantPaths) || !reflect.DeepEqual(top, g.Topology()) {
				t.Fatal("Analyze 应保留完整静态形状且不再调用 ObserverFor")
			}
			resolved := 0
			in, err := plan.Inputs(func(from pulseyaml.SeedFrom) (any, error) {
				resolved++
				if from.Kind != "plugin-x" {
					t.Errorf("只有自定义根 seed 应调用 resolver：%+v", from)
				}
				return "v", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := g.Run(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			wantResolved := 0
			if seed.name == "custom" {
				wantResolved = 1
			}
			var want []string
			if seed.name != "skip" {
				want = []string{"v", "v"}
			}
			if resolved != wantResolved || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(paths, wantPaths) {
				t.Fatalf("每处应收到本轮输入或正常跳过：resolver=%d got=%v paths=%v", resolved, got, paths)
			}
		})
	}
}
