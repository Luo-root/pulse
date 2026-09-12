package kernel

import (
	"sync/atomic"
	"testing"
)

// 本文件是 #168 的投递契约验收面：服务变更按依赖名索引投递（只通知
// 声明了该名字的订阅者），并有索引卫生保证（装载 / 关闭 / 作用域销毁
// 都摘除条目）。

// TestServiceChangeDeliversOnlyToDependents 投递契约：无关服务名的变更
// 不打扰未声明它的订阅者（修复前是全树广播 + 订阅者自行过滤，本用例
// 在旧实现下会失败）。
func TestServiceChangeDeliversOnlyToDependents(t *testing.T) {
	root := New()
	defer root.Dispose()

	keyA := NewServiceKey[string]("test.notify.a")
	keyB := NewServiceKey[string]("test.notify.b")

	var hitsA, hitsB int32
	unsubA := root.onChange(func() { atomic.AddInt32(&hitsA, 1) }, []string{keyA.Name()})
	defer unsubA()
	unsubB := root.onChange(func() { atomic.AddInt32(&hitsB, 1) }, []string{keyB.Name()})
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

// TestServiceChangeDeliversAcrossScopes 索引放根作用域的正当性前提：
// 投递与订阅者所在层级无关——挂子作用域的订阅者能收到 root 的 Provide，
// 挂 root 的也能收到子作用域的 Provide。
func TestServiceChangeDeliversAcrossScopes(t *testing.T) {
	root := New()
	defer root.Dispose()
	child, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}

	keyA := NewServiceKey[string]("test.notify.x")
	keyB := NewServiceKey[string]("test.notify.y")

	var childHits, rootHits int32
	unsubChild := child.onChange(func() { atomic.AddInt32(&childHits, 1) }, []string{keyA.Name()})
	defer unsubChild()
	unsubRoot := root.onChange(func() { atomic.AddInt32(&rootHits, 1) }, []string{keyB.Name()})
	defer unsubRoot()

	mustProvide(t, root, keyA, "v") // root 安装 → 子作用域订阅者
	if got := atomic.LoadInt32(&childHits); got != 1 {
		t.Fatalf("child-scope subscriber hits = %d, want 1（root 的 Provide 必须可达）", got)
	}

	mustProvide(t, child, keyB, "v") // 子作用域安装 → root 订阅者
	if got := atomic.LoadInt32(&rootHits); got != 1 {
		t.Fatalf("root subscriber hits = %d, want 1（子作用域的 Provide 必须可达）", got)
	}
}

// TestOnChangeDedupesDeps 重复依赖名只登记一次：Provide 只投递一次，
// 撤销订阅后索引桶精确归零（去重下沉在 onChange）。
func TestOnChangeDedupesDeps(t *testing.T) {
	root := New()
	defer root.Dispose()
	key := NewServiceKey[string]("test.notify.dup")

	var hits int32
	unsub := root.onChange(func() { atomic.AddInt32(&hits, 1) },
		[]string{key.Name(), key.Name(), key.Name()})

	mustProvide(t, root, key, "v")
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("hits = %d, want 1（重复依赖名不得重复投递）", got)
	}

	unsub()
	root.mu.Lock()
	left := len(root.svcIndex[key.Name()])
	root.mu.Unlock()
	if left != 0 {
		t.Fatalf("index bucket = %d, want 0（撤销后精确归零）", left)
	}
}

// TestServiceIndexCleanup 索引卫生：反复装载 / 关闭与作用域销毁都要把
// 订阅者从根索引摘除——索引不随轮次增长。
func TestServiceIndexCleanup(t *testing.T) {
	root := New()
	defer root.Dispose()
	child, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}

	indexLen := func() int {
		root.mu.Lock()
		defer root.mu.Unlock()
		n := 0
		for _, list := range root.svcIndex {
			n += len(list)
		}
		return n
	}

	for round := 0; round < 3; round++ {
		f, err := Use(child, &countingPlugin{deps: []Dependency{Require(keyStr)}})
		if err != nil {
			t.Fatal(err)
		}
		if got := indexLen(); got != 1 {
			t.Fatalf("round %d: index entries after Use = %d, want 1", round, got)
		}
		f.Close()
		if got := indexLen(); got != 0 {
			t.Fatalf("round %d: index entries after Close = %d, want 0", round, got)
		}
	}

	if _, err := Use(child, &countingPlugin{deps: []Dependency{Require(keyStr)}}); err != nil {
		t.Fatal(err)
	}
	child.Dispose() // 作用域销毁兜底摘除该层全部订阅
	if got := indexLen(); got != 0 {
		t.Fatalf("index entries after scope dispose = %d, want 0", got)
	}
}
