package pulse

import (
	"fmt"
	"sort"
)

// validationDefinition 每个封口定义只保留一份局部声明，子定义通过指针共享。
// 没有绝对挂载路径、池、到达状态、业务值或闭包；这些身份只在求值时展开。
// 在成功检查后的装配边界内发布，之后只读，不需再取子图锁。
type validationDefinition struct {
	graphID    string
	nodes      []validationNode
	outlets    []keyRef
	capacity   int
	limSet     bool
	ownAspects bool
	aspectsCut bool
	needsTree  bool
}

type validationNode struct {
	def    Node // 仅声明字段；不保留原 Node 或其业务闭包。
	child  *validationDefinition
	sender bool
}

// validationPlan 只在最终检查全部通过后安装。来源、端口、接线及普通 DAG
// 已在同一装配边界内通过；各根共享声明，不常驻保留展开后的模板树。
// modes 的下标只有根有效切面 / checkpoint 两个布尔位，不按输入组合缓存。
// plan 与 modes 的发布均由所属 Graph.mu 保护；求值不再锁任何子定义。
type validationPlan struct {
	definition *validationDefinition
	modes      [4]*validationFacts
	pending    bool // 已封子图首次独立检查的候选；成功发布，否则解锁前撤下。
}

type validationFacts struct {
	tree           *streamTree
	outlets        map[*streamGroup][]string
	lifecycle      map[string][]error
	firstLifecycle error
}

// lockValidation 对开放定义仍先取 assemblyMu；封口后只取根图锁。
// 观察到开放状态时先释放图锁，绝不以 graph -> assembly 的反序等锁。
func (g *Graph) lockValidation() bool {
	g.mu.Lock()
	if g.sealed {
		if g.validation == nil && g.validationDefinition != nil && g.validationDefinition.needsTree {
			// 已封子图已有完整纯声明，不必再取子图锁重编。候选只在
			// 根锁内可见；其余本轮检查失败时 unlockValidation 会撤下。
			g.validation = &validationPlan{definition: g.validationDefinition, pending: true}
		}
		return false
	}
	g.mu.Unlock()
	assemblyMu.Lock()
	g.mu.Lock()
	return true
}

func (g *Graph) unlockValidation(assembly bool) {
	if g.validation != nil && g.validation.pending {
		g.validation = nil
	}
	g.mu.Unlock()
	if assembly {
		assemblyMu.Unlock()
	}
}

// 调用方已经完成本轮全部检查，持 g.mu；开放定义还持 assemblyMu。
// 失败的 Validate/Start 不到这里，因此不会安装部分模型或封口任何一层。
func (g *Graph) freezeValidationLocked() {
	if g.validation == nil {
		p := &validationPlan{}
		if g.needsWaitTreeLocked() {
			p.definition = g.freezeValidationDefinitionLocked()
		}
		// 普通无流根只需通过标记；将来被挂载时再收一次局部声明，
		// 不因单独 Validate 给每个普通节点常驻复制一套键和元数据。
		g.validation = p
	}
	g.validation.pending = false
	g.sealTreeLocked(mountChain{g})
}

func (g *Graph) freezeValidationDefinitionLocked() *validationDefinition {
	if g.validationDefinition != nil {
		return g.validationDefinition
	}
	d := &validationDefinition{
		graphID: g.id, capacity: g.declaredCapacity(), limSet: g.limSet,
		ownAspects: len(g.aspects) != 0, aspectsCut: g.aspectsCut,
		needsTree: g.needsWaitTreeLocked(), nodes: make([]validationNode, len(g.nodes)),
	}
	for i, n := range g.nodes {
		dn := &d.nodes[i]
		// 显式收下校验所需字段，不把 Node 的执行闭包、描述或写权限表带进来。
		dn.def = Node{
			id: n.id, requires: append([]keyRef(nil), n.requires...),
			provides:   append([]keyRef(nil), n.provides...),
			streamKind: n.streamKind, compete: n.compete, sub: n.sub,
			binds: append([]SubBind(nil), n.binds...),
		}
		if len(n.aspects) != 0 {
			dn.def.aspects = []Aspect{nil}
		}
		for j := range dn.def.binds {
			b := &dn.def.binds[j]
			b.push = nil
			if b.fix != nil {
				in := Inputs{entries: make(map[string]inputEntry, len(b.fix.in.entries))}
				for name, en := range b.fix.in.entries {
					in.entries[name] = inputEntry{ref: en.ref, skip: en.skip}
				}
				b.fix = &fixBind{in: in}
			}
		}
		dn.sender = nodeStreamSender(&dn.def)
		if n.child != nil {
			n.child.mu.Lock()
			dn.child = n.child.freezeValidationDefinitionLocked()
			n.child.mu.Unlock()
		}
	}
	for _, ref := range g.outPortOrder {
		if isManagedStreamType(ref.typ) {
			d.outlets = append(d.outlets, ref)
		}
	}
	sort.Slice(d.outlets, func(i, j int) bool { return d.outlets[i].name < d.outlets[j].name })
	g.validationDefinition = d
	return d
}

func (p *validationPlan) check(capacity int, known streamKnownInputs, definition bool) error {
	if p.definition == nil || !p.definition.needsTree {
		return nil // 已成功冻结的普通 DAG；本轮输入与 checkpoint 仍由 Start 核验。
	}
	var facts *validationFacts
	if len(known.skipped) != 0 {
		// 根 skip 是任意输入组合：只在私有副本上求值，完成即丢弃。
		facts = p.evaluate(known)
	} else {
		mode := 0
		if known.hasAspects {
			mode |= 1
		}
		if known.checkpoint {
			mode |= 2
		}
		facts = p.modes[mode]
		if facts == nil {
			facts = p.evaluate(known)
			p.modes[mode] = facts
		}
	}
	if facts.tree == nil {
		return facts.firstLifecycle
	}
	out := make(map[string][]error)
	t := facts.tree
	t.problemsSlots(out, capacity)
	for _, group := range t.groups {
		gr := *group // 本轮 host/outlet 事实不写回已发布的模型。
		aliases := facts.outlets[group]
		gr.outlet = definition && len(aliases) != 0
		for _, name := range aliases {
			if known.hostReads[name] {
				gr.host = true
				gr.hostKeys = append(gr.hostKeys, name)
			}
		}
		t.problemsReader(out, &gr)
	}
	if len(out) == 0 {
		return facts.firstLifecycle
	}
	for at, problems := range facts.lifecycle {
		out[at] = append(out[at], problems...)
	}
	return t.firstValidationProblem(out)
}

func (p *validationPlan) evaluate(known streamKnownInputs) *validationFacts {
	t := &streamTree{}
	pool := &streamPool{capacity: p.definition.capacity}
	t.pools = append(t.pools, pool)
	root := t.collectValidation(p.definition, "", pool, nil, known.hasAspects)
	for _, ref := range p.definition.outlets {
		t.key(root, ref)
	}
	t.propagateSkips(known.skipped)
	t.connectAliases()
	t.collectReaders()
	f := &validationFacts{tree: t, lifecycle: make(map[string][]error)}
	t.problemsLifecycle(f.lifecycle, known.checkpoint)
	f.firstLifecycle = t.firstValidationProblem(f.lifecycle)
	if len(t.groups) == 0 {
		// 普通 Sub 的四种模式只保留生命周期结论，整棵展开工作树随即释放。
		f.tree = nil
		f.lifecycle = nil
	} else {
		f.outlets = make(map[*streamGroup][]string)
		for _, ref := range p.definition.outlets {
			gr := root.keys[ref.name].group
			f.outlets[gr] = append(f.outlets[gr], ref.name)
		}
	}
	return f
}

// collectValidation 每处挂载新建 site/node/key/pool，只有纯局部声明共享。
// 即使子定义相同、额度数值相同，也不能复用这些求值身份。
func (t *streamTree) collectValidation(d *validationDefinition, at string, pool *streamPool, mount *streamNode, hasAspects bool) *streamSite {
	s := &streamSite{at: at, graphID: d.graphID, pool: pool, mount: mount, keys: make(map[string]*streamKey),
		hasAspects: hasAspects, ownAspects: d.ownAspects, aspectsCut: d.aspectsCut}
	t.sites = append(t.sites, s)
	for i := range d.nodes {
		dn := &d.nodes[i]
		n := &streamNode{def: &dn.def, at: joinPath(at, dn.def.id), site: s, sender: dn.sender}
		s.nodes = append(s.nodes, n)
		t.nodes = append(t.nodes, n)
		for _, ref := range n.def.requires {
			t.key(s, ref)
		}
		for _, ref := range n.def.provides {
			t.key(s, ref).writer = n
		}
	}
	t.collectMountKeys(s, mount)
	for i, n := range s.nodes {
		if child := d.nodes[i].child; child != nil {
			childPool := pool
			if child.limSet {
				childPool = &streamPool{capacity: child.capacity}
				t.pools = append(t.pools, childPool)
			}
			childAspects := (hasAspects && !child.aspectsCut) || child.ownAspects
			n.child = t.collectValidation(child, n.at, childPool, n, childAspects)
		}
	}
	return s
}

// 与 problemsSubAtLocked 相同的声明顺序和逐挂载包装；已冻结的局部
// 来源/出口/接线问题为空，只有本次流事实可能带来新诊断。
func (t *streamTree) firstValidationProblem(problems map[string][]error) error {
	if len(problems) == 0 {
		return nil
	}
	for _, site := range t.sites {
		if local := problems[site.at]; len(local) != 0 {
			err := local[0]
			for s := site; s.mount != nil; s = s.mount.site {
				err = fmt.Errorf("pulse: subgraph %q (mounted at %q): %w", s.graphID, s.at, err)
			}
			return err
		}
	}
	return nil
}
