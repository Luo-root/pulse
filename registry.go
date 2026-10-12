package pulse

import (
	"fmt"
	"sync"
)

// RunFunc 是节点到达后的执行体，同时也是 YAML 具名工厂的形态：
// 工厂只给 Run，「读哪些 Key、写哪些 Key」的拓扑归属装配期
// （YAML / Registry）持有。
type RunFunc func(*RunCtx) error

// Registry 是装配期对象：具名 Run 工厂 + Key 登记表（供 YAML 的 name/type 对账）。
// 必须用 NewRegistry 建实例，禁止包级全局表（测试隔离）。
type Registry struct {
	mu        sync.Mutex
	factories map[string]RunFunc
	keys      map[string]keyRef // name → keyRef；type 记号 = typ.String()
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{
		factories: make(map[string]RunFunc),
		keys:      make(map[string]keyRef),
	}
}

// Register 登记具名 Run 工厂。同名重复返回错误。
func (r *Registry) Register(name string, f RunFunc) error {
	if r == nil {
		return fmt.Errorf("pulse: nil registry")
	}
	if name == "" {
		return fmt.Errorf("pulse: empty factory name")
	}
	if f == nil {
		return fmt.Errorf("pulse: nil factory %q", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.factories[name]; dup {
		return fmt.Errorf("pulse: factory %q already registered", name)
	}
	r.factories[name] = f
	return nil
}

// MustRegister 同 Register，冲突时 panic。
func (r *Registry) MustRegister(name string, f RunFunc) {
	if err := r.Register(name, f); err != nil {
		panic(err)
	}
}

// Lookup 返回具名工厂。
func (r *Registry) Lookup(name string) (RunFunc, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.factories[name]
	return f, ok
}

// RegisterKey 把 Key[T] 登记进表，YAML type 默认用 reflect.Type.String()。
// 同名必须以同一 T 再登记，否则错误。
func RegisterKey[T any](r *Registry, k Key[T]) error {
	if r == nil {
		return fmt.Errorf("pulse: nil registry")
	}
	if k.name == "" {
		return fmt.Errorf("pulse: empty key name")
	}
	ref := k.asRef()
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.keys[k.name]; ok {
		if old.typ != ref.typ {
			return fmt.Errorf("pulse: key %q already registered as %s, cannot reuse as %s", k.name, old.typ, ref.typ)
		}
		return nil
	}
	r.keys[k.name] = ref
	return nil
}

// MustRegisterKey 同 RegisterKey，冲突时 panic。
func MustRegisterKey[T any](r *Registry, k Key[T]) {
	if err := RegisterKey(r, k); err != nil {
		panic(err)
	}
}

// ResolveKey 按 YAML 的 name + type 查表；未登记或类型不符返回错误。
func (r *Registry) ResolveKey(name, typeTag string) (keyRef, error) {
	var zero keyRef
	if r == nil {
		return zero, fmt.Errorf("pulse: nil registry")
	}
	if name == "" {
		return zero, fmt.Errorf("pulse: empty key name in document")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, ok := r.keys[name]
	if !ok {
		return zero, fmt.Errorf("pulse: key %q not registered", name)
	}
	want := ""
	if ref.typ != nil {
		want = ref.typ.String()
	}
	if typeTag == "" {
		return zero, fmt.Errorf("pulse: key %q missing type tag (want %s)", name, want)
	}
	if typeTag != want {
		return zero, fmt.Errorf("pulse: key %q type %q does not match registered %q", name, typeTag, want)
	}
	return ref, nil
}

// TypeTagOf 返回已登记 Key 的类型记号（`reflect.Type.String()`），未登记或
// 该 Key 没有类型信息时 ok=false。
//
// 给**宿主做诊断**用：YAML 里的 `{name, type}` 与登记表对不上时（`ResolveKey`
// 报「type %q does not match registered %q」），宿主可以拿它把「登记表认为
// 应该是什么」打出来——错误信息里已经有期望值，但排查一批文档时按名字逐个
// 查更顺手。装配本身不需要它：`ResolveKey` / `KeyRefs` 已经做完对账。
func (r *Registry) TypeTagOf(name string) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, ok := r.keys[name]
	if !ok || ref.typ == nil {
		return "", false
	}
	return ref.typ.String(), true
}

// NameType 是 YAML / 装图用的 name+type 对。
type NameType struct {
	Name string
	Type string
}

// KeyRefs 把若干 NameType 解析为 NewNode 可用的 []keyRef。
func (r *Registry) KeyRefs(pairs []NameType) ([]keyRef, error) {
	out := make([]keyRef, 0, len(pairs))
	for _, p := range pairs {
		ref, err := r.ResolveKey(p.Name, p.Type)
		if err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, nil
}

// SeedByName / SkipSeedByName 住在 inputs.go：它们往**一次运行**的输入里写，
// 不再碰图（旧版本是 `pulse.Seed(g, k, v)`，第一个参数是 `*Graph`——那正是
// 「定义被这一次运行写脏」与「一张图只能跑一次」的根）。
