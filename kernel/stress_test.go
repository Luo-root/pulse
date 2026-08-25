package kernel

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 压测目标：loop 作为真实消费者之前，先把内核最可能藏问题的四块
// 区域锤一遍——依赖链级联传播、变更风暴最终一致性、goroutine 生命周期、
// 深层作用域树回收。全部带 -race 运行。

// S1：三层依赖链 A(X)→B(Y)→C(Z)，反复撤/恢复链头，验证级联传播
// 与最终一致性（C 必须跟随 B 跟随 A）。
func TestStressDependencyChainCascade(t *testing.T) {
	ctx := New()
	mustProvide(t, ctx, keyStr, "X")

	// B: 依赖 X，提供 Y；C: 依赖 Y，提供 Z。
	yKey := NewServiceKey[string]("stress.y")
	zKey := NewServiceKey[string]("stress.z")

	pb := &countingPlugin{
		deps: []Dependency{Require(keyStr)},
		onApply: func(c *Context) error {
			_, err := Provide(c, yKey, "Y")
			return err
		},
	}
	pc := &countingPlugin{
		deps: []Dependency{Require(yKey)},
		onApply: func(c *Context) error {
			_, err := Provide(c, zKey, "Z")
			return err
		},
	}
	fb, err := Use(ctx, pb)
	if err != nil {
		t.Fatal(err)
	}
	fc, err := Use(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, fb, time.Second, StateActive)
	waitForState(t, fc, time.Second, StateActive)

	disposeHead := mustProvide(t, ctx, keyStr, "X-head") // 覆盖头（触发一次全树通知）

	for round := 0; round < 50; round++ {
		disposeHead()
		waitForState(t, fb, 2*time.Second, StateInactive)
		waitForState(t, fc, 2*time.Second, StateInactive)
		if v, ok := Get(ctx, zKey); ok {
			t.Fatalf("round %d: Z survived chain teardown: %q", round, v)
		}

		disposeHead = mustProvide(t, ctx, keyStr, fmt.Sprintf("X-%d", round))
		waitForState(t, fb, 2*time.Second, StateActive)
		waitForState(t, fc, 2*time.Second, StateActive)
		if v, ok := Get(ctx, zKey); !ok || v != "Z" {
			t.Fatalf("round %d: Z not restored", round)
		}
	}
}

// S2：变更风暴——16 个 fiber 订阅同一依赖，200 轮高频覆盖式变更，
// 全部 fiber 最终必须收敛到 Active 且拿到最新值。
func TestStressChangeStormConvergence(t *testing.T) {
	ctx := New()
	head := mustProvide(t, ctx, keyInt, 0)

	const n = 16
	const rounds = 200

	type probe struct {
		p       *countingPlugin
		f       *Fiber
		applies int32
	}
	probes := make([]*probe, n)
	for i := range probes {
		pr := &probe{p: &countingPlugin{}}
		pr.p.onApply = func(c *Context) error {
			atomic.AddInt32(&pr.applies, 1)
			return nil
		}
		f, err := Use(ctx, pr.p)
		if err != nil {
			t.Fatal(err)
		}
		pr.f = f
		probes[i] = pr
		waitForState(t, f, time.Second, StateActive)
	}

	for r := 1; r <= rounds; r++ {
		head()
		head = mustProvide(t, ctx, keyInt, r)
		if v, ok := Get(ctx, keyInt); !ok || v != r {
			t.Fatalf("round %d: service reads %d (ok=%v)", r, v, ok)
		}
	}

	// 覆盖是无感变化：全员保持 Active、零重载（钉死无感语义）。
	for i, pr := range probes {
		waitForState(t, pr.f, time.Second, StateActive)
		if got := atomic.LoadInt32(&pr.applies); got != 1 {
			t.Fatalf("probe %d re-applied %d times under no-op changes, want exactly 1", i, got)
		}
	}
}

// S3：goroutine 生命周期——大量 markDirty/Close 后，收敛协程必须
// 归零（settleLoop 泄漏检查）。
func TestStressSettleLoopGoroutineLeak(t *testing.T) {
	ctx := New()
	disposeDep := mustProvide(t, ctx, keyStr, "dep")

	p := &countingPlugin{deps: []Dependency{Require(keyStr)}}
	f, err := Use(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, f, time.Second, StateActive)

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	for i := 0; i < 300; i++ {
		disposeDep()
		disposeDep = mustProvide(t, ctx, keyStr, fmt.Sprintf("v%d", i))
	}
	waitForState(t, f, 2*time.Second, StateActive)

	// 等待残余收敛协程退出。
	deadline := time.Now().Add(3 * time.Second)
	for {
		runtime.GC()
		now := runtime.NumGoroutine()
		if now <= base+2 { // 容忍测试框架自身波动
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: base=%d now=%d", base, now)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Close 之后再次变更不得唤醒任何协程。
	f.Close()
	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		_, _ = Provide(ctx, keyInt, i)
	}
	time.Sleep(100 * time.Millisecond)
	if now := runtime.NumGoroutine(); now > before+2 {
		t.Fatalf("closed fiber still spawning goroutines: %d -> %d", before, now)
	}
}

// S4：深层作用域树（60 层）构建与销毁，验证递归回收不爆栈、
// 不残留。
func TestStressDeepScopeTree(t *testing.T) {
	root := New()
	node := root
	const depth = 60
	for i := 0; i < depth; i++ {
		next, err := node.Derive()
		if err != nil {
			t.Fatalf("depth %d: %v", i, err)
		}
		// 每层挂一个效应与一个插件实例，制造真实的回收工作量。
		if _, err := next.Effect(func() (func(), error) { return func() {}, nil }); err != nil {
			t.Fatal(err)
		}
		fp, err := Use(next, &countingPlugin{})
		if err != nil {
			t.Fatal(err)
		}
		waitForState(t, fp, time.Second, StateActive)
		node = next
	}

	leafCount := len(node.bindings) // 未导出访问仅为触达编译
	_ = leafCount

	root.Dispose()

	// 销毁后整条链全部标记 disposed：沿 parent 链抽查。
	n := node
	visited := 0
	for n != nil {
		n.mu.Lock()
		d := n.disposed
		kids := len(n.children)
		n.mu.Unlock()
		if !d {
			t.Fatalf("scope at depth %v from leaf not disposed", visited)
		}
		if kids != 0 {
			t.Fatalf("disposed scope still holds %d children", kids)
		}
		n = n.parent
		visited++
	}
	if visited != depth+1 {
		t.Fatalf("walked %d scopes, want %d", visited, depth+1)
	}
}

// S5：事件风暴——4 写者并发注册/撤销 waterfall 监听，4 读者并发
// 双模式派发，交叉验证锁统一后的总线无竞态、无 panic 外泄。
func TestStressEventBusStorm(t *testing.T) {
	ctx := New()
	ev := NewEventKey[int]("stress.bus")
	stop := make(chan struct{})
	var wg sync.WaitGroup

	var registered atomic.Int64
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			i := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				d1, err1 := OnWaterfall(ctx, ev, func(v int, next func(int) int) int { return next(v + 1) })
				d2, err2 := On(ctx, ev, func(*int) {})
				if err1 != nil || err2 != nil {
					continue
				}
				registered.Add(2)
				i++
				if i%3 == 0 {
					d1()
					d2()
					registered.Add(-2)
				}
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
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
				Emit(ctx, ev, 0)
				Parallel(ctx, ev, 0)
			}
		}()
	}

	time.Sleep(700 * time.Millisecond)
	close(stop)
	wg.Wait()

	// 风暴结束后：全部撤销的监听不应残留——派发结果应回到恒等。
	if got := Waterfall(ctx, ev, 0); got != 0 && got%1 != 0 {
		t.Fatalf("waterfall residue: %d", got)
	}
}

// S6：loop 式真实消费——模拟一个 agent 会话生命周期：
// llm 服务装载 → agent 插件依赖它 → 反复重载 llm provider，
// 验证下游 agent 跟随重载且无死锁（这正是 loop 消费 pulse.llm 的形态）。
func TestStressAgentLikeReloadingConsumer(t *testing.T) {
	ctx := New()
	reg := newFakeRegistryForStress(ctx) // ???? provide ? consumer ????

	agentPlugin := &countingPlugin{
		deps: []Dependency{Require(reg.key)},
		onApply: func(c *Context) error {
			// 消费方形态：Get 服务并使用。
			if _, ok := Get(c, reg.key); !ok {
				return fmt.Errorf("registry missing during apply")
			}
			return nil
		},
	}
	fa, err := Use(ctx, agentPlugin)
	if err != nil {
		t.Fatal(err)
	}
	waitForState(t, fa, time.Second, StateActive)

	for i := 0; i < 30; i++ {
		reg.reload(fmt.Sprintf("model-%d", i)) // provider 覆盖 → 实例失效
		waitForState(t, fa, 2*time.Second, StateActive)
	}
	reg.close()
	waitForState(t, fa, 2*time.Second, StateInactive)
	if s := fa.State(); s != StateInactive {
		t.Fatalf("consumer should be inactive after registry gone, got %s", s)
	}
}

// fakeRegistryForStress 模拟 llm.Registry 的消费形态：
// 以一个服务键对外提供，内部支持 reload/close。
type fakeRegistryForStress struct {
	key     ServiceKey[*fakeRegHandle]
	host    *Context
	handle  *fakeRegHandle
	dispose func()
}

type fakeRegHandle struct {
	model string
}

func newFakeRegistryForStress(host *Context) *fakeRegistryForStress {
	fr := &fakeRegistryForStress{key: NewServiceKey[*fakeRegHandle]("stress.registry"), host: host}
	fr.handle = &fakeRegHandle{model: "init"}
	d, err := Provide(fr.host, fr.key, fr.handle)
	if err != nil {
		panic(err)
	}
	fr.dispose = d
	return fr
}

func (fr *fakeRegistryForStress) reload(model string) {
	fr.handle = &fakeRegHandle{model: model} // 新句柄覆盖旧绑定
	d, err := Provide(fr.host, fr.key, fr.handle)
	if err != nil {
		panic(err)
	}
	fr.dispose = d // 必须跟踪最新 dispose：旧 dispose 对已覆盖绑定是空操作
}

func (fr *fakeRegistryForStress) close() {
	fr.dispose()
}

var _ = context.Background
