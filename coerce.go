package pulse

import (
	"fmt"
	"math"
	"reflect"
)

// coerceSeedValue 把装配层给出的值对齐到 Key 的登记类型。
//
// 只有「形状」不同才转换：已可赋值直接返回；声明式装图解出的泛型容器
// （[]any、map[string]any）按目标类型递归转换；标量只做数值家族之间与
// 同底层种类（含命名类型）的转换。null / nil 元素落到目标零值。
//
// **刻意不做**的（都是静默错值的来源）：
//
//   - map→struct 的字段猜测（哪些键对应哪些字段是宿主的语义）；
//   - 字符串↔数字互转（"3" 是不是 3 只有调用方知道）；
//   - 浮点截断成整数、越界收窄（1.5 → 1、300 → int8 一律报错）。
//
// 这些一律报错，由宿主用 resolve 回调给出类型正确的值——引擎不猜语义。
func coerceSeedValue(v any, target reflect.Type) (any, error) {
	if target == nil {
		return nil, fmt.Errorf("nil target type")
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return nil, fmt.Errorf("nil value")
	}
	out, err := convertValue(rv, target)
	if err != nil {
		return nil, err
	}
	return out.Interface(), nil
}

// convertValue 递归转换：容器逐元素 / 逐键值，标量走 convertScalar。
func convertValue(v reflect.Value, target reflect.Type) (reflect.Value, error) {
	// null / nil 元素（YAML 的 null、JSON 的 null）：落到目标零值。
	// 与 encoding/json 的 null 语义一致，不报错。
	if !v.IsValid() || (v.Kind() == reflect.Interface && v.IsNil()) {
		return reflect.Zero(target), nil
	}
	// 解包接口：[]any / map[string]any 的元素是 interface 值，
	// 判定与转换都要看动态类型。
	if v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	if v.Type().AssignableTo(target) {
		return v, nil
	}

	switch target.Kind() {
	case reflect.Slice:
		if v.Kind() == reflect.Slice || v.Kind() == reflect.Array {
			out := reflect.MakeSlice(target, v.Len(), v.Len())
			for i := 0; i < v.Len(); i++ {
				ev, err := convertValue(v.Index(i), target.Elem())
				if err != nil {
					return reflect.Value{}, fmt.Errorf("element %d: %w", i, err)
				}
				out.Index(i).Set(ev)
			}
			return out, nil
		}
	case reflect.Array:
		if (v.Kind() == reflect.Slice || v.Kind() == reflect.Array) && v.Len() == target.Len() {
			out := reflect.New(target).Elem()
			for i := 0; i < target.Len(); i++ {
				ev, err := convertValue(v.Index(i), target.Elem())
				if err != nil {
					return reflect.Value{}, fmt.Errorf("element %d: %w", i, err)
				}
				out.Index(i).Set(ev)
			}
			return out, nil
		}
	case reflect.Map:
		if v.Kind() == reflect.Map {
			out := reflect.MakeMapWithSize(target, v.Len())
			iter := v.MapRange()
			for iter.Next() {
				k, err := convertValue(iter.Key(), target.Key())
				if err != nil {
					return reflect.Value{}, fmt.Errorf("map key: %w", err)
				}
				ev, err := convertValue(iter.Value(), target.Elem())
				if err != nil {
					return reflect.Value{}, fmt.Errorf("map key %v: %w", iter.Key().Interface(), err)
				}
				out.SetMapIndex(k, ev)
			}
			return out, nil
		}
	case reflect.Pointer:
		// 指针目标：分配一个元素再转换（null 已在上面落到 nil）。
		ev, err := convertValue(v, target.Elem())
		if err != nil {
			return reflect.Value{}, err
		}
		out := reflect.New(target.Elem())
		out.Elem().Set(ev)
		return out, nil
	case reflect.Interface:
		if v.Type().Implements(target) {
			return v, nil
		}
	}

	if out, ok := convertScalar(v, target); ok {
		return out, nil
	}
	return reflect.Value{}, fmt.Errorf("cannot convert %s to %s", v.Type(), target)
}

// convertScalar 只处理三类同族转换：数值之间、字符串之间（含命名类型）、
// 布尔之间。返回 ok=false 表示不是可安全转换的一对。
func convertScalar(v reflect.Value, target reflect.Type) (reflect.Value, bool) {
	switch {
	case isNumericKind(v.Kind()) && isNumericKind(target.Kind()):
		out := reflect.New(target).Elem()
		switch target.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			i, ok := toInt64(v)
			if !ok || out.OverflowInt(i) {
				return reflect.Value{}, false
			}
			out.SetInt(i)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			u, ok := toUint64(v)
			if !ok || out.OverflowUint(u) {
				return reflect.Value{}, false
			}
			out.SetUint(u)
		default: // float32 / float64
			f := toFloat64(v)
			if out.OverflowFloat(f) {
				return reflect.Value{}, false
			}
			out.SetFloat(f)
		}
		return out, true
	case v.Kind() == reflect.String && target.Kind() == reflect.String:
		out := reflect.New(target).Elem()
		out.SetString(v.String())
		return out, true
	case v.Kind() == reflect.Bool && target.Kind() == reflect.Bool:
		out := reflect.New(target).Elem()
		out.SetBool(v.Bool())
		return out, true
	}
	return reflect.Value{}, false
}

func isNumericKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// 浮点转整数的上界必须按「float 能表示的那个端点」判：float64(math.MaxInt64)
// 会被舍入成 2^63，写 `f > math.MaxInt64` 等于放 2^63 自己过去，而
// int64(2^63) 在 gc 上得到 MinInt64——静默错值。下界没有这个问题
// （float64(math.MinInt64) 恰好是 -2^63）。
const (
	maxInt64AsFloat64  = 9223372036854775808.0  // 2^63
	maxUint64AsFloat64 = 18446744073709551616.0 // 2^64
)

// toInt64 把数值转成 int64；浮点必须是整值且在范围内（不截断）。
func toInt64(v reflect.Value) (int64, bool) {
	switch {
	case isIntKind(v.Kind()):
		return v.Int(), true
	case isUintKind(v.Kind()):
		u := v.Uint()
		if u > math.MaxInt64 {
			return 0, false
		}
		return int64(u), true
	case isFloatKind(v.Kind()):
		f := v.Float()
		if math.Trunc(f) != f || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		if f < math.MinInt64 || f >= maxInt64AsFloat64 {
			return 0, false
		}
		return int64(f), true
	}
	return 0, false
}

// toUint64 把数值转成 uint64；负数与负小数一律拒绝。
func toUint64(v reflect.Value) (uint64, bool) {
	switch {
	case isUintKind(v.Kind()):
		return v.Uint(), true
	case isIntKind(v.Kind()):
		i := v.Int()
		if i < 0 {
			return 0, false
		}
		return uint64(i), true
	case isFloatKind(v.Kind()):
		f := v.Float()
		if math.Trunc(f) != f || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f >= maxUint64AsFloat64 {
			return 0, false
		}
		return uint64(f), true
	}
	return 0, false
}

// toFloat64 把数值转成 float64（整数转浮点允许精度损失，与 Go 转换同口径）。
func toFloat64(v reflect.Value) float64 {
	switch {
	case isFloatKind(v.Kind()):
		return v.Float()
	case isIntKind(v.Kind()):
		return float64(v.Int())
	case isUintKind(v.Kind()):
		return float64(v.Uint())
	}
	return 0
}

func isIntKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

func isUintKind(k reflect.Kind) bool {
	switch k {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return true
	}
	return false
}

func isFloatKind(k reflect.Kind) bool {
	return k == reflect.Float32 || k == reflect.Float64
}
