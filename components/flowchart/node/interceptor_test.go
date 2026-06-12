package node

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/flow"
)

func TestRetryInterceptor_SuccessOnThirdAttempt(t *testing.T) {
	r := NewRetryInterceptor(3, 10*time.Millisecond)
	var count atomic.Int32

	next := func() (map[string]any, error) {
		c := count.Add(1)
		if c < 3 {
			return nil, fmt.Errorf("fail %d", c)
		}
		return map[string]any{"ok": true}, nil
	}

	ctx := flow.NewFlowContext(context.Background())
	out, err := r.Around(ctx, mockNode("test"), next)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["ok"] != true {
		t.Fatalf("unexpected output: %v", out)
	}
	if count.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", count.Load())
	}
}

func TestRetryInterceptor_ShouldRetryFalse(t *testing.T) {
	r := NewRetryInterceptor(5, 10*time.Millisecond)
	r.ShouldRetry = func(err error) bool {
		return err.Error() != "fatal" // "fatal" 不重试
	}

	var count atomic.Int32
	next := func() (map[string]any, error) {
		count.Add(1)
		return nil, fmt.Errorf("fatal")
	}

	ctx := flow.NewFlowContext(context.Background())
	_, err := r.Around(ctx, mockNode("test"), next)
	if err == nil {
		t.Fatal("expected error")
	}
	if count.Load() != 1 {
		t.Fatalf("expected 1 attempt (no retry on fatal), got %d", count.Load())
	}
}

func TestCircuitBreaker_OpensOnThreshold(t *testing.T) {
	cb := NewCircuitBreakerInterceptor(3, 1*time.Second)

	ctx := flow.NewFlowContext(context.Background())
	node := mockNode("test")
	failing := func() (map[string]any, error) {
		return nil, fmt.Errorf("fail")
	}

	// 失败 3 次，触发熔断
	for i := 0; i < 3; i++ {
		cb.Around(ctx, node, failing)
	}

	// 第 4 次应该被熔断
	_, err := cb.Around(ctx, node, func() (map[string]any, error) {
		t.Fatal("should not execute when circuit is open")
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected circuit breaker error")
	}
}

func TestCircuitBreaker_HalfOpen_Recovers(t *testing.T) {
	cb := NewCircuitBreakerInterceptor(2, 50*time.Millisecond)
	cb.HalfOpenMaxCalls = 1

	ctx := flow.NewFlowContext(context.Background())
	node := mockNode("test")

	// 触发熔断
	for i := 0; i < 2; i++ {
		cb.Around(ctx, node, func() (map[string]any, error) {
			return nil, fmt.Errorf("fail")
		})
	}

	// 等待进入半开
	time.Sleep(100 * time.Millisecond)

	// 半开状态下成功一次
	_, err := cb.Around(ctx, node, func() (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatalf("expected success in half-open state, got %v", err)
	}
}

func TestRecoveryInterceptor_CatchesPanic(t *testing.T) {
	r := NewRecoveryInterceptor(nil)

	ctx := flow.NewFlowContext(context.Background())
	node := mockNode("test")

	_, err := r.Around(ctx, node, func() (map[string]any, error) {
		panic("boom")
	})
	if err == nil {
		t.Fatal("expected error from panic")
	}
}

func TestRecoveryInterceptor_FallbackFunc(t *testing.T) {
	r := NewRecoveryInterceptor(func(ctx *flow.FlowContext, node Node, recoverVal any) (map[string]any, error) {
		return map[string]any{"recovered": true, "panic": recoverVal}, nil
	})

	ctx := flow.NewFlowContext(context.Background())
	node := mockNode("test")

	out, err := r.Around(ctx, node, func() (map[string]any, error) {
		panic("boom")
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["recovered"] != true {
		t.Fatalf("expected recovered=true, got %v", out)
	}
}

func TestRecoveryInterceptor_NoPanic(t *testing.T) {
	r := NewRecoveryInterceptor(nil)

	ctx := flow.NewFlowContext(context.Background())
	node := mockNode("test")

	out, err := r.Around(ctx, node, func() (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["ok"] != true {
		t.Fatalf("unexpected output: %v", out)
	}
}

func mockNode(id string) Node {
	return NewNode(id, nil, nil, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
		return nil, nil
	})
}
