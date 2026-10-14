package pulse

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
			k := n.site.keys[ref.name]
			readers[k] = append(readers[k], n)
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
		// 先传递 body 不进入的事实，再判断桥接；祖先跳过意味着根本不创建
		// child execution，不为其内部节点凭空推导一次输出到达。
		for ; bodyAt < len(bodies); bodyAt++ {
			n := bodies[bodyAt]
			work.nodes++
			ancestorSkipped := n.site.mount != nil && n.site.mount.bodySkipped
			if remaining[n] == 0 && len(n.def.requires) != 0 && !ancestorSkipped && !n.site.hasAspects && len(n.def.aspects) == 0 {
				for _, ref := range n.def.provides {
					work.provides++
					markKey(n.site.keys[ref.name])
				}
			}
			if n.child != nil {
				for _, child := range n.child.nodes {
					work.mounts++
					markBody(child)
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
