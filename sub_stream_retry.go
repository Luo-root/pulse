package pulse

// 有写暂存时只判断真正跨越重试边界的端点。确定缺失和无人使用的入口没有
// 可倒带的消息；普通无流执行不走这条检查，也不恢复 Sub 的整体输入门。
type subStreamBoundary struct {
	used    map[keyRef]bool
	skipped map[keyRef]bool
}

// 复用静态流模型，一次展开实际读者和缺失传播。出口可以经过多层 In/Out
// 改名，不能只拿出口名去匹配本层 In；也不能把整个别名组无条件判成 skip，
// 因为中间节点或挂载的切面可能写出另一个结果。
func (g *Graph) subStreamBoundary(rc *RunCtx, binds []SubBind) (*subStreamBoundary, error) {
	g.mu.Lock()
	tree := &streamTree{}
	hasAspects := len(g.aspects) != 0 || (!g.aspectsCut && len(rc.exec.aspects) != 0)
	root := tree.collectLocked(g, "", mountChain{g}, &streamPool{}, nil, hasAspects)
	g.mu.Unlock()
	// 无节点的直接透传、未使用的可选入口也可能没有出现在节点声明里。
	for _, b := range binds {
		if b.fix != nil {
			for _, ref := range b.fixKeys() {
				tree.key(root, ref)
			}
		} else {
			tree.key(root, b.child)
		}
	}
	tree.connectAliases()
	// Retry 边界也必须识别手写 NewNode 的原生 channel Requires。
	for _, group := range tree.groups {
		group.known = true
	}
	tree.collectReaders()
	boundary := &subStreamBoundary{used: make(map[keyRef]bool), skipped: make(map[keyRef]bool)}
	for _, key := range root.keys {
		if key.group != nil && len(key.group.readers) != 0 {
			boundary.used[key.ref] = true
		}
	}
	exported := make(map[*streamGroup]bool)
	for _, b := range binds {
		if !b.intoChild() {
			if group := root.keys[b.child.name].group; group != nil {
				exported[group] = true
			}
		}
	}
	rootSkipped := make(map[string]bool)
	for _, b := range binds {
		if b.fix != nil {
			for _, ref := range b.fixKeys() {
				rootSkipped[ref.name] = b.fix.in.entries[ref.name].skip
			}
			continue
		}
		key := root.keys[b.child.name]
		if !b.in || !isLiveStreamType(b.child.typ) || (!boundary.used[b.child] && !exported[key.group]) {
			continue // 未读取也未导出的入口不增加整个 Sub 的等待门。
		}
		_, err := rc.readTarget(b.parent).wait(rc.Context())
		if isSkipped(err) {
			rootSkipped[b.child.name] = true
		} else if err != nil {
			return nil, err
		}
	}
	tree.propagateSkips(rootSkipped)
	for _, key := range root.keys {
		if key.skipped {
			boundary.skipped[key.ref] = true
		}
	}
	return boundary, nil
}

func (b *subStreamBoundary) absent(bind SubBind, port keyRef) bool {
	return b.skipped[port] || (bind.intoChild() && !b.used[port])
}
