package pulse

import "sync"

// 本文件是**运行时快照**：这张图现在跑到哪了。
//
// 与 topology.go 的分工是「形状 vs 状态」——形状（有哪些节点、谁连谁、每一层怎么嵌套）
// 是声明事实，任何时候都能问；状态是运行事实，只有已经建出来的图对象才有，靠 Snapshot。
//
// 一条纪律贯穿全文件：**状态与事件在同一个点写**（见 runNode 的那几发），顺序是
// 「先写状态、再发回调」。所以「跑完之后的快照」与「事件流折出的终态」逐节点相等，
// 而在回调里拍的快照一定包含这次迁移。

// GraphStatus 是图级状态。
//
// 三个终态（completed / failed / canceled）与 `Observer.OnGraphFinished` 的 reason
// **同一时刻、同一判据**：图终态在 `Wait` 返回时定。只 `Start` 不 `Wait` 的宿主会
// 一直读到 `running`（与「收不到 GraphFinished」是同一条契约）。
//
// 由此有一条**宿主须知**：`running` + `Err != nil` 是**合法**快照——首错在节点失败
// 那一刻就写进了 `Err`，而图终态要等 `Wait`；只看 `Status` 的看板会以为它还在跑。
type GraphStatus string

const (
	// GraphPending：还没 `Start`。
	GraphPending GraphStatus = "pending"
	// GraphRunning：已经 `Start`、还没 `Wait` 返回。
	GraphRunning GraphStatus = "running"
	// GraphCompleted：跑完且没有首错。
	GraphCompleted GraphStatus = "completed"
	// GraphFailed：某个节点失败了（首错原样保留在 `Err`）。
	GraphFailed GraphStatus = "failed"
	// GraphCanceled：这一轮被从外面拆了——取消或截止时间到期。
	GraphCanceled GraphStatus = "canceled"
)

// NodeStatus 是一个节点在**一次节点生命周期**里的状态。
//
// 终态四个（completed / skipped / failed / canceled）与 `NodeFinishReason` 的值
// **逐字相同**（`TestNodeStatusMatchesFinishReason` 钉住）：两边是同一件事实的两个
// 出口——`Observer` 发事件，快照发读数。
//
// **不按 attempt 重走**：`Retry` 重跑期间状态一直是 `running`，整套生命周期走完才写
// 一次终态。事件流那边每个节点只发一次 waiting / running / finished（见 runNode），
// 快照按 attempt 重走就会与事件流不同源，还会破坏单调。
//
// 状态**单调**，但**不是**一律「pending → waiting → running → 终态」这一条链：
// `skipped` 与 `canceled` 都可能紧跟在 `waiting` 之后而中间没有 `running`。
type NodeStatus string

const (
	// NodeStatusPending：图还没 `Start`，或这个节点的 goroutine 还没走到门。
	// 事件流里没有「没启动」这件事——补上它正是快照的主要用途。
	NodeStatusPending NodeStatus = "pending"
	// NodeStatusWaiting 覆盖两件事：**等输入**或**等名额**。事件流分不出来，
	// 快照靠槽位分得出：还有输入是 `pending` ⇒ 在等这些 Key；输入全部到达
	// （或本节点没有输入）⇒ 门已经过了，它在等进 `Run` 的机会。
	//
	// 切面自己的等待不在这里：拿到名额之后的等待状态已经是 `running`，
	// core 之前的等待状态还是 `pending`。
	NodeStatusWaiting NodeStatus = "waiting"
	// NodeStatusRunning：已经进入用户 `Run`（含 `Retry` 的重跑）。
	NodeStatusRunning NodeStatus = "running"
	// NodeStatusCompleted：`Run` 正常返回。返回后仍未写的 `Provides` 会被自动跳过，
	// 那不算失败——所以「节点 completed 而某条输出槽 skipped」是正常的。
	NodeStatusCompleted NodeStatus = "completed"
	// NodeStatusSkipped：本节点没进入 `Run`（输入一条值都没到），或自己 `Skip` 了输出。
	NodeStatusSkipped NodeStatus = "skipped"
	// NodeStatusFailed：返回了真实错误——含 panic 转成的错误与 `Timeout` 切面的超时。
	NodeStatusFailed NodeStatus = "failed"
	// NodeStatusCanceled：这一轮被别人拆掉了（首错取消整图、父 ctx 取消或超时、
	// 排队等名额期间被取消）。
	NodeStatusCanceled NodeStatus = "canceled"
)

// SlotState 是槽位三态，与引擎内部的 `slotState` 同一个词表：`pending` **不是到达**；
// `ready` 与 `skipped` 都是到达。
type SlotState string

const (
	SlotPending SlotState = "pending"
	SlotReady   SlotState = "ready"
	SlotSkipped SlotState = "skipped"
)

// Snapshot 是一张图**此刻**的进度：图一行 + 每个节点一行。
//
// 它回答的是「每个节点在等**谁**」，**不是**「值是多少」——结构里根本没有值这个字段，
// 首版也不给开关（带值等于给引擎加一套公开读槽 API，那是另一张票的事）。
//
// 两条读法上的边界：
//
//   - `Nodes` 与 `Topology().Nodes` **同序、同 id**（`Topology().Nodes[i]` ↔
//     `Snapshot().Nodes[i]`）：度数、边这些**形状**留在 `Topology`，快照不重复一份；
//   - 快照**不是某一瞬间的原子切面**：一组字段是同一次读取里逐个抄下来的，彼此之间
//     可以有真实的时间差。收尾路径上就有一处会真的出现的撕裂——引擎先跳过没写的
//     输出槽、再写节点终态，所以「节点还读到 `running`、它的输出槽已经读到 `skipped`」
//     是合法的：不要写「`running` ⟹ 未写的输出仍是 `pending`」这类断言。
type Snapshot struct {
	Graph GraphState
	Nodes []NodeState
}

// GraphState 是快照里图那一行。
type GraphState struct {
	ID          string
	Description string
	// Path 是这张图在嵌套里的位置：根图为 `""`，一层子图是它的节点 id，再深一层是
	// `outer/inner`（与 `Topology().Graph.Path`、`pulse.AttrPath` 同一套记号）。
	Path   string
	Status GraphStatus
	// Err 是首错摘要（没有错误时为 nil）。它在节点失败那一刻就写进来，而图终态要等
	// `Wait`——所以它可能比 `Status` 早，见 GraphStatus 的宿主须知。
	Err error
}

// NodeState 是快照里节点那一行。
type NodeState struct {
	ID          string
	Description string
	Status      NodeStatus
	Inputs      []KeyState
	Outputs     []KeyState
	// Child 是这一步建出来的子图（决策 I），**快照里唯一一个不是副本的字段**：
	// 只有 `Sub` 那一步可能非 nil，而且是活对象——父图被回收后它失效。
	//
	// 三条边界：
	//
	//   - 有值的时刻是 `build` 回调**返回之后**（不是父图 `Start` 那一刻：`build` 是
	//     那一步进入 `Run` 之后才调的；那一步被判跳过时 `build` 一次都不被调用）；
	//   - `nil` 只意味着「这个节点还没建出子图」：没进 `Run`（输入全跳过、被取消、
	//     校验没过），或 `build` 自己报错返回；
	//   - 子图一旦被认领就是**封住的**：外部拿着它只能读（`Snapshot` / `Topology` /
	//     `Validate`），`Add` / `Seed` / `Start` 一律返回 `ErrGraphStarted`。
	//
	// 「交出来了、后面才失败」（绑错键 / 种值冲突）也看得到：那一步是 `failed`、
	// `Child` 非 nil，而子图快照仍是 `pending`——装出来了、没跑起来，句柄正好是
	// 排查证据。
	Child *Graph
	// Err 是失败 / 取消时的错误摘要；其余情形为 nil。
	Err error
}

// KeyState 是一条键在快照里的读数。
type KeyState struct {
	Name string
	// Type 是类型记号（`reflect.Type.String()`，与 `Topology()` 的 `KeyShape.Type`、
	// 声明式装配的记号同一套）。
	Type string
	// From 只有**输入**有意义（「谁喂了它」）；输出恒为零值——一条输出的来源就是它
	// 自己那个节点，要看那个走 `Topology().Edges`。
	From  Source
	State SlotState
}

// nodeState 是一个节点在**某一张图里**的运行状态。状态存图上、不挂 `Node` 上：
// 同一个 `Node` 可以跨图复用（两张图的槽位互不可见），状态挂节点上会把两张图搅在一起。
//
// 这把小锁只保护这三个字段。**读快照时绝不在持 `g.mu` 的情况下去拿它**——否则会与
// `fail()` / `skipUnwritten` 的路径互相等（见 Snapshot 的两步读法）。
type nodeState struct {
	mu     sync.Mutex
	status NodeStatus
	err    error
	child  *Graph
}

func (s *nodeState) set(st NodeStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
}

// finish 写终态。与 set 分开，只是让调用点读起来是「这一轮结束了」。
func (s *nodeState) finish(st NodeStatus, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
	s.err = err
}

func (s *nodeState) setChild(g *Graph) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.child = g
}

// read 在同一个临界区里抄下这一行的三个字段。
func (s *nodeState) read() (NodeStatus, *Graph, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.child, s.err
}

// nodeStatusOf 把 `NodeFinishReason` 翻成节点状态（终态那一半）。终态字符串逐字相同，
// 由 `TestNodeStatusMatchesFinishReason` 钉住这条对应关系。
func nodeStatusOf(r NodeFinishReason) NodeStatus {
	switch r {
	case NodeCompleted:
		return NodeStatusCompleted
	case NodeSkipped:
		return NodeStatusSkipped
	case NodeCanceled:
		return NodeStatusCanceled
	default:
		return NodeStatusFailed
	}
}

// slotStateOf 把内部的槽位三态翻成公开词表。
func slotStateOf(st slotState) SlotState {
	switch st {
	case slotReady:
		return SlotReady
	case slotSkipped:
		return SlotSkipped
	default:
		return SlotPending
	}
}

// Snapshot 拍一张此刻的快照：**任何时候都能调**（没启动、跑着、跑完都行），
// 包括在 `Observer` 回调里。
//
// 读取算法写死成两步，为的是「全程一次只持一把锁」：
//
//  1. 持 `g.mu` 复制出「每个节点的状态单元 + 各自的 Requires / Provides 声明 +
//     来源表 + 图级 started / done / err / path」。`Add` 也在这把锁下改 `g.nodes`，
//     不复制的话「未启动时并发 `Add` + 拍快照」本身就是一处 data race；
//  2. **放锁之后**，逐个读节点状态（各自的小锁）、逐条键读槽位快照（槽位自己的锁）。
//
// 槽位那一步用**只读查表**（peekSlot）：没有这条键就报 `pending`，不建槽——快照不该
// 在图里留下痕迹。
func (g *Graph) Snapshot() *Snapshot {
	type decl struct {
		id, desc string
		reqs     []keyRef
		provs    []keyRef
	}

	g.mu.Lock()
	states := append([]*nodeState(nil), g.states...)
	decls := make([]decl, len(g.nodes))
	for i, n := range g.nodes {
		decls[i] = decl{
			id:    n.id,
			desc:  n.desc,
			reqs:  append([]keyRef(nil), n.requires...),
			provs: append([]keyRef(nil), n.provides...),
		}
	}
	producer := make(map[string]source, len(g.producer)) // 来源表的写点只有 claimSource，同在 g.mu 下
	for k, v := range g.producer {
		producer[k] = v
	}
	gs := GraphState{ID: g.id, Description: g.desc, Path: g.path, Err: g.err}
	switch {
	case !g.started:
		gs.Status = GraphPending
	case !g.done:
		gs.Status = GraphRunning
	case g.err == nil:
		gs.Status = GraphCompleted
	case isCanceled(g.err):
		gs.Status = GraphCanceled
	default:
		gs.Status = GraphFailed
	}
	g.mu.Unlock()

	out := &Snapshot{Graph: gs, Nodes: make([]NodeState, len(decls))}
	for i, d := range decls {
		ns := NodeState{ID: d.id, Description: d.desc}
		if i < len(states) && states[i] != nil {
			ns.Status, ns.Child, ns.Err = states[i].read()
		}
		ns.Inputs = g.keyStates(d.reqs, producer, true)
		ns.Outputs = g.keyStates(d.provs, producer, false)
		out.Nodes[i] = ns
	}
	return out
}

// keyStates 逐条键抄读数。`withFrom` 表示这是输入那一侧（只有输入有来源）。
func (g *Graph) keyStates(refs []keyRef, producer map[string]source, withFrom bool) []KeyState {
	if len(refs) == 0 {
		return nil
	}
	out := make([]KeyState, len(refs))
	for i, r := range refs {
		ks := KeyState{Name: r.name, State: SlotPending}
		if r.typ != nil {
			ks.Type = r.typ.String()
		}
		if withFrom {
			ks.From = publicSource(producer[r.name])
		}
		if s := g.peekSlot(r.name); s != nil {
			st, _ := s.snapshot()
			ks.State = slotStateOf(st)
		}
		out[i] = ks
	}
	return out
}

// peekSlot 只读查槽：没有这条键就返回 nil（= 还没到达），**不建槽**。
func (g *Graph) peekSlot(name string) *slot {
	g.slotsMu.Lock()
	defer g.slotsMu.Unlock()
	return g.slots[name]
}
