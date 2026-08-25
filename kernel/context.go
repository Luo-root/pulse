package kernel

import (
	"errors"
	"sync"
)

// ErrDisposed 表示对一个已经销毁的作用域执行了非法操作。
var ErrDisposed = errors.New("kernel: context already disposed")

// effectEntry 是一条已登记的可逆效应。
//
// dispose 负责撤销 apply 产生的副作用；Context 销毁时按登记的
// 相反顺序（LIFO）逐条调用，保证"卸载即还原"。
type effectEntry struct {
	dispose func()
}

// binding 是一条服务绑定。
//
// typ 存键值类型指纹，用于同名跨类型冲突检测；响应式依赖的
// 感知由绑定撤除时的全树广播驱动，不需要提供者身份。
type binding struct {
	value any
	typ   any
}

// subscriber 包装服务变更订阅，提供稳定的指针身份——闭包不能用
// 代码指针判等（同一函数字面量的不同闭包指针相同），否则一个
// 订阅者的撤销会误删他人的订阅。
type subscriber struct {
	fn func(changed []string)
}

// Context 是内核的核心抽象：一个服务仓库，同时也是一个效应
// 跟踪器。它对应论文中的统一 context 类型——既承载"环境当前
// 是什么样"，也承载"我们曾对环境做过什么"（效应栈）。
//
// 服务命名空间全局唯一：绑定统一存放在根作用域的仓库中
// （见 service.go），Context 树管理的是生命周期归属与事件传播，
// 不是服务可见性。
//
// Context 组成作用域树：Derive 派生出的子作用域共享全局服务
// 绑定与事件广播；子作用域的销毁只回收它自己登记的效应并从父层
// 摘除自身，父作用域不受影响；销毁父作用域则按派生的逆序级联
// 销毁所有后代（后建先收，与效应栈 LIFO 同构）。
//
// Context 并发安全。
type Context struct {
	parent *Context

	mu       sync.Mutex
	disposed bool
	bindings map[string]*binding // 仅根作用域使用（全局服务仓库）
	effects  []*effectEntry      // 本层效应栈，LIFO unwind
	fibers   []*Fiber            // 直接挂载在本层的插件实例
	children []*Context          // 派生出的子作用域（Dispose 时逆序级联回收）
	events   *eventBus           // 本层事件总线

	onServiceChange []*subscriber // 服务变更订阅（内部使用）
}

// New 创建根作用域。
func New() *Context {
	return &Context{
		bindings: make(map[string]*binding),
		events:   newEventBus(),
	}
}

// Derive 派生一个子作用域。
//
// 子作用域继承全局服务绑定与事件广播；其上的一切注册（服务、
// 效应、插件、事件监听）在子作用域 Dispose 时被回收，且子作用域
// 会从父层摘除自己——反复派生/销毁不会让父层的登记增长。
// 宿主已销毁时返回 ErrDisposed。
func (c *Context) Derive() (*Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disposed {
		return nil, ErrDisposed
	}
	child := &Context{
		parent: c,
		events: newEventBus(),
	}
	c.children = append(c.children, child)
	return child, nil
}

// Effect 登记一条可逆效应：apply 执行装载动作，返回的函数负责
// 将其完全撤销。
//
// 服务 Provide 与事件监听最终都归约为一次 Effect 调用并由此获得
// 跟踪与回收（插件的装载/卸载走另一条生命周期：Fiber，见 plugin.go，
// 不经过 Effect）。apply 在锁外执行，内部可以自由触碰本层或其他层
// 的锁：
//
//	ch, err := ctx.Effect(func() (func(), error) {
//	    ln, err := net.Listen("tcp", ":0")
//	    if err != nil { return nil, err }
//	    go http.Serve(ln, mux)
//	    return func() { ln.Close() }, nil
//	})
//
// dispose 返回后可手动调用以提前撤销该效应（幂等）；未手动调用
// 的部分由作用域销毁兜底。
func (c *Context) Effect(apply func() (func(), error)) (dispose func(), err error) {
	c.mu.Lock()
	if c.disposed {
		c.mu.Unlock()
		return nil, ErrDisposed
	}
	c.mu.Unlock()

	// apply 在锁外执行：其内部可以自由触碰本层或其他层的锁
	// （例如 Provide 经由此路径安装绑定并操作根仓库）。
	undo, err := apply()
	if err != nil {
		return nil, err
	}
	if undo == nil {
		undo = func() {}
	}

	entry := &effectEntry{dispose: undo}
	c.mu.Lock()
	if c.disposed {
		// apply 执行期间作用域被销毁：登记失败，立即回滚。
		c.mu.Unlock()
		undo()
		return nil, ErrDisposed
	}
	c.effects = append(c.effects, entry)
	c.mu.Unlock()

	return func() {
		c.mu.Lock()
		if c.disposed {
			c.mu.Unlock()
			return
		}
		var run func()
		for i := len(c.effects) - 1; i >= 0; i-- {
			if c.effects[i] == entry {
				run = entry.dispose
				c.effects = append(c.effects[:i], c.effects[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
		// 解锁后执行：撤销回调可能广播服务变更、触碰其他层的锁，
		// 持本层锁执行会与其形成锁序环。
		if run != nil {
			run()
		}
	}, nil
}

// Parent 返回父作用域；根作用域返回 nil。
func (c *Context) Parent() *Context { return c.parent }

// Dispose 销毁本作用域：
//
//  1. 卸载挂载在本层的所有 Fiber；
//  2. 按派生的逆序递归销毁全部子作用域（后建先收）；
//  3. 清空本层事件总线（已死作用域的监听不得再被触达）；
//  4. 按 LIFO 执行本层效应栈，还原一切注册。
//
// Dispose 幂等：重复调用是空操作。
func (c *Context) Dispose() {
	c.dispose()
}

func (c *Context) dispose() {
	c.mu.Lock()
	if c.disposed {
		c.mu.Unlock()
		return
	}
	c.disposed = true

	// 快照后解锁执行：dispose 回调可能回调本 Context 的其他方法。
	effects := make([]*effectEntry, len(c.effects))
	copy(effects, c.effects)
	fibers := make([]*Fiber, len(c.fibers))
	copy(fibers, c.fibers)
	kids := make([]*Context, len(c.children))
	copy(kids, c.children)
	c.effects = nil
	c.fibers = nil
	c.children = nil
	if c.bindings != nil {
		c.bindings = make(map[string]*binding) // 根仓库释放引用
	}
	c.onServiceChange = nil
	c.mu.Unlock()

	// 已死作用域的事件总线立即失效。
	c.events.clear()

	// 从父层的 children 登记中摘除自己——反复派生/销毁不让父层
	// 积累死条目。
	if c.parent != nil {
		c.parent.removeChild(c)
	}

	for _, f := range fibers {
		f.forceUnload()
	}
	for i := len(kids) - 1; i >= 0; i-- {
		kids[i].dispose()
	}
	for i := len(effects) - 1; i >= 0; i-- {
		effects[i].dispose()
	}
}

// removeChild 把一个已销毁的子作用域从本层登记中摘除。
func (p *Context) removeChild(child *Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, cur := range p.children {
		if cur == child {
			p.children = append(p.children[:i], p.children[i+1:]...)
			return
		}
	}
}

// root 返回作用域树的根节点（全局服务仓库所在层）。
func (c *Context) root() *Context {
	r := c
	for r.parent != nil {
		r = r.parent
	}
	return r
}

// onChange 订阅本层的服务变更，返回摘除函数（幂等）。
// 内部 API：供插件生命周期使用。
func (c *Context) onChange(fn func(changed []string)) (unsub func()) {
	c.mu.Lock()
	sub := &subscriber{fn: fn}
	c.onServiceChange = append(c.onServiceChange, sub)
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			for i, cur := range c.onServiceChange {
				if cur == sub {
					c.onServiceChange = append(c.onServiceChange[:i], c.onServiceChange[i+1:]...)
					return
				}
			}
		})
	}
}

// notifyServiceChange 将服务变更广播到整棵作用域树的所有订阅者。
//
// 不做方向性裁剪（只向下/只向上）：依赖解析沿全局仓库进行，
// 提供方可能晚于消费方出现在任意层；服务变更是低频事件，全树广播
// 换取语义上的完备与实现的简单。每个订阅者自行过滤是否受影响。
// 调用方不得持有任何层的锁。
func (c *Context) notifyServiceChange(changed []string) {
	root := c.root()
	var walk func(*Context)
	walk = func(n *Context) {
		n.mu.Lock()
		subs := append([]*subscriber{}, n.onServiceChange...)
		kids := append([]*Context{}, n.children...)
		n.mu.Unlock()
		for _, sub := range subs {
			sub.fn(changed)
		}
		for _, k := range kids {
			walk(k)
		}
	}
	walk(root)
}
