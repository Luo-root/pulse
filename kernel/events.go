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
	listenerObserve   listenerKind = iota // Emit / Parallel / Serial 的观察签名 func(*P)
	listenerWaterfall                     // Waterfall 的 around 签名 func(P, func(P) P) P
)

// eventBus 是单个作用域的事件总线。
type eventBus struct {
	mu        sync.Mutex
	listeners map[string][]*listener
	types     map[string]reflect.Type // 事件名 -> 载荷类型指纹
}

func newEventBus() *eventBus {
	return &eventBus{
		listeners: make(map[string][]*listener),
		types:     make(map[string]reflect.Type),
	}
}

// payloadType 返回 P 的类型指纹。
func payloadType[P any]() reflect.Type {
	return reflect.TypeOf((*P)(nil))
}

// add 注册监听器并做同名同类型校验（调用方须持有 bus.mu）。
func (b *eventBus) add(name string, typ reflect.Type, l *listener) error {
	if known, ok := b.types[name]; ok {
		if known != typ {
			return fmt.Errorf("kernel: event %q declared with payload %s, cannot listen as %s",
				name, known, typ)
		}
	} else {
		b.types[name] = typ
	}
	b.listeners[name] = append(b.listeners[name], l)
	return nil
}

func (b *eventBus) remove(name string, l *listener) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ls := b.listeners[name]
	for i, cur := range ls {
		if cur == l {
			b.listeners[name] = append(ls[:i], ls[i+1:]...)
			return
		}
	}
}

// clear 丢弃本层全部监听器（作用域销毁时调用：已死作用域的
// 监听不得再被任何派发触达）。
func (b *eventBus) clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.listeners = make(map[string][]*listener)
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
	d, err := c.Effect(func() (func(), error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if err := c.events.add(k.name, typ, l); err != nil {
			return nil, err
		}
		return func() { c.events.remove(k.name, l) }, nil
	})
	return d, err
}

// On 注册一个观察型监听器（供 Emit / Serial / Parallel 派发）。
//
// 监听器通过 *P 就地修改载荷（Serial 场景下前序监听器的修改对
// 后续可见）。注册本身是一条效应（随作用域销毁自动摘除）；
// 返回撤销函数（幂等）。
func On[P any](c *Context, k EventKey[P], fn func(payload *P)) (func(), error) {
	typ := payloadType[P]()
	l := &listener{kind: listenerObserve, fn: fn}
	d, err := c.Effect(func() (func(), error) {
		c.mu.Lock()
		defer c.mu.Unlock()
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
// 全树广播（与 notifyServiceChange 一致）：插件无论挂在哪个作用域，
// 其监听都能到达；随其作用域销毁自动摘除。
func (c *Context) collectListeners(name string, kinds ...listenerKind) []*listener {
	root := c.root()
	var out []*listener
	var walk func(*Context)
	walk = func(n *Context) {
		n.mu.Lock()
		for _, l := range n.events.listeners[name] {
			for _, k := range kinds {
				if l.kind == k {
					out = append(out, l)
					break
				}
			}
		}
		kids := append([]*Context{}, n.children...)
		n.mu.Unlock()
		for _, kid := range kids {
			walk(kid)
		}
	}
	walk(root)
	return out
}

// Emit 以 observe 语义派发：按上述顺序同步逐个调用。waterfall 型
// 监听器不参与（安静跳过）；单个监听器的 panic 会向上传播
// （观察者不应吞掉编程错误）。
func Emit[P any](c *Context, k EventKey[P], payload P) {
	for _, l := range c.collectListeners(k.name, listenerObserve) {
		fn := l.fn.(func(*P))
		fn(&payload)
	}
}

// Parallel 以并发语义派发全部观察监听器并等待完成。
// 返回各监听器的错误（panic 被转换为 error），顺序与监听顺序对应；
// 无监听器时返回 nil。
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

// Serial 以串行累积语义派发观察监听器：每个监听器通过 *P 的
// 修改对后续监听器可见（例如逐步追加内容）。
func Serial[P any](c *Context, k EventKey[P], payload P) {
	for _, l := range c.collectListeners(k.name, listenerObserve) {
		fn := l.fn.(func(*P))
		fn(&payload)
	}
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
