package kernel

import (
	"fmt"
	"reflect"
)

// ServiceKey 是类型安全的服务键。
//
// 每个服务在定义处声明一个包级键，提供方与消费方共享同一个键
// 实例，而不是通过字符串或 import 具体实现来耦合：
//
//	// 定义处（例如 llm 包）
//	var Key = kernel.NewServiceKey[*Registry]("pulse.llm")
//
//	// 提供方
//	kernel.Provide(ctx, llm.Key, registry)
//
//	// 消费方
//	reg, err := kernel.Get(ctx, llm.Key)
type ServiceKey[T any] struct {
	name string
}

// NewServiceKey 创建服务键。name 建议带包名前缀以避免冲突，
// 例如 "pulse.llm"、"pulse.tools.fs"。
func NewServiceKey[T any](name string) ServiceKey[T] {
	return ServiceKey[T]{name: name}
}

// Name 返回服务的注册名。
func (k ServiceKey[T]) Name() string { return k.name }

// keyType 返回键值类型的指纹，用于同名冲突检测。
func keyType[T any]() any {
	return reflect.TypeOf((*T)(nil))
}

// Provide 向作用域登记一个服务绑定，返回撤销函数。
//
// 语义：
//   - 同层旧绑定的撤除与新绑定的安装合为一次原子变更；
//   - 变更完成后向整棵子树广播通知，声明了该依赖的插件实例
//     会据此重新评估自己的装载状态（激活 / 卸载 / 无感）；
//   - 返回的 dispose 只撤销本次安装（幂等），不影响其他历史。
func Provide[T any](c *Context, k ServiceKey[T], v T) (func(), error) {
	return provide(c, k.name, v, keyType[T]())
}

// provide 是 Provide 的内部形态，附带类型指纹。
//
// 服务绑定统一存放在根作用域的仓库中（对齐 Cordis 的 runtime
// store：作用域管理生命周期归属与事件传播，服务命名空间全局唯一，
// 避免"谁提供谁可见"的作用域陷阱）。安装本身作为登记层的一条
// 效应跟踪——作用域销毁时绑定自动撤除，这正是 Cordis "set 即
// effect"的不变式。
func provide(c *Context, name string, v any, typ any) (func(), error) {
	store := c.root()

	var b *binding
	dispose, err := c.Effect(func() (func(), error) {
		store.mu.Lock()
		if old, ok := store.bindings[name]; ok && old.typ != nil && typ != nil {
			ot, _ := old.typ.(reflect.Type)
			nt, _ := typ.(reflect.Type)
			if ot != nil && nt != nil && ot != nt {
				store.mu.Unlock()
				return nil, fmt.Errorf("kernel: service %q already provided as %s, cannot re-provide as %s",
					name, ot, nt)
			}
		}
		b = &binding{key: name, value: v, typ: typ}
		store.bindings[name] = b
		store.mu.Unlock()

		return func() {
			store.mu.Lock()
			// 只有当前绑定仍是自己安装的那条时才撤除；
			// 若已被后续 Provide 覆盖，则本次撤销是空操作。
			if cur, ok := store.bindings[name]; ok && cur == b {
				delete(store.bindings, name)
				store.mu.Unlock()
				store.notifyServiceChange([]string{name})
				return
			}
			store.mu.Unlock()
		}, nil
	})
	if err != nil {
		return nil, err
	}

	// 广播安装（覆盖语义对外表现为"这个服务变了"）。
	// 此时 Effect 已返回（锁外），广播安全。
	store.notifyServiceChange([]string{name})
	return dispose, nil
}

// Get 读取服务：在全局服务仓库中按键查找并断言类型。
//
// 第二个返回值为 false 表示依赖不存在——这正是插件 Inject
// 未满足时挂起等待的判定依据。
func Get[T any](c *Context, k ServiceKey[T]) (T, bool) {
	var zero T
	root := c.root()
	root.mu.Lock()
	b, ok := root.bindings[k.name]
	root.mu.Unlock()
	if !ok {
		return zero, false
	}
	v, ok := b.value.(T)
	if !ok {
		return zero, false
	}
	return v, true
}

// MustGet 同 Get，但依赖缺失时 panic。仅用于装配期断言。
func MustGet[T any](c *Context, k ServiceKey[T]) T {
	v, ok := Get(c, k)
	if !ok {
		panic(fmt.Sprintf("kernel: service %q not found", k.name))
	}
	return v
}

// Has 报告服务是否存在，不关心具体类型。
func Has(c *Context, name string) bool {
	root := c.root()
	root.mu.Lock()
	defer root.mu.Unlock()
	_, ok := root.bindings[name]
	return ok
}
