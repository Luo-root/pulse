package yaml_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"

	"github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

func TestConfigurationIntegersRejectInvalidScalars(t *testing.T) {
	for _, value := range []string{
		"1.9", "0.9", "-0.9", "2.9", "1.0", "2e0", ".nan", ".inf",
		`"2"`, "true", "[]", "{}", "9223372036854775808", "-9223372036854775809",
		"18446744073709551616",
	} {
		for _, field := range []struct {
			name string
			doc  string
		}{
			{"version", "version: %s\nnodes: [{id: n, uses: noop}]\n"},
			{"root-retry", "nodes: [{id: n, uses: noop, retry: {attempts: %s}}]\n"},
			{"unused-template-retry", "graphs: {unused: {nodes: [{id: leaf, uses: noop, retry: {attempts: %s}}]}}\nnodes: [{id: n, uses: noop}]\n"},
			{"mounted-template-retry", "graphs: {child: {nodes: [{id: leaf, uses: noop, requires: [{name: input, type: string}], retry: {attempts: %s}}]}}\nnodes: [{id: mount, graph: child, in: {input: input}}]\n"},
			{"mount-retry", "graphs: {child: {nodes: [{id: leaf, uses: noop, requires: [{name: input, type: string}]}]}}\nnodes: [{id: mount, graph: child, in: {input: input}, retry: {attempts: %s}}]\n"},
		} {
			t.Run(field.name+"/"+value, func(t *testing.T) {
				assertDecoderRejects(t, fmt.Sprintf(field.doc, value), "integer")
			})
		}
	}
}

// YAML 别名与 merge 不得绕过整数检查；业务值可作为别名目标，但自身仍不受配置限制。
func TestConfigurationIntegersCheckAliasesAndMerges(t *testing.T) {
	for _, doc := range []string{
		"seeds: [{key: {name: input, type: string}, from: {value: &fraction 1.9}}]\nversion: *fraction\nnodes: [{id: n, uses: noop}]\n",
		"seeds: [{key: {name: input, type: string}, from: {value: &retry {attempts: 2.9}}}]\nnodes: [{id: n, uses: noop, retry: {<<: *retry}}]\n",
		"seeds: [{key: {name: input, type: string}, from: {value: &spec {nodes: [{id: leaf, uses: noop, retry: {attempts: 2.9}}]}}}]\ngraphs: {unused: {<<: *spec}}\nnodes: [{id: n, uses: noop}]\n",
	} {
		assertDecoderRejects(t, doc, "integer")
	}
}

func TestConfigurationIntegersPreserveDefaultsAndBudgets(t *testing.T) {
	for _, version := range []string{"", "version: 0\n", "version: 1\n", "version: null\n", "version: 0x1\n"} {
		for _, retry := range []struct {
			fields string
			count  int
		}{
			{"", 1},
			{"retry: {}", 1},
			{"retry: {attempts: null}", 1},
			{"retry: {attempts: 0}", 1},
			{"retry: {attempts: -2}", 1},
			{"retry: {attempts: 1}", 1},
			{"retry: {attempts: 2}", 2},
		} {
			t.Run(fmt.Sprintf("%s/%s", version, retry.fields), func(t *testing.T) {
				reg := pulse.NewRegistry()
				calls := 0
				failure := errors.New("business failure")
				reg.MustRegister("fail", func(*pulse.RunCtx) error {
					calls++
					return failure
				})
				doc := []byte(version + "nodes: [{id: n, uses: fail, " + retry.fields + "}]\n")
				opts := pulseyaml.LoadOptions{GraphID: "integer-defaults"}
				g, _, err := pulseyaml.Load(doc, reg, opts)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pulseyaml.Analyze(doc, opts); err != nil {
					t.Fatal(err)
				}
				if err := g.Run(context.Background(), pulse.Inputs{}); !errors.Is(err, failure) || calls != retry.count {
					t.Fatalf("calls=%d, want %d; err=%v", calls, retry.count, err)
				}
			})
		}
	}
}

func TestConfigurationIntegersAcceptIntRange(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, attempts := range []int{maxInt, -maxInt - 1} {
		reg := pulse.NewRegistry()
		reg.MustRegister("noop", func(*pulse.RunCtx) error { return nil })
		doc := []byte("nodes: [{id: n, uses: noop, retry: {attempts: " + strconv.Itoa(attempts) + "}}]\n")
		opts := pulseyaml.LoadOptions{GraphID: "integer-range"}
		if _, _, err := pulseyaml.Load(doc, reg, opts); err != nil {
			t.Fatal(err)
		}
		if _, err := pulseyaml.Analyze(doc, opts); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfigurationIntegerChecksPreserveBusinessValues(t *testing.T) {
	reg := pulse.NewRegistry()
	payload := pulse.NewKey[any]("payload")
	pulse.MustRegisterKey(reg, payload)
	want := map[string]any{
		"version": 1.9,
		"nodes":   []any{map[string]any{"retry": map[string]any{"attempts": 2.9}}},
	}
	reg.MustRegister("check", func(rc *pulse.RunCtx) error {
		got, err := pulse.Get(rc, payload)
		if err == nil && !reflect.DeepEqual(got, want) {
			t.Errorf("business value changed: %#v", got)
		}
		return err
	})
	const doc = `
seeds:
  - key: {name: payload, type: "interface {}"}
    from:
      value: {version: 1.9, nodes: [{retry: {attempts: 2.9}}]}
nodes:
  - id: check
    uses: check
    requires: [{name: payload, type: "interface {}"}]
`
	opts := pulseyaml.LoadOptions{GraphID: "business-fractions"}
	g, plan, err := pulseyaml.Load([]byte(doc), reg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pulseyaml.Analyze([]byte(doc), opts); err != nil {
		t.Fatal(err)
	}
	in, err := plan.Inputs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
}
