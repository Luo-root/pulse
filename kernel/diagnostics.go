package kernel

import (
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
)

// fiberSeq 是裸 Use 场景下诊断名的序号源（进程内单调递增）。
var fiberSeq atomic.Uint64

// FiberStateChange 是插件实例生命周期状态迁移事件（pulse.kernel.fiber_state）。
// 派发规则：在改 state 的锁外 Emit；from == to 不发。
type FiberStateChange struct {
	// Name 是实例的稳定诊断名：Loader 装载 = Entry.ID；裸 Use = 类型名#序号。
	Name string
	From FiberState
	To   FiberState
	// Err 仅在 To == StateFailed 时非 nil，是 Apply 的失败原因。
	Err error
}

// EventFiberState 观察订阅键：observability.Bootstrap 等旁路消费者使用
// kernel.On 订阅（观察者不得进入 Waterfall 链）。
var EventFiberState = NewEventKey[FiberStateChange]("pulse.kernel.fiber_state")

// LoaderActionKind 枚举 Reconcile 对单个条目执行的动作。
type LoaderActionKind string

const (
	ActionMount    LoaderActionKind = "mount"    // 新装载
	ActionUnmount  LoaderActionKind = "unmount"  // 条目移除/被重建的旧实例卸载
	ActionRecreate LoaderActionKind = "recreate" // Name 或 Config 变化触发整实例重建
	ActionDisable  LoaderActionKind = "disable"  // Disabled=true，不装载只登记
)

// LoaderAction 是 Loader.Reconcile 对单个条目的动作事件（pulse.kernel.loader_action）。
// 无变化的条目不派发（无 noop）。mount 失败逐条带 Err，不再等聚合错误。
type LoaderAction struct {
	Kind    LoaderActionKind
	EntryID string
	// Name 是条目引用的 plugin 注册名（Factory 登记名）。
	Name string
	Err  error
}

// EventLoaderAction 观察订阅键（同上，观察者用 On）。
var EventLoaderAction = NewEventKey[LoaderAction]("pulse.kernel.loader_action")

// diagnosticName 推导实例的稳定诊断名：优先 plugin 类型名（去包路径、
// 去指针前缀），Func 插件统一为 funcPlugin；冲突靠 #序号 区分。
func diagnosticName(p Plugin, seq uint64) string {
	if p == nil {
		return fmt.Sprintf("nil#%d", seq)
	}
	t := reflect.TypeOf(p)
	for t != nil && (t.Kind() == reflect.Pointer || t.Kind() == reflect.Interface) {
		t = t.Elem()
	}
	var base string
	switch {
	case t == nil:
		base = "nil"
	case t.Name() == "" || strings.HasPrefix(t.Name(), "func"):
		base = "funcPlugin"
	default:
		base = t.Name()
	}
	return fmt.Sprintf("%s#%d", base, seq)
}

// setName 完成 Fiber 诊断名初始化。只在 Use / Loader.mount 创建时、
// fiber 尚未发布（未进入 host.fibers）前调用一次——诊断名一经创建
// 不再变更，无需加锁。
func (f *Fiber) setName(name string) { f.name = name }

// Name 返回实例的稳定诊断名（横幅、fiber_state 载荷使用）。
func (f *Fiber) Name() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.name == "" {
		return fmt.Sprintf("fiber#%d", fiberSeq.Load())
	}
	return f.name
}

// transition 锁内完成赋值并派发：供调用方不持 f.mu 的场景。
func (f *Fiber) transition(to FiberState, err error) (FiberStateChange, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lockedTransition(to, err)
}

// lockedTransition 是 transition 的「须持有 f.mu」形态：赋值 + 计算
// 迁移载荷；派发仍由调用方在解锁后用 emitTransition 完成
// （监听器可能 Get/Use，绝不能在持锁状态下触达）。
func (f *Fiber) lockedTransition(to FiberState, err error) (FiberStateChange, bool) {
	from := f.state
	if from == to {
		return FiberStateChange{}, false
	}
	f.state = to
	f.applyErr = err
	return FiberStateChange{Name: f.name, From: from, To: to, Err: err}, true
}

// emitTransition 锁外派发状态迁移到 host 树。host 为 nil 时静默。
func (f *Fiber) emitTransition(ch FiberStateChange, ok bool) {
	if !ok || f.host == nil {
		return
	}
	Emit(f.host, EventFiberState, ch)
}
