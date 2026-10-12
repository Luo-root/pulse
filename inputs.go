package pulse

import (
	"fmt"
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
//   - `v` 是 `nil`——`nil` 分不出「没写」与「写了空」，该用 `SkipSeed` 表达「这次没有值」；
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

// set 是 Seed / SkipSeed 的共用路径（内部入口，给子图绑定用的非泛型版本也走它）。
//
// 它**不碰图**，所以不改任何定义的字段：一次运行的输入就活在这份数据里。
func (in *Inputs) set(ref keyRef, v any, skip bool) error {
	if in == nil {
		return fmt.Errorf("pulse: nil Inputs: take the address of a value (pulse.Seed(&in, k, v))")
	}
	if ref.typ == nil {
		return fmt.Errorf("pulse: %s: zero key: build it with NewKey[T] and pass it whole", ref)
	}
	if !skip && v == nil {
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
// 可赋值的直接写入；声明式装图解出的泛型容器（[]any / map[string]any）
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
	vt := reflect.TypeOf(v)
	if !vt.AssignableTo(ref.typ) {
		converted, cerr := coerceSeedValue(v, ref.typ)
		if cerr != nil {
			return fmt.Errorf("pulse: seed %q: value type %s not assignable to %s (%v)", name, vt, ref.typ, cerr)
		}
		v = converted
	}
	return in.set(ref, v, false)
}

// SkipSeedByName 按登记表解析 Key 后把**这一次运行**的那条输入标成跳过。
func SkipSeedByName(in *Inputs, r *Registry, name, typeTag string) error {
	ref, err := r.ResolveKey(name, typeTag)
	if err != nil {
		return err
	}
	return in.set(ref, nil, true)
}
