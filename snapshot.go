package pulse

import (
	"fmt"
)

// GraphStatus 是一次执行的生命周期状态。图级没有 skipped：跳过是正常的节点结果。
type GraphStatus string

const (
	GraphStatusPending   GraphStatus = "pending"
	GraphStatusRunning   GraphStatus = "running"
	GraphStatusCompleted GraphStatus = "completed"
	GraphStatusFailed    GraphStatus = "failed"
	GraphStatusCanceled  GraphStatus = "canceled"
)

// NodeStatus 是节点在这一次执行中的状态。终态的字面量与 NodeFinishReason 一致。
type NodeStatus string

const (
	NodeStatusPending   NodeStatus = "pending"
	NodeStatusWaiting   NodeStatus = "waiting"
	NodeStatusRunning   NodeStatus = "running"
	NodeStatusCompleted NodeStatus = "completed"
	NodeStatusSkipped   NodeStatus = "skipped"
	NodeStatusFailed    NodeStatus = "failed"
	NodeStatusCanceled  NodeStatus = "canceled"
)

// SlotState 是只读出口使用的槽位三态：ready 和 skipped 都是到达，pending 不是。
type SlotState string

const (
	SlotStatePending SlotState = "pending"
	SlotStateReady   SlotState = "ready"
	SlotStateSkipped SlotState = "skipped"
)

// GraphState 是这一次执行的身份与进度；不包含业务值。
//
// Execution 与 Site.Execution 相同，ID 是定义身份。Status 在成功 Start 后为 running，
// 在 Wait 收尾时写终态；只 Start 不 Wait 时，即使所有节点已经结束，图仍为 running，
// 与图级 finished 回调的生命周期一致。Err 是已经捕获的首错；尚未被节点观察到的
// ctx 取消不提前变成运行结果，正常收尾的 cancel 也不算错误。
type GraphState struct {
	ID          string
	Description string
	Execution   string
	Path        string
	Status      GraphStatus
	Err         error
}

// KeyState 只描述键的类型、来源和到达状态，不携带真实值。
// From 与 Topology 的 KeyShape 共用词表：只有 Inputs 上的来源有意义。
type KeyState struct {
	Name  string
	Type  string
	From  Source
	State SlotState
}

// NodeState 是某个节点在本次执行中的读数；不包含业务值。
//
// waiting 的节点若还有 pending 输入，在等输入；输入全到达（或没有输入），则是在等
// 进入 Run 的机会。切面可以扩展等待，不能据此保证它正阻塞在限额上。
// Child 是挂载点最后一次创建的子 Execution 的活句柄：失败尝试也保留，尚未建出则为 nil。
// 它不是递归拷贝，进一步查看时调 Child.Snapshot；这会取得另一个时刻的读数。
type NodeState struct {
	ID          string
	Description string
	Status      NodeStatus
	Inputs      []KeyState
	Outputs     []KeyState
	Child       *Execution
	Err         error
}

// Snapshot 是一次执行的只读状态副本。Nodes 与 Graph.Topology().Nodes 同序同 ID。
//
// 它不包含业务值，也不递归拷贝子执行。除 Child 活句柄和 error 本身以外，返回的结构与
// 切片都和内部状态隔离，修改它们不会改变执行。
//
// 它不是某一瞬间的原子切面：节点状态与各槽位分别采样，因此 running 节点的出口已
// ready/skipped、其他出口仍 pending 都合法。Retry 的暂存不是已发布槽位，失败尝试的
// 输出不会出现在读数里。Snapshot 是观察而不是提交、恢复或结果读取协议。
type Snapshot struct {
	Graph GraphState
	Nodes []NodeState
}

// nodeRuntime 只属于一份 Execution，由 e.mu 保护；定义 Node 上没有运行状态。
// 以定义指针定位，输出始终按定义声明序，而非 map 迭序。
type nodeRuntime struct {
	status NodeStatus
	child  *Execution
	err    error
}

// Snapshot 随时可读：创建后、运行中、Wait 后均可。nil Execution 返回 nil。
//
// 定义在 Start 前还能新增节点：此时快照展示当次读取到的定义，尚无运行记录的节点是 pending。
// Start 封口后，节点集合固定。读取不创建槽位，不改变定义或推进执行，不发 Observer 事件。
func (e *Execution) Snapshot() *Snapshot {
	if e == nil {
		return nil
	}
	g := e.graph
	g.mu.Lock()
	nodes := append([]*Node(nil), g.nodes...)
	out := &Snapshot{
		Graph: GraphState{ID: g.id, Description: g.desc, Execution: e.site.Execution, Path: e.site.Path},
		Nodes: make([]NodeState, len(nodes)),
	}
	for i, n := range nodes {
		out.Nodes[i] = NodeState{
			ID: n.id, Description: n.desc,
			Inputs:  snapshotKeyShapes(g.keyShapes(n.requires, true)),
			Outputs: snapshotKeyShapes(g.keyShapes(n.provides, false)),
		}
	}
	g.mu.Unlock()

	// 不跨层持锁：定义、执行状态与槽位分别读取，任何用户回调都在这些锁外执行。
	e.mu.Lock()
	out.Graph.Status = GraphStatusPending
	out.Graph.Err = e.err
	if e.started {
		out.Graph.Status = GraphStatusRunning
	}
	done, reason := e.done, e.errReason
	for i, n := range nodes {
		state := e.states[n]
		if state.status == "" {
			state.status = NodeStatusPending
		}
		out.Nodes[i].Status = state.status
		out.Nodes[i].Child = state.child
		out.Nodes[i].Err = state.err
	}
	e.mu.Unlock()
	// 使用与首错一起确定的归因；只读视图不重新调用用户错误方法。
	if done {
		out.Graph.Status = GraphStatus(reason)
	}

	// 只复制已存在的槽位指针。不能走 slotOf：观察晚加的键不该凭空创建运行状态。
	e.slotsMu.Lock()
	slots := make(map[string]*slot, len(e.slots))
	for name, s := range e.slots {
		slots[name] = s
	}
	e.slotsMu.Unlock()
	for i := range out.Nodes {
		readKeyStates(out.Nodes[i].Inputs, slots)
		readKeyStates(out.Nodes[i].Outputs, slots)
	}
	return out
}

func snapshotKeyShapes(shapes []KeyShape) []KeyState {
	if len(shapes) == 0 {
		return nil
	}
	out := make([]KeyState, len(shapes))
	for i, k := range shapes {
		out[i] = KeyState{Name: k.Name, Type: k.Type, From: k.From, State: SlotStatePending}
	}
	return out
}

func readKeyStates(keys []KeyState, slots map[string]*slot) {
	for i := range keys {
		if s := slots[keys[i].Name]; s != nil {
			state, _ := s.snapshot()
			keys[i].State = publicSlotState(state)
		}
	}
}

func publicSlotState(state slotState) SlotState {
	switch state {
	case slotReady:
		return SlotStateReady
	case slotSkipped:
		return SlotStateSkipped
	default:
		return SlotStatePending
	}
}

// recordNodeState 与生命周期回调在同一入口调用，先写状态、后发事件。
// waiting/running 的次数闸仍由 runNode 管住，Retry 不会让状态倒退。
func (e *Execution) recordNodeState(n *Node, status NodeStatus, err error) {
	e.mu.Lock()
	if e.states == nil {
		e.states = make(map[*Node]nodeRuntime)
	}
	state := e.states[n]
	state.status = status
	state.err = err
	e.states[n] = state
	e.mu.Unlock()
}

func (e *Execution) finishNode(n *Node, reason NodeFinishReason, err error) {
	e.recordNodeState(n, NodeStatus(reason), err)
	e.notify(func(o Observer) { o.OnNodeFinished(e.site, n.id, reason, err) })
}

// recordChild 在子执行创建后、Start 前存句柄；失败/取消仍可追踪最后一次尝试。
func (e *Execution) recordChild(n *Node, child *Execution) {
	e.mu.Lock()
	if e.states == nil {
		e.states = make(map[*Node]nodeRuntime)
	}
	state := e.states[n]
	state.child = child
	e.states[n] = state
	e.mu.Unlock()
}

// ReadOutput 读取这一次执行的声明出口。k 必须与本图 DeclareOutput 声明的键同名同类型；
// Key 不携带图来源，同名同类型的键等价。零 Key、内部键、未声明出口和错误类型均返回
// 包装 ErrUndeclared 的错误。nil 或零值 Execution 返回普通契约错误。
//
// 结果以成功 Wait 的收尾为边界：尚未收尾时立即返回普通生命周期错误，不等待，
// 即使节点已经 Set 或全部节点已结束也不返回部分值。OnGraphFinished 回调中可以读取。
// 已捕获的失败或真实 ctx 取消优先返回原始原因，保留 errors.Is；正常 Wait 收尾释放 ctx
// 所产生的取消不算错误。任何错误返回的 value 都是零值，state 都是空串，调用方应先判 err。
//
// 成功收尾后，ready 返回业务值，skipped 返回零值且不报错。无人 Requires、未喂值的
// 可选透传出口保持 pending，返回零值且不报错。ready 的 0、空串、nil 接口和 typed nil
// 都保留 ready，不会被当作缺值。
//
// 读取不创建槽位、不改变状态、不发 Observer 事件，也不把业务值写进快照或错误。
// slice、map、指针等返回原业务值，不深拷贝；宿主对值的并发修改，以及节点自己启动的
// 后台写入，不在本函数的并发保证内。
func ReadOutput[T any](e *Execution, k Key[T]) (value T, state SlotState, err error) {
	if e == nil || e.graph == nil || e.ctx == nil {
		return value, "", fmt.Errorf("pulse: ReadOutput: nil or zero Execution: create it with NewExecution")
	}

	ref := k.asRef()
	g := e.graph
	g.mu.Lock()
	port, declared := g.outPorts[ref.name]
	g.mu.Unlock()
	if ref.name == "" || !declared || port.typ != ref.typ {
		return value, "", fmt.Errorf("pulse: ReadOutput: %s is not a declared output of graph %q (%w)",
			ref, g.id, ErrUndeclared)
	}

	// 分段读锁：终态错误优先于槽位状态，done 后只认 Wait 锁存的结果。
	e.mu.Lock()
	done, cause := e.done, e.err
	if cause == nil && !done {
		cause = e.ctx.Err()
	}
	e.mu.Unlock()
	if cause != nil {
		return value, "", cause
	}
	if !done {
		return value, "", fmt.Errorf("pulse: ReadOutput: execution %q has no final result: call Start and Wait before reading outputs", e.ID())
	}

	// 只查已有槽位：晚声明的可选透传出口可能没有槽，仍然是真实的 pending。
	e.slotsMu.Lock()
	s := e.slots[ref.name]
	e.slotsMu.Unlock()
	if s == nil {
		return value, SlotStatePending, nil
	}
	st, raw := s.snapshot()
	if st != slotReady {
		return value, publicSlotState(st), nil
	}
	// nil 接口没有动态类型，不能用 raw.(T) 判为类型错误；端口类型已在上面核对。
	if raw == nil {
		return value, SlotStateReady, nil
	}
	out, ok := raw.(T)
	if !ok {
		return value, "", fmt.Errorf("pulse: ReadOutput: key %q type assertion failed", ref.name)
	}
	return out, SlotStateReady, nil
}
