package pulse

import "strings"

// 本文件是图的**静态形状**：这一层装成什么样、谁喂谁、排得出什么顺序。
// 与运行时快照的分工是「形状 vs 进度」——形状不随运行变，跑完再问也一样。
//
// 它同时是**依赖边**的唯一一份推导（`depGraphLocked`）：环检测、拓扑序、
// 入度出度、边表都从这里取，免得同一件事写第二遍（两份写法必然漂）。

// SourceKind 是键来源的种类。
type SourceKind string

const (
	// SourceNone 表示**没有来源**：`Requires` 上表示还没人喂它（那种图 `Start`
	// 会拒），`Provides` 上恒为它——一条输出的来源就是它自己那个节点。要看
	// 「谁喂了这条输入」走 `Edges`，不要在 `Provides` 上找。
	SourceNone SourceKind = ""
	// SourceInput 表示**声明过的输入端口**（`DeclareInput`）：这张图对外接受
	// 这条 Key。**外部来源在静态结构里只有这一种写法**——`Seed` / `SkipSeed`
	// 是某一次运行的动作（值活在 `Inputs` 里，不碰定义），所以定义层没有、
	// 也不该有「外部来源」这一档（旧的 `SourceSeed` 在阶段 2 随之一并去掉：
	// 它已经没有产出路径了）。
	SourceInput SourceKind = "input"
	// SourceNode 表示某个节点的 `Provides`，`NodeID` 是那个节点。
	SourceNode SourceKind = "node"
)

// Source 表示一个 Key 的来源。
type Source struct {
	Kind   SourceKind
	NodeID string // 仅 Kind == SourceNode 时有效
}

// String 是给人看的来源名：节点来源 `node %q`、声明过的入口 `input`、没有来源 `none`。
//
// 节点来源**一律**带类型前缀并加引号，不是只给某个哨兵名字开小灶：节点 id 是任意用户
// 字符串——只特判 `seed` 的话，节点叫 `none` 的会与「没有来源」撞、字面量 id
// `node "seed"` 也会与节点 `seed` 撞（两条都实测撞上了，评审在 #308 上点的这条）。
// 与引擎内部报错里的规则是**同一份**（`sourceName`），所以「引擎里分得开」与
// 「日志里分得开」是同一条规则，不会各自漂。
func (s Source) String() string {
	switch s.Kind {
	case SourceInput:
		return sourceName(sourceInput, "")
	case SourceNode:
		return sourceName(sourceNode, s.NodeID)
	default:
		return sourceName(sourceNone, "")
	}
}

// publicSource 把引擎内部的来源记录翻成公开形状，**只此一处**——两处各写一遍
// 就会漂（节点 id 恰好叫 `seed` 那条路径就是这么错的，见 source.go）。
func publicSource(s source) Source {
	switch s.kind {
	case sourceInput:
		return Source{Kind: SourceInput}
	case sourceNode:
		return Source{Kind: SourceNode, NodeID: s.nodeID}
	default:
		return Source{}
	}
}

// KeyShape 是键的形状：名字、类型记号、来源。
//
// Go 定义的 Type 用 reflect.Type.String()；pulse/yaml.Analyze 返回文档里的类型标签，
// 无法推导时为空串。标签能逐字对照，但无注册表的分析不能证明它是实际 Go 类型。
type KeyShape struct {
	Name string
	Type string
	// From 在 Requires 上表示「谁喂了它」，在 InPorts 上表示 SourceInput；
	// Provides / OutPorts 上恒为 SourceNone，出口的来源从图的节点或透传入口推导。
	From Source
}

// GraphShape 是图自己的那点元数据。
type GraphShape struct {
	ID          string
	Description string
	// Path 是这张图在**这一次视图里**的位置（根图为 `""`，一层子图是它的节点 id，再深
	// 一层是 `outer/inner`），由**挂载位置**推导——不需要运行、也不会被运行改写，所以
	// 同一张定义被两处挂载时，两处各看得到自己那份。
	Path string
	// InPorts / OutPorts 是完整声明接口，按首次成功声明序，不受节点是否使用影响。
	// 同一条键可在两侧同时出现（透传）。只有 InPorts 的 From 表示 SourceInput。
	InPorts  []KeyShape
	OutPorts []KeyShape
}

// NodeShape 是一个节点的形状。
//
// 度数按**相邻节点**数计，不是 Key 数：A 提供 `k1`、`k2` 而 B 两条都读，只是
// **一条**边（`{A→B, Keys:[k1,k2]}`）、`InDegree(B) == 1`；再加一个上游节点才是 2。
// 外部 `Seed` 不算邻居，所以它不进度数。
type NodeShape struct {
	ID          string
	Description string
	Requires    []KeyShape
	Provides    []KeyShape
	InDegree    int
	OutDegree   int
	// Sub 是这一步挂的子图**定义**的静态形状（递归）。`Sub` 装的节点才有它。
	//
	// 它让「无运行即可递归看全树」成立：装配完拿到根，一路 `.Sub.Sub…` 就能看到整棵树。
	Sub *Topology
	// Binds 是这一步的父子接线（`In(父, 子)` / `Out(子, 父)` 的箭头读法，来源在前）。
	// 只在 `Sub` 装的节点上有。
	Binds []BindShape
}

// BindShape 是一条父子接线：父侧那条 Key ↔ 子图**声明的端口**。
type BindShape struct {
	// Parent 是父侧的键名，Child 是子侧的端口名。`Fixed` 为真时父侧不参与，
	// `Parent` 是空串。
	Parent string
	Child  string
	// In 为真表示 `In(父, 子)`（父 → 子，父侧那条进 Requires）；为假是 `Out(子, 父)`。
	In bool
	// Fixed 为真表示这条**往子图**的接线送的是一个常量（`Fix` / `FixSkip`）：
	// 值属于**挂载点**、不属于子图定义，父侧没有对应的键。
	Fixed bool
}

// bindShapes 把接线翻成公开形状（`Sub` 装的节点才有）。
func bindShapes(binds []SubBind) []BindShape {
	if len(binds) == 0 {
		return nil
	}
	out := make([]BindShape, 0, len(binds))
	for _, b := range binds {
		out = append(out, BindShape{Parent: b.parent.name, Child: b.child.name,
			In: b.intoChild(), Fixed: b.fix != nil})
	}
	return out
}

// joinPath 拼嵌套路径：根为空串，一层是挂载它的那个节点 id，再深是 `outer/inner`。
// 节点 id 不许含 `/` 就是为它（观测里那条 `pulse.path` 用的是同一套写法）。
func joinPath(at, id string) string {
	if at == "" {
		return id
	}
	return at + "/" + id
}

// mountChain 是「从根一路挂到当前这张图」的图链（含当前那张）。两条用途：
// ① 判环——同一张图不能出现在自己的祖先链上（递归展开不完，加锁时还会两层互等）；
// ② 报错时给出那条链。
//
// 深度就是个嵌套层数，线性扫描足够，不值一份 map。**判据是「这条路径」而不是「访问过」**：
// 同一张子图在**不同挂载位置**重复出现是合法复用，用全局 visited 会把它误判成环。
type mountChain []*Graph

// has 判这张图是不是已经在这条链上。
func (c mountChain) has(g *Graph) bool {
	for _, x := range c {
		if x == g {
			return true
		}
	}
	return false
}

// push 返回「这条链 + 一张子图」。整片复制：链由各层视图共享，往后 append 不能写进别人的
// 底层数组（同一张子图挂两处时，两条分支各推各的）。
func (c mountChain) push(g *Graph) mountChain {
	out := make(mountChain, len(c), len(c)+1)
	copy(out, c)
	return append(out, g)
}

// String 把链拼成人话：`a -> b -> c`。图的 id 入图后不变，读它不必持锁。
func (c mountChain) String() string {
	ids := make([]string, 0, len(c))
	for _, g := range c {
		ids = append(ids, g.id)
	}
	return strings.Join(ids, " -> ")
}

// EdgeShape 是一条节点间依赖边：`From` 必须先跑完，`To` 的 `Keys` 由它提供。
//
// 同一对节点之间只留**一条**边（哪怕喂了多条 Key），`Keys` 按下游 `Requires`
// 的出现顺序列出全部；外部 `Seed` 不建边。
type EdgeShape struct {
	From string
	To   string
	Keys []string
}

// Topology 是图的静态形状 + 静态毛病。
//
// 它是**只读**的，任何时候都能问（没启动、跑着、跑完都行）：形状不随运行变，
// 每次返回的都是与引擎内部**隔离的副本**，改它不影响图。
type Topology struct {
	Graph GraphShape
	// Nodes 是**声明序**（`Add` 顺序）。
	Nodes []NodeShape
	// Edges 按下游节点的声明序排，同一下游再按它 `Requires` 里命中该上游的
	// Key 出现顺序；同一对节点只留一条。所以同一张图每次问都是同一种排法。
	Edges []EdgeShape
	// Order 是确定性拓扑序：`A → B` 就一定 A 排在 B 前。**不承诺没有依赖关系的
	// 节点之间谁先谁后**（这里给的是「剥叶」的自然顺序，同一张图每次一样，
	// 但别把它当业务语义）。有环时它只含剥得掉的那批，环写进 `Problems`。
	Order []string
	// Problems 是**静态可判定**的毛病，一次给全（`Validate` 只报第一条）。
	// 顺序固定：来源 → 出口 → 依赖环 → 流式。空 = 没毛病。
	//
	// 依赖环那一类：**每个环分量（强连通分量）一条**具体见证，不是「每条简单环一条」
	// （简单环的数量可能指数增长），报的也一定是**真环**——只是环下游的节点不算环。
	//
	// 它覆盖的是引擎能从**已装入的图**推导出来的那部分；声明式装配（YAML）的
	// 结构 / 绑定诊断归 `pulse/yaml`，不在这里重复。
	Problems []error
}

// sourceOfLocked 是「这条键的来源是什么」的**唯一**判据（调用方持 g.mu）：
// 「谁会写它」（外部 `Seed` / 某个节点 `Provides`）优先，没有的话看它是不是声明过的入口，
// 两者都不是 = 没有来源（零值）。与 `problemsSourcesLocked` 的判据同源——那边判「有没有」，
// 这边判「写成什么」。
func (g *Graph) sourceOfLocked(k keyRef) source {
	if s, ok := g.producer[k.name]; ok && s.present() {
		return s
	}
	if _, ok := g.inPorts[k.name]; ok {
		return source{kind: sourceInput}
	}
	return source{}
}

// Topology 返回这张图的静态形状快照——**递归**：静态挂载的子图在 `Nodes[i].Sub` 里，
// 一路 `.Sub.Sub…` 就是整棵树（无需运行、也不改任何定义）。
//
// 只读、加锁、任何时候可调；返回的是副本（`Nodes` / `Edges` / `Order` /
// `Problems` / `Sub` 全是新对象），改它不会碰图。
//
// 视图里那张图的 `Graph.Path` 是**它在这次视图里的位置**（根为空串、一层子图是挂载它的
// 节点 id、再深是 `outer/inner`），由挂载位置**推导**——不读运行期盖章的那个字段：同一张
// 定义被两处挂载时，那个字段只会留下最后一次跑过的痕迹，拿它当结构信息是错的。
//
// `Problems` 也是整棵树的：每个挂载点各一条，报文点明是哪一处引用看到的（见
// problemsSubAtLocked）——同一张子图挂两处，你要能照着路径找到该改的那一处。
func (g *Graph) Topology() *Topology {
	g.mu.Lock()
	defer g.mu.Unlock()
	chain, capacity := mountChain{g}, g.declaredCapacity()
	streams := g.problemsStreamLocked("", chain, capacity)
	return g.topologyAtLocked("", chain, capacity, streams)
}

// topologyAt 是递归的**入口**：自己加锁，再交给递归体（与 problemsAt 同一形状、同一理由：
// 父图持着自己的锁，子图那把锁由子图自己取）。`capacity` 是这一层继承下来的有效名额上限。
func (g *Graph) topologyAt(at string, chain mountChain, capacity int, streams map[string][]error) *Topology {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.topologyAtLocked(at, chain, capacity, streams)
}

// topologyAtLocked 是递归主体：`at` 是这张图**在本视图里**的位置，`chain` 是从根挂到这张图
// 的图链，`capacity` 是**这一层继承下来的**有效名额上限（静态视图只有定义，所以根那一层给
// `declaredCapacity()`，往下每层按同一条优先级解析）。调用方持 g.mu。
//
// 锁次序：沿**静态挂载边**往下递归，每往下一层取那一层的锁；挂载边不可能成环
// （`Sub` 在装配期就拒，且装配那一步是全局串行的），所以不会出现「A 等 B 的锁、B 等 A 的锁」。
// 兜底判环在 `chain` 上——真成了环就地收住，不再往下展开（见 problemsSubAtLocked）。
// streams 是本次根检查的诊断，保留挂载点 FixSkip / 别名 / 额度池的上下文；
// 子层不能再把已经确定的输入当成未知。定义字段仍逐层加锁采样。
func (g *Graph) topologyAtLocked(at string, chain mountChain, capacity int, streams map[string][]error) *Topology {
	d := g.depGraphLocked()
	inDeg := make([]int, len(g.nodes))
	outDeg := make([]int, len(g.nodes))
	for ci := range g.nodes {
		inDeg[ci] = len(d.in[ci])
		for _, e := range d.in[ci] {
			outDeg[e.from]++
		}
	}

	t := &Topology{
		Graph: GraphShape{
			ID: g.id, Description: g.desc, Path: at,
			InPorts: g.keyShapes(g.inPortOrder, true), OutPorts: g.keyShapes(g.outPortOrder, false),
		},
		Nodes: make([]NodeShape, 0, len(g.nodes)),
		Edges: make([]EdgeShape, 0),
		Order: d.order(g.nodes),
	}
	for i, n := range g.nodes {
		ns := NodeShape{
			ID:          n.id,
			Description: n.desc,
			Requires:    g.keyShapes(n.requires, true),
			Provides:    g.keyShapes(n.provides, false),
			InDegree:    inDeg[i],
			OutDegree:   outDeg[i],
		}
		if n.child != nil {
			ns.Binds = bindShapes(n.binds)
			// 递归展开静态子树：路径按**挂载位置**推下去（`outer/inner`），子图那份
			// 定义自己的字段一个字都不改——路径是这次视图里现推的，从不上定义。
			// 环那一处**就地收住**（`Sub` 之后没有了）：展开不完，报由 Problems 出。
			if !chain.has(n.child) {
				ns.Sub = n.child.topologyAt(joinPath(at, n.id), chain.push(n.child), n.child.effectiveCapacity(capacity), streams)
			}
		}
		t.Nodes = append(t.Nodes, ns)
	}
	for ci := range g.nodes {
		for _, e := range d.in[ci] {
			t.Edges = append(t.Edges, EdgeShape{
				From: g.nodes[e.from].id,
				To:   g.nodes[ci].id,
				Keys: append([]string(nil), e.keys...),
			})
		}
	}
	t.Problems = g.problemsWithStreamsAtLocked(at, chain, capacity, streams)
	return t
}

// keyShapes 把一组键指纹翻成公开形状。sourced 为真时填来源（Requires / InPorts
// 需要，快照 Inputs 同样复用）——表里没有这条 Key 就是「没有来源」，不是零值落表。
//
// 来源两条路：**谁会写它**（外部 `Seed` / 某个节点 `Provides`）优先；没人写就看它是不是
// 声明过的入口（`DeclareInput`）→ `SourceInput`。端口**不落** producer 表（那张表回答
// 「谁会写这条槽」，端口回答「这张图接受什么」），所以必须两步问，不能只看表。
func (g *Graph) keyShapes(refs []keyRef, sourced bool) []KeyShape {
	out := make([]KeyShape, 0, len(refs))
	for _, k := range refs {
		typ := ""
		if k.typ != nil {
			typ = k.typ.String()
		}
		ks := KeyShape{Name: k.name, Type: typ}
		if sourced {
			ks.From = publicSource(g.sourceOfLocked(k))
		}
		out = append(out, ks)
	}
	return out
}

// depIn 是「谁喂了这条键」：上游节点 + 它提供的全部 Key。
type depIn struct {
	from int
	keys []string
}

// depOut 是「这个上游喂了谁」：下游节点 + 中间那几条 Key。
type depOut struct {
	to   int
	keys []string
}

// depGraph 是「谁喂谁」的唯一一份推导。
type depGraph struct {
	// in 按**下游**分组：in[ci] 是「谁喂了 ci」，按 ci 的 Requires 出现顺序，
	// 同一对上游只留一条边。
	in [][]depIn
	// out 按**上游**分组：out[i] 是「i 喂了谁」，按下游声明序——与旧实现的
	// `succ` 同序，所以环见证的走法逐字不变。
	out [][]depOut
}

// depGraphLocked 推导节点间依赖边。调用方持 g.mu。
//
// 建边判据：N 的某条 `Requires` 由节点 M 提供 → 边 M → N（M 必须先跑完）。
// 来源**不是节点**的 Key 不建边：声明过的入口由这一次运行的输入喂（它不是「上游
// 节点」），零值按「没有来源」处理。自环不可能出现——`Requires` 与 `Provides`
// 同名的节点在 `Add` 就被拒。
//
// 判据读的一定是来源的 `kind`，**不能**拿来源名去查节点索引——一个真叫 `seed`
// 的节点会让「不是节点提供的那条键」凭空多出一条边和一个不存在的环（旧实现就是
// 这么错的）。
func (g *Graph) depGraphLocked() depGraph {
	index := make(map[string]int, len(g.nodes))
	for i, n := range g.nodes {
		index[n.id] = i
	}

	d := depGraph{
		in:  make([][]depIn, len(g.nodes)),
		out: make([][]depOut, len(g.nodes)),
	}
	slot := make(map[[2]int]int) // (上游, 下游) → 在 in[下游] 里的下标
	for ci, n := range g.nodes {
		for _, k := range n.requires {
			src := g.producer[k.name]
			if !src.present() || src.kind != sourceNode {
				continue // 输入端口 / 没人提供（后者由 problemsSourcesLocked 拦下）
			}
			pi, ok := index[src.nodeID]
			if !ok {
				continue // 不可能：登记进来源表的节点一定在 index 里
			}
			pair := [2]int{pi, ci}
			if j, dup := slot[pair]; dup {
				d.in[ci][j].keys = append(d.in[ci][j].keys, k.name)
				continue // 同一对节点被多条 Requires 依赖：只留一条边
			}
			slot[pair] = len(d.in[ci])
			d.in[ci] = append(d.in[ci], depIn{from: pi, keys: []string{k.name}})
			d.out[pi] = append(d.out[pi], depOut{to: ci, keys: []string{k.name}})
		}
	}
	return d
}

// order 是**剥叶**（Kahn）序：先声明序入队，剥得动的一层层往后排。
//
// 它只承诺边的方向（A → B ⇒ A 在前）；没有依赖关系的节点之间的先后是这套剥法的
// 自然结果，同一张图每次一样，但别当业务语义用。有环时返回的是剥得掉的那批
// ——环上的节点剥不动，正好留给 Problems 去报。
func (d depGraph) order(nodes []*Node) []string {
	indeg := make([]int, len(nodes))
	for i := range nodes {
		indeg[i] = len(d.in[i])
	}
	queue := make([]int, 0, len(nodes))
	for i := range nodes {
		if indeg[i] == 0 {
			queue = append(queue, i)
		}
	}
	out := make([]string, 0, len(nodes))
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		out = append(out, nodes[i].id)
		for _, e := range d.out[i] {
			indeg[e.to]--
			if indeg[e.to] == 0 {
				queue = append(queue, e.to)
			}
		}
	}
	return out
}

// cyclicSCCs 按**声明序**求强连通分量（Tarjan），只返回**环分量**（节点数 ≥ 2）。
//
// 环见证需要它：只有分量能回答「这几个节点是不是真在一个环里」。顺着「第一条出边」
// 走是不够的——旧实现就走出了环、走到一个没有出边的节点，还把路径首尾相接报成一句
// 不存在的环（实测 `pulse: dependency cycle: A -> C -> A ()`）。分量内每个节点都必有
// 分量内的出边（强连通的定义），所以分量内的走法一定闭合。
//
// 自环在 `Add` 就被拒（同一节点不能既 `Requires` 又 `Provides` 同一个 Key），所以
// 「节点数 ≥ 2」就是「有环」的完整判据。
//
// 顺序：Tarjan 的弹出顺序（分量之间反向的拓扑序），由声明序与出边顺序唯一确定。
func (d depGraph) cyclicSCCs() [][]int {
	n := len(d.in)
	index := make([]int, n)
	low := make([]int, n)
	onStack := make([]bool, n)
	seq := 0
	var stack []int
	var out [][]int

	var strongConnect func(v int)
	strongConnect = func(v int) {
		seq++
		index[v], low[v] = seq, seq
		stack = append(stack, v)
		onStack[v] = true
		for _, e := range d.out[v] {
			w := e.to
			switch {
			case index[w] == 0:
				strongConnect(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			case onStack[w]:
				if index[w] < low[v] {
					low[v] = index[w]
				}
			}
		}
		if low[v] != index[v] {
			return
		}
		var comp []int
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			comp = append(comp, w)
			if w == v {
				break
			}
		}
		if len(comp) >= 2 {
			out = append(out, comp)
		}
	}
	for v := range d.in {
		if index[v] == 0 {
			strongConnect(v)
		}
	}
	return out
}
