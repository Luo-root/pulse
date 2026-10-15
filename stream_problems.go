package pulse

import (
	"fmt"
	"strings"
)

// streamTree 是一次静态检查的副本。身份是挂载位置，不是 Graph 指针：同一
// 定义挂两处，对应两份子执行、两组节点，自己的 quota 声明也各建一份池。
// 不读运行状态、不建槽位、不改变封口；每层只在自己的锁内复制定义。
type streamTree struct {
	sites  []*streamSite
	nodes  []*streamNode
	keys   []*streamKey
	pools  []*streamPool
	groups []*streamGroup
}

type streamSite struct {
	at         string
	pool       *streamPool
	mount      *streamNode
	nodes      []*streamNode
	keys       map[string]*streamKey
	hasAspects bool
}

type streamNode struct {
	def         Node
	at          string
	site        *streamSite
	child       *streamSite
	bodySkipped bool // 普通节点的全输入确定 skip；结构 Sub 始终可进入
}

type streamKey struct {
	ref     keyRef
	site    *streamSite
	writer  *streamNode
	parent  *streamKey // channel 别名的并查集；普通值不合并到达事件
	group   *streamGroup
	skipped bool // 确定的到达状态；没有事实的入口仍是未知，不能当作 skip
}

type streamPool struct {
	capacity int
	live     []*streamNode
}

type streamGroup struct {
	key     *streamKey
	source  *streamKey // Produce / Tee 的真实 channel 来源；每条出口独立
	readers []*streamNode
	known   bool
}

// problemsStreamLocked 展开整个检查范围，再把诊断放回最小共同图层，保持
// 来源 → 出口 → 普通依赖环 → 流式 → 子树的既有顺序。根 capacity 已解析
// ExecOption > 定义声明；子层有声明就新建池，否则继承同一池（不能按数值合并）。
func (g *Graph) problemsStreamLocked(at string, chain mountChain, capacity int, known ...streamKnownInputs) map[string][]error {
	if !g.needsWaitTreeLocked() {
		return nil
	}
	t := &streamTree{}
	pool := &streamPool{capacity: capacity}
	t.pools = append(t.pools, pool)
	facts := streamKnownInputs{hasAspects: len(g.aspects) != 0}
	if len(known) != 0 {
		facts = known[0]
	}
	t.collectLocked(g, at, chain, pool, nil, facts.hasAspects)
	t.propagateSkips(facts.skipped)
	t.connectAliases()
	t.collectReaders()
	out := make(map[string][]error)
	t.problemsSlots(out)
	t.problemsReaders(out)
	t.problemsLifecycle(out)
	return out
}

// 没有流声明且没有 Sub 的普通图沿用原 DAG 快路；结构 Sub 的实际输入依赖
// 即便都是普通值也必须展开（可选、未读取的 In 不是整个 child 的启动门）。
// 不复制未使用端口；Validate / Start 的 assemblyMu 保证整个检查版本一致。
func (g *Graph) needsWaitTreeLocked() bool {
	for _, n := range g.nodes {
		if n.child != nil || n.streamKind != "" {
			return true
		}
		for _, ref := range n.requires {
			if isLiveStreamType(ref.typ) {
				return true
			}
		}
		for _, ref := range n.provides {
			if isLiveStreamType(ref.typ) {
				return true
			}
		}
	}
	return false
}

// 调用方持 g.mu。Node 在入图后不可改；仍复制切片，后续推导只读这份副本。
// checkBinds 自己取子锁，必须在 collect 的子锁之外调用；组合环先挡住重入。
func (t *streamTree) collectLocked(g *Graph, at string, chain mountChain, pool *streamPool, mount *streamNode, hasAspects bool) *streamSite {
	s := &streamSite{at: at, pool: pool, mount: mount, keys: make(map[string]*streamKey), hasAspects: hasAspects}
	t.sites = append(t.sites, s)
	for _, n := range g.nodes {
		def := *n
		def.requires = append([]keyRef(nil), n.requires...)
		def.provides = append([]keyRef(nil), n.provides...)
		def.binds = append([]SubBind(nil), n.binds...)
		sn := &streamNode{def: def, at: joinPath(at, n.id), site: s}
		s.nodes = append(s.nodes, sn)
		t.nodes = append(t.nodes, sn)
		for _, ref := range def.requires {
			t.key(s, ref)
		}
		for _, ref := range def.provides {
			t.key(s, ref).writer = sn
		}
	}
	// 只取节点声明和这一处实际绑定涉及的键。未使用端口不参与等待，
	// 不应让不断增长的可选端口表放大每次 Topology 的流检查。
	if mount != nil {
		for _, b := range mount.def.binds {
			if b.fix != nil {
				for _, ref := range b.fixKeys() {
					t.key(s, ref).skipped = b.fix.in.entries[ref.name].skip
				}
			} else {
				t.key(s, b.child)
			}
		}
	}
	for _, n := range s.nodes {
		child := n.def.child
		if child == nil || chain.has(child) {
			continue
		}
		if checkBinds(child, n.def.id, n.def.binds) != nil {
			continue // 接线错误由原有子树检查报；不从错误接线猜流关系
		}
		child.mu.Lock()
		childPool := pool
		if child.limSet {
			childPool = &streamPool{capacity: child.declaredCapacity()}
			t.pools = append(t.pools, childPool)
		}
		childAspects := (hasAspects && !child.aspectsCut) || len(child.aspects) != 0
		n.child = t.collectLocked(child, n.at, chain.push(child), childPool, n, childAspects)
		child.mu.Unlock()
	}
	return s
}

func (t *streamTree) key(s *streamSite, ref keyRef) *streamKey {
	if k := s.keys[ref.name]; k != nil {
		return k
	}
	k := &streamKey{ref: ref, site: s}
	k.parent = k
	s.keys[ref.name] = k
	t.keys = append(t.keys, k)
	return k
}

func streamChannel(k *streamKey) bool {
	return k != nil && isLiveStreamType(k.ref.typ)
}

func (k *streamKey) root() *streamKey {
	if k.parent != k {
		k.parent = k.parent.root()
	}
	return k.parent
}

func (t *streamTree) connectAliases() {
	for _, n := range t.nodes {
		if n.child == nil {
			continue
		}
		for _, b := range n.def.binds {
			if b.fix != nil {
				continue // 固定值不推断实际 channel 指针；它不是静态接线
			}
			p, c := n.site.keys[b.parent.name], n.child.keys[b.child.name]
			if streamChannel(p) && streamChannel(c) && p.ref.typ == c.ref.typ {
				// 透传和多层出口可能已经属于同组，不能给并查集制造自引用。
				pr, cr := p.root(), c.root()
				if pr != cr {
					cr.parent = pr
				}
			}
		}
	}
	byRoot := make(map[*streamKey]*streamGroup)
	for _, k := range t.keys {
		if !streamChannel(k) {
			continue
		}
		r := k.root()
		gr := byRoot[r]
		if gr == nil {
			gr = &streamGroup{key: k}
			byRoot[r] = gr
			t.groups = append(t.groups, gr)
		}
		k.group = gr
		if w := k.writer; w != nil && streamSender(w) && w.def.streamKind != "" {
			gr.source, gr.key, gr.known = k, k, true
		}
	}
}

func streamSender(n *streamNode) bool {
	return n.def.streamKind == "produce" || n.def.streamKind == "tee"
}

func (t *streamTree) collectReaders() {
	// 先认出已知流，再展开所有 Requires；Sub 仅搬值，不是 channel 读取者。
	for _, n := range t.nodes {
		if n.def.streamKind != "consume" && n.def.streamKind != "tee" {
			continue
		}
		for _, ref := range n.def.requires {
			if gr := n.site.keys[ref.name].group; gr != nil {
				gr.known = true
			}
		}
	}
	for _, n := range t.nodes {
		if n.def.sub || n.bodySkipped {
			continue
		}
		live := n.def.streamKind != ""
		seen := make(map[*streamGroup]bool)
		for _, ref := range n.def.requires {
			gr := n.site.keys[ref.name].group
			if gr == nil || !gr.known || seen[gr] {
				continue
			}
			seen[gr] = true
			// 原生 channel 糖整段持额，保留并发保护；受管端点的消息等待由
			// 运行期归还名额，不能把它与原生 channel 混成同一套保守计数。
			live = true
			gr.readers = append(gr.readers, n)
		}
		if live {
			n.site.pool.live = append(n.site.pool.live, n)
		}
	}
}

// 仍保守地为同一池里的全部流参与者留名额，避免逐个抢额造成互相阻塞。
// 一名节点读几个别名也只计一次；不同池彼此独立，Sub 代理完全不占额。
func (t *streamTree) problemsSlots(out map[string][]error) {
	for _, p := range t.pools {
		if p.capacity <= 0 || p.capacity >= len(p.live) {
			continue
		}
		ids, at := streamNodeNames(p.live)
		out[at] = append(out[at], fmt.Errorf("pulse: stream needs %d slots at the same time, but WithMaxRunning(%d): %s",
			len(p.live), p.capacity, strings.Join(ids, ", ")))
	}
}

func (t *streamTree) problemsReaders(out map[string][]error) {
	for _, gr := range t.groups {
		if !gr.known {
			continue
		}
		if gr.source != nil && gr.source.writer.bodySkipped {
			continue
		}
		if len(gr.readers) == 0 && gr.source != nil {
			n := gr.source.writer
			out[n.site.at] = append(out[n.site.at], fmt.Errorf("pulse: node %q provides %q but nothing consumes it: "+
				"a stream output nobody reads blocks the sender forever", n.at, gr.source.ref.name))
			continue
		}
		if len(gr.readers) > 1 && !streamSameFanOut(gr.readers) {
			ids, at := streamNodeNames(gr.readers)
			at = streamCommonPath(at, gr.key.site.at)
			out[at] = append(out[at], fmt.Errorf("pulse: stream output %q is read by %d nodes (%s), "+
				"so its values would be split between them: one stream output feeds one downstream — "+
				"add a Tee output per downstream to broadcast, or make it one FanOut to compete",
				gr.key.ref.name, len(gr.readers), strings.Join(ids, ", ")))
		}
	}
}

// compete 的 id 只在一层定义内唯一；同定义的两处挂载也是两次 FanOut。
func streamSameFanOut(rs []*streamNode) bool {
	if rs[0].def.compete == "" {
		return false
	}
	for _, r := range rs[1:] {
		if r.site != rs[0].site || r.def.compete != rs[0].def.compete {
			return false
		}
	}
	return true
}

func streamNodeNames(ns []*streamNode) ([]string, string) {
	ids := make([]string, 0, len(ns))
	at := ns[0].site.at
	for _, n := range ns {
		ids = append(ids, n.at)
		at = streamCommonPath(at, n.site.at)
	}
	return ids, at
}

func streamCommonPath(a, b string) string {
	for a != "" && a != b && !strings.HasPrefix(b, a+"/") {
		if i := strings.LastIndexByte(a, '/'); i >= 0 {
			a = a[:i]
		} else {
			a = ""
		}
	}
	return a
}

// streamKnownInputs 只为流检查补充本次已供给的确定事实，不改 Graph 定义。
// Validate / Topology 没有这份执行事实，只能从 FixSkip 推导。Start 的
// hasAspects 已包含 ExecOption 和继承环境，不能只看定义自己的声明。
type streamKnownInputs struct {
	skipped    map[string]bool
	hasAspects bool
}

// streamSkipWork 记录建索引和传播实际访问的声明数，供包内回归校验线性工作量。
// 每条键/节点至多访问两次，每条 Requires/绑定至多两次，Provides/子节点至多一次。
type streamSkipWork struct {
	inputs, keys, nodes, requires, provides, binds, mounts int
}

type streamSkipBind struct {
	mount *streamNode
	to    *streamKey
}

// propagateSkips 区分「body 不进入」与「输出一定 skip」：门在全部输入
// skipped 时不调用 body，但门外切面仍可能先写 Provides 或短路返回。
// 所以只有无有效切面的跳过节点才推导输出 skip；有切面的输出仍是未知。
// In / Out 转送到达状态，不把不同挂载点的事实存回共享定义。
func (t *streamTree) propagateSkips(rootSkipped map[string]bool) streamSkipWork {
	var work streamSkipWork
	for name, skipped := range rootSkipped {
		work.inputs++
		if k := t.sites[0].keys[name]; skipped && k != nil && k.writer == nil {
			k.skipped = true
		}
	}
	// 只作单调的声明事实传播，不执行节点，也不安排运行顺序。反向索引只建
	// 一次；新增 skip 只通知直接依赖者，避免倒序声明的长链反复全图扫描。
	readers := make(map[*streamKey][]*streamNode)
	binds := make(map[*streamKey][]streamSkipBind)
	remaining := make(map[*streamNode]int, len(t.nodes))
	for _, n := range t.nodes {
		work.nodes++
		remaining[n] = len(n.def.requires)
		for _, ref := range n.def.requires {
			work.requires++
			if !n.def.sub {
				k := n.site.keys[ref.name]
				readers[k] = append(readers[k], n)
			}
		}
		if n.child == nil {
			continue
		}
		for _, b := range n.def.binds {
			work.binds++
			if b.fix != nil {
				continue // collectLocked 已收下固定输入的 skip 标记
			}
			from, to := n.site.keys[b.parent.name], n.child.keys[b.child.name]
			if !b.in {
				if n.site.hasAspects || len(n.def.aspects) != 0 {
					continue // 挂载步骤的切面也可能抢先写父侧 Provides
				}
				from, to = to, from
			}
			binds[from] = append(binds[from], streamSkipBind{mount: n, to: to})
		}
	}
	var keys []*streamKey
	for _, k := range t.keys {
		work.keys++
		if k.skipped {
			keys = append(keys, k)
		}
	}
	markKey := func(k *streamKey) {
		if streamMarkSkipped(k) {
			keys = append(keys, k)
		}
	}
	var bodies []*streamNode
	markBody := func(n *streamNode) {
		if !n.bodySkipped {
			n.bodySkipped = true
			bodies = append(bodies, n)
		}
	}
	for keyAt, bodyAt := 0, 0; keyAt < len(keys); keyAt++ {
		k := keys[keyAt]
		work.keys++
		for _, n := range readers[k] {
			work.requires++
			remaining[n]--
			if remaining[n] == 0 {
				markBody(n)
			}
		}
		// Sub 不设整体输入门；只传播真实普通节点按自己 Requires 得到的 skip。
		for ; bodyAt < len(bodies); bodyAt++ {
			n := bodies[bodyAt]
			work.nodes++
			if remaining[n] == 0 && len(n.def.requires) != 0 && !n.site.hasAspects && len(n.def.aspects) == 0 {
				for _, ref := range n.def.provides {
					work.provides++
					markKey(n.site.keys[ref.name])
				}
			}
		}
		for _, b := range binds[k] {
			work.binds++
			if !b.mount.bodySkipped {
				markKey(b.to)
			}
		}
	}
	return work
}

func streamMarkSkipped(k *streamKey) bool {
	if k.skipped {
		return false
	}
	k.skipped = true
	return true
}
