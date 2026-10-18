package pulse

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"runtime/debug"
	"strings"
	"testing"
	"time"
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

type inputRecursiveMap map[string]inputRecursiveMap
type inputRecursiveSlice []inputRecursiveSlice
type inputRecursivePointer *inputRecursivePointer
type inputIndirectPointer *inputIndirectPointerTail
type inputIndirectPointerTail *inputIndirectPointer
type inputMixedMap map[string]inputMixedSlice
type inputMixedSlice []inputMixedMap
type inputPointerTree *[1]inputPointerTree

// 崩溃型回归只在小栈子进程里跑；即使判环失效，也不能耗尽整套测试的栈内存。
func TestInputConversionCycles(t *testing.T) {
	const envKey = "PULSE_TEST_INPUT_CONVERSION_CYCLE"
	if mode := os.Getenv(envKey); mode != "" {
		debug.SetMaxStack(128 * 1024)
		shape, entry, _ := strings.Cut(mode, "/")
		fixed := entry == "fix"
		switch shape {
		case "map", "indirect-map":
			raw := map[string]any{}
			if shape == "map" {
				raw["private-cycle-key"] = raw
			} else {
				raw["private-cycle-key"] = map[string]any{"private-cycle-key": raw}
			}
			inputConversionRejectsCycle[inputRecursiveMap](t, raw, fixed, "map value")
		case "slice", "indirect-slice":
			raw := make([]any, 1)
			if shape == "slice" {
				raw[0] = raw
			} else {
				raw[0] = []any{raw}
			}
			inputConversionRejectsCycle[inputRecursiveSlice](t, raw, fixed, "element 0")
		case "mixed":
			raw := map[string]any{}
			raw["private-cycle-key"] = []any{raw}
			inputConversionRejectsCycle[inputMixedMap](t, raw, fixed, "map value", "element 0")
		case "pointer":
			inputConversionRejectsCycle[inputRecursivePointer](t, 1, fixed)
		case "indirect-pointer":
			inputConversionRejectsCycle[inputIndirectPointer](t, [1]int{1}, fixed)
		default:
			t.Fatalf("unknown conversion cycle case %q", mode)
		}
		return
	}
	for _, shape := range []string{"map", "indirect-map", "slice", "indirect-slice", "mixed", "pointer", "indirect-pointer"} {
		for _, entry := range []string{"seed", "fix"} {
			mode := shape + "/" + entry
			t.Run(mode, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInputConversionCycles$", "-test.timeout=2s")
				cmd.Env = append(os.Environ(), envKey+"="+mode, "GOTRACEBACK=single",
					"GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
				output, err := cmd.CombinedOutput()
				if err != nil {
					if len(output) > 1600 {
						output = output[:1600]
					}
					t.Fatalf("cyclic conversion did not return an error safely: %v\n%s", err, output)
				}
			})
		}
	}
}

func inputConversionRejectsCycle[T any](t *testing.T, raw any, fixed bool, context ...string) {
	t.Helper()
	reg := NewRegistry()
	key := NewKey[T]("cycle")
	MustRegisterKey(reg, key)
	tag, _ := reg.TypeTagOf(key.Name())
	var in Inputs
	if err := Seed(&in, NewKey[int]("kept"), 7); err != nil {
		t.Fatal(err)
	}
	var err error
	if fixed {
		var bind SubBind
		bind, err = FixByName(reg, key.Name(), tag, raw)
		if bind.fix != nil || bind.child.valid() || bind.parent.valid() || bind.in || bind.push != nil {
			t.Fatal("rejected conversion left a partial binding")
		}
	} else {
		err = SeedByName(&in, reg, key.Name(), tag, raw)
	}
	if err == nil {
		t.Fatal("non-terminating conversion accepted")
	}
	for _, want := range append(context, key.Name(), "cyclic conversion", reflect.TypeFor[T]().String()) {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("conversion error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "private-cycle-key") {
		t.Fatalf("conversion error exposed business map data: %v", err)
	}
	if len(in.entries) != 1 || in.entries["kept"].value != 7 {
		t.Fatal("rejected conversion changed Inputs")
	}
	if err := SeedByName(&in, reg, key.Name(), tag, *new(T)); err != nil {
		t.Fatalf("failed conversion prevented a later valid input: %v", err)
	}
}

func TestInputConversionAcyclic(t *testing.T) {
	sharedMap := map[string]any{"leaf": nil}
	sharedSlice := []any{nil}
	view := make([]any, 1)
	view[0] = view[:0]
	cyclicMap := map[string]any{}
	cyclicMap["self"] = cyclicMap
	cyclicSlice := make([]any, 1)
	cyclicSlice[0] = cyclicSlice
	pointerLeaf := inputPointerTree(new([1]inputPointerTree))
	for _, test := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"recursive-map", func(t *testing.T) {
			namedDynamicInputs(t, map[string]any{"nil": nil, "empty": map[string]any{}}, inputRecursiveMap{"nil": nil, "empty": {}})
		}},
		{"recursive-slice", func(t *testing.T) {
			namedDynamicInputs(t, []any{[]any{nil}, []any{}, []any(nil)}, inputRecursiveSlice{{nil}, {}, nil})
		}},
		{"shared-map", func(t *testing.T) {
			namedDynamicInputs(t, map[string]any{"left": sharedMap, "right": sharedMap}, inputRecursiveMap{"left": {"leaf": nil}, "right": {"leaf": nil}})
		}},
		{"shared-slice", func(t *testing.T) {
			namedDynamicInputs(t, []any{sharedSlice, sharedSlice}, inputRecursiveSlice{{nil}, {nil}})
		}},
		{"same-backing-slice-view", func(t *testing.T) {
			namedDynamicInputs(t, view, inputRecursiveSlice{{}})
		}},
		{"map-target-changes", func(t *testing.T) {
			namedDynamicInputs(t, cyclicMap, map[string]map[string]map[string]any{"self": {"self": cyclicMap}})
		}},
		{"slice-target-changes", func(t *testing.T) {
			namedDynamicInputs(t, cyclicSlice, [][][]any{{cyclicSlice}})
		}},
		{"recursive-pointer-tree", func(t *testing.T) {
			namedDynamicInputs(t, [1]any{[1]any{nil}}, inputPointerTree(&[1]inputPointerTree{pointerLeaf}))
		}},
		{"nil-map", func(t *testing.T) { namedDynamicInputs(t, map[string]any(nil), inputRecursiveMap(nil)) }},
		{"empty-map", func(t *testing.T) { namedDynamicInputs(t, map[string]any{}, inputRecursiveMap{}) }},
		{"nil-slice", func(t *testing.T) { namedDynamicInputs(t, []any(nil), inputRecursiveSlice(nil)) }},
		{"empty-slice", func(t *testing.T) { namedDynamicInputs(t, []any{}, inputRecursiveSlice{}) }},
	} {
		t.Run(test.name, test.run)
	}
}

func TestInputConversionAssignableCycles(t *testing.T) {
	// 未命名容器/指针可赋值给同底层命名类型；入口必须保留原引用，不能继续遍历。
	rawMap := map[string]inputRecursiveMap{}
	rawMap["self"] = rawMap
	rawSlice := make([]inputRecursiveSlice, 1)
	rawSlice[0] = rawSlice
	var rawPointer inputRecursivePointer
	rawPointer = &rawPointer
	t.Run("map", func(t *testing.T) { inputAssignableCycle[inputRecursiveMap](t, rawMap) })
	t.Run("slice", func(t *testing.T) { inputAssignableCycle[inputRecursiveSlice](t, rawSlice) })
	t.Run("pointer", func(t *testing.T) { inputAssignableCycle[inputRecursivePointer](t, &rawPointer) })
}

func inputAssignableCycle[T any](t *testing.T, raw any) {
	t.Helper()
	reg := NewRegistry()
	key := NewKey[T]("value")
	MustRegisterKey(reg, key)
	tag, _ := reg.TypeTagOf(key.Name())
	for _, fixed := range []bool{false, true} {
		var in Inputs
		if fixed {
			bind, err := FixByName(reg, key.Name(), tag, raw)
			if err != nil {
				t.Fatal(err)
			}
			in = bind.fix.in
		} else if err := SeedByName(&in, reg, key.Name(), tag, raw); err != nil {
			t.Fatal(err)
		}
		got := reflect.ValueOf(in.entries[key.Name()].value)
		if got.Type() != reflect.TypeFor[T]() || got.UnsafePointer() != reflect.ValueOf(raw).UnsafePointer() {
			t.Fatalf("fixed=%t: assignable cyclic value lost its declared type or reference", fixed)
		}
	}
}
