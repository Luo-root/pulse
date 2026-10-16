package pulse

import (
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
