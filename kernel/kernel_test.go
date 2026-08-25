package kernel

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	keyStr = NewServiceKey[string]("test.str")
	keyInt = NewServiceKey[int]("test.int")
)

func mustProvide[T any](t *testing.T, c *Context, k ServiceKey[T], v T) func() {
	t.Helper()
	d, err := Provide(c, k, v)
	if err != nil {
		t.Fatalf("Provide %q: %v", k.Name(), err)
	}
	return d
}

func TestProvideGetAcrossScopes(t *testing.T) {
	root := New()
	mustProvide(t, root, keyStr, "root-value")

	child := root.Derive()
	if v, ok := Get(child, keyStr); !ok || v != "root-value" {
		t.Fatalf("child should see global service, got %q %v", v, ok)
	}
	// 全局单仓：子作用域 Provide 同名服务 => 撤旧装新（覆盖）。
	mustProvide(t, child, keyStr, "child-value")
	if v, _ := Get(child, keyStr); v != "child-value" {
		t.Fatalf("override failed: %q", v)
	}
	if v, _ := Get(root, keyStr); v != "child-value" {
		t.Fatalf("global store not visible from root: %q", v)
	}

	// 类型安全：string 键读 int 服务 => 不存在。
	if _, ok := Get(root, keyInt); ok {
		t.Fatal("int key should not resolve")
	}
}

func TestServiceTypeConflict(t *testing.T) {
	c := New()
	if _, err := Provide(c, keyStr, "x"); err != nil {
		t.Fatal(err)
	}
	// 同名不同类型键 => 拒绝。
	conflict := NewServiceKey[int]("test.str")
	if _, err := Provide(c, conflict, 42); err == nil {
		t.Fatal("expected type conflict error")
	}
}

func TestEffectLIFO(t *testing.T) {
	c := New()
	var order []string
	reg := func(name string) {
		_, err := c.Effect(func() (func(), error) {
			order = append(order, "+"+name)
			return func() { order = append(order, "-"+name) }, nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	reg("a")
	reg("b")
	reg("c")
	c.Dispose()
	want := []string{"+a", "+b", "+c", "-c", "-b", "-a"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("LIFO order = %v, want %v", order, want)
		}
	}
}

func TestDeriveDisposeIsolation(t *testing.T) {
	root := New()
	child := root.Derive()

	var disposed int
	child.Effect(func() (func(), error) {
		return func() { disposed++ }, nil
	})
	child.Dispose()
	if disposed != 1 {
		t.Fatalf("child effect not unwound: %d", disposed)
	}
	// 根不受影响，仍可用。
	if _, err := root.Effect(func() (func(), error) { return nil, nil }); err != nil {
		t.Fatalf("root should stay alive: %v", err)
	}
}

func TestParentDisposeCascades(t *testing.T) {
	root := New()
	child := root.Derive()
	grand := child.Derive()

	var disposed int32
	for _, c := range []*Context{child, grand} {
		c.Effect(func() (func(), error) {
			return func() { atomic.AddInt32(&disposed, 1) }, nil
		})
	}
	root.Dispose()
	if got := atomic.LoadInt32(&disposed); got != 2 {
		t.Fatalf("cascade unwind = %d, want 2", got)
	}
}

type countingPlugin struct {
	deps    []Dependency
	applies int32
	onApply func(c *Context) error
}

func (p *countingPlugin) Inject() []Dependency { return p.deps }
func (p *countingPlugin) Apply(c *Context) error {
	atomic.AddInt32(&p.applies, 1)
	if p.onApply != nil {
		return p.onApply(c)
	}
	return nil
}

func waitForState(t *testing.T, f *Fiber, timeout time.Duration, targets ...FiberState) {
	t.Helper()
	if err := f.WaitState(timeout, targets...); err != nil {
		t.Fatalf("wait state: %v (state=%s err=%v)", err, f.State(), f.Err())
	}
}

func TestPluginLifecycleReactive(t *testing.T) {
	ctx := New()
	disposeDep := mustProvide(t, ctx, keyStr, "dep")

	p := &countingPlugin{deps: []Dependency{Require(keyStr)}}
	f, err := Use(ctx, p)
	if err != nil {
		t.Fatalf("Use: %v", err)
	}
	waitForState(t, f, time.Second, StateActive)
	if n := atomic.LoadInt32(&p.applies); n != 1 {
		t.Fatalf("applies = %d, want 1", n)
	}

	// 依赖撤除 => 自动卸载。
	disposeDep()
	waitForState(t, f, time.Second, StateInactive)

	// 依赖恢复 => 自动重新装载。
	mustProvide(t, ctx, keyStr, "dep-v2")
	waitForState(t, f, time.Second, StateActive)
	if n := atomic.LoadInt32(&p.applies); n != 2 {
		t.Fatalf("applies after reload = %d, want 2", n)
	}
}

func TestPluginProvidesTrackedService(t *testing.T) {
	ctx := New()
	disposeDep := mustProvide(t, ctx, keyStr, "dep")

	providerKey := NewServiceKey[string]("test.provided")
	p := &countingPlugin{
		deps: []Dependency{Require(keyStr)},
		onApply: func(c *Context) error {
			_, err := Provide(c, providerKey, "produced")
			return err
		},
	}
	f, err := Use(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, f, time.Second, StateActive)
	if _, ok := Get(ctx, providerKey); !ok {
		t.Fatal("provided service not visible on host scope")
	}

	// 卸载后产物随之消失。
	disposeDep()
	waitForState(t, f, time.Second, StateInactive)
	if _, ok := Get(ctx, providerKey); ok {
		t.Fatal("provided service survived plugin unload")
	}
}

func TestPluginFailedThenRetry(t *testing.T) {
	ctx := New()
	disposeFlag := mustProvide(t, ctx, keyInt, 0)

	fail := true
	p := &countingPlugin{
		deps: []Dependency{Require(keyInt)},
		onApply: func(c *Context) error {
			if fail {
				return fmt.Errorf("boom")
			}
			return nil
		},
	}
	f, err := Use(ctx, p)
	if err == nil {
		t.Fatal("expected apply error from Use")
	}
	waitForState(t, f, time.Second, StateFailed)

	fail = false
	disposeFlag()
	mustProvide(t, ctx, keyInt, 1)
	waitForState(t, f, time.Second, StateActive)
}

func TestWaterfallOrderAndShortCircuit(t *testing.T) {
	ctx := New()
	type Request struct{ N int }
	ev := NewEventKey[Request]("test.req")

	unsub1, _ := OnWaterfall(ctx, ev, func(r Request, next func(Request) Request) Request {
		r.N += 1 // 第一个监听器：+1 后委托
		return next(r)
	})
	_, _ = OnWaterfall(ctx, ev, func(r Request, next func(Request) Request) Request {
		r.N += 10 // 第二个：+10 后短路
		return r
	})
	_, _ = OnWaterfall(ctx, ev, func(r Request, next func(Request) Request) Request {
		r.N += 100 // 不会执行
		return next(r)
	})

	out := Waterfall(ctx, ev, Request{})
	if out.N != 11 {
		t.Fatalf("waterfall result = %d, want 11", out.N)
	}

	unsub1()
	out = Waterfall(ctx, ev, Request{})
	if out.N != 10 {
		t.Fatalf("after unsubscribe result = %d, want 10", out.N)
	}
}

func TestEventModes(t *testing.T) {
	ctx := New()
	ev := NewEventKey[int]("test.num")

	var mu sync.Mutex
	var seen []int

	_, _ = On(ctx, ev, func(p *int) { mu.Lock(); seen = append(seen, *p); mu.Unlock() })

	Serial(ctx, ev, 7) // 观察者可就地修改 => Serial 传递修改
	Emit(ctx, ev, 8)
	errs := Parallel(ctx, ev, 9)
	if errs != nil {
		t.Fatalf("parallel errors: %v", errs)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("seen = %v", seen)
	}
}

func TestParallelCollectsPanics(t *testing.T) {
	ctx := New()
	ev := NewEventKey[int]("test.panic")
	_, _ = On(ctx, ev, func(p *int) { panic("listener blew up") })
	if errs := Parallel(ctx, ev, 1); len(errs) != 1 {
		t.Fatalf("want collected error, got %v", errs)
	}
}

func TestEventNameTypeConflict(t *testing.T) {
	ctx := New()
	k1 := NewEventKey[int]("test.conflict")
	k2 := NewEventKey[string]("test.conflict")
	if _, err := On(ctx, k1, func(*int) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := On(ctx, k2, func(*string) {}); err == nil {
		t.Fatal("expected event type conflict error")
	}
}

func TestLoaderReconcile(t *testing.T) {
	ctx := New()
	l := NewLoader(ctx)

	var applies int32
	l.MustRegister("counter", func() Plugin {
		return &countingPlugin{
			onApply: func(c *Context) error {
				atomic.AddInt32(&applies, 1)
				cfg, _ := Get(c, ConfigKey)
				if v, ok := cfg["tag"]; ok {
					mustProvideForTest(t, c, v.(string))
				}
				return nil
			},
		}
	})

	err := l.Reconcile([]Entry{{ID: "a", Name: "counter", Config: map[string]any{"tag": "v1"}}})
	if err != nil {
		t.Fatal(err)
	}
	f := l.Fiber("a")
	waitForState(t, f, time.Second, StateActive)
	if n := atomic.LoadInt32(&applies); n != 1 {
		t.Fatalf("applies = %d", n)
	}

	// Config 变化 => 重建。
	if err := l.Reconcile([]Entry{{ID: "a", Name: "counter", Config: map[string]any{"tag": "v2"}}}); err != nil {
		t.Fatal(err)
	}
	waitForState(t, l.Fiber("a"), time.Second, StateActive)
	if n := atomic.LoadInt32(&applies); n != 2 {
		t.Fatalf("rebuild applies = %d, want 2", n)
	}

	// Disabled => 卸载保留记录。
	if err := l.Reconcile([]Entry{{ID: "a", Name: "counter", Disabled: true}}); err != nil {
		t.Fatal(err)
	}
	if l.Fiber("a") != nil {
		t.Fatal("disabled entry should not keep a fiber")
	}

	// 移除。
	if err := l.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
	if len(l.Snapshot()) != 0 {
		t.Fatalf("snapshot after remove: %v", l.Snapshot())
	}
}

func mustProvideForTest(t *testing.T, c *Context, tag string) {
	t.Helper()
	if _, err := Provide(c, NewServiceKey[string]("test.tag"), tag); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownFactoryRejected(t *testing.T) {
	ctx := New()
	l := NewLoader(ctx)
	err := l.Reconcile([]Entry{{ID: "x", Name: "nope"}})
	if err == nil {
		t.Fatal("expected unknown factory error")
	}
}

func TestConcurrentDisposeRace(t *testing.T) {
	// 并发 Provide/Dispose 不应产生数据竞争（go test -race 覆盖）。
	ctx := New()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			child := ctx.Derive()
			_, _ = Provide(child, keyInt, i)
			_, _ = child.Effect(func() (func(), error) { return func() {}, nil })
			child.Dispose()
		}(i)
	}
	wg.Wait()
	ctx.Dispose()
}
