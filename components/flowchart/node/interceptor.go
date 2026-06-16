package node

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ============================================================================
// RetryAspect —— 重试切面
// ============================================================================

// RetryAspect 节点失败时自动重试
type RetryAspect struct {
	MaxAttempts int           // 最大尝试次数（至少为1）
	Delay       time.Duration // 每次重试间隔
	ShouldRetry func(err error) bool
}

func NewRetryAspect(maxAttempts int, delay time.Duration) *RetryAspect {
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	return &RetryAspect{
		MaxAttempts: maxAttempts,
		Delay:       delay,
	}
}

func (r *RetryAspect) Around(ctx *AspectContext, node Node, next func() (map[string]any, error)) (map[string]any, error) {
	var out map[string]any
	var err error
	for i := 0; i < r.MaxAttempts; i++ {
		out, err = next()
		if err == nil {
			return out, nil
		}
		if r.ShouldRetry != nil && !r.ShouldRetry(err) {
			break
		}
		if i < r.MaxAttempts-1 && r.Delay > 0 {
			time.Sleep(r.Delay)
		}
	}
	return out, err
}

// ============================================================================
// TimeoutAspect —— 超时控制切面
// ============================================================================

// TimeoutAspect 限制节点执行时间，超时时取消本层 context 并返回错误
type TimeoutAspect struct {
	Timeout time.Duration
}

func NewTimeoutAspect(timeout time.Duration) *TimeoutAspect {
	return &TimeoutAspect{Timeout: timeout}
}

func (t *TimeoutAspect) Around(ctx *AspectContext, node Node, next func() (map[string]any, error)) (map[string]any, error) {
	type result struct {
		out map[string]any
		err error
	}
	done := make(chan result, 1)

	// 创建带超时的子 context，超时时自动取消
	timeoutCtx, cancel := WithTimeout(ctx, t.Timeout)
	defer cancel()

	go func() {
		out, err := next()
		done <- result{out, err}
	}()

	select {
	case r := <-done:
		return r.out, r.err
	case <-timeoutCtx.Done():
		return nil, fmt.Errorf("node %s execution timeout after %v", node.ID(), t.Timeout)
	}
}

// WithTimeout 创建带超时的 AspectContext 子节点
func WithTimeout(parent *AspectContext, timeout time.Duration) (*AspectContext, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent.Context(), timeout)
	return &AspectContext{Flow: parent.Flow, ctx: ctx, cancel: cancel}, cancel
}

// ============================================================================
// CircuitBreakerAspect —— 熔断降级切面
// ============================================================================

// CircuitState 熔断器状态
type CircuitState int

const (
	StateClosed   CircuitState = iota // 关闭（正常通行）
	StateOpen                         // 打开（熔断）
	StateHalfOpen                     // 半开（试探）
)

// CircuitBreakerAspect 熔断降级切面
// 当连续失败次数达到阈值时，进入熔断状态，直接返回 Fallback，不再执行真实逻辑
type CircuitBreakerAspect struct {
	mu               sync.Mutex
	state            CircuitState
	failureCount     int
	threshold        int
	timeout          time.Duration
	lastFailTime     time.Time
	halfOpenCalls    int
	halfOpenSuccess  int
	HalfOpenMaxCalls int

	FallbackFunc func(ctx *AspectContext, node Node) (map[string]any, error)
}

func NewCircuitBreakerAspect(threshold int, timeout time.Duration) *CircuitBreakerAspect {
	return &CircuitBreakerAspect{
		threshold:        threshold,
		timeout:          timeout,
		state:            StateClosed,
		HalfOpenMaxCalls: 3,
	}
}

func (cb *CircuitBreakerAspect) Around(ctx *AspectContext, node Node, next func() (map[string]any, error)) (map[string]any, error) {
	if !cb.allow() {
		if cb.FallbackFunc != nil {
			return cb.FallbackFunc(ctx, node)
		}
		return nil, fmt.Errorf("circuit breaker is OPEN for node %s", node.ID())
	}

	out, err := next()
	cb.recordResult(err)
	return out, err
}

func (cb *CircuitBreakerAspect) allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case StateClosed:
		return true
	case StateOpen:
		if time.Since(cb.lastFailTime) > cb.timeout {
			cb.state = StateHalfOpen
			cb.halfOpenCalls = 0
			cb.halfOpenSuccess = 0
			return true
		}
		return false
	case StateHalfOpen:
		if cb.halfOpenCalls < cb.HalfOpenMaxCalls {
			cb.halfOpenCalls++
			return true
		}
		return false
	}
	return false
}

func (cb *CircuitBreakerAspect) recordResult(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if err != nil {
		cb.lastFailTime = time.Now()
		if cb.state == StateHalfOpen {
			cb.state = StateOpen
			cb.halfOpenCalls = 0
			cb.halfOpenSuccess = 0
		} else if cb.state == StateClosed {
			cb.failureCount++
			if cb.failureCount >= cb.threshold {
				cb.state = StateOpen
			}
		}
	} else {
		if cb.state == StateHalfOpen {
			cb.halfOpenSuccess++
			if cb.halfOpenSuccess >= cb.HalfOpenMaxCalls {
				cb.state = StateClosed
				cb.failureCount = 0
				cb.halfOpenCalls = 0
				cb.halfOpenSuccess = 0
			}
		} else {
			cb.failureCount = 0
		}
	}
}
