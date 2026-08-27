package flow

import (
	"fmt"
)

// NodeFinishReason 是 NodeFinished 的终止原因。
type NodeFinishReason string

const (
	NodeCompleted NodeFinishReason = "completed"
	NodeSkipped   NodeFinishReason = "skipped"
	NodeFailed    NodeFinishReason = "failed"
	NodeCanceled  NodeFinishReason = "canceled"
)

// Observer 观察单次 Graph 运行里每个节点的生命周期。
// 默认无观察者（no-op）。实现必须并发安全：每个节点在独立 goroutine
// 里回调。panic / error 不得升格为节点失败（由 Graph 吞掉）。
//
// 每节点次数契约（E1）：Waiting ≤ 1、Running ≤ 1、Finished = 1。
// Retry 多次 attempt 不会重复打 Waiting/Running。
type Observer interface {
	OnNodeWaiting(nodeID string)
	OnNodeRunning(nodeID string)
	OnNodeFinished(nodeID string, reason NodeFinishReason, err error)
}

// ObserverFunc 把三个回调收成一个结构，便于测试与桥装配。
type ObserverFunc struct {
	Waiting  func(nodeID string)
	Running  func(nodeID string)
	Finished func(nodeID string, reason NodeFinishReason, err error)
}

// OnNodeWaiting 实现 Observer。
func (o ObserverFunc) OnNodeWaiting(nodeID string) {
	if o.Waiting != nil {
		o.Waiting(nodeID)
	}
}

// OnNodeRunning 实现 Observer。
func (o ObserverFunc) OnNodeRunning(nodeID string) {
	if o.Running != nil {
		o.Running(nodeID)
	}
}

// OnNodeFinished 实现 Observer。
func (o ObserverFunc) OnNodeFinished(nodeID string, reason NodeFinishReason, err error) {
	if o.Finished != nil {
		o.Finished(nodeID, reason, err)
	}
}

// MultiObserver 按序扇出；nil 成员跳过。
type MultiObserver []Observer

// OnNodeWaiting 实现 Observer。
func (m MultiObserver) OnNodeWaiting(nodeID string) {
	for _, o := range m {
		if o != nil {
			o.OnNodeWaiting(nodeID)
		}
	}
}

// OnNodeRunning 实现 Observer。
func (m MultiObserver) OnNodeRunning(nodeID string) {
	for _, o := range m {
		if o != nil {
			o.OnNodeRunning(nodeID)
		}
	}
}

// OnNodeFinished 实现 Observer。
func (m MultiObserver) OnNodeFinished(nodeID string, reason NodeFinishReason, err error) {
	for _, o := range m {
		if o != nil {
			o.OnNodeFinished(nodeID, reason, err)
		}
	}
}

// WithObserver 挂载图级生命周期观察者；后写覆盖前写（单槽）。
// 需要多个时用 MultiObserver 组合后再传入。
func WithObserver(o Observer) Option {
	return func(g *Graph) { g.observer = o }
}

// notify 调用观察者并吞掉 panic，避免只读 seam 变成节点失败。
func (g *Graph) notify(fn func(Observer)) {
	if g == nil || g.observer == nil || fn == nil {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			// 只读 seam：记录到 stderr 形态的字符串不够；保持静默，
			// 测试可用自定义 observer 自证。避免引入 log 依赖。
			_ = fmt.Sprintf("flow: observer panic: %v", rec)
		}
	}()
	fn(g.observer)
}

func finishReason(err error) NodeFinishReason {
	switch {
	case err == nil:
		return NodeCompleted
	case isSkipped(err):
		return NodeSkipped
	case isCanceled(err):
		return NodeCanceled
	default:
		return NodeFailed
	}
}
