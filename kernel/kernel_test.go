package kernel

import (
	"errors"
	"fmt"
	"strings"
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
	conflict := NewServiceKey[int]("test.str")
	if _, err := Provide(c, conflict, 42); err == nil {
		t.Fatal("expected type conflict error")
	}
}

// #6：同名覆盖语义钉死——覆盖即撤旧，被覆盖方的撤销不再复活前值；
// 覆盖者卸载后服务消失、依赖方自动卸载。这是有意行为，不是漏测。
func TestProvideOverwriteSemantics(t *testing.T) {
	ctx := New()
	dA := mustProvide(t, ctx, keyStr, "A")

	consumer := &countingPlugin{deps: []Dependency{Require(keyStr)}}
	fc, err := Use(ctx, consumer)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, fc, time.Second, StateActive)

	dB := mustProvide(t, ctx, keyStr, "B") // B 覆盖 A
	if v, _ := Get(ctx, keyStr); v != "B" {
		t.Fatalf("overwrite failed: %q", v)
	}
	waitForState(t, fc, time.Second, StateActive) // 服务仍在 => 消费方无感保持

	if v, _ := (func() (string, bool) { return Get(ctx, keyStr) })(); true {
		_ = v
	}
	dB() // B 卸载 => 服务消失（A 的绑定已被覆盖作废，不复活）
	waitForState(t, fc, time.Second, StateInactive)
	if _, ok := Get(ctx, keyStr); ok {
		t.Fatal("service should be gone after overwriter disposal (documented semantics)")
	}

	dA() // A 的旧 dispose 是空操作（绑定指针已不同），不得 panic 或复活
	if _, ok := Get(ctx, keyStr); ok {
		t.Fatal("stale dispose must not resurrect binding")
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

// #5：反复装载/卸载后，宿主层的效应栈与 children 不随次数增长。
func TestDeriveDetachNoLeak(t *testing.T) {
	ctx := New()
	disposeDep := mustProvide(t, ctx, keyStr, "dep")

	p := &countingPlugin{deps: []Dependency{Require(keyStr)}}
	f, err := Use(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, f, time.Second, StateActive)

	baseEffects := len(ctx.effects)
	baseChildren := len(ctx.children)
	if baseChildren == 0 {
		t.Fatal("expected fiber child scope registered")
	}

	for i := 0; i < 5; i++ {
		disposeDep()
		waitForState(t, f, time.Second, StateInactive)
		disposeDep = mustProvide(t, ctx, keyStr, fmt.Sprintf("dep-%d", i))
		waitForState(t, f, time.Second, StateActive)
	}

	if got := len(ctx.effects); got != baseEffects {
		t.Fatalf("effects leak: base=%d now=%d", baseEffects, got)
	}
	if got := len(ctx.children); got != baseChildren {
		t.Fatalf("children leak: base=%d now=%d", baseChildren, got)
	}

	// 手工派生/销毁同样不应留下死登记。
	extra := ctx.Derive()
	extra.Dispose()
	if got := len(ctx.effects); got != baseEffects {
		t.Fatalf("manual derive leak: %d -> %d", baseEffects, got)
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

	disposeDep()
	waitForState(t, f, time.Second, StateInactive)

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
				return errors.New("boom")
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

// #8：Close 与进行中的 doLoad 竞态——已注销实例不得被救活。
func TestCloseDuringLoading(t *testing.T) {
	ctx := New()
	gate := make(chan struct{})
	entered := make(chan struct{})
	var block atomic.Bool

	disposeDep := mustProvide(t, ctx, keyStr, "dep")

	p := &countingPlugin{
		deps: []Dependency{Require(keyStr)},
		onApply: func(c *Context) error {
			if block.Load() {
				entered <- struct{}{}
				<-gate // 卡在 Apply 中间
				return errors.New("boom after close")
			}
			return nil
		},
	}
	f, err := Use(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, f, time.Second, StateActive)

	// 先卸到 Inactive，再以阻塞模式重装：依赖恢复 => 异步 doLoad。
	disposeDep()
	waitForState(t, f, time.Second, StateInactive)
	block.Store(true)
	disposeDep = mustProvide(t, ctx, keyStr, "dep-v2") // 触发重试装载
	<-entered                                          // doLoad 正在执行（Loading 态）

	f.Close() // 在 Apply 返回前注销
	close(gate)

	waitForState(t, f, time.Second, StateInactive)
	if s := f.State(); s != StateInactive {
		t.Fatalf("closed fiber ended in %s, want inactive", s)
	}
	if f.Err() != nil {
		t.Fatalf("closed fiber should carry no error, got %v", f.Err())
	}
}

func TestWaterfallOrderAndShortCircuit(t *testing.T) {
	ctx := New()
	type Request struct{ N int }
	ev := NewEventKey[Request]("test.req")

	unsub1, _ := OnWaterfall(ctx, ev, func(r Request, next func(Request) Request) Request {
		r.N += 1
		return next(r)
	})
	_, _ = OnWaterfall(ctx, ev, func(r Request, next func(Request) Request) Request {
		r.N += 10
		return r // 短路
	})
	_, _ = OnWaterfall(ctx, ev, func(r Request, next func(Request) Request) Request {
		r.N += 100
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

	Serial(ctx, ev, 7)
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

// #7：同一事件混用观察与 waterfall 监听器——各自独立派发，
// 不串台、不 panic。
func TestMixedListenerKinds(t *testing.T) {
	ctx := New()
	ev := NewEventKey[int]("test.mixed")

	var obsHit, wfHit int32
	_, _ = On(ctx, ev, func(p *int) { atomic.AddInt32(&obsHit, 1) })
	_, _ = OnWaterfall(ctx, ev, func(v int, next func(int) int) int {
		atomic.AddInt32(&wfHit, 1)
		return next(v + 1)
	})

	Serial(ctx, ev, 5)
	if atomic.LoadInt32(&obsHit) != 1 || atomic.LoadInt32(&wfHit) != 0 {
		t.Fatalf("emit crossed kinds: obs=%d wf=%d", obsHit, wfHit)
	}

	got := Waterfall(ctx, ev, 5)
	if atomic.LoadInt32(&obsHit) != 1 || atomic.LoadInt32(&wfHit) != 1 {
		t.Fatalf("waterfall crossed kinds: obs=%d wf=%d", obsHit, wfHit)
	}
	if got != 6 {
		t.Fatalf("waterfall result = %d", got)
	}
}

// #1：兄弟作用域上的监听必须能收到任意作用域的派发（全树广播）。
func TestEventListenersAcrossSiblingScopes(t *testing.T) {
	root := New()
	ev := NewEventKey[int]("test.sibling")

	var hit int32
	pListener := Func(func(c *Context) error {
		_, err := OnWaterfall(c, ev, func(v int, next func(int) int) int {
			atomic.AddInt32(&hit, 1)
			return next(v + 42)
		})
		return err
	})
	if _, err := Use(root, pListener); err != nil {
		t.Fatal(err)
	}

	sibling := root.Derive() // 与监听插件私有作用域平行的兄弟层
	got := Waterfall(sibling, ev, 0)
	if atomic.LoadInt32(&hit) != 1 {
		t.Fatal("sibling-scope listener never invoked")
	}
	if got != 42 {
		t.Fatalf("sibling waterfall result = %d, want 42", got)
	}
}

// #2：事件监听是效应——Apply 中丢弃 dispose，作用域销毁后监听消失；
// 已销毁作用域的监听不再被任何派发触达。
func TestEventDisposeOnScopeDispose(t *testing.T) {
	root := New()
	ev := NewEventKey[int]("test.dispose")

	child := root.Derive()
	if _, err := On(child, ev, func(p *int) { t.Error("dead-scope listener invoked") }); err != nil {
		t.Fatal(err)
	}
	child.Dispose()

	Emit(root, ev, 1)   // 不应触达已死层
	Waterfall(root, ev, 2)
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

// #2（审核补充）：并发注册/撤销/派发同一事件总线——锁统一后
// -race 必须保持干净（此前 add 无 bus 锁、collect 裸读切片的窗口
// 由本测试钉住）。
func TestEventBusConcurrentAddRemoveDispatch(t *testing.T) {
	ctx := New()
	ev := NewEventKey[int]("test.bus.race")

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// goroutine A：同层反复注册+撤销（add/remove 竞争窗口）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			d, err := OnWaterfall(ctx, ev, func(v int, next func(int) int) int { return next(v) })
			if err != nil {
				return
			}
			d()
		}
	}()

	// goroutine B：并发派发（collectListeners 读快照窗口）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = Waterfall(ctx, ev, 0)
		}
	}()

	// goroutine C：另一层并发观察注册 + 派发。
	child := ctx.Derive()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			d, err := On(child, ev, func(*int) {})
			if err == nil {
				Emit(ctx, ev, 7)
				d()
			}
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// 回归：同层多个插件的变更订阅互不误删。Use 为每个 Fiber 注册的
// 订阅闭包来自同一函数字面量（仅捕获变量不同），按函数代码指针
// 判等会把它们当成同一个订阅——第一个 Close 会摘掉别人的订阅。
func TestSiblingFiberSubscriptionNotCrossRemoved(t *testing.T) {
	ctx := New()
	disposeDep := mustProvide(t, ctx, keyStr, "dep")

	pa := &countingPlugin{deps: []Dependency{Require(keyStr)}}
	fa, err := Use(ctx, pa)
	if err != nil {
		t.Fatal(err)
	}
	pb := &countingPlugin{deps: []Dependency{Require(keyStr)}}
	fb, err := Use(ctx, pb)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, fa, time.Second, StateActive)
	waitForState(t, fb, time.Second, StateActive)

	fa.Close() // A 注销——不得连带摘除 B 的订阅

	disposeDep() // 依赖消失 => B 必须感知并卸载
	waitForState(t, fb, time.Second, StateInactive)
}

// ---- Loader ----

type tagPlugin struct {
	countingPlugin
	entryID string
	tag     string
	configured bool
}

func (p *tagPlugin) Configure(cfg map[string]any) error {
	p.tag, _ = cfg["tag"].(string)
	p.entryID, _ = cfg["entry"].(string)
	p.configured = true
	return nil
}

func TestLoaderReconcile(t *testing.T) {
	ctx := New()
	l := NewLoader(ctx)

	var applies int32
	var madeMu sync.Mutex
	var made []*tagPlugin // 工厂产出的实例，按创建顺序记录
	l.MustRegister("tagged", func() Plugin {
		p := &tagPlugin{
			countingPlugin: countingPlugin{
				onApply: func(c *Context) error {
					atomic.AddInt32(&applies, 1)
					return nil
				},
			},
		}
		madeMu.Lock()
		made = append(made, p)
		madeMu.Unlock()
		return p
	})

	// #4：两个条目各自的 config 私有且隔离——并且值各自正确到达。
	// （Reconcile 内部按 map 迭代，装载顺序不定，故条目在 Config 里
	// 自报身份来断言对应关系。）
	if err := l.Reconcile([]Entry{
		{ID: "a", Name: "tagged", Config: map[string]any{"entry": "a", "tag": "v1"}},
		{ID: "b", Name: "tagged", Config: map[string]any{"entry": "b", "tag": "v2"}},
	}); err != nil {
		t.Fatal(err)
	}
	fa, fb := l.Fiber("a"), l.Fiber("b")
	waitForState(t, fa, time.Second, StateActive)
	waitForState(t, fb, time.Second, StateActive)

	madeMu.Lock()
	if len(made) != 2 {
		t.Fatalf("instances built = %d, want 2", len(made))
	}
	instA, instB := made[0], made[1]
	madeMu.Unlock()

	if !instA.configured || !instB.configured {
		t.Fatal("Configure not invoked for both entries")
	}
	byID := map[string]*tagPlugin{instA.entryID: instA, instB.entryID: instB}
	if len(byID) != 2 {
		t.Fatalf("instances lost identity: %q %q", instA.entryID, instB.entryID)
	}
	if byID["a"].tag != "v1" || byID["b"].tag != "v2" {
		t.Fatalf("config crossed wires: a=%q b=%q, want v1/v2", byID["a"].tag, byID["b"].tag)
	}

	// Config 变化 => 重建（新实例拿到新值；旧实例不再被触碰）。
	before := atomic.LoadInt32(&applies)
	if err := l.Reconcile([]Entry{
		{ID: "a", Name: "tagged", Config: map[string]any{"entry": "a", "tag": "v1-changed"}},
		{ID: "b", Name: "tagged", Config: map[string]any{"entry": "b", "tag": "v2"}},
	}); err != nil {
		t.Fatal(err)
	}
	waitForState(t, l.Fiber("a"), time.Second, StateActive)

	madeMu.Lock()
	rebuilt := made[2]
	madeMu.Unlock()
	for _, old := range []*tagPlugin{instA, instB} {
		if rebuilt == old {
			t.Fatal("config change must rebuild the entry, not reuse instance")
		}
	}
	if rebuilt.entryID != "a" || rebuilt.tag != "v1-changed" {
		t.Fatalf("rebuilt = (%q,%q), want (a,v1-changed)", rebuilt.entryID, rebuilt.tag)
	}
	if got := atomic.LoadInt32(&applies); got-before < 1 {
		t.Fatal("config change did not re-apply entry a")
	}
	if byID["b"].tag != "v2" {
		t.Fatalf("untouched entry b mutated: %q", byID["b"].tag)
	}

	// Disabled => 卸载保留记录；另一条目实例与配置不受影响。
	if err := l.Reconcile([]Entry{
		{ID: "a", Name: "tagged", Disabled: true},
		{ID: "b", Name: "tagged", Config: map[string]any{"entry": "b", "tag": "v2"}},
	}); err != nil {
		t.Fatal(err)
	}
	if l.Fiber("a") != nil {
		t.Fatal("disabled entry should not keep a fiber")
	}
	waitForState(t, l.Fiber("b"), time.Second, StateActive)
	if byID["b"].tag != "v2" {
		t.Fatalf("entry b config disturbed by disabling a: %q", byID["b"].tag)
	}

	if err := l.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
	if len(l.Snapshot()) != 0 {
		t.Fatalf("snapshot after remove: %v", l.Snapshot())
	}
}

// #10c：多条目失败聚合可见。
func TestReconcileAggregatesErrors(t *testing.T) {
	ctx := New()
	l := NewLoader(ctx)
	err := l.Reconcile([]Entry{
		{ID: "x", Name: "nope1"},
		{ID: "y", Name: "nope2"},
	})
	if err == nil {
		t.Fatal("expected aggregated error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "nope1") || !strings.Contains(msg, "nope2") {
		t.Fatalf("aggregated error missing entries: %v", msg)
	}
}

func TestUnknownFactoryRejected(t *testing.T) {
	ctx := New()
	l := NewLoader(ctx)
	if err := l.Reconcile([]Entry{{ID: "x", Name: "nope"}}); err == nil {
		t.Fatal("expected unknown factory error")
	}
}

func TestConcurrentDisposeRace(t *testing.T) {
	ctx := New()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			child := ctx.Derive()
			_, _ = Provide(child, keyInt, i)
			_, _ = child.Effect(func() (func(), error) { return func() {}, nil })
			_, _ = On(child, NewEventKey[int]("race.ev"), func(*int) {})
			child.Dispose()
		}(i)
	}
	wg.Wait()
	ctx.Dispose()
}
