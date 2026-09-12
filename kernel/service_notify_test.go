package kernel

import (
	"sync/atomic"
	"testing"
)

// 本文件是 #168 的投递契约验收面：服务变更按依赖名索引投递（只通知
// 声明了变更名的订阅者），并有索引卫生保证（装载/关闭/作用域销毁都摘除条目）。

// TestServiceChangeDeliversOnlyToDependents 投递契约：无关服务名的变更
// 不打扰未声明它的订阅者（修复前是全树广播 + 订阅者自行过滤，本用例
// 在旧实现下会失败）。
func TestServiceChangeDeliversOnlyToDependents(t *testing.T) {
	root := New()
	defer root.Dispose()

	keyA := NewServiceKey[string]("test.notify.a")
	keyB := NewServiceKey[string]("test.notify.b")

	var hitsA, hitsB int32
	unsubA := root.onChange(func(changed []string) { atomic.AddInt32(&hitsA, 1) }, []string{keyA.Name()})
	defer unsubA()
	unsubB := root.onChange(func(changed []string) { atomic.AddInt32(&hitsB, 1) }, []string{keyB.Name()})
	defer unsubB()

	undoA := mustProvide(t, root, keyA, "a")
	if got := atomic.LoadInt32(&hitsA); got != 1 {
		t.Fatalf("A hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&hitsB); got != 0 {
		t.Fatalf("B hits = %d, want 0（未声明的服务名不得投递）", got)
	}

	mustProvide(t, root, keyB, "b")
	if got := atomic.LoadInt32(&hitsB); got != 1 {
		t.Fatalf("B hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&hitsA); got != 1 {
		t.Fatalf("A hits = %d, want 1（未声明 B，不得追加投递）", got)
	}

	undoA() // 撤销绑定同样是一次变更
	if got := atomic.LoadInt32(&hitsA); got != 2 {
		t.Fatalf("A hits after undo = %d, want 2", got)
	}

	unsubA() // 撤销订阅后索引条目摘除，再变也不投递
	mustProvide(t, root, keyA, "a2")
	if got := atomic.LoadInt32(&hitsA); got != 2 {
		t.Fatalf("A hits after unsub = %d, want 2（索引条目应已摘除）", got)
	}
}

// TestServiceIndexCleanup 索引卫生：Fiber 关闭与作用域销毁都要把订阅者
// 从根索引摘除——反复装载/派生不让索引增长。
func TestServiceIndexCleanup(t *testing.T) {
	root := New()
	defer root.Dispose()
	child, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}

	indexLen := func(name string) int {
		root.mu.Lock()
		defer root.mu.Unlock()
		return len(root.svcIndex[name])
	}

	f, err := Use(child, &countingPlugin{deps: []Dependency{Require(keyStr)}})
	if err != nil {
		t.Fatal(err)
	}
	if got := indexLen(keyStr.Name()); got != 1 {
		t.Fatalf("index entries after Use = %d, want 1", got)
	}

	f.Close() // Fiber 关闭：摘除自己的索引条目
	if got := indexLen(keyStr.Name()); got != 0 {
		t.Fatalf("index entries after Close = %d, want 0", got)
	}

	if _, err := Use(child, &countingPlugin{deps: []Dependency{Require(keyStr)}}); err != nil {
		t.Fatal(err)
	}
	child.Dispose() // 作用域销毁：兜底摘除该层全部订阅
	root.mu.Lock()
	left := len(root.svcIndex)
	root.mu.Unlock()
	if left != 0 {
		t.Fatalf("index len after scope dispose = %d, want 0", left)
	}
}
