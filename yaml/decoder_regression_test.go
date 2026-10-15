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

// 配置结构严格对账；from.value 的业务键不属于配置字段。
func TestDecoderRejectsUnknownConfigurationFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		doc   string
		field string
	}{
		{"document", "descripton: typo\nnodes: [{id: n, uses: noop}]", "descripton"},
		{"graph", "graphs: {unused: {descripton: typo, nodes: [{id: leaf, uses: noop}]}}\nnodes: [{id: n, uses: noop}]", "descripton"},
		{"node", "nodes: [{id: n, uses: noop, timeuot: 30s}]", "timeuot"},
		{"nested-node", "graphs: {unused: {nodes: [{id: leaf, uses: noop, timeuot: 30s}]}}\nnodes: [{id: n, uses: noop}]", "timeuot"},
		{"retry", "nodes: [{id: n, uses: noop, retry: {attmpts: 3}}]", "attmpts"},
		{"seed", "seeds: [{key: {name: input, type: string}, from: {value: x}, skp: true}]\nnodes: [{id: n, uses: noop}]", "skp"},
		{"key", "seeds: [{key: {name: input, type: string, typ: typo}, from: {value: x}}]\nnodes: [{id: n, uses: noop}]", "typ"},
		{"from", "seeds: [{key: {name: input, type: string}, from: {value: x, vlue: typo}}]\nnodes: [{id: n, uses: noop}]", "vlue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertDecoderRejects(t, tc.doc, "field "+tc.field+" not found")
		})
	}
}

// 第二份文档只用合法字段，独立验证文档数检查，而非依靠 KnownFields 报错。
func TestDecoderRejectsExtraDocuments(t *testing.T) {
	const first = "version: 1\nnodes: [{id: first, uses: noop}]\n"
	for _, tc := range []struct {
		name string
		tail string
		want string
	}{
		{"valid", "---\nversion: 1\nnodes: [{id: second, uses: noop}]\n", "multiple documents"},
		{"unsupported-version", "---\nversion: 999\nnodes: [{id: second, uses: noop}]\n", "multiple documents"},
		{"empty", "---\n", "multiple documents"},
		{"comment-only", "---\n# empty second document\n", "multiple documents"},
		{"null", "---\nnull\n", "multiple documents"},
		{"after-end-marker", "...\n---\n...\n", "multiple documents"},
		{"empty-before-third", "---\n---\nnodes: [{id: third, uses: noop}]\n", "multiple documents"},
		{"malformed-second", "---\nnodes: [\n", "decode:"},
		{"malformed-tail", "...\ntrailing: [\n", "decode:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertDecoderRejects(t, first+tc.tail, tc.want)
		})
	}
}

func assertDecoderRejects(t *testing.T, doc, want string) {
	t.Helper()
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("input"))
	reg.MustRegister("noop", func(*pulse.RunCtx) error { return nil })
	opts := pulseyaml.LoadOptions{GraphID: "decoder"}
	g, plan, le := pulseyaml.Load([]byte(doc), reg, opts)
	top, ae := pulseyaml.Analyze([]byte(doc), opts)
	if le == nil || ae == nil {
		t.Fatalf("两个入口都应拒绝：Load=%v Analyze=%v", le, ae)
	}
	if g != nil || plan != nil || top != nil {
		t.Fatal("解码失败不应返回部分图、SeedPlan 或拓扑")
	}
	if le.Error() != ae.Error() || !strings.HasPrefix(le.Error(), "pulse/yaml: decode:") || !strings.Contains(le.Error(), want) {
		t.Fatalf("两个入口应给出同源的 %q 解码错误：Load=%v Analyze=%v", want, le, ae)
	}
}

func TestDecoderAcceptsSingleDocumentEndings(t *testing.T) {
	const first = "version: 1\nnodes: [{id: first, uses: noop}]\n"
	reg := pulse.NewRegistry()
	reg.MustRegister("noop", func(*pulse.RunCtx) error { return nil })
	for _, tail := range []string{"", "\n  \n", "# final comment\n", "...\n", "...\n# final comment\n\n"} {
		t.Run(fmt.Sprintf("%q", tail), func(t *testing.T) {
			opts := pulseyaml.LoadOptions{GraphID: "single"}
			g, _, err := pulseyaml.Load([]byte("---\n"+first+tail), reg, opts)
			if err != nil {
				t.Fatal(err)
			}
			top, err := pulseyaml.Analyze([]byte("---\n"+first+tail), opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(top.Nodes) != 1 || top.Nodes[0].ID != "first" || !reflect.DeepEqual(top, g.Topology()) {
				t.Fatalf("单文档应保留 first 且拓扑一致：%+v", top)
			}
		})
	}
}

func TestDecoderPreservesSeedValuesAndKinds(t *testing.T) {
	reg := pulse.NewRegistry()
	payload := pulse.NewKey[any]("payload")
	custom := pulse.NewKey[string]("custom")
	pulse.MustRegisterKey(reg, payload)
	pulse.MustRegisterKey(reg, custom)
	want := map[string]any{
		"timeuot": "business value",
		"retry":   map[string]any{"attmpts": 3},
		"items":   []any{map[string]any{"custom-field": true}},
	}
	calls := 0
	reg.MustRegister("check", func(rc *pulse.RunCtx) error {
		calls++
		v, err := pulse.Get(rc, payload)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(v, want) {
			t.Errorf("业务值不应被限制或改写：%#v", v)
		}
		v2, err := pulse.Get(rc, custom)
		if err == nil && v2 != "resolved" {
			t.Errorf("自定义来源的值 = %q", v2)
		}
		return err
	})
	const doc = `
observer: host-hint
seeds:
  - key: {name: payload, type: "interface {}"}
    from:
      value: {timeuot: business value, retry: {attmpts: 3}, items: [{custom-field: true}]}
  - key: {name: custom, type: string}
    from: {kind: plugin-x, env: META_ENV, path: sample.json, key: request.user, value: {selector: item-id}}
nodes:
  - id: check
    uses: check
    requires: [{name: payload, type: "interface {}"}, {name: custom, type: string}]
`
	opts := pulseyaml.LoadOptions{GraphID: "business"}
	g, plan, le := pulseyaml.Load([]byte(doc), reg, opts)
	_, ae := pulseyaml.Analyze([]byte(doc), opts)
	if le != nil || ae != nil {
		t.Fatalf("两个入口都应接受业务值与自定义来源：Load=%v Analyze=%v", le, ae)
	}
	if calls != 0 || len(plan.Entries) != 2 || !reflect.DeepEqual(plan.Entries[0].From.Value, want) {
		t.Fatalf("装配应保留原始计划且不执行工厂：calls=%d plan=%+v", calls, plan)
	}
	resolved := 0
	in, err := plan.Inputs(func(from pulseyaml.SeedFrom) (any, error) {
		resolved++
		wantFrom := pulseyaml.SeedFrom{Kind: "plugin-x", Env: "META_ENV", Path: "sample.json", Key: "request.user", Value: map[string]any{"selector": "item-id"}}
		if !reflect.DeepEqual(from, wantFrom) {
			t.Errorf("resolver 应收到完整的自定义来源：%+v", from)
		}
		return "resolved", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if resolved != 1 || calls != 1 {
		t.Fatalf("resolver=%d body=%d，均应仅调用一次", resolved, calls)
	}
}

// timeout <= 0、attempts <= 0、delay <= 0 仍按已文档化的默认语义运行。
func TestDecoderPreservesAspectDefaults(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fields   string
		attempts int
		deadline bool
	}{
		{"omitted", "", 1, false},
		{"zero", "timeout: 0s, retry: {attempts: 0, delay: 0s},", 1, false},
		{"negative", "timeout: -1s, retry: {attempts: -2, delay: -1s},", 1, false},
		{"positive", "timeout: 30s, retry: {attempts: 3, delay: 0s},", 3, true},
		{"negative-delay", "retry: {attempts: 2, delay: -1s},", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := pulse.NewRegistry()
			attempts := 0
			failure := errors.New("body failed")
			reg.MustRegister("fail", func(rc *pulse.RunCtx) error {
				attempts++
				if _, deadline := rc.Context().Deadline(); deadline != tc.deadline {
					t.Errorf("deadline=%v，want %v", deadline, tc.deadline)
				}
				return failure
			})
			doc := []byte("nodes: [{id: n, uses: fail, " + tc.fields + "}]\n")
			opts := pulseyaml.LoadOptions{GraphID: "defaults"}
			g, _, err := pulseyaml.Load(doc, reg, opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pulseyaml.Analyze(doc, opts); err != nil {
				t.Fatal(err)
			}
			if err := g.Run(context.Background(), pulse.Inputs{}); !errors.Is(err, failure) || attempts != tc.attempts {
				t.Fatalf("attempts=%d，want %d；运行应保留业务错误：%v", attempts, tc.attempts, err)
			}
		})
	}
}
