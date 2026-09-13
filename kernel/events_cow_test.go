package kernel

import (
	"testing"
)

// 本文件是 #177 的语义护栏：事件派发改 COW 快照后，以下契约必须原样保持。
// 这些用例在重构前后都应通过——它们是「语义不变」的证据面。

type cowPayload struct {
	N int
}

var (
	cowKey = NewEventKey[cowPayload]("test.cow.evt")
)

// TestEventBusSnapshotWindow COW 快照窗口：取到快照后，
// add/remove 通过复制重建生效——旧快照不被改动（在途派发窗口），
// 新快照反映最新集合。
func TestEventBusSnapshotWindow(t *testing.T) {
	c := New()
	defer c.Dispose()

	l1 := &listener{kind: listenerObserve, fn: func(*cowPayload) {}}
	if err := c.events.add(cowKey.name, payloadType[cowPayload](), l1); err != nil {
		t.Fatal(err)
	}
	snapBefore := c.events.list(cowKey.name, listenerObserve)
	if len(snapBefore) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snapBefore))
	}

	// add 之后：旧快照不变（COW 不变式），新快照含两条。
	l2 := &listener{kind: listenerObserve, fn: func(*cowPayload) {}}
	if err := c.events.add(cowKey.name, payloadType[cowPayload](), l2); err != nil {
		t.Fatal(err)
	}
	if len(snapBefore) != 1 {
		t.Fatalf("旧快照被就地改写：len = %d, want 1（COW 不变式）", len(snapBefore))
	}
	if got := c.events.list(cowKey.name, listenerObserve); len(got) != 2 {
		t.Fatalf("新快照 len = %d, want 2", len(got))
	}

	// remove 之后：在途快照仍含被移除者（派发窗口语义，与重构前一致）。
	inFlight := c.events.list(cowKey.name, listenerObserve)
	c.events.remove(cowKey.name, l1)
	if len(inFlight) != 2 {
		t.Fatalf("在途快照被改写：len = %d, want 2（窗口内仍应含已摘除者）", len(inFlight))
	}
	if inFlight[0] != l1 {
		t.Fatal("在途快照的首元素应仍是 l1")
	}
	after := c.events.list(cowKey.name, listenerObserve)
	if len(after) != 1 || after[0] != l2 {
		t.Fatalf("摘除后快照 = %v, want 仅 l2", after)
	}

	// 摘到最后一条：整键删除。
	c.events.remove(cowKey.name, l2)
	if got := c.events.list(cowKey.name, listenerObserve); len(got) != 0 {
		t.Fatalf("全摘除后 = %v, want 空", got)
	}
}

// TestEventBusListZeroCopy list 返回**同一不可变快照**（零拷贝的直接断言）：
// 连续两次调用拿到同一底层数组；只有 add/remove 之后才换成新切片（COW）。
func TestEventBusListZeroCopy(t *testing.T) {
	c := New()
	defer c.Dispose()
	fn := func(*cowPayload) {}

	if err := c.events.add(cowKey.name, payloadType[cowPayload](), &listener{kind: listenerObserve, fn: fn}); err != nil {
		t.Fatal(err)
	}
	a := c.events.list(cowKey.name, listenerObserve)
	b := c.events.list(cowKey.name, listenerObserve)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("len a=%d b=%d, want 1", len(a), len(b))
	}
	if &a[0] != &b[0] {
		t.Fatal("连续两次 list 应返回同一快照（零拷贝），不得每次新建切片")
	}

	if err := c.events.add(cowKey.name, payloadType[cowPayload](), &listener{kind: listenerObserve, fn: fn}); err != nil {
		t.Fatal(err)
	}
	after := c.events.list(cowKey.name, listenerObserve)
	if len(after) != 2 {
		t.Fatalf("add 后快照 len=%d, want 2", len(after))
	}
	if &after[0] == &a[0] {
		t.Fatal("add 后应换成新切片（COW 不变式）")
	}
}

// TestEventBusKindSeparation observe / waterfall 两张表互不干扰。
func TestEventBusKindSeparation(t *testing.T) {
	c := New()
	defer c.Dispose()

	obs := &listener{kind: listenerObserve, fn: func(*cowPayload) {}}
	wf := &listener{kind: listenerWaterfall, fn: func(cowPayload, func(cowPayload) cowPayload) cowPayload { return cowPayload{} }}
	if err := c.events.add(cowKey.name, payloadType[cowPayload](), obs); err != nil {
		t.Fatal(err)
	}
	if err := c.events.add(cowKey.name, payloadType[cowPayload](), wf); err != nil {
		t.Fatal(err)
	}

	if got := c.events.list(cowKey.name, listenerObserve); len(got) != 1 || got[0] != obs {
		t.Fatalf("observe 表 = %v, want 仅 obs", got)
	}
	if got := c.events.list(cowKey.name, listenerWaterfall); len(got) != 1 || got[0] != wf {
		t.Fatalf("waterfall 表 = %v, want 仅 wf", got)
	}

	// 摘除 observe 不影响 waterfall。
	c.events.remove(cowKey.name, obs)
	if got := c.events.list(cowKey.name, listenerWaterfall); len(got) != 1 {
		t.Fatalf("摘除 observe 后 waterfall 表 = %v, want 1 条", got)
	}
}

// TestEventTypeMismatchStillRejected 同名同类型校验不受 COW 改动影响。
func TestEventTypeMismatchStillRejected(t *testing.T) {
	c := New()
	defer c.Dispose()

	other := NewEventKey[cowPayload]("test.cow.evt") // 同类型：允许
	if _, err := On(c, other, func(*cowPayload) {}); err != nil {
		t.Fatalf("同类型监听应允许：%v", err)
	}
	// 同名不同类型的键：注册应被拒。
	type otherPayload struct{ X int }
	mismatch := NewEventKey[otherPayload]("test.cow.evt")
	if _, err := On(c, mismatch, func(*otherPayload) {}); err == nil {
		t.Fatal("同名不同类型的监听应被拒")
	}
}

// TestEmitLocalDispatchOrderAndIsolation 注册顺序 = 派发顺序；只本层；
// observe 与 waterfall 互不参与。
func TestEmitLocalDispatchOrderAndIsolation(t *testing.T) {
	root := New()
	defer root.Dispose()
	child, err := root.Derive()
	if err != nil {
		t.Fatal(err)
	}

	var order []int
	for i := 1; i <= 3; i++ {
		i := i
		if _, err := On(child, cowKey, func(*cowPayload) { order = append(order, i) }); err != nil {
			t.Fatal(err)
		}
	}
	var wfRan bool
	if _, err := OnWaterfall(child, cowKey, func(p cowPayload, next func(cowPayload) cowPayload) cowPayload {
		wfRan = true
		return next(p)
	}); err != nil {
		t.Fatal(err)
	}

	// root 上挂一个监听：EmitLocal 不应触达它（隔离面）。
	rootHit := false
	if _, err := On(root, cowKey, func(*cowPayload) { rootHit = true }); err != nil {
		t.Fatal(err)
	}

	EmitLocal(child, cowKey, cowPayload{})
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("派发顺序 = %v, want [1 2 3]", order)
	}
	if wfRan {
		t.Fatal("EmitLocal 不应触达 waterfall 监听器")
	}
	if rootHit {
		t.Fatal("EmitLocal 不应触达父层监听器（隔离面）")
	}
}

// TestDispatchWindowAddDuringDispatch 派发期间新增的监听器不参与本次派发
// （COW 快照语义：本次迭代拿到的是进入时的不可变切片），下一次派发才生效。
func TestDispatchWindowAddDuringDispatch(t *testing.T) {
	c := New()
	defer c.Dispose()

	var calls []string
	if _, err := On(c, cowKey, func(*cowPayload) {
		calls = append(calls, "first")
		if len(calls) == 1 {
			// 派发中途再注册一个：不得进本次派发。
			if _, err := On(c, cowKey, func(*cowPayload) { calls = append(calls, "late") }); err != nil {
				t.Errorf("late On: %v", err)
			}
		}
	}); err != nil {
		t.Fatal(err)
	}

	EmitLocal(c, cowKey, cowPayload{})
	if len(calls) != 1 || calls[0] != "first" {
		t.Fatalf("首次派发 calls = %v, want 仅 first（快照窗口）", calls)
	}
	// 第二次派发的快照为 [first, late]（注册顺序），逐个调用即 [first, first, late]。
	// 注意此时 first 是第二次被执行，测试里的注册守卫不会再新增监听。
	EmitLocal(c, cowKey, cowPayload{})
	if len(calls) != 3 || calls[0] != "first" || calls[1] != "first" || calls[2] != "late" {
		t.Fatalf("二次派发 calls = %v, want [first first late]（新监听器只进下一次派发）", calls)
	}
}

// TestDispatchWindowRemoveDuringDispatch 派发期间摘除「尚未被调用」的监听器：
// 在途快照仍会调用它一次（与重构前的 copyMatching 快照语义一致）。
func TestDispatchWindowRemoveDuringDispatch(t *testing.T) {
	c := New()
	defer c.Dispose()

	var calls []string
	var undo func()
	if _, err := On(c, cowKey, func(*cowPayload) {
		calls = append(calls, "first")
		undo() // 摘除后面那个（尚未被调用）
	}); err != nil {
		t.Fatal(err)
	}
	var err error
	undo, err = On(c, cowKey, func(*cowPayload) { calls = append(calls, "second") })
	if err != nil {
		t.Fatal(err)
	}

	EmitLocal(c, cowKey, cowPayload{})
	if len(calls) != 2 || calls[0] != "first" || calls[1] != "second" {
		t.Fatalf("calls = %v, want [first second]（在途快照窗口）", calls)
	}
	EmitLocal(c, cowKey, cowPayload{})
	if len(calls) != 3 || calls[2] != "first" {
		t.Fatalf("二次派发 calls = %v, want 摘除后只剩 first", calls)
	}
}

// TestWaterfallChainOrderUnchanged around 链的包裹顺序（先注册在外层）。
func TestWaterfallChainOrderUnchanged(t *testing.T) {
	c := New()
	defer c.Dispose()

	var trace []string
	if _, err := OnWaterfall(c, cowKey, func(p cowPayload, next func(cowPayload) cowPayload) cowPayload {
		trace = append(trace, "outer-in")
		out := next(p)
		trace = append(trace, "outer-out")
		return out
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := OnWaterfall(c, cowKey, func(p cowPayload, next func(cowPayload) cowPayload) cowPayload {
		trace = append(trace, "inner-in")
		p.N++
		out := next(p)
		trace = append(trace, "inner-out")
		return out
	}); err != nil {
		t.Fatal(err)
	}

	got := WaterfallLocal(c, cowKey, cowPayload{N: 1})
	want := []string{"outer-in", "inner-in", "inner-out", "outer-out"}
	if len(trace) != len(want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
	for i := range want {
		if trace[i] != want[i] {
			t.Fatalf("trace = %v, want %v", trace, want)
		}
	}
	if got.N != 2 {
		t.Fatalf("载荷就地改写应沿链可见：N = %d, want 2", got.N)
	}
}
