package kernel

import (
	"sync"
	"testing"
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
