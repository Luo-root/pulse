package pulse

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type inputMapKey string

func TestInputMapKeyCollision(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  any
	}{
		{"mixed-number-types", map[any]any{int(1): "secret-integer", float64(1): "secret-float"}},
		{"integer-float64-rounding", map[int64]string{1 << 53: "secret-even", 1<<53 + 1: "secret-odd"}},
		{"float64-float32-rounding", map[float64]string{1 << 24: "secret-even", 1<<24 + 1: "secret-odd"}},
		{"named-string-types", map[any]any{"private-map-key": "secret-first", inputMapKey("private-map-key"): "secret-second"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			switch test.name {
			case "float64-float32-rounding":
				inputMapCollision[map[float32]string](t, test.raw)
			case "named-string-types":
				inputMapCollision[map[string]string](t, test.raw)
			default:
				inputMapCollision[map[float64]string](t, test.raw)
			}
		})
	}
}

func inputMapCollision[T any](t *testing.T, raw any) {
	t.Helper()
	reg := NewRegistry()
	key := NewKey[T]("collision")
	MustRegisterKey(reg, key)
	tag, _ := reg.TypeTagOf(key.Name())
	var in Inputs
	for _, fixed := range []bool{false, true} {
		var err error
		if fixed {
			_, err = FixByName(reg, key.Name(), tag, raw)
		} else {
			err = SeedByName(&in, reg, key.Name(), tag, raw)
		}
		if err == nil {
			t.Fatalf("fixed=%t: colliding map keys accepted", fixed)
		}
		for _, want := range []string{key.Name(), "map key", reflect.TypeFor[T]().Key().String()} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("fixed=%t: error must name key and target type %q: %v", fixed, want, err)
			}
		}
		if strings.Contains(err.Error(), "secret-") || strings.Contains(err.Error(), "private-map-key") {
			t.Fatalf("business map data leaked into conversion error: %v", err)
		}
	}
	if len(in.entries) != 0 {
		t.Fatal("rejected collision left a partial input entry")
	}
}

func TestInputNestedMapCollisionPrivacy(t *testing.T) {
	reg := NewRegistry()
	key := NewKey[map[string]map[int]string]("mapping")
	MustRegisterKey(reg, key)
	tag, _ := reg.TypeTagOf(key.Name())
	raw := map[string]any{"business-customer-123": map[any]any{int(1): "value-a", float64(1): "value-b"}}
	for _, fixed := range []bool{false, true} {
		var in Inputs
		var err error
		if fixed {
			_, err = FixByName(reg, key.Name(), tag, raw)
		} else {
			err = SeedByName(&in, reg, key.Name(), tag, raw)
		}
		if err == nil || !strings.Contains(err.Error(), "collides") || !strings.Contains(err.Error(), key.Name()) {
			t.Fatalf("fixed=%t: nested collision lost its input context: %v", fixed, err)
		}
		for _, private := range []string{"business-customer-123", "value-a", "value-b"} {
			if strings.Contains(err.Error(), private) {
				t.Fatalf("fixed=%t: nested map error exposed business data: %v", fixed, err)
			}
		}
		if len(in.entries) != 0 {
			t.Fatal("nested map rejection left a partial input")
		}
	}
	namedDynamicInputs(t, map[string]any{"business-customer-123": map[any]any{int(1): "a", float64(2): "b"}}, map[string]map[int]string{"business-customer-123": {1: "a", 2: "b"}})
}

func TestInputMapDistinctKeys(t *testing.T) {
	want := map[float64]string{1: "integer", 2: "float"}
	namedDynamicInputs(t, map[any]any{int(1): "integer", float64(2): "float"}, want)
	// 同样的数值舍入在没有第二条碰撞键时仍然合法。
	namedDynamicInputs(t, map[int64]string{1<<53 + 1: "rounded"}, map[float64]string{1 << 53: "rounded"})
}

type inputNames []string
type inputLookup map[string]int
type inputArray [1]string
type inputPointer *int
type inputRecord struct{ N int }

func TestNamedDynamicInputs(t *testing.T) {
	names := []string{"x"}
	lookup := map[string]int{"x": 1}
	pointed := 42
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"slice", func(t *testing.T) { namedDynamicInputs(t, names, inputNames(names)) }},
		{"map", func(t *testing.T) { namedDynamicInputs(t, lookup, inputLookup(lookup)) }},
		{"array", func(t *testing.T) { namedDynamicInputs(t, [1]string{"x"}, inputArray{"x"}) }},
		{"struct", func(t *testing.T) { namedDynamicInputs(t, struct{ N int }{1}, inputRecord{1}) }},
		{"pointer", func(t *testing.T) { namedDynamicInputs(t, &pointed, inputPointer(&pointed)) }},
		{"pointer-from-scalar", func(t *testing.T) { namedDynamicInputs(t, 42, inputPointer(&pointed)) }},
		{"already-named", func(t *testing.T) { namedDynamicInputs(t, inputNames(names), inputNames(names)) }},
		{"generic-elements", func(t *testing.T) { namedDynamicInputs(t, []any{"x"}, inputNames(names)) }},
		{"nil-slice", func(t *testing.T) { namedDynamicInputs(t, []string(nil), inputNames(nil)) }},
		{"nil-map", func(t *testing.T) { namedDynamicInputs(t, map[string]int(nil), inputLookup(nil)) }},
		{"nil-pointer", func(t *testing.T) { namedDynamicInputs(t, (*int)(nil), inputPointer(nil)) }},
		{"interface-dynamic-type", func(t *testing.T) { namedDynamicInputs[any](t, names, names) }},
		{"interface-typed-nil", func(t *testing.T) { namedDynamicInputs[any](t, (*int)(nil), (*int)(nil)) }},
	} {
		t.Run(test.name, test.run)
	}
}

// 每个入口都经 Get 和宿主 ReadOutput 读回；不能只核对中间 Inputs 的内容。
func namedDynamicInputs[T any](t *testing.T, raw any, want T) {
	t.Helper()
	for _, fixed := range []bool{false, true} {
		t.Run(fmt.Sprint("fixed=", fixed), func(t *testing.T) {
			ctx := checkpointTestContext(t)
			key, out := NewKey[T]("value"), NewKey[T]("out")
			reg := NewRegistry()
			MustRegisterKey(reg, key)
			tag, _ := reg.TypeTagOf(key.Name())
			g := mustNew(t, ctx, "named-input")
			declareIn(t, g, NewPort(key))
			declareOut(t, g, NewPort(key))
			mustAdd(t, g, NewNode("reader", "", Requires(key), nil, func(rc *RunCtx) error {
				got, err := Get(rc, key)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(got, want) {
					return fmt.Errorf("Get returned %#v, want %#v", got, want)
				}
				return nil
			}))
			var in Inputs
			output := key
			if fixed {
				bind, err := FixByName(reg, key.Name(), tag, raw)
				if err != nil {
					t.Fatal(err)
				}
				parent := mustNew(t, ctx, "named-fixed-input")
				declareOut(t, parent, NewPort(out))
				if err := Sub(parent, "child", "", []SubBind{bind, Out(key, out)}, g); err != nil {
					t.Fatal(err)
				}
				g, output = parent, out
			} else if err := SeedByName(&in, reg, key.Name(), tag, raw); err != nil {
				t.Fatal(err)
			}
			e, err := g.NewExecution(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			if err := e.Wait(); err != nil {
				t.Fatal(err)
			}
			got, state, err := ReadOutput(e, output)
			if err != nil || state != SlotStateReady || !reflect.DeepEqual(got, want) {
				t.Fatalf("ReadOutput = (%#v, %s, %v), want %#v ready", got, state, err, want)
			}
		})
	}
}

func TestNamedDynamicInputsShareReferences(t *testing.T) {
	slice := []string{"before"}
	lookup := map[string]int{"x": 1}
	pointed := 1
	reg := NewRegistry()
	s, m, p := NewKey[inputNames]("slice"), NewKey[inputLookup]("map"), NewKey[inputPointer]("pointer")
	MustRegisterKey(reg, s)
	MustRegisterKey(reg, m)
	MustRegisterKey(reg, p)
	var in Inputs
	for _, test := range []struct {
		ref keyRef
		raw any
	}{
		{s.asRef(), slice}, {m.asRef(), lookup}, {p.asRef(), &pointed},
	} {
		if err := SeedByName(&in, reg, test.ref.name, test.ref.typ.String(), test.raw); err != nil {
			t.Fatal(err)
		}
	}
	slice[0], lookup["x"], pointed = "after", 2, 2
	if got, ok := in.entries[s.Name()].value.(inputNames); !ok || got[0] != "after" {
		t.Fatalf("slice lost type or shared backing array: %T", in.entries[s.Name()].value)
	}
	if got, ok := in.entries[m.Name()].value.(inputLookup); !ok || got["x"] != 2 {
		t.Fatalf("map lost type or shared reference: %T", in.entries[m.Name()].value)
	}
	if got, ok := in.entries[p.Name()].value.(inputPointer); !ok || got != inputPointer(&pointed) {
		t.Fatalf("pointer lost type or shared reference: %T", in.entries[p.Name()].value)
	}
}

func TestNamedDynamicInputRejectsNilInterface(t *testing.T) {
	reg := NewRegistry()
	MustRegisterKey(reg, NewKey[any]("value"))
	var in Inputs
	if err := SeedByName(&in, reg, "value", "interface {}", nil); err == nil {
		t.Fatal("host nil interface must remain rejected")
	}
	if err := (&Inputs{}).setArrived(NewKey[any]("arrived").asRef(), nil, false); err != nil {
		t.Fatal(err)
	}
}
