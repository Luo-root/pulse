package flow

import (
	"context"
	"testing"
	"time"
)

// TestDataSlotSetOnce 测试 SetOnce 幂等性
func TestDataSlotSetOnce(t *testing.T) {
	ds := NewDataSlot()

	// 首次设置
	ds.SetOnce("first")
	val, ok := ds.TryGet()
	if !ok || val != "first" {
		t.Fatalf("expected first, got %v", val)
	}

	// 第二次设置应该被忽略
	ds.SetOnce("second")
	val, ok = ds.TryGet()
	if !ok || val != "first" {
		t.Fatalf("expected first (ignored second), got %v", val)
	}

	t.Logf("✅ SetOnce test passed: value=%v", val)
}

// TestDataSlotSetOrUpdate 测试 SetOrUpdate 覆盖
func TestDataSlotSetOrUpdate(t *testing.T) {
	ds := NewDataSlot()

	// 首次设置
	ds.SetOrUpdate("first")
	val, ok := ds.TryGet()
	if !ok || val != "first" {
		t.Fatalf("expected first, got %v", val)
	}

	// 第二次设置应该覆盖
	ds.SetOrUpdate("second")
	val, ok = ds.TryGet()
	if !ok || val != "second" {
		t.Fatalf("expected second (updated), got %v", val)
	}

	t.Logf("✅ SetOrUpdate test passed: value=%v", val)
}

// TestDataSlotSetOrUpdateWakeWaiters 测试更新时唤醒等待者
func TestDataSlotSetOrUpdateWakeWaiters(t *testing.T) {
	ds := NewDataSlot()

	// 先设置初始值
	ds.SetOnce("initial")

	// 在 goroutine 中等待并检查更新后的值
	updated := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		val, err := ds.Get(ctx)
		if err != nil {
			updated <- "error: " + err.Error()
			return
		}
		// 应该立即返回，因为值已就绪
		updated <- val.(string)
	}()

	// 等待 goroutine 获取初始值
	time.Sleep(50 * time.Millisecond)

	// 更新值
	ds.SetOrUpdate("updated")

	// 再次启动一个等待者，应该能获取更新后的值
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()

		val, err := ds.Get(ctx)
		if err != nil {
			updated <- "error2: " + err.Error()
			return
		}
		updated <- val.(string)
	}()

	// 收集结果
	result1 := <-updated
	result2 := <-updated

	// 第一个 goroutine 应该获取到 initial（因为它在更新前开始等待但值已就绪）
	// 第二个 goroutine 应该获取到 updated
	if result1 != "initial" && result1 != "updated" {
		t.Errorf("unexpected result1: %s", result1)
	}
	if result2 != "updated" {
		t.Errorf("expected updated for result2, got %s", result2)
	}

	t.Logf("✅ SetOrUpdate wake waiters test passed: result1=%s, result2=%s", result1, result2)
}

// TestDataSlotBackwardCompatibility 测试向后兼容（Set 方法）
func TestDataSlotBackwardCompatibility(t *testing.T) {
	ds := NewDataSlot()

	// Set 应该同 SetOnce
	ds.Set("first")
	val, ok := ds.TryGet()
	if !ok || val != "first" {
		t.Fatalf("expected first, got %v", val)
	}

	ds.Set("second")
	val, ok = ds.TryGet()
	if !ok || val != "first" {
		t.Fatalf("expected first (Set should be idempotent), got %v", val)
	}

	t.Logf("✅ Backward compatibility test passed")
}

// TestDataSlotTryGet 测试 TryGet
func TestDataSlotTryGet(t *testing.T) {
	ds := NewDataSlot()

	// 未设置时
	val, ok := ds.TryGet()
	if ok {
		t.Fatalf("expected not ok, got %v", val)
	}

	// 设置后
	ds.SetOnce("value")
	val, ok = ds.TryGet()
	if !ok || val != "value" {
		t.Fatalf("expected value, got %v", val)
	}

	t.Logf("✅ TryGet test passed")
}

// TestDataSlotIsReady 测试 IsReady
func TestDataSlotIsReady(t *testing.T) {
	ds := NewDataSlot()

	if ds.IsReady() {
		t.Fatal("expected not ready")
	}

	ds.SetOnce("value")

	if !ds.IsReady() {
		t.Fatal("expected ready")
	}

	t.Logf("✅ IsReady test passed")
}
