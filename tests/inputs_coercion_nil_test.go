package pulse_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	p "github.com/Luo-root/pulse"
	pulseyaml "github.com/Luo-root/pulse/yaml"
)

func inputsCoercionNilOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func inputsCoercionNilGraph(t *testing.T, id string) *p.Graph {
	t.Helper()
	g, err := p.New(id, "")
	inputsCoercionNilOK(t, err)
	return g
}

// 两个公开入口共用转换：直接输入与挂载点固定值都必须保留 nil，且仍然 ready。
func inputsCoercionNilValue[T any](t *testing.T, raw any, want T) {
	t.Helper()
	for _, mode := range []string{"SeedByName", "FixByName"} {
		t.Run(mode, func(t *testing.T) {
			key := p.NewKey[T]("value")
			reg := p.NewRegistry()
			inputsCoercionNilOK(t, p.RegisterKey(reg, key))
			typeTag := reflect.TypeFor[T]().String()
			g := inputsCoercionNilGraph(t, "coercion")
			inputsCoercionNilOK(t, g.DeclareInput(p.NewPort(key)))
			inputsCoercionNilOK(t, g.DeclareOutput(p.NewPort(key)))
			inputsCoercionNilOK(t, g.Add(p.NewNode("read", "", p.Requires(key), nil, func(rc *p.RunCtx) error {
				got, ready, skipped, err := p.TryGet(rc, key)
				if err != nil || !ready || skipped || !reflect.DeepEqual(got, want) {
					return fmt.Errorf("TryGet = (%#v, %t, %t, %v), want (%#v, true, false, nil)", got, ready, skipped, err, want)
				}
				got, err = p.Get(rc, key)
				if err != nil || !reflect.DeepEqual(got, want) {
					return fmt.Errorf("Get = (%#v, %v), want (%#v, nil)", got, err, want)
				}
				return nil
			})))

			var in p.Inputs
			output := key
			if mode == "SeedByName" {
				inputsCoercionNilOK(t, p.SeedByName(&in, reg, key.Name(), typeTag, raw))
			} else {
				bind, err := p.FixByName(reg, key.Name(), typeTag, raw)
				inputsCoercionNilOK(t, err)
				parent := inputsCoercionNilGraph(t, "parent")
				output = p.NewKey[T]("parent.value")
				inputsCoercionNilOK(t, parent.DeclareOutput(p.NewPort(output)))
				inputsCoercionNilOK(t, p.Sub(parent, "child", "", []p.SubBind{bind, p.Out(key, output)}, g))
				g = parent
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			t.Cleanup(cancel)
			e, err := g.NewExecution(ctx, in)
			inputsCoercionNilOK(t, err)
			t.Cleanup(e.Cancel)
			inputsCoercionNilOK(t, e.Start())
			inputsCoercionNilOK(t, e.Wait())
			got, state, err := p.ReadOutput(e, output)
			if err != nil || state != p.SlotStateReady || !reflect.DeepEqual(got, want) {
				t.Fatalf("ReadOutput = (%#v, %q, %v), want (%#v, ready, nil)", got, state, err, want)
			}
		})
	}
}

func TestInputsCoercionPreservesNilContainers(t *testing.T) {
	t.Run("nested-slices", func(t *testing.T) {
		inputsCoercionNilValue(t,
			[]any{[]any(nil), nil, []int(nil), []any{}, []int{}, []any{1, int64(2)}},
			[][]int{nil, nil, nil, {}, {}, {1, 2}})
	})
	t.Run("nested-maps", func(t *testing.T) {
		inputsCoercionNilValue(t,
			[]any{map[string]any(nil), nil, map[string]int(nil), map[string]any{}, map[string]int{}, map[string]any{"count": int64(2)}},
			[]map[string]int{nil, nil, nil, {}, {}, {"count": 2}})
	})
	t.Run("map-values-are-slices", func(t *testing.T) {
		inputsCoercionNilValue(t,
			map[string]any{"nil": []any(nil), "plain": nil, "assignable": []int(nil), "empty": []any{}, "values": []any{1, int64(2)}},
			map[string][]int{"nil": nil, "plain": nil, "assignable": nil, "empty": {}, "values": {1, 2}})
	})
	t.Run("map-values-are-maps", func(t *testing.T) {
		inputsCoercionNilValue(t,
			map[string]any{"nil": map[string]any(nil), "plain": nil, "assignable": map[string]int(nil), "empty": map[string]any{}, "values": map[string]any{"count": 2}},
			map[string]map[string]int{"nil": nil, "plain": nil, "assignable": nil, "empty": {}, "values": {"count": 2}})
	})
	t.Run("deeply-nested-slices", func(t *testing.T) {
		inputsCoercionNilValue(t, []any{[]any{[]any(nil), []any{}}}, [][][]int{{nil, {}}})
	})
	for _, tc := range []struct {
		name string
		raw  any
		want []int
	}{
		{"nil-slice", []any(nil), nil},
		{"assignable-nil-slice", []int(nil), nil},
		{"empty-slice", []any{}, []int{}},
		{"zero-length-array-to-slice", [0]any{}, []int{}},
	} {
		t.Run(tc.name, func(t *testing.T) { inputsCoercionNilValue(t, tc.raw, tc.want) })
	}
	for _, tc := range []struct {
		name string
		raw  any
		want map[string]int
	}{
		{"nil-map", map[string]any(nil), nil},
		{"assignable-nil-map", map[string]int(nil), nil},
		{"empty-map", map[string]any{}, map[string]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) { inputsCoercionNilValue(t, tc.raw, tc.want) })
	}
	t.Run("nil-slice-to-zero-length-array", func(t *testing.T) {
		inputsCoercionNilValue(t, []any(nil), [0]int{})
	})
	t.Run("plain-nil-elements-still-become-zero", func(t *testing.T) {
		inputsCoercionNilValue(t, []any{nil}, []int{0})
	})
	t.Run("plain-nil-map-value-still-becomes-zero", func(t *testing.T) {
		inputsCoercionNilValue(t, map[string]any{"value": nil}, map[string]int{"value": 0})
	})
	t.Run("pointer-target-still-allocates", func(t *testing.T) {
		var want []int
		inputsCoercionNilValue(t, []any(nil), &want)
	})
}

// typed nil 只在支持的容器形状之间保留；不能靠 nil 绕过结构、标量或长度检查。
func inputsCoercionNilReject[T any](t *testing.T, raw any, detail string) {
	t.Helper()
	key := p.NewKey[T]("value")
	reg := p.NewRegistry()
	inputsCoercionNilOK(t, p.RegisterKey(reg, key))
	for _, mode := range []string{"SeedByName", "FixByName"} {
		t.Run(mode, func(t *testing.T) {
			var err error
			if mode == "SeedByName" {
				var in p.Inputs
				err = p.SeedByName(&in, reg, key.Name(), reflect.TypeFor[T]().String(), raw)
			} else {
				_, err = p.FixByName(reg, key.Name(), reflect.TypeFor[T]().String(), raw)
			}
			if err == nil {
				t.Fatalf("accepted %T as %s", raw, reflect.TypeFor[T]())
			}
			for _, fragment := range []string{key.Name(), detail} {
				if !strings.Contains(err.Error(), fragment) {
					t.Fatalf("error %q does not contain %q", err, fragment)
				}
			}
		})
	}
}

type inputsCoercionNilDoc struct {
	Name string
}

func TestInputsCoercionRejectsUnsupportedNilConversions(t *testing.T) {
	t.Run("map-to-slice", func(t *testing.T) {
		inputsCoercionNilReject[[][]int](t, []any{map[string]any(nil)}, "element 0: cannot convert")
	})
	t.Run("slice-to-map", func(t *testing.T) {
		inputsCoercionNilReject[[]map[string]int](t, []any{[]any(nil)}, "element 0: cannot convert")
	})
	t.Run("map-to-struct", func(t *testing.T) {
		inputsCoercionNilReject[[]inputsCoercionNilDoc](t, []any{map[string]any(nil)}, "element 0: cannot convert")
	})
	t.Run("map-to-scalar", func(t *testing.T) {
		inputsCoercionNilReject[[]int](t, []any{map[string]any(nil)}, "element 0: cannot convert")
	})
	t.Run("slice-to-scalar", func(t *testing.T) {
		inputsCoercionNilReject[[]int](t, []any{[]any(nil)}, "element 0: cannot convert")
	})
	t.Run("pointer-to-scalar", func(t *testing.T) {
		inputsCoercionNilReject[[]int](t, []any{(*int)(nil)}, "element 0: cannot convert")
	})
	t.Run("channel-to-slice", func(t *testing.T) {
		inputsCoercionNilReject[[][]int](t, []any{(chan int)(nil)}, "element 0: cannot convert")
	})
	t.Run("nil-slice-to-nonzero-array", func(t *testing.T) {
		inputsCoercionNilReject[[][1]int](t, []any{[]any(nil)}, "element 0: cannot convert")
	})
	t.Run("float-truncation", func(t *testing.T) {
		inputsCoercionNilReject[[]int](t, []any{1.5}, "element 0: cannot convert")
	})
	t.Run("integer-overflow", func(t *testing.T) {
		inputsCoercionNilReject[[]uint8](t, []any{256}, "element 0: cannot convert")
	})
	t.Run("string-to-number", func(t *testing.T) {
		inputsCoercionNilReject[[]int](t, []any{"3"}, "element 0: cannot convert")
	})
	t.Run("host-plain-nil", func(t *testing.T) {
		inputsCoercionNilReject[[]int](t, nil, "nil value")
	})
}

func TestYAMLInputMapKeyCollision(t *testing.T) {
	for _, collision := range []bool{true, false} {
		t.Run(fmt.Sprint("collision=", collision), func(t *testing.T) {
			key := p.NewKey[map[int]string]("mapping")
			reg := p.NewRegistry()
			p.MustRegisterKey(reg, key)
			reg.MustRegister("reader", func(rc *p.RunCtx) error {
				got, err := p.Get(rc, key)
				if err != nil {
					return err
				}
				if len(got) != 2 || got[1] != "secret-integer" || got[2] != "secret-float" {
					return fmt.Errorf("distinct map keys were not preserved: len=%d", len(got))
				}
				return nil
			})
			second := "2.0"
			if collision {
				second = "1.0"
			}
			doc := fmt.Sprintf(`version: 1
seeds:
  - key: {name: mapping, type: 'map[int]string'}
    from: {value: {1: secret-integer, %s: secret-float}}
nodes:
  - id: reader
    uses: reader
    requires: [{name: mapping, type: 'map[int]string'}]
`, second)
			g, plan, err := pulseyaml.Load([]byte(doc), reg, pulseyaml.LoadOptions{GraphID: "map-conversion"})
			if err != nil {
				t.Fatal(err)
			}
			in, err := plan.Inputs(nil)
			if collision {
				if err == nil {
					t.Fatal("YAML seed plan accepted colliding int/float map keys")
				}
				if !strings.Contains(err.Error(), "mapping") || !strings.Contains(err.Error(), "map key") || strings.Contains(err.Error(), "secret-") {
					t.Fatalf("collision error lost key context or leaked values: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := g.Run(context.Background(), in); err != nil {
				t.Fatal(err)
			}
		})
	}
}
