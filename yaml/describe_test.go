package yaml_test

import (
	"testing"

	"github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

// YAML 三处 `description` 各自落到对应对象上：根图在文档顶层，子图在 `graphs.<名>`，
// 节点在 `nodes[]` 里。这条只钉**根图**——子图与节点那两处要等 `Topology()`
// 才读得回来，那是同一阶段的第二步。
func TestLoadRootDescription(t *testing.T) {
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("d.in"))
	reg.MustRegister("f", func(rc *pulse.RunCtx) error { return nil })

	const doc = `
version: 1
description: 主流程
seeds:
  - key: {name: d.in, type: string}
    from: {kind: literal, value: "v"}
nodes:
  - id: step
    uses: f
    requires: [{name: d.in, type: string}]
`
	g, _, err := pulseyaml.Load([]byte(doc), reg, pulseyaml.LoadOptions{GraphID: "P"})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Description(); got != "主流程" {
		t.Fatalf("根图描述 = %q，want 主流程", got)
	}

	// 没写就是空串（不是「没有描述」这类第三态）。
	g2, _, err := pulseyaml.Load([]byte(`
version: 1
seeds:
  - key: {name: d.in, type: string}
    from: {kind: literal, value: "v"}
nodes:
  - id: step
    uses: f
    requires: [{name: d.in, type: string}]
`), reg, pulseyaml.LoadOptions{GraphID: "P2"})
	if err != nil {
		t.Fatal(err)
	}
	if got := g2.Description(); got != "" {
		t.Fatalf("没写 description 时应该是 \"\"，got %q", got)
	}
}
