package pulse

import (
	"fmt"
	"strings"
)

// 以下边的方向是「事件等待哪个前提」，仅用于静态检查，不参与运行调度。
// 普通用户节点的 Set 最早可在 start 后发生，不能猜它要等 finish；Sub 的
// 普通值 Out 等 child.Wait，实时 Out 只等子出口到达，挂载本身没有整体输入门。
// Produce / Tee 活着同步发送；Tee 所有出口的关闭都等所有下游能开始读取，
// 并且等输入关闭。Consume 正常完成要等输入关闭。缓冲不消除这些结构约束。
// 业务 callback / Aspect 内的隐藏等待、手写 channel 的关闭时机均不推断。
type streamWaitEvent struct {
	label string
	at    string
}

type streamWaitEdge struct {
	from, to int
	reason   string
}

type streamWaitGraph struct {
	dep    depGraph
	events []streamWaitEvent
	sends  []streamWaitEdge // 流完成环优先锚在已知发送等待；组合依赖环另取真实边
}

func (w *streamWaitGraph) event(label, at string) int {
	i := len(w.events)
	w.events = append(w.events, streamWaitEvent{label: label, at: at})
	w.dep.in = append(w.dep.in, nil)
	w.dep.out = append(w.dep.out, nil)
	return i
}

func (w *streamWaitGraph) wait(from, to int, reason string) {
	w.dep.out[from] = append(w.dep.out[from], depOut{to: to, keys: []string{reason}})
}

func (w *streamWaitGraph) sendWait(from, to int, reason string) {
	w.wait(from, to, reason)
	w.sends = append(w.sends, streamWaitEdge{from: from, to: to, reason: reason})
}

func (t *streamTree) problemsLifecycle(out map[string][]error) {
	// 含 Sub 的层由这份展开图核真实依赖；普通简单图仍走既有 DAG 判据。
	expanded := make(map[string]bool)
	for _, n := range t.nodes {
		if n.child != nil {
			expanded[n.site.at] = true
		}
	}
	w := &streamWaitGraph{}
	starts, finishes := make(map[*streamNode]int), make(map[*streamNode]int)
	arrivals, completed := make(map[*streamKey]int), make(map[*streamSite]int)
	closed, sent := make(map[*streamGroup]int), make(map[*streamNode]int)
	bridged := make(map[*streamNode]int)
	for _, n := range t.nodes {
		starts[n] = w.event(fmt.Sprintf("node %q start", n.at), n.site.at)
		finishes[n] = w.event(fmt.Sprintf("node %q finish", n.at), n.site.at)
		if n.child != nil {
			bridged[n] = w.event(fmt.Sprintf("Sub %q ordinary output bridge", n.at), n.site.at)
		}
		if streamSender(n) {
			sent[n] = w.event(fmt.Sprintf("node %q sends complete", n.at), n.site.at)
		}
	}
	for _, s := range t.sites {
		completed[s] = w.event(fmt.Sprintf("graph at %q completion", s.at), s.at)
	}
	for _, k := range t.keys {
		arrivals[k] = w.event(fmt.Sprintf("key %q arrival at %q", k.ref.name, k.site.at), k.site.at)
	}
	for _, gr := range t.groups {
		if gr.known {
			closed[gr] = w.event(fmt.Sprintf("stream %q close at %q", gr.key.ref.name, gr.key.site.at), gr.key.site.at)
		}
	}

	for _, s := range t.sites {
		if s.mount != nil {
			w.wait(completed[s], starts[s.mount], "child execution starts after its mount starts")
		}
		for _, n := range s.nodes {
			w.wait(completed[s], finishes[n], "graph completion waits for every node to finish")
		}
	}
	for _, n := range t.nodes {
		start, finish := starts[n], finishes[n]
		if n.bodySkipped {
			// body 不进入，不发生同步 send / 等 close / child.Wait。门仍等
			// 输入以 skip 到达；无切面时，未写输出随后由收尾标成 skipped。
			for _, ref := range n.def.requires {
				w.wait(finish, arrivals[n.site.keys[ref.name]], "skipped input gate waits for arrival")
			}
			for _, ref := range n.def.provides {
				k := n.site.keys[ref.name]
				if k.skipped {
					w.wait(arrivals[k], finish, "skipped node settles its unwritten output")
				}
			}
			continue
		}
		w.wait(finish, start, "finish waits for start")
		if n.site.mount != nil {
			w.wait(start, starts[n.site.mount], "child nodes start after the mount starts")
		}
		if !n.def.sub {
			for _, ref := range n.def.requires {
				w.wait(start, arrivals[n.site.keys[ref.name]], fmt.Sprintf("input gate requires %q", ref.name))
			}
		}
		if n.child != nil {
			w.wait(finish, bridged[n], "Sub waits for its output bridge before returning")
			w.wait(bridged[n], completed[n.child], "Sub output bridge waits for child completion")
			for _, b := range n.def.binds {
				if b.fix != nil {
					for _, ref := range b.fixKeys() {
						w.wait(arrivals[n.child.keys[ref.name]], start, "fixed input is installed when the mount starts")
					}
					continue
				}
				p, c := n.site.keys[b.parent.name], n.child.keys[b.child.name]
				if b.in {
					w.wait(arrivals[c], start, "In binds the child input when the mount starts")
					w.wait(arrivals[c], arrivals[p], fmt.Sprintf("In aliases parent %q to child %q", b.parent.name, b.child.name))
				} else {
					w.wait(bridged[n], arrivals[c], "Sub checks every bound output before returning")
					if isLiveStreamType(b.child.typ) {
						w.wait(arrivals[p], start, "live Out binds before the child starts")
					} else {
						w.wait(arrivals[p], bridged[n], "ordinary Out bridges only after child Wait completes")
					}
					w.wait(arrivals[p], arrivals[c], fmt.Sprintf("Out exposes child %q as parent %q", b.child.name, b.parent.name))
				}
			}
		} else if !n.def.sub {
			for _, ref := range n.def.provides {
				w.wait(arrivals[n.site.keys[ref.name]], start, "a node can publish its output after it starts")
			}
		}
		if n.def.streamKind == "consume" || n.def.streamKind == "tee" {
			for _, ref := range n.def.requires {
				if gr := n.site.keys[ref.name].group; gr != nil {
					w.wait(finish, closed[gr], "stream reader finishes after its input channel closes")
				}
			}
		}
		if !streamSender(n) {
			continue
		}
		// Tee 的每个出口是不同 channel，但同一次发送循环持有全部出口直到
		// 输入耗尽；B 出口不能在 A 尚未开始消费时先关闭。共用 sends 事件，
		// 避免为 N 个出口各复制 N 份等待边。
		w.wait(sent[n], start, "sending begins after the sender starts")
		for _, ref := range n.def.provides {
			key := n.site.keys[ref.name]
			gr := key.group
			if gr == nil || gr.source != key {
				continue
			}
			closeEvent := closed[gr]
			w.wait(finish, closeEvent, "stream sender closes its channels before finishing")
			w.wait(closeEvent, sent[n], "stream channel closes after all synchronous sends complete")
			readers := gr.readers
			if len(readers) > 1 && streamSameFanOut(readers) {
				// 同组的 gate 与挂载位置相同；发送只需要这组开始竞争。
				readers = readers[:1]
			}
			for _, r := range readers {
				w.sendWait(sent[n], starts[r], fmt.Sprintf("synchronous send on %q needs reader %q to start", ref.name, r.at))
			}
		}
		if n.def.streamKind == "tee" {
			for _, input := range n.def.requires {
				if ig := n.site.keys[input.name].group; ig != nil {
					w.wait(sent[n], closed[ig], "Tee closes its outputs after its input channel closes")
				}
			}
		}
	}
	comps := w.dep.cyclicSCCs()
	component := make(map[int]int)
	anchors := make([]*streamWaitEdge, len(comps))
	streamCycles := make([]bool, len(comps))
	for i, comp := range comps {
		for _, v := range comp {
			component[v] = i
		}
	}
	// 全部流等待边只扫一遍；每个见证的回程只走自己的分量。
	for i := range w.sends {
		e := &w.sends[i]
		ci, from := component[e.from]
		cj, to := component[e.to]
		if from && to && ci == cj && anchors[ci] == nil {
			anchors[ci], streamCycles[ci] = e, true
		}
	}
	for i, comp := range comps {
		if anchors[i] == nil {
			// 不依赖同步 send 的真实组合环也必须拒绝，不能仅删除 Sub 的原子依赖。
			// 简单层的普通环仍留给原 DAG 报，避免重复诊断和改变既有报文。
			for _, from := range comp {
				if !expanded[w.events[from].at] {
					continue
				}
				for _, e := range w.dep.out[from] {
					if ci, ok := component[e.to]; ok && ci == i {
						anchors[i] = &streamWaitEdge{from: from, to: e.to, reason: e.keys[0]}
						break
					}
				}
				if anchors[i] != nil {
					break
				}
			}
		}
		if anchors[i] != nil {
			kind := "dependency"
			if streamCycles[i] {
				kind = "stream lifecycle"
			}
			if at, err := w.witness(comp, anchors[i], kind); err != nil {
				out[at] = append(out[at], err)
			}
		}
	}
}

// 每个环分量一条具体见证，在分量内找锚点的真实回程。
func (w *streamWaitGraph) witness(comp []int, anchor *streamWaitEdge, kind string) (string, error) {
	members := make(map[int]bool, len(comp))
	for _, i := range comp {
		members[i] = true
	}
	prev := map[int]streamWaitEdge{anchor.to: {from: -1}}
	queue := []int{anchor.to}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		if v == anchor.from {
			break
		}
		for _, e := range w.dep.out[v] {
			if _, seen := prev[e.to]; !members[e.to] || seen {
				continue
			}
			prev[e.to] = streamWaitEdge{from: v, to: e.to, reason: e.keys[0]}
			queue = append(queue, e.to)
		}
	}
	if _, found := prev[anchor.from]; !found {
		return "", nil // 防御性：只报告真实闭环
	}
	var back []streamWaitEdge
	for v := anchor.from; v != anchor.to; {
		e := prev[v]
		back = append(back, e)
		v = e.from
	}
	path := []streamWaitEdge{*anchor}
	for i := len(back) - 1; i >= 0; i-- {
		path = append(path, back[i])
	}
	labels := []string{w.events[anchor.from].label}
	reasons := make([]string, 0, len(path))
	at := w.events[anchor.from].at
	for _, e := range path {
		labels = append(labels, w.events[e.to].label)
		reasons = append(reasons, e.reason)
		at = streamCommonPath(at, w.events[e.to].at)
	}
	return at, fmt.Errorf("pulse: %s cycle: %s (%s)", kind, strings.Join(labels, " -> "), strings.Join(reasons, "; "))
}
