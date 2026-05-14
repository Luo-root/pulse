package node

import (
	"fmt"
	"sync"
	"time"

	"github.com/Luo-root/pulse/components/flow"
)

// ============================================================================
// 1. RetryInterceptor —— 重试切面
// ============================================================================

// RetryInterceptor 节点失败时自动重试
type RetryInterceptor struct {
	MaxAttempts int           // 最大尝试次数（至少为1）
	Delay       time.Duration // 每次重试间隔
	ShouldRetry func(err error) bool
}

// NewRetryInterceptor 创建重试拦截器
func NewRetryInterceptor(maxAttempts int, delay time.Duration) *RetryInterceptor {
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	return &RetryInterceptor{
		MaxAttempts: maxAttempts,
		Delay:       delay,
	}
}

func (r *RetryInterceptor) Before(ctx *flow.FlowContext, node Node)           {}
func (r *RetryInterceptor) After(ctx *flow.FlowContext, node Node, err error) {}

func (r *RetryInterceptor) Around(ctx *flow.FlowContext, node Node, next func() (map[string]any, error)) (map[string]any, error) {
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
// 2. TimeoutInterceptor —— 超时控制切面
// ============================================================================

// TimeoutInterceptor 限制节点执行时间，超时时返回错误
type TimeoutInterceptor struct {
	Timeout time.Duration
}

// NewTimeoutInterceptor 创建超时拦截器
func NewTimeoutInterceptor(timeout time.Duration) *TimeoutInterceptor {
	return &TimeoutInterceptor{Timeout: timeout}
}

func (t *TimeoutInterceptor) Before(ctx *flow.FlowContext, node Node)           {}
func (t *TimeoutInterceptor) After(ctx *flow.FlowContext, node Node, err error) {}

func (t *TimeoutInterceptor) Around(ctx *flow.FlowContext, node Node, next func() (map[string]any, error)) (map[string]any, error) {
	type result struct {
		out map[string]any
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := next()
		done <- result{out, err}
	}()

	timer := time.NewTimer(t.Timeout)
	defer timer.Stop()

	select {
	case r := <-done:
		return r.out, r.err
	case <-timer.C:
		return nil, fmt.Errorf("node %s execution timeout after %v", node.ID(), t.Timeout)
	}
}

// ============================================================================
// 3. CircuitBreakerInterceptor —— 熔断降级切面
// ============================================================================

// CircuitState 熔断器状态
type CircuitState int

const (
	StateClosed   CircuitState = iota // 关闭（正常通行）
	StateOpen                         // 打开（熔断）
	StateHalfOpen                     // 半开（试探）
)

// CircuitBreakerInterceptor 熔断降级拦截器
// 当连续失败次数达到阈值时，进入熔断状态，直接返回 Fallback，不再执行真实逻辑。
type CircuitBreakerInterceptor struct {
	mu               sync.Mutex
	state            CircuitState
	failureCount     int           // 连续失败计数（Closed 状态下）
	threshold        int           // 触发熔断的失败次数阈值
	timeout          time.Duration // 熔断持续时间，之后进入 HalfOpen
	lastFailTime     time.Time
	halfOpenCalls    int // HalfOpen 状态下已发出的试探调用数
	halfOpenSuccess  int // HalfOpen 状态下成功次数
	HalfOpenMaxCalls int // 半开时最多允许的试探次数，达到即关闭

	// FallbackFunc 降级函数，熔断时调用。若 nil，则返回错误。
	FallbackFunc func(ctx *flow.FlowContext, node Node) (map[string]any, error)
}

// NewCircuitBreakerInterceptor 创建熔断拦截器
// threshold: 触发熔断的失败次数阈值
// timeout: 熔断后多久尝试恢复（进入半开）
func NewCircuitBreakerInterceptor(threshold int, timeout time.Duration) *CircuitBreakerInterceptor {
	return &CircuitBreakerInterceptor{
		threshold:        threshold,
		timeout:          timeout,
		state:            StateClosed,
		HalfOpenMaxCalls: 3,
	}
}

func (cb *CircuitBreakerInterceptor) Before(ctx *flow.FlowContext, node Node)           {}
func (cb *CircuitBreakerInterceptor) After(ctx *flow.FlowContext, node Node, err error) {}

func (cb *CircuitBreakerInterceptor) Around(ctx *flow.FlowContext, node Node, next func() (map[string]any, error)) (map[string]any, error) {
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

// allow 判断是否允许本次调用
func (cb *CircuitBreakerInterceptor) allow() bool {
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

// recordResult 根据执行结果更新熔断器状态
func (cb *CircuitBreakerInterceptor) recordResult(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if err != nil {
		cb.lastFailTime = time.Now()
		if cb.state == StateHalfOpen {
			// 半开状态失败，立即再次熔断
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
				// 半开状态连续成功足够次数，关闭熔断
				cb.state = StateClosed
				cb.failureCount = 0
				cb.halfOpenCalls = 0
				cb.halfOpenSuccess = 0
			}
		} else {
			// 关闭状态下成功，重置失败计数
			cb.failureCount = 0
		}
	}
}

// ============================================================================
// 4. RecoveryInterceptor —— 全局异常捕获与兜底切面
// ============================================================================

// RecoveryInterceptor 捕获 panic 并执行兜底逻辑，防止单个节点拖垮整个工作流。
type RecoveryInterceptor struct {
	// FallbackFunc 兜底函数，接收 panic 值，返回兜底结果。
	// 若 nil，则 panic 被转为 error 返回。
	FallbackFunc func(ctx *flow.FlowContext, node Node, recoverVal any) (map[string]any, error)
}

// NewRecoveryInterceptor 创建兜底拦截器
func NewRecoveryInterceptor(fallback func(ctx *flow.FlowContext, node Node, recoverVal any) (map[string]any, error)) *RecoveryInterceptor {
	return &RecoveryInterceptor{FallbackFunc: fallback}
}

func (r *RecoveryInterceptor) Before(ctx *flow.FlowContext, node Node)           {}
func (r *RecoveryInterceptor) After(ctx *flow.FlowContext, node Node, err error) {}

func (r *RecoveryInterceptor) Around(ctx *flow.FlowContext, node Node, next func() (map[string]any, error)) (outputs map[string]any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			if r.FallbackFunc != nil {
				outputs, err = r.FallbackFunc(ctx, node, rec)
			} else {
				err = fmt.Errorf("panic in node %s: %v", node.ID(), rec)
			}
		}
	}()
	return next()
}
