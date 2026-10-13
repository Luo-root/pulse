package pulse

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// MermaidOption 配置静态拓扑的 Mermaid 输出。
type MermaidOption func(*mermaidConfig)

type mermaidConfig struct {
	snapshot *Snapshot
}

// WithMermaidSnapshot 给静态形状叠加一次执行的节点状态。
//
// 只匹配图 ID、挂载 Path 和节点 ID 都相同的节点；未知状态不着色。
// 子状态从匹配节点的 Child.Snapshot() 逐层读取，未创建子执行时只有静态子树。
// 这不是整棵执行树在同一瞬间的原子切面：子执行仍在推进时，重复渲染的状态可能不同。
// 快照不会补建节点、端口或边，输入值、固定值和错误内容都不进入输出。
// s 为 nil 时与不传此选项相同。
func WithMermaidSnapshot(s *Snapshot) MermaidOption {
	return func(c *mermaidConfig) { c.snapshot = s }
}

// Mermaid 返回整棵静态拓扑的 Mermaid flowchart TD 文本，可直接放入 Mermaid 代码块。
//
// 图、节点、端口及挂载接线沿视图的声明序展开，内部 ID 由遍历序数产生；用户标识只作为
// 经过编码的显示文本。未使用端口也会保留，同一张子图挂多处会各展开一份。
// Fix 与 FixSkip 在静态形状中都显示为 fixed，不展示值，也不推测运行状态。
// 不给 WithMermaidSnapshot 时没有状态标记或着色；nil 接收者返回只有 flowchart 声明的文本。
// 调用方不应在渲染时并发修改传入的 Topology 或 Snapshot 副本。
func (t *Topology) Mermaid(opts ...MermaidOption) string {
	cfg := mermaidConfig{}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	r := mermaidRenderer{}
	r.out.WriteString("flowchart TD\n")
	if t != nil {
		g := r.index(t)
		r.graph(g, cfg.snapshot, nil, "", 1)
		r.edges(g)
		r.styles()
	}
	return r.out.String()
}

type mermaidGraph struct {
	top      *Topology
	id       string
	in, out  map[string]string
	nodes    []*mermaidNode
	byID     map[string]*mermaidNode
	producer map[string]*mermaidNode
}

type mermaidNode struct {
	shape *NodeShape
	id    string
	child *mermaidGraph
	binds []mermaidBind
}

type mermaidBind struct {
	shape BindShape
	id    string
}

type mermaidClass struct {
	id     string
	status NodeStatus
}

type mermaidRenderer struct {
	out     strings.Builder
	seq     int
	classes []mermaidClass
}

func (r *mermaidRenderer) id() string {
	id := "m" + strconv.Itoa(r.seq)
	r.seq++
	return id
}

// index 只沿切片分配 ID；map 仅查找，不参与输出顺序。
func (r *mermaidRenderer) index(t *Topology) *mermaidGraph {
	g := &mermaidGraph{
		top: t, id: r.id(),
		in: make(map[string]string), out: make(map[string]string),
		byID: make(map[string]*mermaidNode), producer: make(map[string]*mermaidNode),
	}
	for _, p := range t.Graph.InPorts {
		g.in[p.Name] = r.id()
	}
	for _, p := range t.Graph.OutPorts {
		g.out[p.Name] = r.id()
	}
	for i := range t.Nodes {
		n := &mermaidNode{shape: &t.Nodes[i], id: r.id()}
		g.nodes = append(g.nodes, n)
		g.byID[n.shape.ID] = n
		for _, p := range n.shape.Provides {
			g.producer[p.Name] = n
		}
		if n.shape.Sub != nil {
			for _, b := range n.shape.Binds {
				n.binds = append(n.binds, mermaidBind{shape: b, id: r.id()})
			}
			n.child = r.index(n.shape.Sub)
		}
	}
	return g
}

func (r *mermaidRenderer) graph(g *mermaidGraph, s *Snapshot, mount *mermaidNode, mountLabel string, depth int) {
	label := g.top.Graph.ID
	if label == "" {
		label = "graph"
	}
	if g.top.Graph.Description != "" {
		label += " · " + g.top.Graph.Description
	}
	if g.top.Graph.Path != "" {
		label += " @ " + g.top.Graph.Path
	}
	r.line(depth, "subgraph "+g.id+"[\""+mermaidText(label)+"\"]")
	if mount != nil {
		r.vertex(depth+1, mount.id, "Sub · "+mountLabel, false)
	}
	for _, p := range g.top.Graph.InPorts {
		r.vertex(depth+1, g.in[p.Name], mermaidKeyLabel("in", p), true)
	}
	for _, p := range g.top.Graph.OutPorts {
		r.vertex(depth+1, g.out[p.Name], mermaidKeyLabel("out", p), true)
	}
	states := make(map[string]NodeState)
	if s != nil && s.Graph.ID == g.top.Graph.ID && s.Graph.Path == g.top.Graph.Path {
		for _, n := range s.Nodes {
			states[n.ID] = n
		}
	}
	for _, n := range g.nodes {
		label := n.shape.ID
		if n.shape.Description != "" {
			label += " · " + n.shape.Description
		}
		state, matched := states[n.shape.ID]
		if matched && mermaidStyle(state.Status) != "" {
			label += " · " + string(state.Status)
			r.classes = append(r.classes, mermaidClass{id: n.id, status: state.Status})
		}
		if n.child == nil {
			r.vertex(depth+1, n.id, label, false)
			continue
		}
		// 这些端点属于挂载点，不属于子图定义；固定输入只有形状，没有载荷。
		for _, b := range n.binds {
			if b.shape.Fixed {
				r.line(depth+1, b.id+"{\"fixed\"}")
				continue
			}
			kind, keys := "out", n.shape.Provides
			if b.shape.In {
				kind, keys = "in", n.shape.Requires
			}
			p := KeyShape{Name: b.shape.Parent}
			for _, k := range keys {
				if k.Name == p.Name {
					p.Type = k.Type
					break
				}
			}
			r.vertex(depth+1, b.id, n.shape.ID+" · "+mermaidKeyLabel(kind, p), true)
		}
		var childState *Snapshot
		if matched && state.Child != nil {
			childState = state.Child.Snapshot()
		}
		r.graph(n.child, childState, n, label, depth+1)
	}
	r.line(depth, "end")
}

func mermaidKeyLabel(kind string, k KeyShape) string {
	label := kind + " · " + k.Name
	if k.Type != "" {
		label += ": " + k.Type
	}
	return label
}

func (r *mermaidRenderer) vertex(depth int, id, label string, port bool) {
	if port {
		r.line(depth, id+"([\""+mermaidText(label)+"\"])")
		return
	}
	r.line(depth, id+"[\""+mermaidText(label)+"\"]")
}

func (r *mermaidRenderer) line(depth int, text string) {
	r.out.WriteString(strings.Repeat("  ", depth))
	r.out.WriteString(text)
	r.out.WriteByte('\n')
}

func (r *mermaidRenderer) arrow(from, to, label string) {
	if from != "" && to != "" {
		r.line(1, from+" -->|\""+mermaidText(label)+"\"| "+to)
	}
}

// input/output 让父图依赖落到挂载点的对应键端点，避免把整个子图误画成一个循环节点。
func (n *mermaidNode) input(key string) []string {
	var ids []string
	for _, b := range n.binds {
		if b.shape.In && !b.shape.Fixed && b.shape.Parent == key {
			ids = append(ids, b.id)
		}
	}
	if len(ids) == 0 {
		ids = append(ids, n.id)
	}
	return ids
}

func (n *mermaidNode) output(key string) string {
	for _, b := range n.binds {
		if !b.shape.In && !b.shape.Fixed && b.shape.Parent == key {
			return b.id
		}
	}
	return n.id
}

func (r *mermaidRenderer) edges(g *mermaidGraph) {
	for _, n := range g.nodes {
		for _, k := range n.shape.Requires {
			if k.From.Kind == SourceInput {
				for _, to := range n.input(k.Name) {
					r.arrow(g.in[k.Name], to, k.Name)
				}
			}
		}
	}
	for _, e := range g.top.Edges {
		from, to := g.byID[e.From], g.byID[e.To]
		if from == nil || to == nil {
			continue
		}
		for _, key := range e.Keys {
			for _, target := range to.input(key) {
				r.arrow(from.output(key), target, key)
			}
		}
	}
	for _, p := range g.top.Graph.OutPorts {
		from := g.in[p.Name] // 同名进口/出口的透传。
		if n := g.producer[p.Name]; n != nil {
			from = n.output(p.Name)
		}
		r.arrow(from, g.out[p.Name], p.Name)
	}
	for _, n := range g.nodes {
		if n.child == nil {
			continue
		}
		for _, b := range n.binds {
			if b.shape.Fixed {
				r.arrow(b.id, n.child.in[b.shape.Child], "Fix / FixSkip")
			} else if b.shape.In {
				r.arrow(b.id, n.child.in[b.shape.Child], b.shape.Parent+" → "+b.shape.Child)
			} else {
				r.arrow(n.child.out[b.shape.Child], b.id, b.shape.Child+" → "+b.shape.Parent)
			}
		}
		r.edges(n.child)
	}
}

func (r *mermaidRenderer) styles() {
	// 样式按固定状态序输出，只有实际匹配的节点会使对应样式出现。
	for _, s := range []NodeStatus{
		NodeStatusPending, NodeStatusWaiting, NodeStatusRunning, NodeStatusCompleted,
		NodeStatusSkipped, NodeStatusFailed, NodeStatusCanceled,
	} {
		for _, c := range r.classes {
			if c.status == s {
				r.line(1, "classDef pulse_"+string(s)+" "+mermaidStyle(s))
				break
			}
		}
	}
	for _, c := range r.classes {
		r.line(1, "class "+c.id+" pulse_"+string(c.status))
	}
}

func mermaidStyle(s NodeStatus) string {
	switch s {
	case NodeStatusPending:
		return "fill:#f1f5f9,stroke:#64748b,color:#334155"
	case NodeStatusWaiting:
		return "fill:#fef3c7,stroke:#d97706,color:#92400e"
	case NodeStatusRunning:
		return "fill:#dbeafe,stroke:#2563eb,color:#1e40af"
	case NodeStatusCompleted:
		return "fill:#dcfce7,stroke:#16a34a,color:#166534"
	case NodeStatusSkipped:
		return "fill:#f3f4f6,stroke:#9ca3af,color:#4b5563"
	case NodeStatusFailed:
		return "fill:#fee2e2,stroke:#dc2626,color:#991b1b"
	case NodeStatusCanceled:
		return "fill:#ffedd5,stroke:#ea580c,color:#9a3412"
	default:
		return ""
	}
}

// mermaidText 使用 Mermaid 的十进制实体（#34; 等）。Mermaid 会把它们转为 HTML 字符引用，
// 而非重新解析成标签；原始 # 和 & 也必须编码，免得用户自己提供的实体再解一遍。
// 控制字符显示成转义文字，既不结束当前语句，也不让 HTML 标签或指令进入输出。
func mermaidText(s string) string {
	var out strings.Builder
	for _, c := range s {
		switch c {
		case '\n':
			out.WriteString("#92;n")
		case '\r':
			out.WriteString("#92;r")
		case '\t':
			out.WriteString("#92;t")
		default:
			if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) || c == '\u2028' || c == '\u2029' {
				fmt.Fprintf(&out, "#92;u%04X", c)
			} else if unicode.IsLetter(c) || unicode.IsNumber(c) || strings.ContainsRune(" .,-_/@·", c) {
				out.WriteRune(c)
			} else {
				out.WriteByte('#')
				out.WriteString(strconv.Itoa(int(c)))
				out.WriteByte(';')
			}
		}
	}
	return out.String()
}
