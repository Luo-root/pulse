package flow

// NodeFinishReason 是 NodeFinished 的终止原因。
type NodeFinishReason string

const (
	NodeCompleted NodeFinishReason = "completed"
	NodeSkipped   NodeFinishReason = "skipped"
	NodeFailed    NodeFinishReason = "failed"
	NodeCanceled  NodeFinishReason = "canceled"
)

// 观测 attrs key 契约（归属 flow 层）：官方适配 NewRecordObserver 折
// 叠节点分段记录时使用。
//
// key 约定 <组件>.<字段> 点分，各组件独立 key 空间互不冲突。
const (
	// AttrNode 是产生生命周期事件的节点 ID。
	AttrNode = "flow.node"
	// AttrGraph 是产生生命周期事件的图 ID（New 的 graphID）：不同业务
	// 用不同图组装（node 可跨图复用），多图共用 scope 时区分所属图。
	AttrGraph = "flow.graph"
)

// Observer 观察单次 Graph 运行里每个节点的生命周期。
// 默认无观察者（no-op）。实现必须并发安全：每个节点在独立 goroutine
// 里回调。panic / error 不得升格为节点失败（由 Graph 吞掉）。
//
// graphID 是图身份（New 的 graphID，必填）：随每次回调发出，实现侧
// 无需从构造参数另行携带——多图复用同一 Observer 实现时归因不漂移。
//
// 每节点次数契约（E1）：Waiting ≤ 1、Running ≤ 1、Finished = 1。
// Retry 多次 attempt 不会重复打 Waiting/Running。
type Observer interface {
	OnNodeWaiting(graphID, nodeID string)
	OnNodeRunning(graphID, nodeID string)
	OnNodeFinished(graphID, nodeID string, reason NodeFinishReason, err error)
}

// ObserverFunc 把三个回调收成一个结构，便于测试与桥装配。
type ObserverFunc struct {
	Waiting  func(graphID, nodeID string)
	Running  func(graphID, nodeID string)
	Finished func(graphID, nodeID string, reason NodeFinishReason, err error)
}

// OnNodeWaiting 实现 Observer。
func (o ObserverFunc) OnNodeWaiting(graphID, nodeID string) {
	if o.Waiting != nil {
		o.Waiting(graphID, nodeID)
	}
}

// OnNodeRunning 实现 Observer。
func (o ObserverFunc) OnNodeRunning(graphID, nodeID string) {
	if o.Running != nil {
		o.Running(graphID, nodeID)
	}
}

// OnNodeFinished 实现 Observer。
func (o ObserverFunc) OnNodeFinished(graphID, nodeID string, reason NodeFinishReason, err error) {
	if o.Finished != nil {
		o.Finished(graphID, nodeID, reason, err)
	}
}

// MultiObserver 按序扇出；nil 成员跳过。
type MultiObserver []Observer

// OnNodeWaiting 实现 Observer。
func (m MultiObserver) OnNodeWaiting(graphID, nodeID string) {
	for _, o := range m {
		if o != nil {
			o.OnNodeWaiting(graphID, nodeID)
		}
	}
}

// OnNodeRunning 实现 Observer。
func (m MultiObserver) OnNodeRunning(graphID, nodeID string) {
	for _, o := range m {
		if o != nil {
			o.OnNodeRunning(graphID, nodeID)
		}
	}
}

// OnNodeFinished 实现 Observer。
func (m MultiObserver) OnNodeFinished(graphID, nodeID string, reason NodeFinishReason, err error) {
	for _, o := range m {
		if o != nil {
			o.OnNodeFinished(graphID, nodeID, reason, err)
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
	defer func() { _ = recover() }() // 只读 seam：panic 静默，不升格为节点失败
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
