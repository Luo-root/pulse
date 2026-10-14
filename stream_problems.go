package pulse

import (
	"fmt"
	"reflect"
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
	bodySkipped bool // 全输入确定 skip，或挂载它的祖先不会进入 Sub body
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
	if !g.hasStreamsLocked(chain) {
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

// 没有已知流糖就沿用普通检查，不为大量未使用端口建流模型。仍逐层取锁，
// 判定不承诺跨层原子快照；Validate / Start 的 assemblyMu 保证其版本一致。
func (g *Graph) hasStreamsLocked(chain mountChain) bool {
	for _, n := range g.nodes {
		if n.streamKind != "" {
			return true
		}
	}
	for _, n := range g.nodes {
		if n.child == nil || chain.has(n.child) {
			continue
		}
		n.child.mu.Lock()
		has := n.child.hasStreamsLocked(chain.push(n.child))
		n.child.mu.Unlock()
		if has {
			return true
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
	return k != nil && k.ref.typ != nil && k.ref.typ.Kind() == reflect.Chan
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
				c.root().parent = p.root()
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
		if w := k.writer; w != nil && streamSender(w) {
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
			seen[gr], live = true, true
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
		if !gr.known || (gr.source != nil && gr.source.writer.bodySkipped) {
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
