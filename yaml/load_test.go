package yaml_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

func TestLoadLinearThreeNodes(t *testing.T) {
	in := pulse.NewKey[string]("demo.user_input")
	q := pulse.NewKey[string]("demo.query_text")
	docs := pulse.NewKey[string]("demo.context_docs")
	out := pulse.NewKey[string]("demo.final_text")

	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, in)
	pulse.MustRegisterKey(reg, q)
	pulse.MustRegisterKey(reg, docs)
	pulse.MustRegisterKey(reg, out)

	reg.MustRegister("demo.extract_text", func(rc *pulse.RunCtx) error {
		v, err := pulse.Get(rc, in)
		if err != nil {
			return err
		}
		return pulse.Set(rc, q, strings.TrimSpace(v))
	})
	reg.MustRegister("demo.retrieve", func(rc *pulse.RunCtx) error {
		v, err := pulse.Get(rc, q)
		if err != nil {
			return err
		}
		return pulse.Set(rc, docs, "doc:"+v)
	})
	var final string
	reg.MustRegister("demo.answer", func(rc *pulse.RunCtx) error {
		d, err := pulse.Get(rc, docs)
		if err != nil {
			return err
		}
		final = "ans:" + d
		return pulse.Set(rc, out, final)
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

	g, plan, err := pulseyaml.Load(doc, reg, pulseyaml.LoadOptions{GraphID: "test", Context: context.Background()})
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
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, pulse.NewKey[string]("demo.q"))
	reg.MustRegister("n", func(*pulse.RunCtx) error { return nil })
	doc := []byte(`
nodes:
  - id: n
    uses: n
    requires: [{ name: demo.q, type: int }]
    provides: []
`)
	_, _, err := pulseyaml.Load(doc, reg, pulseyaml.LoadOptions{GraphID: "test"})
	if err == nil || !strings.Contains(err.Error(), "type") {
		t.Fatalf("want type error, got %v", err)
	}
}

func TestLoadMissingUses(t *testing.T) {
	reg := pulse.NewRegistry()
	doc := []byte(`
nodes:
  - id: n
    requires: []
    provides: []
`)
	_, _, err := pulseyaml.Load(doc, reg, pulseyaml.LoadOptions{GraphID: "test"})
	if err == nil || !strings.Contains(err.Error(), "uses") {
		t.Fatalf("want uses error, got %v", err)
	}
}

func TestLoadUnknownFactory(t *testing.T) {
	reg := pulse.NewRegistry()
	doc := []byte(`
nodes:
  - id: n
    uses: missing
    requires: []
    provides: []
`)
	_, _, err := pulseyaml.Load(doc, reg, pulseyaml.LoadOptions{GraphID: "test"})
	if err == nil || !strings.Contains(err.Error(), "unknown factory") {
		t.Fatalf("want unknown factory, got %v", err)
	}
}

func TestLoadTimeoutRetryOrderCompiles(t *testing.T) {
	// 只验证 timeout+retry 能装上并跑通空图节点（无边）。
	reg := pulse.NewRegistry()
	out := pulse.NewKey[string]("demo.out")
	pulse.MustRegisterKey(reg, out)
	reg.MustRegister("ok", func(rc *pulse.RunCtx) error {
		return pulse.Set(rc, out, "x")
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
	g, plan, err := pulseyaml.Load(doc, reg, pulseyaml.LoadOptions{GraphID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	_ = plan
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyResolveAndSkip(t *testing.T) {
	reg := pulse.NewRegistry()
	a := pulse.NewKey[string]("demo.a")
	b := pulse.NewKey[string]("demo.b")
	pulse.MustRegisterKey(reg, a)
	pulse.MustRegisterKey(reg, b)
	reg.MustRegister("pass", func(rc *pulse.RunCtx) error {
		if _, err := pulse.Get(rc, a); err != nil {
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
	g, plan, err := pulseyaml.Load(doc, reg, pulseyaml.LoadOptions{GraphID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(g, func(from pulseyaml.SeedFrom) (any, error) {
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
	reg := pulse.NewRegistry()
	out := pulse.NewKey[string]("demo.out")
	pulse.MustRegisterKey(reg, out)
	reg.MustRegister("ok", func(rc *pulse.RunCtx) error {
		return pulse.Set(rc, out, "x")
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
	g, plan, err := pulseyaml.LoadFile(path, reg, pulseyaml.LoadOptions{GraphID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	_ = plan
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	_, _, err = pulseyaml.Load([]byte("version: 99\nnodes: [{id: n, uses: ok, requires: [], provides: []}]"), reg, pulseyaml.LoadOptions{GraphID: "test"})
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("want version error, got %v", err)
	}
}

func TestLoadYAMLTimeoutFires(t *testing.T) {
	reg := pulse.NewRegistry()
	in := pulse.NewKey[string]("demo.wait")
	out := pulse.NewKey[string]("demo.out")
	pulse.MustRegisterKey(reg, in)
	pulse.MustRegisterKey(reg, out)
	reg.MustRegister("blocked", func(rc *pulse.RunCtx) error {
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
	g, _, err := pulseyaml.Load(doc, reg, pulseyaml.LoadOptions{GraphID: "test"})
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
	reg := pulse.NewRegistry()
	k := pulse.NewKey[string]("demo.dup")
	pulse.MustRegisterKey(reg, k)
	reg.MustRegister("a", func(*pulse.RunCtx) error { return nil })
	reg.MustRegister("b", func(*pulse.RunCtx) error { return nil })
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
	_, _, err := pulseyaml.Load(doc, reg, pulseyaml.LoadOptions{GraphID: "test"})
	if err == nil {
		t.Fatal("want Add duplicate source error")
	}
}

func TestLoadGraphIDRequired(t *testing.T) {
	reg := pulse.NewRegistry()
	doc := []byte(`
nodes:
  - id: n
    uses: missing
    requires: []
    provides: []
`)
	_, _, err := pulseyaml.Load(doc, reg, pulseyaml.LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "graph id is required") {
		t.Fatalf("want graph id required error, got %v", err)
	}
}

// 字面量列表 seed：YAML 解出的 []any 由 SeedByName 对齐成登记的 []string，
// 开箱即用（不需要 resolve）。
func TestApplyLiteralListSeed(t *testing.T) {
	reg := pulse.NewRegistry()
	in := pulse.NewKey[[]string]("demo.in")
	count := pulse.NewKey[int]("demo.count")
	pulse.MustRegisterKey(reg, in)
	pulse.MustRegisterKey(reg, count)
	got := -1
	reg.MustRegister("count", func(rc *pulse.RunCtx) error {
		v, err := pulse.Get(rc, in) // 写入的不是 []string 就会在这里报错
		if err != nil {
			return err
		}
		got = len(v)
		return pulse.Set(rc, count, got)
	})
	doc := []byte(`
version: 1
seeds:
  - key: {name: demo.in, type: "[]string"}
    from: {kind: literal, value: ["a", "b", "c"]}
nodes:
  - id: count
    uses: count
    requires: [{name: demo.in, type: "[]string"}]
    provides: [{name: demo.count, type: int}]
`)
	g, plan, err := pulseyaml.Load(doc, reg, pulseyaml.LoadOptions{GraphID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(g, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Fatalf("node saw %d items, want 3", got)
	}
}

// 结构性元素（map→struct）不做字段猜测：报错指明元素与目标类型，
// 取值应由宿主的 resolve 回调给出。
func TestApplyLiteralStructElementNeedsResolve(t *testing.T) {
	type doc0 struct{ Name string }
	reg := pulse.NewRegistry()
	docs := pulse.NewKey[[]doc0]("demo.docs")
	out := pulse.NewKey[string]("demo.out")
	pulse.MustRegisterKey(reg, docs)
	pulse.MustRegisterKey(reg, out)
	reg.MustRegister("noop", func(rc *pulse.RunCtx) error {
		return pulse.Set(rc, out, "x")
	})
	tag := reflect.TypeOf([]doc0{}).String()
	doc := []byte(fmt.Sprintf(`
version: 1
seeds:
  - key: {name: demo.docs, type: %q}
    from: {kind: literal, value: [{name: x}]}
nodes:
  - id: n
    uses: noop
    requires: []
    provides: [{name: demo.out, type: string}]
`, tag))
	g, plan, err := pulseyaml.Load(doc, reg, pulseyaml.LoadOptions{GraphID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	err = plan.Apply(g, nil)
	if err == nil {
		t.Fatal("want error for map element into struct")
	}
	if !strings.Contains(err.Error(), "element 0") || !strings.Contains(err.Error(), "not assignable") {
		t.Fatalf("error should name the element and the mismatch: %v", err)
	}
}
