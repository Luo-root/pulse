package flow

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// ============================================================
// FlowContext 测试
// ============================================================

func TestFlowContext_SetAndGet(t *testing.T) {
	ctx := NewFlowContext(context.Background())
	ctx.Set("key1", "value1")

	val, err := ctx.Get("key1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "value1" {
		t.Fatalf("expected 'value1', got %v", val)
	}
}

func TestFlowContext_WaitAll_ContextCancelled(t *testing.T) {
	cancelCtx, cancel := context.WithCancel(context.Background())
	ctx := NewFlowContext(cancelCtx)

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := ctx.WaitAll("never_set")
	if err == nil {
		t.Fatal("expected error when context cancelled")
	}
}

func TestFlowContext_WaitForAny(t *testing.T) {
	ctx := NewFlowContext(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		ctx.Set("fast", "done")
	}()

	// WaitForAny 应该在 "fast" 设置后立即返回
	start := time.Now()
	key, val, err := ctx.WaitForAny("fast", "slow")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "fast" {
		t.Fatalf("expected key 'fast', got %q", key)
	}
	if val != "done" {
		t.Fatalf("expected 'done', got %v", val)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("WaitForAny took too long: %v", elapsed)
	}
}

func TestFlowContext_WaitForAny_AlreadyReady(t *testing.T) {
	ctx := NewFlowContext(context.Background())
	ctx.Set("ready", 42)

	key, val, err := ctx.WaitForAny("ready", "other")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "ready" || val != 42 {
		t.Fatalf("expected (ready, 42), got (%s, %v)", key, val)
	}
}

func TestFlowContext_Cancel(t *testing.T) {
	ctx := NewFlowContext(context.Background())
	testErr := errors.New("test error")

	ctx.Cancel(testErr)

	if ctx.Err() == nil {
		t.Fatal("expected error after cancel")
	}
	if !errors.Is(ctx.Err(), testErr) {
		t.Fatalf("expected test error, got %v", ctx.Err())
	}
}

func TestFlowContext_Cancel_FirstErrorWins(t *testing.T) {
	ctx := NewFlowContext(context.Background())

	ctx.Cancel(errors.New("first"))
	ctx.Cancel(errors.New("second"))

	if ctx.Err().Error() != "first" {
		t.Fatalf("expected first error, got %v", ctx.Err())
	}
}

func TestFlowContext_TryGet_NotReady(t *testing.T) {
	ctx := NewFlowContext(context.Background())

	_, ok := ctx.TryGet("missing")
	if ok {
		t.Fatal("expected false for missing key")
	}
}

func TestFlowContext_TryGet_Ready(t *testing.T) {
	ctx := NewFlowContext(context.Background())
	ctx.Set("key", "value")

	val, ok := ctx.TryGet("key")
	if !ok {
		t.Fatal("expected true")
	}
	if val != "value" {
		t.Fatalf("expected 'value', got %v", val)
	}
}

func TestFlowContext_ConcurrentSetAndGet(t *testing.T) {
	ctx := NewFlowContext(context.Background())
	const n = 100

	var wg sync.WaitGroup
	// 并发写入不同 key
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "key_" + string(rune('A'+i%26)) + "_" + string(rune('0'+i/26))
			ctx.Set(key, i)
		}(i)
	}
	wg.Wait()

	// 并发读取
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := "key_" + string(rune('A'+i%26)) + "_" + string(rune('0'+i/26))
			ctx.Get(key) // 可能成功，也可能 key 不存在（并发顺序）
		}(i)
	}
	wg.Wait()
}

// ============================================================
// DataSlot 测试
// ============================================================

func TestDataSlot_SetOnce_Idempotent(t *testing.T) {
	slot := NewDataSlot()
	slot.SetOnce("first")
	slot.SetOnce("second")

	ctx := context.Background()
	val, err := slot.Get(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "first" {
		t.Fatalf("expected 'first', got %v", val)
	}
}

func TestDataSlot_SetOrUpdate(t *testing.T) {
	slot := NewDataSlot()
	slot.SetOrUpdate("first")
	slot.SetOrUpdate("second")

	ctx := context.Background()
	val, err := slot.Get(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "second" {
		t.Fatalf("expected 'second', got %v", val)
	}
}

func TestDataSlot_Get_BlocksUntilReady(t *testing.T) {
	slot := NewDataSlot()

	go func() {
		time.Sleep(100 * time.Millisecond)
		slot.SetOnce("delayed")
	}()

	ctx := context.Background()
	start := time.Now()
	val, err := slot.Get(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "delayed" {
		t.Fatalf("expected 'delayed', got %v", val)
	}
	if elapsed < 50*time.Millisecond {
		t.Fatalf("Get returned too quickly: %v", elapsed)
	}
}

func TestDataSlot_Get_ContextCancelled(t *testing.T) {
	slot := NewDataSlot()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := slot.Get(ctx)
	if err == nil {
		t.Fatal("expected error when context cancelled")
	}
}

func TestDataSlot_Done_Channel(t *testing.T) {
	slot := NewDataSlot()

	select {
	case <-slot.Done():
		t.Fatal("Done channel should not be closed yet")
	default:
		// OK
	}

	slot.SetOnce("value")

	select {
	case <-slot.Done():
		// OK
	case <-time.After(time.Second):
		t.Fatal("Done channel should be closed after SetOnce")
	}
}

func TestDataSlot_ConcurrentSetAndGet(t *testing.T) {
	slot := NewDataSlot()
	const goroutines = 50

	var wg sync.WaitGroup

	// 多个 goroutine 同时 SetOnce（只有第一个生效）
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			slot.SetOnce(i)
		}(i)
	}

	// 多个 goroutine 同时 Get
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			slot.Get(ctx)
		}()
	}

	wg.Wait()

	// 验证值已设置
	ctx := context.Background()
	_, err := slot.Get(ctx)
	if err != nil {
		t.Fatalf("expected value to be set, got error: %v", err)
	}
}

// ============================================================
// SafeMap 测试
// ============================================================

func TestSafeMap_SetAndGet(t *testing.T) {
	var m SafeMap[string, int]
	m.Set("a", 1)

	val, ok := m.Get("a")
	if !ok || val != 1 {
		t.Fatalf("expected (1, true), got (%d, %v)", val, ok)
	}

	_, ok = m.Get("missing")
	if ok {
		t.Fatal("expected false for missing key")
	}
}

func TestSafeMap_GetOrSet(t *testing.T) {
	var m SafeMap[string, int]

	// 第一次创建
	val := m.GetOrSet("key", func() int { return 42 })
	if val != 42 {
		t.Fatalf("expected 42, got %d", val)
	}

	// 第二次不创建
	val = m.GetOrSet("key", func() int { return 99 })
	if val != 42 {
		t.Fatalf("expected 42 (existing), got %d", val)
	}
}
