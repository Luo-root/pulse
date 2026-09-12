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

// ProvideOption 配置一次 Provide 的可见性（当前仅此一维）。
type ProvideOption func(*provideOpts)

type provideOpts struct {
	local bool
}

// Local 让绑定只在本作用域子树可见：
//
//	kernel.Provide(scope, key, v, kernel.Local())
//
// 语义：绑定存**本层**，不写全局仓库、不投递变更、不进依赖索引——
// 它是请求级数据而不是装配面（Fiber 的 Inject 只看全局命名空间）；
// 本 scope 与其全部后代可读（Get 沿父链近因优先），父 / 兄弟不可见；
// 同名时遮蔽全局；随作用域销毁撤除；同层重复登记 = 覆盖（后者胜）。
func Local() ProvideOption {
	return func(o *provideOpts) { o.local = true }
}

// Provide 向服务仓库登记一个绑定，返回撤销函数。
//
// 默认是**全局**绑定（任何作用域 Get 得到）；带 Local() 选项则为
// 作用域局部绑定（仅本子树可见，见 Local）。
//
// 语义：
//   - 同名旧绑定的撤除与新绑定的安装合为一次原子变更（覆盖即撤旧，
//     被覆盖方的旧 dispose 不复活前值——有意语义，有测试背书）；
//   - 全局绑定的变更完成后按依赖名投递给声明了该服务的订阅者，声明该依赖
//     的插件实例会据此重新评估自己的装载状态（激活 / 卸载 / 无感）；
//     局部绑定不投递（不参与 fiber 依赖解析）；
//   - 返回的 dispose 只撤销本次安装（幂等），不影响其他历史。
func Provide[T any](c *Context, k ServiceKey[T], v T, opts ...ProvideOption) (func(), error) {
	var o provideOpts
	for _, opt := range opts {
		opt(&o)
	}
	if o.local {
		return provideLocal(c, k.name, v, keyType[T]())
	}
	return provide(c, k.name, v, keyType[T]())
}

// provide 是 Provide 全局形态的内部实现，附带类型指纹。
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
		if err := typeConflict(name, store.bindings[name], typ); err != nil {
			store.mu.Unlock()
			return nil, err
		}
		b = &binding{value: v, typ: typ}
		store.bindings[name] = b
		store.mu.Unlock()

		return func() {
			store.mu.Lock()
			// 只有当前绑定仍是自己安装的那条时才撤除；
			// 若已被后续 Provide 覆盖，则本次撤销是空操作。
			if cur, ok := store.bindings[name]; ok && cur == b {
				delete(store.bindings, name)
				store.mu.Unlock()
				store.notifyServiceChange(name)
				return
			}
			store.mu.Unlock()
		}, nil
	})
	if err != nil {
		return nil, err
	}

	// 通知安装（覆盖语义对外表现为"这个服务变了"）。
	// 此时 Effect 已返回（锁外），投递安全。
	store.notifyServiceChange(name)
	return dispose, nil
}

// provideLocal 是 Provide(..., Local()) 的内部实现：绑定存本层快照，
// 不写全局仓库、不投递变更。类型闸对同层已有绑定与全局同名绑定各查
// 一次（防同名异义：局部类型与全局类型同名不同型会让读方随位置而变）。
func provideLocal(c *Context, name string, v any, typ any) (func(), error) {
	var b *binding
	dispose, err := c.Effect(func() (func(), error) {
		if existing, ok := c.localGet(name); ok {
			if err := typeConflict(name, existing, typ); err != nil {
				return nil, err
			}
		}
		root := c.root()
		root.mu.Lock()
		gb := root.bindings[name]
		root.mu.Unlock()
		if err := typeConflict(name, gb, typ); err != nil {
			return nil, err
		}

		b = &binding{value: v, typ: typ}
		c.setLocal(name, b)
		return func() { c.removeLocal(name, b) }, nil
	})
	if err != nil {
		return nil, err
	}
	return dispose, nil
}

// typeConflict 校验同名绑定的类型指纹一致性（任一侧为空则跳过，
// 与既有全局行为一致）。
func typeConflict(name string, existing *binding, typ any) error {
	if existing == nil || existing.typ == nil || typ == nil {
		return nil
	}
	ot, _ := existing.typ.(reflect.Type)
	nt, _ := typ.(reflect.Type)
	if ot != nil && nt != nil && ot != nt {
		return fmt.Errorf("kernel: service %q already provided as %s, cannot re-provide as %s",
			name, ot, nt)
	}
	return nil
}

// Get 读取服务：先沿作用域链向上找**局部绑定**（近因优先，本 scope →
// 祖先 → 根），未命中回全局服务仓库；命中后按类型断言返回。
//
// 第二个返回值为 false 表示依赖不存在——这正是插件 Inject
// 未满足时挂起等待的判定依据。局部绑定存在时遮蔽全局：子树内读到
// 局部值，子树外照读全局值（同名同型由 Provide 的类型闸保证）。
func Get[T any](c *Context, k ServiceKey[T]) (T, bool) {
	var zero T
	for s := c; s != nil; s = s.parent {
		if b, ok := s.localGet(k.name); ok {
			v, ok := b.value.(T)
			if !ok {
				return zero, false
			}
			return v, true
		}
	}
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
