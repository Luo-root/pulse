package pulse

import (
	"fmt"
	"math"
	"reflect"
	"sort"
)

// 本文件是**一次运行的输入**：`Inputs` 是那份数据，`Seed` / `SkipSeed` 往它里面放值。
//
// 它取代了旧入口 `pulse.Seed(g, k, v)`——那个版本的第一个参数是 `*Graph`，也就是
// 「往**定义**里写这一次的值」：一张图因此只能跑一次，而且「声明外部来源」与「写这一次的值」
// 挤在同一个动作里，`Validate()` 于是分不出「这本来就是外部输入」与「你忘了接线」。
// 现在两件事各有各的入口：**声明**归 `DeclareInput`，**值**归这里。

// Inputs 是一次运行的输入：每条键要么有一个值，要么被显式标成跳过。
//
// 零值可用（不需要构造函数），也不要复制着传给两次运行用——`NewExecution` 在入口
// 那一刻把它**拷成内部只读表**，此后再改这个值与已经创建的执行无关。
//
// 用法：
//
//	var in pulse.Inputs
//	pulse.Seed(&in, ghost, "hi")        // 给一条
//	pulse.SkipSeed(&in, optional)        // 显式跳过一条（跳过是到达，不是失败）
//	err := g.Run(ctx, in)
//
// 喂一条**没 `DeclareInput` 过**的键、或漏喂一条这次真要用到的进口，都在
// `NewExecution` / `Run` 那一刻报错并点名是哪条键——不会留到运行期变成挂死。
type Inputs struct {
	entries map[string]inputEntry
}

// inputEntry 是一条输入：要么有值，要么是跳过（两者互斥）。
type inputEntry struct {
	ref   keyRef
	value any
	skip  bool
}

// Seed 给一次运行的某条键放一个值。语义与旧入口 `pulse.Seed(g, k, v)` 完全一致，
// 只是载体从**定义**换成了**这一次的输入**——签名一换，老写法一处都编译不过，
// 迁移靠编译器兜底。
//
// 三条判据都在这里当场报（不等到运行）：
//
//   - 键是零值（`Key[T]{}`）——名字或类型没写全；
//   - `v` 是 nil 接口——宿主喂值入口拒它，该用 `SkipSeed` 表达「这次没有值」；typed nil 仍是值；
//   - 同一条键这次已经放过（值或跳过）——幂等首写会**静默**丢掉后一笔，正是要拦的那类错。
//
// 键有没有被声明成进口、值对不对得上类型，都要等见到图才判得了（`NewExecution`）。
func Seed[T any](in *Inputs, k Key[T], v T) error {
	return in.set(k.asRef(), v, false)
}

// SkipSeed 把一次运行的某条键标成**跳过**：它这次没有值，但这不是失败——
// 「跳过是到达」，下游照常被唤醒并按自己的规则决定收不收。
//
// 图要跑起来、而这条进口这次确实没有值时，就是用它（漏喂是错误，跳过是声明）。
func SkipSeed[T any](in *Inputs, k Key[T]) error {
	return in.set(k.asRef(), nil, true)
}

// set 是宿主 Seed / SkipSeed 与固定值 Fix / FixSkip 的共用路径。
func (in *Inputs) set(ref keyRef, v any, skip bool) error {
	return in.setEntry(ref, v, skip, false)
}

// setArrived 转送父槽已经确认的到达状态：ready 的 nil 接口仍是值，不能改成 skipped。
// 只供子图的 In 桥接用；宿主与固定值入口仍走 set，拒绝 nil 接口。
func (in *Inputs) setArrived(ref keyRef, v any, skip bool) error {
	return in.setEntry(ref, v, skip, true)
}

// setEntry 不碰图；两条入口共用键有效性、重复写与入表判据。
func (in *Inputs) setEntry(ref keyRef, v any, skip, arrived bool) error {
	if in == nil {
		return fmt.Errorf("pulse: nil Inputs: take the address of a value (pulse.Seed(&in, k, v))")
	}
	if !ref.valid() {
		return fmt.Errorf("pulse: %s: zero key: build it with NewKey[T] and pass it whole", ref)
	}
	if !skip && v == nil && (!arrived || ref.typ.Kind() != reflect.Interface) {
		return fmt.Errorf("pulse: input %s: nil value: use SkipSeed to say \"no value this run\"", ref)
	}
	if _, dup := in.entries[ref.name]; dup {
		return fmt.Errorf("pulse: input %s: already set in this Inputs: a second write would be "+
			"silently dropped by the slot's idempotent first write, so it is rejected here", ref)
	}
	if in.entries == nil {
		in.entries = make(map[string]inputEntry, 1)
	}
	in.entries[ref.name] = inputEntry{ref: ref, value: v, skip: skip}
	return nil
}

// refs 是这份输入里的键，按**名字排序**（报文与判据的顺序都靠它稳定）。
// 包内入口：`Sub` 的固定值绑定（`Fix`）要按同一套进口判据逐条核。
func (in *Inputs) refs() []keyRef {
	if in == nil || len(in.entries) == 0 {
		return nil
	}
	names := make([]string, 0, len(in.entries))
	for name := range in.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]keyRef, 0, len(names))
	for _, name := range names {
		out = append(out, in.entries[name].ref)
	}
	return out
}

// SeedByName 按登记表解析 Key 后写入**一次运行的输入**。值必须能对齐到登记类型：
// 可赋值的按声明类型写入；声明式装图解出的泛型容器（[]any / map[string]any）
// 按目标类型做**形状对齐**（容器递归、标量同族转换，判据见
// coerceSeedValue）。map→struct 这类结构性猜测不做——那类取值应由
// 宿主的 resolve 回调给出类型正确的值。
func SeedByName(in *Inputs, r *Registry, name, typeTag string, v any) error {
	ref, err := r.ResolveKey(name, typeTag)
	if err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("pulse: seed %q: nil value", name)
	}
	converted, err := coerceSeedValue(v, ref.typ)
	if err != nil {
		return fmt.Errorf("pulse: seed %q: value type %s not assignable to %s (%v)", name, reflect.TypeOf(v), ref.typ, err)
	}
	return in.set(ref, converted, false)
}

// SkipSeedByName 按登记表解析 Key 后把**这一次运行**的那条输入标成跳过。
func SkipSeedByName(in *Inputs, r *Registry, name, typeTag string) error {
	ref, err := r.ResolveKey(name, typeTag)
	if err != nil {
		return err
	}
	return in.set(ref, nil, true)
}

// coerceSeedValue 把装配层给出的值对齐到 Key 的登记类型。
//
// 只有「形状」不同才转换：已可赋值按声明类型返回；声明式装图解出的泛型容器
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

// valueForType 按 Go 的赋值规则装入声明类型；命名容器/指针不能把原动态类型
// 留在 any 里，否则入口成功后 Get[T] 仍会断言失败。赋值保留接口动态类型和共享引用。
// 无效 Value 仅代表合法的 nil 接口；宿主输入是否允许 nil 由各自入口判断。
func valueForType(v reflect.Value, target reflect.Type) (reflect.Value, bool) {
	if !v.IsValid() {
		if target.Kind() == reflect.Interface {
			return reflect.Zero(target), true
		}
		return reflect.Value{}, false
	}
	if !v.Type().AssignableTo(target) {
		return reflect.Value{}, false
	}
	out := reflect.New(target).Elem()
	out.Set(v)
	return out, true
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
	if out, ok := valueForType(v, target); ok {
		return out, nil
	}

	switch target.Kind() {
	case reflect.Slice:
		if v.Kind() == reflect.Slice || v.Kind() == reflect.Array {
			// 只在支持的形状转换中保留 nil；非 nil 空切片仍要分配。
			if v.Kind() == reflect.Slice && v.IsNil() {
				return reflect.Zero(target), nil
			}
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
			// nil map 不能在逐键值转换前被分配成非 nil 空 map。
			if v.IsNil() {
				return reflect.Zero(target), nil
			}
			out := reflect.MakeMapWithSize(target, v.Len())
			iter := v.MapRange()
			for iter.Next() {
				k, err := convertValue(iter.Key(), target.Key())
				if err != nil {
					return reflect.Value{}, fmt.Errorf("map key: %w", err)
				}
				if out.MapIndex(k).IsValid() {
					return reflect.Value{}, fmt.Errorf("map key: conversion from %s to %s collides with another key", iter.Key().Type(), target.Key())
				}
				ev, err := convertValue(iter.Value(), target.Elem())
				if err != nil {
					return reflect.Value{}, fmt.Errorf("map value of %s: %w", target, err)
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
		assigned, _ := valueForType(out, target) // *Elem 可赋值给同底层的命名指针。
		return assigned, nil
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
