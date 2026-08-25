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
	listenerObserve   listenerKind = iota // emit / parallel / serial 共用的观察签名 func(*P)
	listenerWaterfall                     // around 中间件签名 func(P, next func(P) P) P
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

// add 注册监听器并做同名同类型校验（调用方持有 bus.mu）。
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

// OnWaterfall 注册一个 waterfall（around 中间件）监听器。
//
// 监听器收到载荷和一个 next 函数：
//   - 调用 next(p) 把（可能改写后的）载荷委托给后续监听器，
//     其返回值即整条链的最终结果；
//   - 不调用 next 直接返回 => 短路，后续监听器不再执行；
//   - 典型用法是改写共享请求/决策对象后委托。
//
// 执行顺序按注册顺序；返回撤销函数（幂等）。
func OnWaterfall[P any](c *Context, k EventKey[P], fn func(payload P, next func(P) P) P) (func(), error) {
	typ := payloadType[P]()
	c.mu.Lock()
	c.assertAlive()
	l := &listener{kind: listenerWaterfall, fn: fn}
	if err := c.events.add(k.name, typ, l); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() { c.removeEventListener(k.name, l) })
	}, nil
}

// On 注册一个观察型监听器（供 Emit / Parallel / Serial 派发）。
//
// 监听器通过 *P 就地修改载荷（Serial 场景下前序监听器的修改
// 对后续可见）。返回撤销函数（幂等）。
func On[P any](c *Context, k EventKey[P], fn func(payload *P)) (func(), error) {
	typ := payloadType[P]()
	c.mu.Lock()
	c.assertAlive()
	l := &listener{kind: listenerObserve, fn: fn}
	if err := c.events.add(k.name, typ, l); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() { c.removeEventListener(k.name, l) })
	}, nil
}

func (c *Context) removeEventListener(name string, l *listener) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ls := c.events.listeners[name]
	for i, cur := range ls {
		if cur == l {
			c.events.listeners[name] = append(ls[:i], ls[i+1:]...)
			return
		}
	}
}

// collectListeners 收集本层及全部祖先层上某事件的观察监听器，
// 按"从根到叶"的层级、层内按注册顺序排列——祖先先于后代执行，
// 符合"外层策略包裹内层行为"的直觉。须持有各层锁之外调用。
func (c *Context) collectListeners(name string) []*listener {
	var chain []*Context
	for cur := c; cur != nil; cur = cur.parent {
		chain = append([]*Context{cur}, chain...) // 头插 => chain[0] 是根
	}
	var out []*listener
	for _, layer := range chain {
		layer.mu.Lock()
		out = append(out, layer.events.listeners[name]...)
		layer.mu.Unlock()
	}
	return out
}

// Emit 以 observe 语义派发：按上述顺序同步逐个调用，忽略返回。
// 单个监听器的 panic 会向上传播（观察者不应吞掉编程错误）。
func Emit[P any](c *Context, k EventKey[P], payload P) {
	for _, l := range c.collectListeners(k.name) {
		fn := l.fn.(func(*P))
		fn(&payload)
	}
}

// Parallel 以并发语义派发全部观察监听器并等待完成。
// 返回各监听器的错误（panic 被转换为 error），顺序与监听顺序对应；
// 无监听器时返回 nil。
func Parallel[P any](c *Context, k EventKey[P], payload P) []error {
	ls := c.collectListeners(k.name)
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
	// 全部为 nil 时返回 nil 切片，方便 if err != nil 判断。
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
	for _, l := range c.collectListeners(k.name) {
		fn := l.fn.(func(*P))
		fn(&payload)
	}
}

// Waterfall 以 around 链语义派发：监听器依次包裹，最终结果沿
// next 链回流。无监听器时原样返回载荷。
func Waterfall[P any](c *Context, k EventKey[P], payload P) P {
	next := func(p P) P { return p }
	ls := c.collectListeners(k.name)
	for i := len(ls) - 1; i >= 0; i-- {
		fn := ls[i].fn.(func(P, func(P) P) P)
		prevNext := next
		next = func(p P) P { return fn(p, prevNext) }
	}
	return next(payload)
}
