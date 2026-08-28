package flow

import (
	"fmt"
	"reflect"
	"sync"
)

// RunFunc 是节点到达后的执行体（E2 拓扑归属 A：工厂只给 Run）。
type RunFunc func(*RunCtx) error

// NodeFactory 是具名 Run 工厂；不返回 *Node，不进 kernel.Loader。
type NodeFactory = RunFunc

// Registry 是装配期对象：具名 Run 工厂 + Key 登记表（供 YAML 的 name/type 对账）。
// 必须用 NewRegistry 建实例，禁止包级全局表（测试隔离）。
type Registry struct {
	mu        sync.Mutex
	factories map[string]NodeFactory
	keys      map[string]keyRef // name → keyRef
	typeTags  map[string]string // name → reflect.Type.String()，与 YAML type 对账
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{
		factories: make(map[string]NodeFactory),
		keys:      make(map[string]keyRef),
		typeTags:  make(map[string]string),
	}
}

// Register 登记具名 Run 工厂。同名重复返回错误。
func (r *Registry) Register(name string, f NodeFactory) error {
	if r == nil {
		return fmt.Errorf("flow: nil registry")
	}
	if name == "" {
		return fmt.Errorf("flow: empty factory name")
	}
	if f == nil {
		return fmt.Errorf("flow: nil factory %q", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.factories[name]; dup {
		return fmt.Errorf("flow: factory %q already registered", name)
	}
	r.factories[name] = f
	return nil
}

// MustRegister 同 Register，冲突时 panic。
func (r *Registry) MustRegister(name string, f NodeFactory) {
	if err := r.Register(name, f); err != nil {
		panic(err)
	}
}

// Lookup 返回具名工厂。
func (r *Registry) Lookup(name string) (NodeFactory, bool) {
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
		return fmt.Errorf("flow: nil registry")
	}
	if k.name == "" {
		return fmt.Errorf("flow: empty key name")
	}
	ref := k.asRef()
	tag := ref.typ.String()
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.keys[k.name]; ok {
		if old.typ != ref.typ {
			return fmt.Errorf("flow: key %q already registered as %s, cannot reuse as %s", k.name, old.typ, ref.typ)
		}
		return nil
	}
	r.keys[k.name] = ref
	r.typeTags[k.name] = tag
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
		return zero, fmt.Errorf("flow: nil registry")
	}
	if name == "" {
		return zero, fmt.Errorf("flow: empty key name in document")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, ok := r.keys[name]
	if !ok {
		return zero, fmt.Errorf("flow: key %q not registered", name)
	}
	want := r.typeTags[name]
	if typeTag == "" {
		return zero, fmt.Errorf("flow: key %q missing type tag (want %s)", name, want)
	}
	if typeTag != want {
		return zero, fmt.Errorf("flow: key %q type %q does not match registered %q", name, typeTag, want)
	}
	return ref, nil
}

// TypeTagOf 返回已登记 Key 的类型记号（测试/诊断用）。
func (r *Registry) TypeTagOf(name string) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tag, ok := r.typeTags[name]
	return tag, ok
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

// SeedByName 按登记表解析 Key 后写入初始值（类型必须可赋给登记 T）。
func SeedByName(g *Graph, r *Registry, name, typeTag string, v any) error {
	ref, err := r.ResolveKey(name, typeTag)
	if err != nil {
		return err
	}
	if v == nil {
		return fmt.Errorf("flow: seed %q: nil value", name)
	}
	vt := reflect.TypeOf(v)
	if !vt.AssignableTo(ref.typ) {
		return fmt.Errorf("flow: seed %q: value type %s not assignable to %s", name, vt, ref.typ)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started {
		return ErrGraphStarted
	}
	if err := g.keys.register(ref); err != nil {
		return err
	}
	if err := g.claimSource(ref.name, "seed"); err != nil {
		return err
	}
	return g.slotOfLocked(ref).resolveValue(v)
}

// SkipSeedByName 按登记表解析 Key 后标记跳过。
func SkipSeedByName(g *Graph, r *Registry, name, typeTag string) error {
	ref, err := r.ResolveKey(name, typeTag)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started {
		return ErrGraphStarted
	}
	if err := g.keys.register(ref); err != nil {
		return err
	}
	if err := g.claimSource(ref.name, "seed"); err != nil {
		return err
	}
	return g.slotOfLocked(ref).resolveSkip()
}
