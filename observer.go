package pulse

import "fmt"

// NodeFinishReason 是 NodeFinished / GraphFinished 的终止原因。
//
// 图级只会出现三个：completed / failed / canceled——「跳过」是节点级的事实
// （一次运行不会因为某些节点跳过就整体跳过）。
type NodeFinishReason string

const (
	// NodeCompleted：Run 正常返回。返回后仍未写的 Provides 会被自动跳过，
	// 那不算失败。
	NodeCompleted NodeFinishReason = "completed"
	// NodeSkipped：全部 Requires 都以跳过到达，或节点/切面返回正常跳过
	// （如 NoValue 或 *SkipError）。输入只要有一条值就进入 Run，到几个收几个。
	// 单独 Skip 某条输出后返回 nil 仍是 NodeCompleted；可靠提交失败仍是失败。
	NodeSkipped NodeFinishReason = "skipped"
	// NodeFailed：节点返回了真实错误——含 panic 被转成的错误，以及
	// Timeout 切面的节点超时。
	NodeFailed NodeFinishReason = "failed"
	// NodeCanceled：这一轮被别人拆掉了——首错取消整图、父 ctx 被取消或
	// 截止时间到期、排队等名额期间被取消。独立 RPC 的 context 错误在执行域仍
	// 有效时属于 NodeFailed；归因在引擎自行取消前锁存，判据见 isCanceled。
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
	// 值与 `Site.Path` 同源同格式（根图 `""`、一层 `"step1"`、再深
	// `"step1/inner"`）：出口直接从回调拿到的 `Site` 里读，不必再按层建出口。
	// 根图的记录**不写**这个 key（`Attrs` 是「有才有」，空串会让「根」与
	// 「忘了传」长得一样）。
	//
	// 与 AttrGraph / AttrExecution 的分工：`pulse.graph` 是**你起的图 id**（定义身份），
	// `pulse.path` 是**挂载位置**（同一张子图挂两处就是两条 path），
	// `pulse.execution` 是**这一次运行**（同一位置跑两次是两个执行）。
	AttrPath = "pulse.path"
	// AttrExecution 是产生生命周期事件的**本次运行身份**（`Site.Execution`）：
	// 同一张图跑两次是两个不同的值。并发跑同一张图时，只靠 graph + path
	// 两份记录会合并成一份——归因要靠它分开。
	//
	// 它只在**运行期**的出口上有值：静态出口（`Topology`）只有定义身份。
	AttrExecution = "pulse.execution"
)

// Observer 观察单次运行：图级两条 + 每个节点三条。
// 默认无观察者（no-op）。实现必须并发安全：节点回调在各自的节点 goroutine
// 上，图回调在 Start / Wait 的调用方 goroutine 上。panic / error 不得升格为
// 节点失败（由 Execution 吞掉）。
//
// 每条回调带一个 `Site`：定义身份（`New` 的 graphID）+ 本次运行身份 + 挂载位置。
// 实现侧无需从构造参数另行携带——多图、同一张图多跑几次，归因都不漂移
// （同一张图跑两次各是一个执行，见 `Site.Execution`）。
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
// 图级两条在 `Start` / `Wait` 的调用路径上同步执行——所以**别在回调里调同一个
// 执行的 `Start` / `Wait`**：那是同一个 goroutine 等自己，会死等。
type Observer interface {
	// OnGraphStarted 在这次运行通过启动校验、开始提交节点时发出一次。
	// 校验失败与重复 Start 都到不了这里（那时这次运行没有启动）。
	OnGraphStarted(site Site)
	// OnGraphFinished 在 Wait 返回前发出一次：reason 是运行终态（图只会是
	// completed / failed / canceled），err 是首错（即 Execution.Err()）。
	// 只 Start 不 Wait 的宿主收不到它；重复 Wait 不重复发。
	OnGraphFinished(site Site, reason NodeFinishReason, err error)
	OnNodeWaiting(site Site, nodeID string)
	OnNodeRunning(site Site, nodeID string)
	OnNodeFinished(site Site, nodeID string, reason NodeFinishReason, err error)
}

// Site 是「谁在哪里」——一次回调、一次运行都靠它说位置。
//
// 它是**稳定的位置视图**，只描述定义身份、本次运行身份与挂载位置，**不带任何 Key 的值**：
// 「绕开声明权限去读值」在这条路上没有入口（需要值的集成走各自类型校验过的出口）。
//
// 三处出口共用同一份位置记号，且必须逐字一致：`Topology().Graph.Path`（静态全树）、
// `Observer` 回调里的 `Site.Path`（运行期）、以及后续的快照（一次运行的进度）。
// 路径格式是契约：根图 `""`、一层子图是那一步的节点 id（`"step"`）、再深一层
// `"step/inner"`，用 `/` 连接——节点 id 里不许出现 `/` 就是为了让它两两可分。
//
// `Execution` 为空串表示「不在运行中」：静态出口（`Topology`）只有定义身份。
type Site struct {
	// Graph 是定义身份（`New` 的 graphID）。
	Graph string
	// Execution 是**本次运行**的身份，对每个执行实例唯一。同一张定义跑两次
	// 得到两个不同的值——观测记录靠它把两次运行分开（否则同位置的两份记录会合并）。
	Execution string
	// Path 是挂载位置：根图 `""`、一层子图 `"step"`、再深 `"step/inner"`。
	Path string
}

// String 是给人看的写法（日志与报错用），不是身份本身。
func (s Site) String() string {
	path := s.Path
	if path == "" {
		path = "<root>"
	}
	if s.Execution == "" {
		return fmt.Sprintf("graph %q at %s", s.Graph, path)
	}
	return fmt.Sprintf("execution %q (graph %q) at %s", s.Execution, s.Graph, path)
}

// ObserverFunc 把五条回调收成一个结构，便于测试与桥装配：只填要用的那几个字段。
type ObserverFunc struct {
	GraphStarted  func(site Site)
	GraphFinished func(site Site, reason NodeFinishReason, err error)
	Waiting       func(site Site, nodeID string)
	Running       func(site Site, nodeID string)
	Finished      func(site Site, nodeID string, reason NodeFinishReason, err error)
}

// OnGraphStarted 实现 Observer。
func (o ObserverFunc) OnGraphStarted(site Site) {
	if o.GraphStarted != nil {
		o.GraphStarted(site)
	}
}

// OnGraphFinished 实现 Observer。
func (o ObserverFunc) OnGraphFinished(site Site, reason NodeFinishReason, err error) {
	if o.GraphFinished != nil {
		o.GraphFinished(site, reason, err)
	}
}

// OnNodeWaiting 实现 Observer。
func (o ObserverFunc) OnNodeWaiting(site Site, nodeID string) {
	if o.Waiting != nil {
		o.Waiting(site, nodeID)
	}
}

// OnNodeRunning 实现 Observer。
func (o ObserverFunc) OnNodeRunning(site Site, nodeID string) {
	if o.Running != nil {
		o.Running(site, nodeID)
	}
}

// OnNodeFinished 实现 Observer。
func (o ObserverFunc) OnNodeFinished(site Site, nodeID string, reason NodeFinishReason, err error) {
	if o.Finished != nil {
		o.Finished(site, nodeID, reason, err)
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
func (m MultiObserver) OnGraphStarted(site Site) {
	for _, o := range m {
		if o != nil {
			callSafely(func() { o.OnGraphStarted(site) })
		}
	}
}

// OnGraphFinished 实现 Observer。
func (m MultiObserver) OnGraphFinished(site Site, reason NodeFinishReason, err error) {
	for _, o := range m {
		if o != nil {
			callSafely(func() { o.OnGraphFinished(site, reason, err) })
		}
	}
}

// OnNodeWaiting 实现 Observer。
func (m MultiObserver) OnNodeWaiting(site Site, nodeID string) {
	for _, o := range m {
		if o != nil {
			callSafely(func() { o.OnNodeWaiting(site, nodeID) })
		}
	}
}

// OnNodeRunning 实现 Observer。
func (m MultiObserver) OnNodeRunning(site Site, nodeID string) {
	for _, o := range m {
		if o != nil {
			callSafely(func() { o.OnNodeRunning(site, nodeID) })
		}
	}
}

// OnNodeFinished 实现 Observer。
func (m MultiObserver) OnNodeFinished(site Site, nodeID string, reason NodeFinishReason, err error) {
	for _, o := range m {
		if o != nil {
			callSafely(func() { o.OnNodeFinished(site, nodeID, reason, err) })
		}
	}
}

// callSafely 跑一个扇出成员的调用，把它抛出的 panic 限制在这一格内。
func callSafely(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// WithObserver 声明这张**定义**的默认观察者；后写覆盖前写（单槽）。
// 需要多个时用 MultiObserver 组合后再传入。
//
// 它是**默认值**：某一次运行可以用 `WithExecObserver` 覆盖（见 ExecOption），
// 子图没自己声明时会继承父执行环境的出口。
func WithObserver(o Observer) Option {
	return func(g *Graph) { g.observer = o }
}

// notify 调用本次运行的观察者并吞掉 panic，避免只读 seam 变成节点失败。
func (e *Execution) notify(fn func(Observer)) {
	if e == nil || fn == nil {
		return
	}
	obs := e.observer
	if obs == nil {
		return
	}
	defer func() { _ = recover() }() // 只读 seam：panic 静默，不升格为节点失败
	fn(obs)
}

func finishReason(err error) NodeFinishReason {
	switch {
	case err == nil:
		return NodeCompleted
	case isCheckpointFailure(err), isPanicFailure(err):
		return NodeFailed
	case isSkipped(err):
		return NodeSkipped
	case isCanceled(err):
		return NodeCanceled
	default:
		return NodeFailed
	}
}
