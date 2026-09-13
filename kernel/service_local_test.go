package kernel

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件是 #170 的验收面：作用域局部绑定 Provide(..., Local())——
// 可见范围（本层 + 后代）、遮蔽全局、类型闸、撤除语义、并发隔离与
// Get 的链上开销。

func mustProvideLocal[T any](t *testing.T, c *Context, k ServiceKey[T], v T) func() {
	t.Helper()
	d, err := Provide(c, k, v, Local())
	if err != nil {
		t.Fatalf("Provide local %q: %v", k.Name(), err)
	}
	return d
}

// TestProvideLocalVisibility 可见范围：本 scope ✓、后代 ✓、父 ✗、兄弟 ✗。
func TestProvideLocalVisibility(t *testing.T) {
	root := New()
	defer root.Dispose()
	child, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	grand, err := child.Derive()
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}

	key := NewServiceKey[string]("test.local.vis")
	mustProvideLocal(t, child, key, "child-local")

	if v, ok := Get(child, key); !ok || v != "child-local" {
		t.Fatalf("child read = %q,%v", v, ok)
	}
	if v, ok := Get(grand, key); !ok || v != "child-local" {
		t.Fatalf("descendant read = %q,%v（后代必须可见）", v, ok)
	}
	if _, ok := Get(root, key); ok {
		t.Fatal("root must not see a child's local binding（父不可见）")
	}
	if _, ok := Get(sibling, key); ok {
		t.Fatal("sibling must not see another child's local binding（兄弟不可见）")
	}
}

// TestProvideLocalShadowsGlobal 同名局部遮蔽全局：子树内读局部，
// 子树外照读全局；子树销毁后子树侧回落全局。
func TestProvideLocalShadowsGlobal(t *testing.T) {
	root := New()
	defer root.Dispose()
	child, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}

	key := NewServiceKey[string]("test.local.shadow")
	mustProvide(t, root, key, "global")
	mustProvideLocal(t, child, key, "local")

	if v, _ := Get(child, key); v != "local" {
		t.Fatalf("child read = %q, want local（局部遮蔽全局）", v)
	}
	if v, _ := Get(root, key); v != "global" {
		t.Fatalf("root read = %q, want global（子树外不受影响）", v)
	}

	child.Dispose()
	if v, ok := Get(root, key); !ok || v != "global" {
		t.Fatalf("after child dispose: root read = %q,%v, want global", v, ok)
	}
}

// TestProvideLocalTypeConflict 类型闸：同名异类型（对照同层已有绑定与
// 全局同名绑定）一律拒绝。
func TestProvideLocalTypeConflict(t *testing.T) {
	root := New()
	defer root.Dispose()
	child, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}

	const name = "test.local.type"
	mustProvide(t, root, NewServiceKey[string](name), "global")

	if _, err := Provide(child, NewServiceKey[int](name), 42, Local()); err == nil {
		t.Fatal("local binding conflicting with the global type must be rejected")
	}
	if _, err := Provide(child, NewServiceKey[string](name), "l1", Local()); err != nil {
		t.Fatalf("same-type local binding: %v", err)
	}
	if _, err := Provide(child, NewServiceKey[int](name), 42, Local()); err == nil {
		t.Fatal("same-scope conflicting type must be rejected")
	}
}

// TestProvideLocalOverwriteAndRemoval 覆盖与撤除：同层后者胜；被覆盖方
// 的撤销是空操作；本次撤销与作用域销毁都能撤除。
func TestProvideLocalOverwriteAndRemoval(t *testing.T) {
	root := New()
	defer root.Dispose()
	child, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}

	key := NewServiceKey[string]("test.local.ow")
	undo1 := mustProvideLocal(t, child, key, "v1")
	undo2 := mustProvideLocal(t, child, key, "v2")

	if v, _ := Get(child, key); v != "v2" {
		t.Fatalf("read = %q, want v2（后者胜）", v)
	}
	undo1() // 被覆盖方的撤销不复活前值
	if v, _ := Get(child, key); v != "v2" {
		t.Fatalf("after stale undo read = %q, want v2", v)
	}
	undo2()
	if _, ok := Get(child, key); ok {
		t.Fatal("binding must be gone after its own undo")
	}

	mustProvideLocal(t, child, key, "v3")
	child.Dispose()
	if _, ok := Get(child, key); ok {
		t.Fatal("binding must be gone after scope dispose")
	}
}

// TestProvideLocalIsolationConcurrent 并发隔离（-race 下）：两个并列
// scope 各自登记同名局部绑定，读方各读各的；写方持续覆盖同值以压
// 写时复制路径。
func TestProvideLocalIsolationConcurrent(t *testing.T) {
	root := New()
	defer root.Dispose()
	key := NewServiceKey[string]("test.local.iso")

	a, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	b, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}
	mustProvideLocal(t, a, key, "A")
	mustProvideLocal(t, b, key, "B")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) { // 读者：各 scope 只应读到自己那份
			defer wg.Done()
			scope, want := a, "A"
			if i%2 == 1 {
				scope, want = b, "B"
			}
			for j := 0; j < 500; j++ {
				if v, ok := Get(scope, key); !ok || v != want {
					t.Errorf("concurrent read = %q,%v, want %q", v, ok, want)
					return
				}
			}
		}(i)
	}
	for _, w := range []struct {
		scope *Context
		value string
	}{{a, "A"}, {b, "B"}} {
		wg.Add(1)
		go func(scope *Context, value string) { // 写者：反复覆盖**自己的值**（压 copy-on-write）
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if _, err := Provide(scope, key, value, Local()); err != nil {
					t.Errorf("re-provide: %v", err)
					return
				}
			}
		}(w.scope, w.value)
	}
	wg.Wait()
}

// TestProvideLocalDoesNotSatisfyFiberDependency 守卫：局部绑定**不满足**
// fiber 依赖（依赖解析只看全局仓库）——仅局部时保持 Inactive；补全局
// 装载；撤全局卸载（局部仍在也不顶用）。「Active ⇒ 依赖满足」的不变式
// 因此不会被局部绑定的撤除打破。
func TestProvideLocalDoesNotSatisfyFiberDependency(t *testing.T) {
	root := New()
	defer root.Dispose()
	host, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}

	key := NewServiceKey[string]("test.local.dep")
	mustProvideLocal(t, host, key, "local-only") // 仅局部

	p := &countingPlugin{deps: []Dependency{Require(key)}}
	f, err := Use(host, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.State(); got != StateInactive {
		t.Fatalf("state = %s, want Inactive（局部绑定不得满足依赖）", got)
	}
	if n := atomic.LoadInt32(&p.applies); n != 0 {
		t.Fatalf("applies = %d, want 0", n)
	}

	undoGlobal := mustProvide(t, root, key, "global")
	waitForState(t, f, 2*time.Second, StateActive)
	if n := atomic.LoadInt32(&p.applies); n != 1 {
		t.Fatalf("applies = %d, want 1", n)
	}

	undoGlobal()
	waitForState(t, f, 2*time.Second, StateInactive) // 局部还在，但依赖只看全局
}

// TestProvideLocalDoesNotNotify 「不投递」契约：局部绑定的安装与撤除
// 都不产生变更投递；同层全局绑定照常投递（对照组）。
func TestProvideLocalDoesNotNotify(t *testing.T) {
	root := New()
	defer root.Dispose()
	child, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}

	key := NewServiceKey[string]("test.local.notify")
	var hits int32
	unsub := child.onChange(func() { atomic.AddInt32(&hits, 1) }, []string{key.Name()})
	defer unsub()

	undoLocal := mustProvideLocal(t, child, key, "v")
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("local provide delivered %d notifications, want 0", got)
	}
	undoLocal()
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("local undo delivered %d notifications, want 0", got)
	}

	undoGlobal := mustProvide(t, root, key, "g") // 全局：照常投递
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("global provide delivered %d notifications, want 1", got)
	}
	undoGlobal()
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("global undo delivered %d notifications, want 2", got)
	}
}

// BenchmarkGetGlobalOnly 请求 scope 读全局绑定（链上两层无局部：nil 检查 + 根锁）。
func BenchmarkGetGlobalOnly(b *testing.B) {
	root := New()
	defer root.Dispose()
	reqScope, err := root.Derive()
	if err != nil {
		b.Fatal(err)
	}
	key := NewServiceKey[string]("test.bench.get.global")
	if _, err := Provide(root, key, "v"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := Get(reqScope, key); !ok {
			b.Fatal("miss")
		}
	}
}

// BenchmarkGetWithLocal 请求 scope 读自己的局部绑定（近因优先命中）。
func BenchmarkGetWithLocal(b *testing.B) {
	root := New()
	defer root.Dispose()
	reqScope, err := root.Derive()
	if err != nil {
		b.Fatal(err)
	}
	key := NewServiceKey[string]("test.bench.get.local")
	if _, err := Provide(reqScope, key, "v", Local()); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := Get(reqScope, key); !ok {
			b.Fatal("miss")
		}
	}
}

// WaitingFor 是「插件为什么没激活」唯一可见的信号（#185 让消费方靠它排查）：
// 局部绑定满足 Get、不满足 Require，所以声明 Require 的 fiber 停在 inactive，
// 快照按 Inject 声明序给出未满足的服务名。
func TestWaitingForNamesUnmetLocalDependency(t *testing.T) {
	ctx := New()
	defer ctx.Dispose()

	key := NewServiceKey[int]("test.waitingfor.local")
	if _, err := Provide(ctx, key, 7, Local()); err != nil {
		t.Fatal(err)
	}
	if _, ok := Get(ctx, key); !ok {
		t.Fatal("local binding must be readable from its own scope")
	}

	p := &countingPlugin{deps: []Dependency{Require(key)}}
	f, err := Use(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.State(); got != StateInactive {
		t.Fatalf("state = %v, want inactive（局部绑定不满足依赖）", got)
	}
	if n := atomic.LoadInt32(&p.applies); n != 0 {
		t.Fatalf("Apply ran %d times, want 0", n)
	}

	var seen bool
	for _, s := range ctx.FiberSnapshots() {
		if s.Name != f.Name() {
			continue
		}
		seen = true
		if got := s.WaitingFor; len(got) != 1 || got[0] != key.Name() {
			t.Fatalf("WaitingFor = %v, want [%s]", got, key.Name())
		}
	}
	if !seen {
		t.Fatalf("fiber %q missing from FiberSnapshots", f.Name())
	}
}
