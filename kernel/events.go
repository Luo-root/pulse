package kernel

import (
	"fmt"
	"reflect"
	"sync"
)

// EventKey 是类型安全的事件键，对应一类载荷为 P 的事件。
//
// 与 ServiceKey 同理：事件在定义处声明包级键，监听与派发共享
// 同一键实例。事件名全局唯一，同名不同类型的键在注册时被拒绝。
type EventKey[P any] struct {
	name string
}

// NewEventKey 创建事件键。name 建议带包名前缀。
func NewEventKey[P any](name string) EventKey[P] {
	return EventKey[P]{name: name}
}

// Name 返回事件的注册名。
func (k EventKey[P]) Name() string { return k.name }

// listener 是类型擦除后的监听器。
type listener struct {
	kind listenerKind
	fn   any
}

type listenerKind int

const (
	listenerObserve   listenerKind = iota // Emit / Parallel 的观察签名 func(*P)
	listenerWaterfall                     // Waterfall 的 around 签名 func(P, func(P) P) P
)

// eventBus 是单个作用域的事件总线。
//
// 锁约定：mu 守护本结构全部字段（types 与两张监听器表），是叶子锁——
// 其方法全部自持锁，方法内部不得触碰 Context.mu 或其他任何锁；
// 调用方需要同时访问层结构（children 等）与总线时，锁序固定为
// Context.mu -> bus.mu，全库不存在反向获取。
//
// 监听器表按 kind 分列并**写时复制（COW）**维护：add/remove 复制重建
// 切片（注册/摘除是低频路径），读取侧直接拿到不可变快照——派发因此
// 零拷贝（此前每次派发都要按 kind 过滤并新建切片）。快照窗口语义与
// 重构前一致：派发期间的增删不影响本次派发（在途派发持有旧切片，
// 正在卸载的作用域仍可能收到最后一次派发）。
type eventBus struct {
	mu        sync.Mutex
	types     map[string]reflect.Type // 事件名 -> 载荷类型指纹
	observe   map[string][]*listener  // COW：观察型监听器（Emit / EmitLocal / Parallel）
	waterfall map[string][]*listener  // COW：around 型监听器（Waterfall / WaterfallLocal）
}

func newEventBus() *eventBus {
	return &eventBus{
		types:     make(map[string]reflect.Type),
		observe:   make(map[string][]*listener),
		waterfall: make(map[string][]*listener),
	}
}

// payloadType 返回 P 的类型指纹。
func payloadType[P any]() reflect.Type {
	return reflect.TypeOf((*P)(nil))
}

// tableLocked 返回该 kind 对应的监听器表（自持锁语义：调用方持 b.mu）。
func (b *eventBus) tableLocked(kind listenerKind) map[string][]*listener {
	if kind == listenerWaterfall {
		return b.waterfall
	}
	return b.observe
}

// list 返回该事件指定 kind 的监听器快照（COW 切片，调用方只读；
// 自持锁）。无监听时返回 nil。
func (b *eventBus) list(name string, kind listenerKind) []*listener {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tableLocked(kind)[name]
}

// add 注册监听器并做同名同类型校验（自持锁；COW 重建该 kind 的切片）。
func (b *eventBus) add(name string, typ reflect.Type, l *listener) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if known, ok := b.types[name]; ok {
		if known != typ {
			return fmt.Errorf("kernel: event %q declared with payload %s, cannot listen as %s",
				name, known, typ)
		}
	} else {
		b.types[name] = typ
	}
	t := b.tableLocked(l.kind)
	t[name] = appendCopyListener(t[name], l)
	return nil
}

// remove 按 listener 身份摘除（自持锁；COW 重建，未找到为空操作）。
func (b *eventBus) remove(name string, l *listener) {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.tableLocked(l.kind)
	ls := t[name]
	for i, cur := range ls {
		if cur != l {
			continue
		}
		if len(ls) == 1 {
			delete(t, name)
			return
		}
		out := make([]*listener, 0, len(ls)-1)
		out = append(out, ls[:i]...)
		out = append(out, ls[i+1:]...)
		t[name] = out
		return
	}
}

// appendCopyListener 复制重建并追加——COW 不变式：绝不原地修改已有切片
// （在途派发可能正持有它）。
func appendCopyListener(ls []*listener, l *listener) []*listener {
	out := make([]*listener, len(ls), len(ls)+1)
	copy(out, ls)
	return append(out, l)
}

// clear 丢弃本层全部监听器（作用域销毁时调用：已死作用域的
// 监听不得再被任何派发触达）。类型指纹保留（类型校验与生命周期无关）。
func (b *eventBus) clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.observe = make(map[string][]*listener)
	b.waterfall = make(map[string][]*listener)
}

// OnWaterfall 注册一个 waterfall（around 中间件）监听器。
//
// 监听器收到载荷和一个 next 函数：
//   - 调用 next(p) 把（可能改写后的）载荷委托给后续监听器，
//     其返回值即整条链的最终结果；
//   - 不调用 next 直接返回 => 短路，后续监听器不再执行；
//   - 典型用法是改写共享请求/决策对象后委托。
//
// 注册本身是一条效应（随作用域销毁自动摘除——Apply 中丢弃返回的
// dispose 也不会泄漏）；执行顺序按注册顺序，同一事件上与 On 混用
// 时两类监听器各自独立派发、互不干扰。返回撤销函数（幂等）。
func OnWaterfall[P any](c *Context, k EventKey[P], fn func(payload P, next func(P) P) P) (func(), error) {
	typ := payloadType[P]()
	l := &listener{kind: listenerWaterfall, fn: fn}
	// bus.add/remove 自持锁，apply 无需触碰 Context.mu。
	d, err := c.Effect(func() (func(), error) {
		if err := c.events.add(k.name, typ, l); err != nil {
			return nil, err
		}
		return func() { c.events.remove(k.name, l) }, nil
	})
	return d, err
}

// On 注册一个观察型监听器（供 Emit / Parallel 派发）。
//
// 监听器通过 *P 就地修改载荷（Emit 串行派发时前序监听器的修改
// 对后续可见）。注册本身是一条效应（随作用域销毁自动摘除）；
// 返回撤销函数（幂等）。
func On[P any](c *Context, k EventKey[P], fn func(payload *P)) (func(), error) {
	typ := payloadType[P]()
	l := &listener{kind: listenerObserve, fn: fn}
	d, err := c.Effect(func() (func(), error) {
		if err := c.events.add(k.name, typ, l); err != nil {
			return nil, err
		}
		return func() { c.events.remove(k.name, l) }, nil
	})
	return d, err
}

// collectListeners 收集整棵作用域树上某事件、指定 kind 的监听器，
// 先序遍历保证「从根到叶」的层级顺序、层内保持注册顺序——祖先层
// 监听器先于后代层执行，即「外层策略包裹内层行为」。事件派发是
// 全树广播（与按依赖名投递的服务变更不同：事件没有「声明」可依，
// 只能逐层遍历；两者共同点是跨作用域可达——插件无论挂在哪个作用域，
// 其监听都能到达，且随其作用域销毁自动摘除）。
//
// 锁序：逐层先持 Context.mu 快照 children，经 bus.mu 取监听快照
// （叶子锁随取随放）；全部派发在所有锁释放之后进行。
func (c *Context) collectListeners(name string, kind listenerKind) []*listener {
	root := c.root()
	var out []*listener
	var walk func(*Context)
	walk = func(n *Context) {
		n.mu.Lock()
		snap := n.events.list(name, kind) // COW 快照：只读、零拷贝
		kids := append([]*Context{}, n.children...)
		n.mu.Unlock()
		out = append(out, snap...)
		for _, kid := range kids {
			walk(kid)
		}
	}
	walk(root)
	return out
}

// Emit 以观察语义派发：按上述顺序同步逐个调用。waterfall 型
// 监听器不参与（安静跳过）；单个监听器的 panic 会向上传播
// （观察者不应吞掉编程错误）。
//
// 监听器可通过 *P 就地修改载荷，且修改对后续监听器可见——Go 的
// 同步调用天然就是串行累积语义（对应 Cordis 的 serial 模式），
// 因此不设单独的 Serial 入口；需要"各拿独立副本"的并发语义用
// Parallel。
func Emit[P any](c *Context, k EventKey[P], payload P) {
	for _, l := range c.collectListeners(k.name, listenerObserve) {
		fn := l.fn.(func(*P))
		fn(&payload)
	}
}

// Parallel 以并发语义派发全部观察监听器并等待完成：每个监听器
// 拿到独立副本起点，互不可见。返回各监听器的错误（panic 被转换
// 为 error），顺序与监听顺序对应；无监听器时返回 nil。
func Parallel[P any](c *Context, k EventKey[P], payload P) []error {
	ls := c.collectListeners(k.name, listenerObserve)
	if len(ls) == 0 {
		return nil
	}
	errs := make([]error, len(ls))
	var wg sync.WaitGroup
	for i, l := range ls {
		wg.Add(1)
		go func(i int, l *listener) {
			defer wg.Done()
			func() {
				defer func() {
					if r := recover(); r != nil {
						errs[i] = fmt.Errorf("kernel: parallel listener %d panicked: %v", i, r)
					}
				}()
				fn := l.fn.(func(*P))
				cp := payload // 并发语义：每个监听器拿到独立副本起点
				fn(&cp)
			}()
		}(i, l)
	}
	wg.Wait()
	allNil := true
	for _, e := range errs {
		if e != nil {
			allNil = false
			break
		}
	}
	if allNil {
		return nil
	}
	return errs
}

// Waterfall 以 around 链语义派发：监听器依次包裹，最终结果沿
// next 链回流。无监听器时原样返回载荷。观察型监听器不参与。
func Waterfall[P any](c *Context, k EventKey[P], payload P) P {
	next := func(p P) P { return p }
	ls := c.collectListeners(k.name, listenerWaterfall)
	for i := len(ls) - 1; i >= 0; i-- {
		fn := ls[i].fn.(func(P, func(P) P) P)
		prevNext := next
		next = func(p P) P { return fn(p, prevNext) }
	}
	return next(payload)
}

// localListeners 只收集 c 本层 eventBus 上的监听器：不走 root()、
// 不向父链冒泡、不向子树广播。这是 EmitLocal / WaterfallLocal 的
// 派发边界——请求级隔离的 API 契约，不是文档建议。
func (c *Context) localListeners(name string, kind listenerKind) []*listener {
	if c == nil {
		return nil
	}
	return c.events.list(name, kind) // COW 快照：只读、零拷贝
}

// EmitLocal 只派发到 c 自身的观察监听器。
//
// 与 Emit 的分工：
//   - Emit：从 root 全树广播（宿主级观察，如 fiber_state / loader_action）；
//   - EmitLocal：只本 scope（请求级观察，如 tool/turn/llm generate）。
//
// nil scope 安全：直接返回。父链/子树/兄弟 scope 上的监听收不到。
func EmitLocal[P any](c *Context, k EventKey[P], payload P) {
	if c == nil {
		return
	}
	for _, l := range c.localListeners(k.name, listenerObserve) {
		fn := l.fn.(func(*P))
		fn(&payload)
	}
}

// WaterfallLocal 只在 c 自身跑 around 链。
//
// 与 Waterfall 的分工同 EmitLocal / Emit。HITL（before_tool_call）
// 必须走本函数，否则 A 请求的拒绝/改写会进 B 的 around 链。
//
// nil scope 安全：原样返回载荷。
func WaterfallLocal[P any](c *Context, k EventKey[P], payload P) P {
	if c == nil {
		return payload
	}
	next := func(p P) P { return p }
	ls := c.localListeners(k.name, listenerWaterfall)
	for i := len(ls) - 1; i >= 0; i-- {
		fn := ls[i].fn.(func(P, func(P) P) P)
		prevNext := next
		next = func(p P) P { return fn(p, prevNext) }
	}
	return next(payload)
}
