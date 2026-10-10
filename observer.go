package pulse

// NodeFinishReason 是 NodeFinished / GraphFinished 的终止原因。
//
// 图级只会出现三个：completed / failed / canceled——「跳过」是节点级的事实
// （一次运行不会因为某些节点跳过就整体跳过）。
type NodeFinishReason string

const (
	// NodeCompleted：Run 正常返回。返回后仍未写的 Provides 会被自动跳过，
	// 那不算失败。
	NodeCompleted NodeFinishReason = "completed"
	// NodeSkipped：本节点没进入 Run，或自己 Skip 了输出。没进入 Run 的判据
	// 是「输入一条值都没到」（全部 Requires 都以跳过到达），不是「有输入
	// 跳过」——到几个收几个（skip 是到达，不是失败）。
	NodeSkipped NodeFinishReason = "skipped"
	// NodeFailed：节点返回了真实错误——含 panic 被转成的错误，以及
	// Timeout 切面的节点超时。
	NodeFailed NodeFinishReason = "failed"
	// NodeCanceled：这一轮被别人拆掉了——首错取消整图、父 ctx 被取消或
	// 截止时间到期、排队等名额期间被取消。判据见 isCanceled。
	NodeCanceled NodeFinishReason = "canceled"
)

// 观测 attrs key 契约（归属 pulse 层）：字段语义的知识留在事实归属包，
// 折叠适配（observe.NewRecordObserver）只消费不定义——「同一出口 ≠
// Record 变万能袋」。
//
// key 约定 <组件>.<字段> 点分，各组件独立 key 空间互不冲突。
const (
	// AttrNode 是产生生命周期事件的节点 ID。
	AttrNode = "pulse.node"
	// AttrGraph 是产生生命周期事件的图 ID（New 的 graphID）：不同业务
	// 用不同图组装（node 可跨图复用），多图共用 scope 时区分所属图。
	AttrGraph = "pulse.graph"
	// AttrPath 是产生生命周期事件的**嵌套层级路径**：不透明字符串，`/` 连接
	// 各层（每一层是子图在它父图里的那个节点 id），**层级由出口自行拆分**——
	// 引擎不提供结构化树，也不定义「第几段」的语义。
	//
	// 值来自 Sub：根图的记录**不写**这个 key（`Attrs` 是「有才有」，空串会让
	// 「根」与「忘了传」长得一样），一层子图是 `"step1"`、再深一层是
	// `"step1/inner"`（见 sub.go 的 SubCtx.Path）。
	//
	// 与 AttrGraph 的分工：`pulse.graph` 是**你起的图 id**（多图复用同一出口时
	// 的归因），`pulse.path` 是**引擎记的层级**——同一张子图模板跑两遍就是两个
	// 图实例、两条 path，靠它分得开。
	AttrPath = "pulse.path"
)

// Observer 观察单次 Graph 运行：图级两条 + 每个节点三条。
// 默认无观察者（no-op）。实现必须并发安全：节点回调在各自的节点 goroutine
// 上，图回调在 Start / Wait 的调用方 goroutine 上。panic / error 不得升格为
// 节点失败（由 Graph 吞掉）。
//
// graphID 是图身份（New 的 graphID，必填）：随每次回调发出，实现侧
// 无需从构造参数另行携带——多图复用同一 Observer 实现时归因不漂移。
//
// 次数契约（冻结面）：
//
//   - 图级（E0）：GraphStarted ≤ 1、GraphFinished ≤ 1；
//   - 节点级（E1）：Waiting ≤ 1、Running ≤ 1、Finished = 1，
//     Retry 多次 attempt 不重复打 Waiting/Running。
//
// 时序：GraphStarted 在提交**任何**节点 goroutine 之前发出，GraphFinished 在
// **全部**节点终止之后发出——一段完整的观测里，图的两条天然把节点事件夹在中间。
// 并发 / 重复 `Wait` 都在 GraphFinished 那一发返回之后才返回（任何一个 Wait
// 返回时，本轮的收尾都已经在出口落地）。
//
// 图级两条在 `Start` / `Wait` 的调用路径上同步执行——所以**别在回调里调本图的
// `Start` / `Wait`**：那是同一个 goroutine 等自己，会死等。
type Observer interface {
	// OnGraphStarted 在图通过启动校验、开始提交节点时发出一次。
	// 校验失败与重复 Start 都到不了这里（那时图没有启动）。
	OnGraphStarted(graphID string)
	// OnGraphFinished 在 Wait 返回前发出一次：reason 是运行终态（图只会是
	// completed / failed / canceled），err 是首错（即 Graph.Err()）。
	// 只 Start 不 Wait 的宿主收不到它；重复 Wait 不重复发。
	OnGraphFinished(graphID string, reason NodeFinishReason, err error)
	OnNodeWaiting(graphID, nodeID string)
	OnNodeRunning(graphID, nodeID string)
	OnNodeFinished(graphID, nodeID string, reason NodeFinishReason, err error)
}

// ObserverFunc 把五条回调收成一个结构，便于测试与桥装配。
// 图级的两个字段带 Graph 前缀，节点的三个不带——前者是后加的，
// 改名会波及所有构造方，不值得为对称付这份破坏。
type ObserverFunc struct {
	GraphStarted  func(graphID string)
	GraphFinished func(graphID string, reason NodeFinishReason, err error)
	Waiting       func(graphID, nodeID string)
	Running       func(graphID, nodeID string)
	Finished      func(graphID, nodeID string, reason NodeFinishReason, err error)
}

// OnGraphStarted 实现 Observer。
func (o ObserverFunc) OnGraphStarted(graphID string) {
	if o.GraphStarted != nil {
		o.GraphStarted(graphID)
	}
}

// OnGraphFinished 实现 Observer。
func (o ObserverFunc) OnGraphFinished(graphID string, reason NodeFinishReason, err error) {
	if o.GraphFinished != nil {
		o.GraphFinished(graphID, reason, err)
	}
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
//
// **成员的 panic 被隔离在它自己那一格**：前面的观察者抛 panic，不会让后面的
// 观察者收不到这条事件——扇出的全部意义就是「组合多个出口」（例如宿主自己的
// observer 与 observe.NewRecordObserver），一个坏掉把邻居一起带走是最坏的结果。
// 引擎侧的 notify 另有一层兜底，保证 panic 不升格为节点失败。
type MultiObserver []Observer

// OnGraphStarted 实现 Observer。
func (m MultiObserver) OnGraphStarted(graphID string) {
	for _, o := range m {
		if o != nil {
			callSafely(func() { o.OnGraphStarted(graphID) })
		}
	}
}

// OnGraphFinished 实现 Observer。
func (m MultiObserver) OnGraphFinished(graphID string, reason NodeFinishReason, err error) {
	for _, o := range m {
		if o != nil {
			callSafely(func() { o.OnGraphFinished(graphID, reason, err) })
		}
	}
}

// OnNodeWaiting 实现 Observer。
func (m MultiObserver) OnNodeWaiting(graphID, nodeID string) {
	for _, o := range m {
		if o != nil {
			callSafely(func() { o.OnNodeWaiting(graphID, nodeID) })
		}
	}
}

// OnNodeRunning 实现 Observer。
func (m MultiObserver) OnNodeRunning(graphID, nodeID string) {
	for _, o := range m {
		if o != nil {
			callSafely(func() { o.OnNodeRunning(graphID, nodeID) })
		}
	}
}

// OnNodeFinished 实现 Observer。
func (m MultiObserver) OnNodeFinished(graphID, nodeID string, reason NodeFinishReason, err error) {
	for _, o := range m {
		if o != nil {
			callSafely(func() { o.OnNodeFinished(graphID, nodeID, reason, err) })
		}
	}
}

// callSafely 跑一个扇出成员的调用，把它抛出的 panic 限制在这一格内。
func callSafely(fn func()) {
	defer func() { _ = recover() }()
	fn()
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
