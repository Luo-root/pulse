package pulse

import (
	"fmt"
	"reflect"
	"sync"
)

// Key 是类型化的数据槽标识。name 用于调试与 YAML 对账；类型用于编译期
// 安全，以及装配期的同名跨型拒绝——同名 Key 必须以同一个 T 注册
// （keyRegistry.register / Registry.RegisterKey），不做「同名换类型」
// 的静默覆盖。
type Key[T any] struct {
	name string
}

// NewKey 创建槽位键。同名必须始终对应同一 T，否则后续 Register
// 到 Graph 时会被拒绝。
func NewKey[T any](name string) Key[T] {
	return Key[T]{name: name}
}

// Name 返回键名。
func (k Key[T]) Name() string { return k.name }

func (k Key[T]) asRef() keyRef {
	return keyRef{name: k.name, typ: reflect.TypeOf((*T)(nil)).Elem()}
}

// keyRef 是去掉泛型后的键指纹，供 Graph / Slot 内部使用。
type keyRef struct {
	name string
	typ  reflect.Type
}

func (k keyRef) String() string {
	if k.typ == nil {
		return k.name
	}
	return k.name + ":" + k.typ.String()
}

// keyRegistry 检测同名不同类型。
type keyRegistry struct {
	mu   sync.Mutex
	seen map[string]reflect.Type
}

func (r *keyRegistry) register(k keyRef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = make(map[string]reflect.Type)
	}
	if old, ok := r.seen[k.name]; ok && old != k.typ {
		return fmt.Errorf("pulse: key %q already registered as %s, cannot reuse as %s", k.name, old, k.typ)
	}
	r.seen[k.name] = k.typ
	return nil
}
