package node

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse/components/flowchart/flow"
)

func newTestAspectCtx() *AspectContext {
	return NewAspectContext(flow.NewFlowContext(context.Background()), context.Background())
}

func TestRetryAspect_SuccessOnThirdAttempt(t *testing.T) {
	r := NewRetryAspect(3, 10*time.Millisecond)
	var count atomic.Int32

	next := func() (map[string]any, error) {
		c := count.Add(1)
		if c < 3 {
			return nil, fmt.Errorf("fail %d", c)
		}
		return map[string]any{"ok": true}, nil
	}

	ac := newTestAspectCtx()
	out, err := r.Around(ac, mockNode("test"), next)
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

func TestRetryAspect_ShouldRetryFalse(t *testing.T) {
	r := NewRetryAspect(5, 10*time.Millisecond)
	r.ShouldRetry = func(err error) bool {
		return err.Error() != "fatal"
	}

	var count atomic.Int32
	next := func() (map[string]any, error) {
		count.Add(1)
		return nil, fmt.Errorf("fatal")
	}

	ac := newTestAspectCtx()
	_, err := r.Around(ac, mockNode("test"), next)
	if err == nil {
		t.Fatal("expected error")
	}
	if count.Load() != 1 {
		t.Fatalf("expected 1 attempt (no retry on fatal), got %d", count.Load())
	}
}

func TestCircuitBreaker_OpensOnThreshold(t *testing.T) {
	cb := NewCircuitBreakerAspect(3, 1*time.Second)

	ac := newTestAspectCtx()
	node := mockNode("test")
	failing := func() (map[string]any, error) {
		return nil, fmt.Errorf("fail")
	}

	for i := 0; i < 3; i++ {
		cb.Around(ac, node, failing)
	}

	_, err := cb.Around(ac, node, func() (map[string]any, error) {
		t.Fatal("should not execute when circuit is open")
		return nil, nil
	})
	if err == nil {
		t.Fatal("expected circuit breaker error")
	}
}

func TestCircuitBreaker_HalfOpen_Recovers(t *testing.T) {
	cb := NewCircuitBreakerAspect(2, 50*time.Millisecond)
	cb.HalfOpenMaxCalls = 1

	ac := newTestAspectCtx()
	node := mockNode("test")

	for i := 0; i < 2; i++ {
		cb.Around(ac, node, func() (map[string]any, error) {
			return nil, fmt.Errorf("fail")
		})
	}

	time.Sleep(100 * time.Millisecond)

	_, err := cb.Around(ac, node, func() (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatalf("expected success in half-open state, got %v", err)
	}
}

func TestRecoveryAspect_CatchesPanic(t *testing.T) {
	r := NewRecoveryAspect(nil)

	ac := newTestAspectCtx()
	node := mockNode("test")

	_, err := r.Around(ac, node, func() (map[string]any, error) {
		panic("boom")
	})
	if err == nil {
		t.Fatal("expected error from panic")
	}
}

func TestRecoveryAspect_FallbackFunc(t *testing.T) {
	r := NewRecoveryAspect(func(ctx *AspectContext, node Node, recoverVal any) (map[string]any, error) {
		return map[string]any{"recovered": true, "panic": recoverVal}, nil
	})

	ac := newTestAspectCtx()
	node := mockNode("test")

	out, err := r.Around(ac, node, func() (map[string]any, error) {
		panic("boom")
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["recovered"] != true {
		t.Fatalf("expected recovered=true, got %v", out)
	}
}

func TestRecoveryAspect_NoPanic(t *testing.T) {
	r := NewRecoveryAspect(nil)

	ac := newTestAspectCtx()
	node := mockNode("test")

	out, err := r.Around(ac, node, func() (map[string]any, error) {
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
