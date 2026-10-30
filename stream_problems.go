package pulse

import (
	"fmt"
	"sort"
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

	includeUnbound bool // 仅 Topology 的根级补充检查展开漏绑子树，既有绑定仍沿真实挂载解释。
	unbound        bool // 正常检查是否因绑定失败跳过了子树。
}

type streamSite struct {
	at         string
	graphID    string
	pool       *streamPool
	mount      *streamNode
	nodes      []*streamNode
	keys       map[string]*streamKey
	hasAspects bool
	ownAspects bool // 声明事实；冻结模型按本轮根切面重新解析继承
	aspectsCut bool
}

type streamNode struct {
	def         *Node // 开放检查的局部副本，或封口后共享的纯声明。
	at          string
	site        *streamSite
	child       *streamSite
	sender      bool // 每个节点只扫描一次 Provides，别名输出复用这个声明事实。
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
	key      *streamKey
	source   *streamKey // Produce / Tee 的真实 channel 来源；每条出口独立
	readers  []*streamNode
	known    bool
	host     bool
	outlet   bool     // 定义检查允许受管根出口稍后登记宿主；不算真实读者，Start 仍严查。
	hostKeys []string // 同一实际流上的根出口名，按名字排序；两个别名订阅也算两个读者
}

// problemsStreamLocked 展开整个检查范围，再把诊断放回最小共同图层，保持
// 来源 → 出口 → 普通依赖环 → 流式 → 子树的既有顺序。根 capacity 已解析
// ExecOption > 定义声明；子层有声明就新建池，否则继承同一池（不能按数值合并）。
func (g *Graph) problemsStreamLocked(at string, chain mountChain, capacity int, known ...streamKnownInputs) map[string][]error {
	out, _ := g.problemsStreamModelLocked(at, chain, capacity, false, known...)
	return out
}

// problemsTopologyStreamsLocked 保留正常检查的诊断与顺序，只为未覆盖的子视图补齐事实。
// 补充检查仍从真实根展开，不能把子图的出口当根出口，或丢失进入子图的 skip、别名、池与切面。
func (g *Graph) problemsTopologyStreamsLocked(at string, chain mountChain, capacity int) map[string][]error {
	out, unbound := g.problemsStreamModelLocked(at, chain, capacity, false)
	if !unbound {
		return out
	}
	complete, _ := g.problemsStreamModelLocked(at, chain, capacity, true)
	for site, problems := range complete {
		if _, checked := out[site]; !checked {
			out[site] = problems
		}
	}
	return out
}

// problemsStreamModelLocked 的第二个返回值表示有子树因绑定失败而未纳入正常检查。
// 只在 Topology 的例外路径展开这些子树；Validate / Start 始终使用正常模型。
func (g *Graph) problemsStreamModelLocked(at string, chain mountChain, capacity int, includeUnbound bool, known ...streamKnownInputs) (map[string][]error, bool) {
	if !g.needsWaitTreeLocked() {
		return nil, false
	}
	t := &streamTree{includeUnbound: includeUnbound}
	pool := &streamPool{capacity: capacity}
	t.pools = append(t.pools, pool)
	facts := streamKnownInputs{hasAspects: len(g.aspects) != 0}
	if len(known) != 0 {
		facts = known[0]
	}
	root := t.collectLocked(g, at, chain, pool, nil, facts.hasAspects)
	var hostOutlets []*streamKey
	if len(known) == 0 {
		// 定义尚无这轮宿主的注册事实，只接受声明出口的连接可能。
		// 不把它算成一次订阅；Start 总会以本轮 known 重新严格核查。
		for _, ref := range g.outPortOrder {
			if isManagedStreamType(ref.typ) {
				hostOutlets = append(hostOutlets, t.key(root, ref))
			}
		}
	}
	t.propagateSkips(facts.skipped)
	t.connectAliases()
	for _, outlet := range hostOutlets {
		outlet.group.outlet = true
	}
	t.collectHostReaders(facts.hostReads)
	t.collectReaders()
	// 键存在表示这一处挂载已纳入事件模型；没有诊断也登记，不能与未检查的子树混淆。
	out := make(map[string][]error, len(t.sites))
	for _, site := range t.sites {
		out[site.at] = nil
	}
	t.problemsSlots(out)
	t.problemsReaders(out)
	t.problemsLifecycle(out, facts.checkpoint)
	return out, t.unbound
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
	s := &streamSite{at: at, graphID: g.id, pool: pool, mount: mount, keys: make(map[string]*streamKey),
		hasAspects: hasAspects, ownAspects: len(g.aspects) != 0, aspectsCut: g.aspectsCut}
	t.sites = append(t.sites, s)
	for _, n := range g.nodes {
		def := *n
		def.requires = append([]keyRef(nil), n.requires...)
		def.provides = append([]keyRef(nil), n.provides...)
		def.binds = append([]SubBind(nil), n.binds...)
		sn := &streamNode{def: &def, at: joinPath(at, n.id), site: s, sender: nodeStreamSender(&def)}
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
	t.collectMountKeys(s, mount)
	for _, n := range s.nodes {
		child := n.def.child
		if child == nil || chain.has(child) {
			continue
		}
		if checkBinds(child, n.def.id, n.def.binds) != nil {
			t.unbound = true
			if !t.includeUnbound || !n.def.sub {
				continue // 正常检查仍由原有子树判据报绑定错误。
			}
			// Sub 初装已校验并复制全部绑定；公开装配只能追加声明和节点，不能改型或删除端口。
			// 新增必需入口漏绑不使既有绑定失效。补充视图只沿这些真实绑定展开，不猜新入口的值。
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

func (t *streamTree) collectMountKeys(s *streamSite, mount *streamNode) {
	if mount == nil {
		return
	}
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
			gr = &streamGroup{key: k, known: isManagedStreamType(k.ref.typ)}
			byRoot[r] = gr
			t.groups = append(t.groups, gr)
		}
		k.group = gr
		if w := k.writer; w != nil && streamSender(w) && (w.def.streamKind != "" || isManagedStreamType(k.ref.typ)) {
			gr.source, gr.key, gr.known = k, k, true
		}
	}
}

// 宿主注册在 Start 前完成；沿真实别名组计数，不能把两个根出口当作两份广播。
func (t *streamTree) collectHostReaders(reads map[string]bool) {
	names := make([]string, 0, len(reads))
	for name, registered := range reads {
		if registered {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		k := t.sites[0].keys[name]
		if k == nil || !isManagedStreamType(k.ref.typ) || k.group == nil {
			continue // 公开注册入口已核方向和类型；这里不推测未声明的宿主消费。
		}
		k.group.host = true
		k.group.hostKeys = append(k.group.hostKeys, name)
	}
}

func streamSender(n *streamNode) bool { return n.sender }

func nodeStreamSender(n *Node) bool {
	if n.streamKind == "produce" || n.streamKind == "tee" {
		return true
	}
	if !n.sub {
		for _, ref := range n.provides {
			if isManagedStreamType(ref.typ) {
				return true
			}
		}
	}
	return false
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
			live = live || !isManagedStreamType(ref.typ)
			gr.readers = append(gr.readers, n)
		}
		if live {
			n.site.pool.live = append(n.site.pool.live, n)
		}
	}
}

// 仍保守地为同一池里的全部流参与者留名额，避免逐个抢额造成互相阻塞。
// 一名节点读几个别名也只计一次；不同池彼此独立，Sub 代理完全不占额。
func (t *streamTree) problemsSlots(out map[string][]error, rootCapacity ...int) {
	for i, p := range t.pools {
		capacity := p.capacity
		if i == 0 && len(rootCapacity) != 0 {
			capacity = rootCapacity[0] // 池身份不变，只替换本轮根池的有效容量。
		}
		if capacity <= 0 || capacity >= len(p.live) {
			continue
		}
		ids, at := streamNodeNames(p.live)
		out[at] = append(out[at], fmt.Errorf("pulse: stream needs %d slots at the same time, but WithMaxRunning(%d): %s",
			len(p.live), capacity, strings.Join(ids, ", ")))
	}
}

func (t *streamTree) problemsReaders(out map[string][]error) {
	for _, gr := range t.groups {
		t.problemsReader(out, gr)
	}
}

// 单组判据也供冻结模型使用；调用方给本轮宿主事实的私有浅副本。
func (t *streamTree) problemsReader(out map[string][]error, gr *streamGroup) {
	if !gr.known {
		return
	}
	if gr.host && len(gr.readers)+len(gr.hostKeys) > 1 {
		ids := make([]string, 0, len(gr.readers)+len(gr.hostKeys))
		for _, name := range gr.hostKeys {
			ids = append(ids, fmt.Sprintf("host output %q", name))
		}
		for _, n := range gr.readers {
			ids = append(ids, fmt.Sprintf("node %q", n.at))
		}
		out[t.sites[0].at] = append(out[t.sites[0].at], fmt.Errorf("pulse: stream output %q is read by %d readers (%s), "+
			"so its values would be split between them: a host subscription and graph readers, or two subscribed outlet aliases, "+
			"must use separate Tee outputs", gr.key.ref.name, len(ids), strings.Join(ids, ", ")))
		return
	}
	// 业务体被输入门跳过时，门外切面仍可发布流；只有输出确定
	// skipped（没有端点）才不需要读者，复用 propagateSkips 的事实。
	if gr.source != nil && gr.source.skipped {
		return
	}
	if len(gr.readers) == 0 && !gr.host && !gr.outlet && gr.source != nil {
		n := gr.source.writer
		out[n.site.at] = append(out[n.site.at], fmt.Errorf("pulse: node %q provides %q but nothing consumes it: "+
			"a stream output nobody reads blocks the sender forever", n.at, gr.source.ref.name))
		return
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
// checkpoint 同样只属于这次检查，完整链普通值的提交边界适用于整棵执行树。
type streamKnownInputs struct {
	skipped    map[string]bool
	hasAspects bool
	hostReads  map[string]bool
	checkpoint bool
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
