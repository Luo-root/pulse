package yaml_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/kernel/flow"
	flowyaml "github.com/Luo-root/pulse/kernel/flow/yaml"
)

func TestLoadLinearThreeNodes(t *testing.T) {
	in := flow.NewKey[string]("demo.user_input")
	q := flow.NewKey[string]("demo.query_text")
	docs := flow.NewKey[string]("demo.context_docs")
	out := flow.NewKey[string]("demo.final_text")

	reg := flow.NewRegistry()
	flow.MustRegisterKey(reg, in)
	flow.MustRegisterKey(reg, q)
	flow.MustRegisterKey(reg, docs)
	flow.MustRegisterKey(reg, out)

	reg.MustRegister("demo.extract_text", func(rc *flow.RunCtx) error {
		v, err := flow.Get(rc, in)
		if err != nil {
			return err
		}
		return flow.Set(rc, q, strings.TrimSpace(v))
	})
	reg.MustRegister("demo.retrieve", func(rc *flow.RunCtx) error {
		v, err := flow.Get(rc, q)
		if err != nil {
			return err
		}
		return flow.Set(rc, docs, "doc:"+v)
	})
	var final string
	reg.MustRegister("demo.answer", func(rc *flow.RunCtx) error {
		d, err := flow.Get(rc, docs)
		if err != nil {
			return err
		}
		final = "ans:" + d
		return flow.Set(rc, out, final)
	})

	doc := []byte(`
version: 1
seeds:
  - key: { name: demo.user_input, type: string }
    from: { kind: literal, value: "  hello  " }
nodes:
  - id: extract_text
    uses: demo.extract_text
    requires: [{ name: demo.user_input, type: string }]
    provides: [{ name: demo.query_text, type: string }]
  - id: retrieve
    uses: demo.retrieve
    requires: [{ name: demo.query_text, type: string }]
    provides: [{ name: demo.context_docs, type: string }]
  - id: answer
    uses: demo.answer
    requires:
      - { name: demo.user_input, type: string }
      - { name: demo.query_text, type: string }
      - { name: demo.context_docs, type: string }
    provides: [{ name: demo.final_text, type: string }]
`)

	g, plan, err := flowyaml.Load(doc, reg, flowyaml.LoadOptions{Context: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(g, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if final != "ans:doc:hello" {
		t.Fatalf("final = %q", final)
	}
}

func TestLoadTypeMismatch(t *testing.T) {
	reg := flow.NewRegistry()
	flow.MustRegisterKey(reg, flow.NewKey[string]("demo.q"))
	reg.MustRegister("n", func(*flow.RunCtx) error { return nil })
	doc := []byte(`
nodes:
  - id: n
    uses: n
    requires: [{ name: demo.q, type: int }]
    provides: []
`)
	_, _, err := flowyaml.Load(doc, reg, flowyaml.LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "type") {
		t.Fatalf("want type error, got %v", err)
	}
}

func TestLoadMissingUses(t *testing.T) {
	reg := flow.NewRegistry()
	doc := []byte(`
nodes:
  - id: n
    requires: []
    provides: []
`)
	_, _, err := flowyaml.Load(doc, reg, flowyaml.LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "uses") {
		t.Fatalf("want uses error, got %v", err)
	}
}

func TestLoadUnknownFactory(t *testing.T) {
	reg := flow.NewRegistry()
	doc := []byte(`
nodes:
  - id: n
    uses: missing
    requires: []
    provides: []
`)
	_, _, err := flowyaml.Load(doc, reg, flowyaml.LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "unknown factory") {
		t.Fatalf("want unknown factory, got %v", err)
	}
}

func TestLoadTimeoutRetryOrderCompiles(t *testing.T) {
	// 只验证 timeout+retry 能装上并跑通空图节点（无边）。
	reg := flow.NewRegistry()
	out := flow.NewKey[string]("demo.out")
	flow.MustRegisterKey(reg, out)
	reg.MustRegister("ok", func(rc *flow.RunCtx) error {
		return flow.Set(rc, out, "x")
	})
	doc := []byte(`
nodes:
  - id: n
    uses: ok
    requires: []
    provides: [{ name: demo.out, type: string }]
    timeout: 2s
    retry: { attempts: 2, delay: 1ms }
`)
	g, plan, err := flowyaml.Load(doc, reg, flowyaml.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = plan
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyResolveAndSkip(t *testing.T) {
	reg := flow.NewRegistry()
	a := flow.NewKey[string]("demo.a")
	b := flow.NewKey[string]("demo.b")
	flow.MustRegisterKey(reg, a)
	flow.MustRegisterKey(reg, b)
	reg.MustRegister("pass", func(rc *flow.RunCtx) error {
		if _, err := flow.Get(rc, a); err != nil {
			return err
		}
		// b 被 SkipSeed：下游不应依赖它；本节点只读 a
		return nil
	})
	doc := []byte(`
seeds:
  - key: { name: demo.a, type: string }
    from: { kind: env, env: PULSE_YAML_TEST_A }
  - key: { name: demo.b, type: string }
    skip: true
nodes:
  - id: n
    uses: pass
    requires: [{ name: demo.a, type: string }]
    provides: []
`)
	g, plan, err := flowyaml.Load(doc, reg, flowyaml.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(g, func(from flowyaml.SeedFrom) (any, error) {
		if from.Kind != "env" || from.Env != "PULSE_YAML_TEST_A" {
			t.Fatalf("unexpected from: %+v", from)
		}
		return "from-env", nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFileAndBadVersion(t *testing.T) {
	reg := flow.NewRegistry()
	out := flow.NewKey[string]("demo.out")
	flow.MustRegisterKey(reg, out)
	reg.MustRegister("ok", func(rc *flow.RunCtx) error {
		return flow.Set(rc, out, "x")
	})
	dir := t.TempDir()
	path := dir + "/g.yaml"
	body := []byte(`
version: 1
nodes:
  - id: n
    uses: ok
    requires: []
    provides: [{ name: demo.out, type: string }]
`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	g, plan, err := flowyaml.LoadFile(path, reg, flowyaml.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = plan
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	_, _, err = flowyaml.Load([]byte("version: 99\nnodes: [{id: n, uses: ok, requires: [], provides: []}]"), reg, flowyaml.LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("want version error, got %v", err)
	}
}

func TestLoadYAMLTimeoutFires(t *testing.T) {
	reg := flow.NewRegistry()
	in := flow.NewKey[string]("demo.wait")
	out := flow.NewKey[string]("demo.out")
	flow.MustRegisterKey(reg, in)
	flow.MustRegisterKey(reg, out)
	reg.MustRegister("blocked", func(rc *flow.RunCtx) error {
		t.Fatal("should not run")
		return nil
	})
	doc := []byte(`
nodes:
  - id: n
    uses: blocked
    requires: [{ name: demo.wait, type: string }]
    provides: [{ name: demo.out, type: string }]
    timeout: 30ms
`)
	g, _, err := flowyaml.Load(doc, reg, flowyaml.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// 不 Seed demo.wait → WaitAll 阻塞直到 Timeout
	err = g.Run()
	if err == nil {
		t.Fatal("want timeout error")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("want timeout in error, got %v", err)
	}
}

func TestLoadAddRejectsDuplicateProvide(t *testing.T) {
	reg := flow.NewRegistry()
	k := flow.NewKey[string]("demo.dup")
	flow.MustRegisterKey(reg, k)
	reg.MustRegister("a", func(*flow.RunCtx) error { return nil })
	reg.MustRegister("b", func(*flow.RunCtx) error { return nil })
	doc := []byte(`
nodes:
  - id: n1
    uses: a
    requires: []
    provides: [{ name: demo.dup, type: string }]
  - id: n2
    uses: b
    requires: []
    provides: [{ name: demo.dup, type: string }]
`)
	_, _, err := flowyaml.Load(doc, reg, flowyaml.LoadOptions{})
	if err == nil {
		t.Fatal("want Add duplicate source error")
	}
}
