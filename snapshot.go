package pulse

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
	if e.done {
		out.Graph.Status = GraphStatus(finishReason(e.err))
	}
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
