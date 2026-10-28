package pulse

import (
	"fmt"
	"sort"
)

// validationPlan 只在最终检查全部通过后安装。来源、端口、接线及普通 DAG
// 已在同一装配边界内通过；保留的树只含声明，不含执行、输入值或用户闭包。
// modes 的下标只有根有效切面 / checkpoint 两个布尔位，不按输入组合缓存。
// plan 与 modes 的发布均由所属 Graph.mu 保护；求值不再锁任何子定义。
type validationPlan struct {
	tree    *streamTree
	outlets []keyRef
	modes   [4]*validationFacts
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
		return false
	}
	g.mu.Unlock()
	assemblyMu.Lock()
	g.mu.Lock()
	return true
}

func (g *Graph) unlockValidation(assembly bool) {
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
			p.tree = &streamTree{}
			pool := &streamPool{capacity: g.declaredCapacity()}
			p.tree.pools = append(p.tree.pools, pool)
			root := p.tree.collectLocked(g, "", mountChain{g}, pool, nil, len(g.aspects) != 0)
			for _, ref := range g.outPortOrder {
				if isManagedStreamType(ref.typ) {
					p.outlets = append(p.outlets, ref)
					p.tree.key(root, ref)
				}
			}
			sort.Slice(p.outlets, func(i, j int) bool { return p.outlets[i].name < p.outlets[j].name })
			// 校验只需切面的存在性、Fix 的类型与 skip。不能保留业务值、
			// run/push 闭包或另一个 Graph 的可变字段作为求值状态。
			for _, n := range p.tree.nodes {
				n.def.run, n.def.child = nil, nil
				if len(n.def.aspects) != 0 {
					n.def.aspects = []Aspect{nil}
				}
				for i := range n.def.binds {
					b := &n.def.binds[i]
					b.push = nil
					if b.fix != nil {
						in := Inputs{entries: make(map[string]inputEntry, len(b.fix.in.entries))}
						for name, en := range b.fix.in.entries {
							in.entries[name] = inputEntry{ref: en.ref, skip: en.skip}
						}
						b.fix = &fixBind{in: in}
					}
				}
			}
		}
		g.validation = p
	}
	g.sealTreeLocked(mountChain{g})
}

func (p *validationPlan) check(capacity int, known streamKnownInputs, definition bool) error {
	if p.tree == nil {
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
	return p.firstProblem(out)
}

func (p *validationPlan) evaluate(known streamKnownInputs) *validationFacts {
	t := p.tree.validationCopy(known.hasAspects)
	t.propagateSkips(known.skipped)
	t.connectAliases()
	t.collectReaders()
	f := &validationFacts{tree: t, lifecycle: make(map[string][]error)}
	t.problemsLifecycle(f.lifecycle, known.checkpoint)
	f.firstLifecycle = p.firstProblem(f.lifecycle)
	if len(t.groups) == 0 {
		// 普通 Sub 只保留生命周期结论，不把求值副本留成第二棵声明树。
		f.tree = nil
	} else {
		f.outlets = make(map[*streamGroup][]string)
		for _, ref := range p.outlets {
			gr := t.sites[0].keys[ref.name].group
			f.outlets[gr] = append(f.outlets[gr], ref.name)
		}
	}
	return f
}

// 与 problemsSubAtLocked 相同的声明顺序和逐挂载包装；已冻结的局部
// 来源/出口/接线问题为空，只有本次流事实可能带来新诊断。
func (p *validationPlan) firstProblem(problems map[string][]error) error {
	if len(problems) == 0 {
		return nil
	}
	for _, site := range p.tree.sites {
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

// validationCopy 复制求值状态，声明切片共享且只读。模板保持 FixSkip 原始
// 事实、未合并的别名及空 reader/pool 状态；动态求值永远不能改模板。
func (t *streamTree) validationCopy(hasAspects bool) *streamTree {
	out := &streamTree{}
	pools := make(map[*streamPool]*streamPool, len(t.pools))
	for _, p := range t.pools {
		copy := &streamPool{capacity: p.capacity}
		pools[p] = copy
		out.pools = append(out.pools, copy)
	}
	sites := make(map[*streamSite]*streamSite, len(t.sites))
	for _, s := range t.sites {
		copy := *s
		copy.pool, copy.nodes, copy.keys = pools[s.pool], nil, make(map[string]*streamKey, len(s.keys))
		sites[s] = &copy
		out.sites = append(out.sites, &copy)
	}
	nodes := make(map[*streamNode]*streamNode, len(t.nodes))
	for _, n := range t.nodes {
		copy := *n
		copy.site, copy.child = sites[n.site], sites[n.child]
		nodes[n] = &copy
		out.nodes = append(out.nodes, &copy)
		copy.site.nodes = append(copy.site.nodes, &copy)
	}
	for _, s := range t.sites {
		copy := sites[s]
		copy.mount = nodes[s.mount]
		copy.hasAspects = hasAspects
		if copy.mount != nil {
			copy.hasAspects = (copy.mount.site.hasAspects && !copy.aspectsCut) || copy.ownAspects
		}
	}
	for _, k := range t.keys {
		copy := *k
		copy.site, copy.writer, copy.group = sites[k.site], nodes[k.writer], nil
		copy.parent = &copy
		copy.site.keys[copy.ref.name] = &copy
		out.keys = append(out.keys, &copy)
	}
	return out
}
