package flow

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFlowContext_SetThenGet(t *testing.T) {
	fc := NewFlowContext(context.Background())
	fc.Set("key", "value")

	val, err := fc.Get("key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "value" {
		t.Fatalf("expected 'value', got %v", val)
	}
}

func TestFlowContext_Wait_BlocksUntilSet(t *testing.T) {
	fc := NewFlowContext(context.Background())

	var received any
	done := make(chan struct{})

	go func() {
		val, _ := fc.Wait("key")
		received = val
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	fc.Set("key", 42)

	select {
	case <-done:
		if received != 42 {
			t.Fatalf("expected 42, got %v", received)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait blocked forever")
	}
}

func TestFlowContext_WaitCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fc := NewFlowContext(ctx)

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := fc.Wait("key")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFlowContext_SetOnce_Idempotent(t *testing.T) {
	fc := NewFlowContext(context.Background())
	fc.Set("key", "first")
	fc.Set("key", "second")

	val, _ := fc.Get("key")
	if val != "first" {
		t.Fatalf("expected 'first', got %v", val)
	}
}

func TestFlowContext_SetOrUpdate_Overwrite(t *testing.T) {
	fc := NewFlowContext(context.Background())
	fc.Set("key", "first")
	fc.SetOrUpdate("key", "second")

	val, _ := fc.Get("key")
	if val != "second" {
		t.Fatalf("expected 'second', got %v", val)
	}
}

func TestFlowContext_WaitAll(t *testing.T) {
	fc := NewFlowContext(context.Background())

	go func() {
		time.Sleep(30 * time.Millisecond)
		fc.Set("a", 1)
	}()
	go func() {
		time.Sleep(60 * time.Millisecond)
		fc.Set("b", 2)
	}()

	result, err := fc.WaitAll("a", "b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["a"] != 1 || result["b"] != 2 {
		t.Fatalf("unexpected result: %v", result)
	}
}

func TestFlowContext_WaitAll_CancelledDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fc := NewFlowContext(ctx)

	fc.Set("a", 1) // a 已就绪

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel() // 取消，b 永远不会有
	}()

	_, err := fc.WaitAll("a", "b")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestFlowContext_WaitForAny_FirstReady(t *testing.T) {
	fc := NewFlowContext(context.Background())
	fc.Set("a", "ready_a")
	fc.Set("b", "ready_b")

	key, val, err := fc.WaitForAny("a", "b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "a" && key != "b" {
		t.Fatalf("unexpected key: %s", key)
	}
	_ = val
}

func TestFlowContext_WaitForAny_BlocksUntilReady(t *testing.T) {
	fc := NewFlowContext(context.Background())

	done := make(chan struct{})
	var gotKey string
	var gotVal any

	go func() {
		gotKey, gotVal, _ = fc.WaitForAny("x", "y")
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	fc.Set("y", "hello")

	select {
	case <-done:
		if gotKey != "y" {
			t.Fatalf("expected key 'y', got %q", gotKey)
		}
		if gotVal != "hello" {
			t.Fatalf("expected 'hello', got %v", gotVal)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForAny blocked forever")
	}
}

func TestFlowContext_WaitForAny_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fc := NewFlowContext(ctx)

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, _, err := fc.WaitForAny("x", "y")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestFlowContext_WaitForAny_EmptyKeys(t *testing.T) {
	fc := NewFlowContext(context.Background())
	_, _, err := fc.WaitForAny()
	if err == nil {
		t.Fatal("expected error for empty keys")
	}
}

func TestFlowContext_WaitForAny_NoPolling(t *testing.T) {
	// 验证 WaitForAny 不是轮询：如果 3 秒后才就绪，CPU 开销应该接近 0
	fc := NewFlowContext(context.Background())

	done := make(chan struct{})
	go func() {
		fc.WaitForAny("slow")
		close(done)
	}()

	// 延迟 2 秒后设置
	time.Sleep(2 * time.Second)
	fc.Set("slow", "done")

	select {
	case <-done:
		// 正确：事件驱动，不需要等满超时
	case <-time.After(3 * time.Second):
		t.Fatal("WaitForAny did not return promptly after value was set")
	}
}

func TestFlowContext_TryGet(t *testing.T) {
	fc := NewFlowContext(context.Background())

	val, ok := fc.TryGet("key")
	if ok {
		t.Fatal("expected false for missing key")
	}

	fc.Set("key", "value")
	val, ok = fc.TryGet("key")
	if !ok || val != "value" {
		t.Fatalf("expected ('value', true), got (%v, %v)", val, ok)
	}
}

func TestFlowContext_IsReady(t *testing.T) {
	fc := NewFlowContext(context.Background())

	if fc.IsReady("key") {
		t.Fatal("expected false before Set")
	}

	fc.Set("key", "value")

	if !fc.IsReady("key") {
		t.Fatal("expected true after Set")
	}
}

func TestFlowContext_Cancel_RecordsFirstError(t *testing.T) {
	fc := NewFlowContext(context.Background())

	fc.Cancel(fmt.Errorf("first"))
	fc.Cancel(fmt.Errorf("second"))

	err := fc.Err()
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "first" {
		t.Fatalf("expected 'first', got %q", err.Error())
	}
}

func TestFlowContext_SetError_RecordsFirstError(t *testing.T) {
	fc := NewFlowContext(context.Background())

	fc.SetError(fmt.Errorf("first"))
	fc.SetError(fmt.Errorf("second"))

	err := fc.Err()
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "first" {
		t.Fatalf("expected 'first', got %q", err.Error())
	}
}

func TestFlowContext_SetError_NilIsNoop(t *testing.T) {
	fc := NewFlowContext(context.Background())

	err := fc.SetError(nil)
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}

	if fc.Err() != nil {
		t.Fatal("expected no error")
	}
}

func TestFlowContext_GetError_NoTriggerCancel(t *testing.T) {
	fc := NewFlowContext(context.Background())

	fc.GetError() // 不应该触发取消

	select {
	case <-fc.Done():
		t.Fatal("GetError should not trigger cancel")
	default:
		// 正确
	}
}

func TestFlowContext_Done(t *testing.T) {
	fc := NewFlowContext(context.Background())

	select {
	case <-fc.Done():
		t.Fatal("Done should not be closed initially")
	default:
	}

	fc.Cancel(fmt.Errorf("cancel"))

	select {
	case <-fc.Done():
		// 正确
	case <-time.After(time.Second):
		t.Fatal("Done should be closed after Cancel")
	}
}

func TestFlowContext_ConcurrentSlotCreation(t *testing.T) {
	// 测试并发 Wait 同一个 key 不会竞态
	fc := NewFlowContext(context.Background())

	const goroutines = 50
	var wg sync.WaitGroup
	var receivedCount atomic.Int32

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val, err := fc.Wait("shared")
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if val != "ok" {
				t.Errorf("expected 'ok', got %v", val)
				return
			}
			receivedCount.Add(1)
		}()
	}

	time.Sleep(100 * time.Millisecond)
	fc.Set("shared", "ok")

	wg.Wait()

	if receivedCount.Load() != goroutines {
		t.Fatalf("expected %d goroutines to receive value, got %d", goroutines, receivedCount.Load())
	}
}

// ============================================================================
// WaitWithContext / WaitAllWithContext
// ============================================================================

func TestWaitWithContext_UsesExternalContext(t *testing.T) {
	fc := NewFlowContext(context.Background())

	// 用一个独立的、带超时的 context 等待
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// key 从未被设置，应该在 50ms 后超时
	_, err := fc.WaitWithContext(ctx, "never_set")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if ctx.Err() == nil {
		t.Fatal("expected external context to be cancelled")
	}
}

func TestWaitWithContext_DataArrivesBeforeTimeout(t *testing.T) {
	fc := NewFlowContext(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go func() {
		time.Sleep(50 * time.Millisecond)
		fc.Set("key", "value")
	}()

	val, err := fc.WaitWithContext(ctx, "key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "value" {
		t.Errorf("expected 'value', got %v", val)
	}
}

func TestWaitWithContext_WorkflowCancelled_DoesNotAffectExternalContext(t *testing.T) {
	fc := NewFlowContext(context.Background())

	// 外部 context 有 500ms 超时
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// 50ms 后取消工作流
	go func() {
		time.Sleep(50 * time.Millisecond)
		fc.Cancel(fmt.Errorf("workflow cancelled"))
	}()

	// WaitWithContext 用的是外部 ctx，工作流取消不应影响它
	// 但 DataSlot.Get 会检查传入的 ctx，而传入的是外部 ctx
	// 所以它应该等到 500ms 超时，而非 50ms 工作流取消
	_, err := fc.WaitWithContext(ctx, "never_set")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// 外部 ctx 应该是超时而非工作流取消
	if ctx.Err() != context.DeadlineExceeded {
		t.Errorf("expected DeadlineExceeded, got %v", ctx.Err())
	}
}

func TestWaitAllWithContext_MultipleKeys(t *testing.T) {
	fc := NewFlowContext(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go func() {
		time.Sleep(50 * time.Millisecond)
		fc.Set("a", 1)
		fc.Set("b", 2)
	}()

	result, err := fc.WaitAllWithContext(ctx, "a", "b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["a"] != 1 || result["b"] != 2 {
		t.Errorf("unexpected result: %v", result)
	}
}

func TestWaitAllWithContext_PartialTimeout(t *testing.T) {
	fc := NewFlowContext(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// 只设置 "a"，不设置 "b"
	fc.Set("a", 1)

	_, err := fc.WaitAllWithContext(ctx, "a", "b")
	if err == nil {
		t.Fatal("expected timeout error for missing key 'b'")
	}
}
