package kernel

import (
	"fmt"
	"sync"
	"time"
)

// Plugin 是插件的声明：依赖什么（Inject），装载时做什么（Apply）。
//
// Apply 在插件实例的私有作用域内执行，其中的一切注册——Provide
// 服务、监听事件、Effect 登记的资源——都随实例卸载自动回收。
// Apply 返回错误视为装载失败，实例进入 Failed 状态。
type Plugin interface {
	// Inject 声明本插件依赖的服务键。全部满足才会装载；
	// 任一消失则自动卸载，全部恢复则自动重新装载。
	Inject() []Dependency

	// Apply 执行装载。在 c 上注册的一切都会被跟踪。
	Apply(c *Context) error
}

// Dependency 是一条依赖声明。用 Require 构造；字段不公开，
// 依赖的种类由内核演进，插件只通过 Require 表达。
type Dependency struct {
	name string
	check func(c *Context) bool
}

// Name 返回依赖的服务名（诊断与变更过滤用）。
func (d Dependency) depName() string { return d.name }

// satisfied 报告该依赖在当前作用域视图下是否满足。
func (d Dependency) satisfied(c *Context) bool { return d.check(c) }

// Require 声明对一个服务的依赖。类型不符视同不存在。
func Require[T any](k ServiceKey[T]) Dependency {
	return Dependency{
		name: k.name,
		check: func(c *Context) bool {
			_, ok := Get(c, k)
			return ok
		},
	}
}

// Func 将普通函数适配为 Plugin（零依赖）。
func Func(apply func(c *Context) error) Plugin { return &funcPlugin{apply: apply} }

type funcPlugin struct{ apply func(c *Context) error }

func (p *funcPlugin) Inject() []Dependency            { return nil }
func (p *funcPlugin) Apply(c *Context) error          { return p.apply(c) }

// FiberState 描述插件实例的生命周期状态。
//
//	Inactive   未装载（依赖未满足，或尚未首次评估，或已被卸载）
//	Loading    装载中（Apply 正在执行）
//	Active     服务中
//	Unloading  卸载中（正在回收副作用）
//	Failed     装载失败（Err 给出原因）；依赖视图变化后会自动重试
type FiberState int

const (
	StateInactive FiberState = iota
	StateLoading
	StateActive
	StateUnloading
	StateFailed
)

func (s FiberState) String() string {
	switch s {
	case StateInactive:
		return "inactive"
	case StateLoading:
		return "loading"
	case StateActive:
		return "active"
	case StateUnloading:
		return "unloading"
	case StateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// Fiber 是一个插件的运行实例（对应论文中的 fiber / 惯性状态机）。
//
// # 并发模型（与 Cordis 异步惯性的差异）
//
// Cordis 依靠 async/await 让"卸载等待依赖方先行退出"，Go 的同步
// 调用无法安全重入，因此运行期的状态收敛采用"脏标记 + 单飞
// goroutine"：服务变更只把受影响的 Fiber 标记为 dirty 并唤醒其
// 专属收敛协程，绝不同步递归进其他 Fiber 的转换，从结构上消除
// 卸载环导致的死锁。Use 的首次装载是同步的（装配期无并发，
// "Use 返回即 Active 或 Failed"），运行期收敛用 WaitState 等待。
type Fiber struct {
	plugin Plugin
	host   *Context
	inject []Dependency

	mu        sync.Mutex
	state     FiberState
	ctx       *Context // Active 期间的私有作用域；其余状态为 nil
	applyErr  error
	dirty     bool
	settling  bool
	unsub     func() // 摘除 host 上的变更订阅
	closed    bool
	waitersMu sync.Mutex
	waiters   []chan struct{}
}

// Use 在 host 上装载一个插件。
//
// 首次装载同步执行：依赖满足则返回时已是 Active；不满足则进入
// Inactive 挂起等待（依赖出现时自动激活）；Apply 报错则进入
// Failed（Err 说明原因）。宿主已销毁时返回 ErrDisposed。
func Use(host *Context, p Plugin) (*Fiber, error) {
	f := &Fiber{
		plugin: p,
		host:   host,
		inject: p.Inject(),
	}

	// 订阅挂载层的服务变更（变更通知会从变更层广播到全树），
	// 仅当变更触及自己声明的依赖名时才标记脏。
	f.unsub = host.onChange(func(changed []string) {
		names := make(map[string]struct{}, len(f.inject))
		for _, d := range f.inject {
			names[d.depName()] = struct{}{}
		}
		for _, k := range changed {
			if _, hit := names[k]; hit {
				f.markDirty()
				return
			}
		}
	})

	host.mu.Lock()
	if host.disposed {
		host.mu.Unlock()
		f.unsub()
		return nil, ErrDisposed
	}
	host.fibers = append(host.fibers, f)
	host.mu.Unlock()

	// 首次评估：同步装载（装配期无并发，Use 返回即 Active / Failed
	// / Inactive-挂起）。
	f.settleSync()
	if err := f.Err(); err != nil {
		// 装载失败不回滚 Use：fiber 保持 Failed 态，依赖视图变化
		// 时会自动重试。调用方可选择立即 Close。
		return f, err
	}
	return f, nil
}

// settleSync 是首次装载的同步收敛路径：依赖满足则立即装载，
// 不满足则保持 Inactive 挂起等待。
func (f *Fiber) settleSync() {
	f.mu.Lock()
	if f.state != StateInactive || !f.satisfied() {
		f.mu.Unlock()
		return
	}
	f.state = StateLoading
	f.mu.Unlock()
	f.doLoad()
}

// State 返回当前生命周期状态。
func (f *Fiber) State() FiberState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

// Err 返回装载失败的错误；非 Failed 状态返回 nil。
func (f *Fiber) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applyErr
}

// satisfied 判断全部依赖是否可达（须持有 f.mu）。
func (f *Fiber) satisfied() bool {
	for _, d := range f.inject {
		if !d.satisfied(f.host) {
			return false
		}
	}
	return true
}

// markDirty 标记需要重新收敛，并确保恰有一个收敛协程在跑。
func (f *Fiber) markDirty() {
	f.mu.Lock()
	f.dirty = true
	if f.settling || f.closed {
		f.mu.Unlock()
		return
	}
	f.settling = true
	f.mu.Unlock()
	go f.settleLoop()
}

// settleLoop 是单飞收敛协程：在锁内决定动作、解锁执行，循环消化
// dirty 直到稳定。退出前必须在锁内确认 dirty==false 才置
// settling=false，因此不会丢失并发到达的新变更。
func (f *Fiber) settleLoop() {
	for {
		f.mu.Lock()
		if !f.dirty {
			f.settling = false
			f.mu.Unlock()
			f.signalWaiters()
			return
		}
		f.dirty = false

		sat := f.satisfied()
		var act func()
		switch {
		case sat && f.state == StateInactive, sat && f.state == StateFailed:
			act = f.doLoad
		case !sat && f.state == StateActive:
			act = f.doUnload
		}
		f.mu.Unlock()

		if act != nil {
			act()
			f.signalWaiters()
		}
		// 无感变化（状态与依赖视图一致）时回到循环开头检查新 dirty。
	}
}

// doLoad 执行装载。
// 执行期间不持有 f.mu；Apply 内部可以自由 Use 子插件、Provide 服务。
func (f *Fiber) doLoad() {
	f.mu.Lock()
	f.state = StateLoading
	f.applyErr = nil
	f.mu.Unlock()

	ctx, derr := f.host.Derive()
	if derr != nil {
		// 宿主已销毁：等价于被卸载——forceUnload 已将（或即将将）
		// 本实例标记为 closed/inactive，这里不产生任何副作用。
		f.mu.Lock()
		f.state = StateInactive
		f.mu.Unlock()
		return
	}
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("kernel: plugin apply panicked: %v", r)
			}
		}()
		err = f.plugin.Apply(ctx)
	}()

	f.mu.Lock()
	if f.closed {
		// Apply 执行期间实例被 Close / 宿主销毁：立即回滚本次装载，
		// 不得让副作用以 Active 形态存活在已注销的实例上。
		ctx.Dispose()
		f.state = StateInactive
		f.applyErr = nil
		f.mu.Unlock()
		return
	}
	if err != nil {
		ctx.Dispose()
		f.applyErr = err
		f.state = StateFailed
		f.mu.Unlock()
		return
	}
	f.ctx = ctx
	f.state = StateActive
	f.mu.Unlock()
}

// doUnload 执行卸载：丢弃私有作用域（其中一切注册按 LIFO 回收，
// 绑定撤除自动广播通知，从而驱动下游插件卸载）。
func (f *Fiber) doUnload() {
	f.mu.Lock()
	f.state = StateUnloading
	ctx := f.ctx
	f.ctx = nil
	f.mu.Unlock()

	if ctx != nil {
		ctx.Dispose()
	}

	f.mu.Lock()
	f.state = StateInactive
	f.mu.Unlock()
}

// forceUnload 由宿主作用域销毁时调用：整棵树都在拆除，不再广播
// 通知，仅同步状态并释放引用。（私有作用域由宿主的效应栈级联回收。）
func (f *Fiber) forceUnload() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.dirty = false
	f.state = StateInactive
	f.ctx = nil
}

// Close 主动卸载并注销本实例：撤销订阅、从宿主摘除、回收副作用。
// 幂等。
func (f *Fiber) Close() {
	if f.unsub != nil {
		f.unsub()
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	f.dirty = false
	state := f.state
	ctx := f.ctx
	f.ctx = nil
	f.mu.Unlock()

	if state == StateActive && ctx != nil {
		f.mu.Lock()
		f.state = StateUnloading
		f.mu.Unlock()
		ctx.Dispose()
		f.mu.Lock()
		f.state = StateInactive
		f.mu.Unlock()
	}

	f.host.mu.Lock()
	for i, cur := range f.host.fibers {
		if cur == f {
			f.host.fibers = append(f.host.fibers[:i], f.host.fibers[i+1:]...)
			break
		}
	}
	f.host.mu.Unlock()

	f.signalWaiters()
}

// WaitState 阻塞直到 fiber 进入 targets 之一或超时。
// targets 为空表示等待"不在转换中"（收敛完成）。
func (f *Fiber) WaitState(timeout time.Duration, targets ...FiberState) error {
	deadline := time.Now().Add(timeout)
	ch := f.newWaiter()
	defer f.removeWaiter(ch)
	for {
		f.mu.Lock()
		st := f.state
		f.mu.Unlock()
		if len(targets) == 0 {
			if st != StateLoading && st != StateUnloading {
				return nil
			}
		} else {
			for _, t := range targets {
				if st == t {
					return nil
				}
			}
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return fmt.Errorf("kernel: wait state timeout (current=%s)", st)
		}
		select {
		case <-ch:
		case <-time.After(remain):
			return fmt.Errorf("kernel: wait state timeout (current=%s)", st)
		}
	}
}

func (f *Fiber) newWaiter() chan struct{} {
	f.waitersMu.Lock()
	defer f.waitersMu.Unlock()
	ch := make(chan struct{}, 1)
	f.waiters = append(f.waiters, ch)
	return ch
}

func (f *Fiber) removeWaiter(ch chan struct{}) {
	f.waitersMu.Lock()
	defer f.waitersMu.Unlock()
	for i, cur := range f.waiters {
		if cur == ch {
			f.waiters = append(f.waiters[:i], f.waiters[i+1:]...)
			return
		}
	}
}

func (f *Fiber) signalWaiters() {
	f.waitersMu.Lock()
	defer f.waitersMu.Unlock()
	for _, ch := range f.waiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
