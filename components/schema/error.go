package schema

import (
	"errors"
	"fmt"
)

var (
	ErrWorkflowRunning          = errors.New("workflow is already running")
	ErrWorkflowResetRunning     = errors.New("cannot reset a running workflow")
	ErrWorkflowSubmitNodeToPool = errors.New("failed to submit node to pool")
	ErrWorkflowClosed           = errors.New("workflow has been closed and cannot be used")
	ErrNoNodesSubmitted         = errors.New("no nodes were successfully submitted to the pool")
)

// LoopStatus 循环执行状态
type LoopStatus string

const (
	LoopStatusCompleted       LoopStatus = "completed"        // 正常完成
	LoopStatusMaxIterations   LoopStatus = "max_iterations"   // 达到最大循环次数
	LoopStatusTimeout         LoopStatus = "timeout"          // 超时
	LoopStatusCancelled       LoopStatus = "cancelled"        // 被取消
	LoopStatusConditionFailed LoopStatus = "condition_failed" // 条件不满足
)

// LoopResult 循环执行结果
type LoopResult struct {
	Status     LoopStatus `json:"status"`     // 结束状态
	Iterations int        `json:"iterations"` // 实际执行次数
	Message    string     `json:"message"`    // 详细描述
}

// String 实现 Stringer 接口
func (s LoopStatus) String() string {
	return string(s)
}

// Loop 相关的错误
var (
	ErrLoopCancelled = errors.New("loop cancelled")
	ErrLoopTimeout   = errors.New("loop timeout")
)

// NewLoopResult 创建循环结果
func NewLoopResult(status LoopStatus, iterations int, message string) *LoopResult {
	return &LoopResult{
		Status:     status,
		Iterations: iterations,
		Message:    message,
	}
}

// NewLoopCompletedResult 创建正常完成的循环结果
func NewLoopCompletedResult(iterations int) *LoopResult {
	return NewLoopResult(
		LoopStatusCompleted,
		iterations,
		fmt.Sprintf("completed: %d iterations", iterations),
	)
}

// NewLoopMaxIterationsResult 创建达到最大次数的循环结果
func NewLoopMaxIterationsResult(maxIterations, actualIterations int) *LoopResult {
	return NewLoopResult(
		LoopStatusMaxIterations,
		actualIterations,
		fmt.Sprintf("max_iterations_reached: %d/%d", actualIterations, maxIterations),
	)
}

// NewLoopTimeoutResult 创建超时的循环结果
func NewLoopTimeoutResult(iterations int) *LoopResult {
	return NewLoopResult(
		LoopStatusTimeout,
		iterations,
		fmt.Sprintf("timeout: executed %d iterations", iterations),
	)
}

// NewLoopCancelledResult 创建被取消的循环结果
func NewLoopCancelledResult(iterations int, err error) *LoopResult {
	return NewLoopResult(
		LoopStatusCancelled,
		iterations,
		fmt.Sprintf("cancelled: %v", err),
	)
}

// NewLoopConditionFailedResult 创建条件不满足的循环结果
func NewLoopConditionFailedResult(iterations int) *LoopResult {
	return NewLoopResult(
		LoopStatusConditionFailed,
		iterations,
		fmt.Sprintf("condition_failed: executed %d iterations", iterations),
	)
}

// IsSuccess 判断循环是否成功完成
func (r *LoopResult) IsSuccess() bool {
	return r.Status == LoopStatusCompleted || r.Status == LoopStatusConditionFailed
}

// IsError 判断循环是否因错误而结束
func (r *LoopResult) IsError() bool {
	return r.Status == LoopStatusCancelled || r.Status == LoopStatusTimeout
}

// ToMap 转换为 map，方便放入 FlowContext
func (r *LoopResult) ToMap() map[string]any {
	return map[string]any{
		"status":     r.Status,
		"iterations": r.Iterations,
		"message":    r.Message,
	}
}
