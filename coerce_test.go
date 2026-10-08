package pulse

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type coerceTag string

type coerceDoc struct {
	Name string
}

func TestCoerceSeedValue(t *testing.T) {
	target := func(v any) reflect.Type { return reflect.TypeOf(v) }
	cases := []struct {
		name    string
		in      any
		target  reflect.Type
		want    any
		wantErr bool
	}{
		// 已可赋值：原样返回
		{"assignable slice", []string{"a"}, target([]string{}), []string{"a"}, false},
		{"assignable scalar", "x", target(""), "x", false},

		// 泛型容器按目标类型对齐
		{"[]any to []string", []any{"a", "b"}, target([]string{}), []string{"a", "b"}, false},
		{"[]any to []int64", []any{1, 2}, target([]int64{}), []int64{1, 2}, false},
		{"[]any to []int8", []any{1, 2}, target([]int8{}), []int8{1, 2}, false},
		{"nested []any", []any{[]any{"x"}}, target([][]string{}), [][]string{{"x"}}, false},
		{"map to typed map", map[string]any{"a": 1}, target(map[string]int{}), map[string]int{"a": 1}, false},
		{"named string elem", []any{"a"}, target([]coerceTag{}), []coerceTag{"a"}, false},
		{"null elem to zero", []any{nil}, target([]string{}), []string{""}, false},
		{"scalar int to int64", 1, target(int64(0)), int64(1), false},
		{"map value int to float", map[string]any{"a": 1}, target(map[string]float64{}), map[string]float64{"a": 1}, false},
		{"array length match", []any{1, 2}, target([2]int{}), [2]int{1, 2}, false},

		// 拒绝：不做静默错值的转换
		{"float into int", []any{1.5}, target([]int{}), nil, true},
		{"string into int", []any{"a"}, target([]int{}), nil, true},
		{"int into string", []any{1}, target([]string{}), nil, true},
		{"overflow int8", []any{200}, target([]int8{}), nil, true},
		{"negative into uint", []any{-1}, target([]uint{}), nil, true},
		{"map into slice", map[string]any{"a": 1}, target([]string{}), nil, true},
		{"struct elem needs resolve", []any{map[string]any{"name": "x"}}, target([]coerceDoc{}), nil, true},
		{"array length mismatch", []any{1}, target([2]int{}), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := coerceSeedValue(tc.in, tc.target)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %#v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if reflect.TypeOf(got) != tc.target {
				t.Fatalf("result type = %v, want %v", reflect.TypeOf(got), tc.target)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

// 端到端：SeedByName 收下 []any（YAML 解出的形状），节点按登记类型读回。
func TestSeedByNameCoercesGenericContainers(t *testing.T) {
	r := NewRegistry()
	docs := NewKey[[]string]("demo.docs")
	out := NewKey[string]("demo.out")
	MustRegisterKey(r, docs)
	MustRegisterKey(r, out)

	g := mustNew(t, context.Background(), "test")
	if err := SeedByName(g, r, "demo.docs", "[]string", []any{"a", "b"}); err != nil {
		t.Fatal(err)
	}

	var seen string
	if err := g.Add(NewNode("join", Requires(docs), Provides(out), func(rc *RunCtx) error {
		v, err := Get(rc, docs) // 类型断言失败即说明写入的不是 []string
		if err != nil {
			return err
		}
		seen = strings.Join(v, ",")
		return Set(rc, out, seen)
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if seen != "a,b" {
		t.Fatalf("node saw %q, want \"a,b\"", seen)
	}
}

// 结构性元素（map→struct）不做猜测：报错里带上不可转换的原因。
func TestSeedByNameRejectsStructuralElement(t *testing.T) {
	r := NewRegistry()
	MustRegisterKey(r, NewKey[[]coerceDoc]("demo.docs"))
	g := mustNew(t, context.Background(), "test")
	err := SeedByName(g, r, "demo.docs", "[]pulse.coerceDoc", []any{map[string]any{"name": "x"}})
	if err == nil {
		t.Fatal("want error for map element into struct")
	}
	if !strings.Contains(err.Error(), "not assignable") || !strings.Contains(err.Error(), "element 0") {
		t.Fatalf("error should name the element and the mismatch: %v", err)
	}
}
