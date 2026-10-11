package pulse

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
	// SourceSeed 表示外部来源：`Seed` / `SkipSeed`。它不是节点间依赖，
	// **不建边**。
	SourceSeed SourceKind = "seed"
	// SourceNode 表示某个节点的 `Provides`，`NodeID` 是那个节点。
	SourceNode SourceKind = "node"
)

// Source 表示一个 Key 的来源。
type Source struct {
	Kind   SourceKind
	NodeID string // 仅 Kind == SourceNode 时有效
}

// String 是给人看的来源名：节点 id / `seed` / `none`。
func (s Source) String() string {
	switch s.Kind {
	case SourceNode:
		return s.NodeID
	case SourceSeed:
		return "seed"
	default:
		return "none"
	}
}

// publicSource 把引擎内部的来源记录翻成公开形状，**只此一处**——两处各写一遍
// 就会漂（节点 id 恰好叫 `seed` 那条路径就是这么错的，见 source.go）。
func publicSource(s source) Source {
	switch s.kind {
	case sourceSeed:
		return Source{Kind: SourceSeed}
	case sourceNode:
		return Source{Kind: SourceNode, NodeID: s.nodeID}
	default:
		return Source{}
	}
}

// KeyShape 是键的形状：名字、类型记号、来源。
//
// `Type` 用的是 `reflect.Type.String()`，与声明式装配（`pulse/yaml`）的 type
// 记号是同一套字面量。
type KeyShape struct {
	Name string
	Type string
	// From 只有 `Requires` 有意义（那是「谁喂了它」）；`Provides` 上恒为
	// `SourceNone`。
	From Source
}

// GraphShape 是图自己的那点元数据。
type GraphShape struct {
	ID          string
	Description string
	// Path 是这张图在嵌套里的位置（根图为 `""`，一层子图是它的节点 id，再深一层
	// 是 `outer/inner`）。由 `Sub` 在跑子图之前盖上，所以**子图 build 回调返回前
	// 读它可能还是上一层的值**；一旦被 `Sub` 认领并进入启动流程，路径即稳定。
	Path string
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
	// 顺序固定：来源 → 依赖环 → 流式。空 = 没毛病。
	//
	// 它覆盖的是引擎能从**已装入的图**推导出来的那部分；声明式装配（YAML）的
	// 结构 / 绑定诊断归 `pulse/yaml`，不在这里重复。
	Problems []error
}

// Topology 返回这张图的静态形状快照。
//
// 只读、加锁、任何时候可调；返回的是副本（`Nodes` / `Edges` / `Order` /
// `Problems` 全是新切片），改它不会碰图。
func (g *Graph) Topology() *Topology {
	g.mu.Lock()
	defer g.mu.Unlock()

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
		Graph: GraphShape{ID: g.id, Description: g.desc, Path: g.path},
		Nodes: make([]NodeShape, 0, len(g.nodes)),
		Edges: make([]EdgeShape, 0),
		Order: d.order(g.nodes),
	}
	for i, n := range g.nodes {
		t.Nodes = append(t.Nodes, NodeShape{
			ID:          n.id,
			Description: n.desc,
			Requires:    g.keyShapes(n.requires, true),
			Provides:    g.keyShapes(n.provides, false),
			InDegree:    inDeg[i],
			OutDegree:   outDeg[i],
		})
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
	t.Problems = g.problemsLocked()
	return t
}

// keyShapes 把一组键指纹翻成公开形状。sourced 为真时填来源（只有 `Requires`
// 需要）——表里没有这条 Key 就是「没有来源」，不是零值落表。
func (g *Graph) keyShapes(refs []keyRef, sourced bool) []KeyShape {
	out := make([]KeyShape, 0, len(refs))
	for _, k := range refs {
		typ := ""
		if k.typ != nil {
			typ = k.typ.String()
		}
		ks := KeyShape{Name: k.name, Type: typ}
		if sourced {
			ks.From = publicSource(g.producer[k.name])
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
// 来源**不是节点**的 Key 不建边：外部 `Seed` / `SkipSeed` 在 `Start` 之前就已经
// 到达了，零值按「没有来源」处理。自环不可能出现——`Requires` 与 `Provides`
// 同名的节点在 `Add` 就被拒。
//
// 判据读的一定是来源的 `kind`，**不能**拿来源名去查节点索引——一个真叫 `seed`
// 的节点会让「来自外部 Seed 的 Key」凭空多出一条边和一个不存在的环（旧实现就是
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
				continue // 外部 Seed，或没人提供（后者由 problemsSourcesLocked 拦下）
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
