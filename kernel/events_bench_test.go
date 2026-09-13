package kernel

import (
	"testing"
)

// 本文件是事件派发的**常驻性能基线**（#177）：任何事件系统改动都应先跑它
// 对照。口径：分配数为一等证据（跨运行稳定），ns 只做同轮相对参考。

type benchEvPayload struct {
	A string
	B string
	N int64
}

var benchEvKey = NewEventKey[benchEvPayload]("test.bench.evt")

func benchScopeWithObservers(b *testing.B, n int) *Context {
	b.Helper()
	c := New()
	for i := 0; i < n; i++ {
		if _, err := On(c, benchEvKey, func(*benchEvPayload) {}); err != nil {
			b.Fatal(err)
		}
	}
	return c
}

// BenchmarkEmitLocal_NoListener 无监听器的派发下限。
func BenchmarkEmitLocal_NoListener(b *testing.B) {
	c := New()
	defer c.Dispose()
	p := benchEvPayload{A: "aaaa", B: "bbbb", N: 42}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		EmitLocal(c, benchEvKey, p)
	}
}

// BenchmarkEmitLocal_OneObserver 单监听器（请求路径的典型形态）。
func BenchmarkEmitLocal_OneObserver(b *testing.B) {
	c := benchScopeWithObservers(b, 1)
	defer c.Dispose()
	p := benchEvPayload{A: "aaaa", B: "bbbb", N: 42}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		EmitLocal(c, benchEvKey, p)
	}
}

// BenchmarkEmitLocal_ThreeObservers 多监听器扇出。
func BenchmarkEmitLocal_ThreeObservers(b *testing.B) {
	c := benchScopeWithObservers(b, 3)
	defer c.Dispose()
	p := benchEvPayload{A: "aaaa", B: "bbbb", N: 42}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		EmitLocal(c, benchEvKey, p)
	}
}

// BenchmarkEmit_TreeNoListener 全树派发（root + 两层子作用域，无监听器）。
func BenchmarkEmit_TreeNoListener(b *testing.B) {
	root := New()
	defer root.Dispose()
	child, err := root.Derive()
	if err != nil {
		b.Fatal(err)
	}
	if _, err := child.Derive(); err != nil {
		b.Fatal(err)
	}
	p := benchEvPayload{A: "aaaa", B: "bbbb", N: 42}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Emit(root, benchEvKey, p)
	}
}

// BenchmarkEmit_TreeLeafObserver 全树派发 + 叶子层单监听器。
func BenchmarkEmit_TreeLeafObserver(b *testing.B) {
	root := New()
	defer root.Dispose()
	child, err := root.Derive()
	if err != nil {
		b.Fatal(err)
	}
	leaf, err := child.Derive()
	if err != nil {
		b.Fatal(err)
	}
	if _, err := On(leaf, benchEvKey, func(*benchEvPayload) {}); err != nil {
		b.Fatal(err)
	}
	p := benchEvPayload{A: "aaaa", B: "bbbb", N: 42}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Emit(root, benchEvKey, p)
	}
}

// BenchmarkWaterfallLocal_TwoAround 两条 around 链。
func BenchmarkWaterfallLocal_TwoAround(b *testing.B) {
	c := New()
	defer c.Dispose()
	for i := 0; i < 2; i++ {
		if _, err := OnWaterfall(c, benchEvKey, func(p benchEvPayload, next func(benchEvPayload) benchEvPayload) benchEvPayload {
			return next(p)
		}); err != nil {
			b.Fatal(err)
		}
	}
	p := benchEvPayload{A: "aaaa", B: "bbbb", N: 42}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		WaterfallLocal(c, benchEvKey, p)
	}
}

// BenchmarkEventRegisterUnregister 注册/摘除路径（COW 复制重建的成本面）：
// 应保持低频路径量级，不得成为新的热点。
func BenchmarkEventRegisterUnregister(b *testing.B) {
	c := New()
	defer c.Dispose()
	// 预置 8 条，测「追加第 9 条 + 摘除」的复制成本。
	for i := 0; i < 8; i++ {
		if _, err := On(c, benchEvKey, func(*benchEvPayload) {}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		undo, err := On(c, benchEvKey, func(*benchEvPayload) {})
		if err != nil {
			b.Fatal(err)
		}
		undo()
	}
}
