package pulse

import (
	"fmt"
	"strings"
)

// 本文件是**静态校验**那一套的唯一实现：来源 → 依赖环 → 流式，一次给全。
//
// 两个入口共用它：`Start` / `Validate` 只取第一条（`validateLocked`），
// `Topology().Problems` 取全部。所以「装配期提前报的那句」与「启动时拒的那句」
// 逐字同源——「一份判据」这条纪律在这里就是「一个函数」。

// problemsLocked 返回这张图**全部**静态可判定的毛病，顺序固定为
// 来源 → 依赖环 → 流式。调用方持 g.mu。
func (g *Graph) problemsLocked() []error {
	var out []error
	out = append(out, g.problemsSourcesLocked()...)
	out = append(out, g.problemsCycleLocked()...)
	out = append(out, g.problemsStreamLocked()...)
	if len(out) == 0 {
		return nil
	}
	return out
}

// problemsSourcesLocked 报「每个 Requires 都有来源」。调用方持 g.mu。
//
// 判据是 producer 表里有没有这个 Key **且那条来源真的存在**：外部 Seed/SkipSeed
// 记为 seed，节点 Provides 记为 node + 节点 id；某种来源至多一个（see
// claimSource）。Requires 本身不登记来源，所以「表里没有」= 没人会写它。零值
// （`sourceNone`）按「没有来源」处理——它不该落表，万一落了也不能当有来源，
// 否则等它的节点会永远停在 pending。
//
// 一条缺来源的 `Requires` 一条毛病，按（节点声明序、Requires 出现顺序）给。
func (g *Graph) problemsSourcesLocked() []error {
	var out []error
	for _, n := range g.nodes {
		for _, k := range n.requires {
			if s, ok := g.producer[k.name]; !ok || !s.present() {
				out = append(out, fmt.Errorf("pulse: node %q requires %q but nothing provides or seeds it",
					n.id, k.name))
			}
		}
	}
	return out
}

// problemsCycleLocked 报「依赖关系有环」。调用方持 g.mu。
//
// 与 problemsSourcesLocked 互补：**有来源不等于能满足**。环里的槽谁也等不来谁，
// 门（等全部输入到达）就永远不开——这不是可执行的反馈环，而是不可完成的拓扑。
//
// 判据与见证合成一步：先按声明序求**强连通分量**（Tarjan，见 depGraph.cyclicSCCs），
// 只有**环分量**（节点数 ≥ 2）才算环；落在环下游的节点是单独的分量，不会被误报。
// 每个环分量给**一条**见证——「一个分量一条」，既不枚举同一个分量里的所有简单环
// （数量可能指数增长），也不对同一个分量报第二条。
//
// 顺序：Tarjan 的弹出顺序（分量之间反向的拓扑序），由声明序与出边顺序唯一确定——这与旧
// 实现的「按声明序起走第一条出边」**不是**同一套排序，所以**多分量**时逐条顺序不保证相同；
// 该保证只到「两节点环 / 三节点环那几张既有用例图上的首条逐字未变」（那几条用例没改、仍绿）。
func (g *Graph) problemsCycleLocked() []error {
	if len(g.nodes) < 2 {
		return nil // 自环在 Add 就被拒，单节点图不可能有环
	}
	d := g.depGraphLocked()
	var out []error
	for _, comp := range d.cyclicSCCs() {
		if err := g.sccWitness(d, comp); err != nil {
			out = append(out, err)
		}
	}
	return out
}

// sccWitness 在**一个环分量内部**走出一条真环，返回那句报错。
//
// 走法：从分量里声明序最靠前的节点出发，只走**留在分量内**的第一条出边，走回走过的
// 节点即为一个真环。强连通的定义保证分量内每个节点都有分量内的出边，所以一定闭合。
//
// 走不回去就**不报**——宁可不报，也不能拼一个不存在的环出去。旧实现顺着「第一条出边」
// 走出了环、走到一个没有出边的节点，还把路径首尾相接报成 `A -> C -> A ()`（括号里是
// 空的，因为一条边都没轮到），而真正的环 A↔B 被它盖掉了；这条判据现在由「只在分量内
// 走」保证，防御性出口留着只为不做出格的事。
func (g *Graph) sccWitness(d depGraph, comp []int) error {
	inComp := make([]bool, len(g.nodes))
	start := comp[0]
	for _, i := range comp {
		inComp[i] = true
		if i < start {
			start = i // 声明序最靠前的那一个
		}
	}

	var path []int
	var keys []string // keys[i] 是 path[i] → path[i+1] 那条边
	pos := make(map[int]int, len(comp))
	cur := start
	for {
		if p, seen := pos[cur]; seen {
			path, keys = path[p:], keys[p:]
			break
		}
		pos[cur] = len(path)
		path = append(path, cur)
		next, key := -1, ""
		for _, e := range d.out[cur] {
			if inComp[e.to] {
				next, key = e.to, e.keys[0]
				break
			}
		}
		if next < 0 {
			return nil // 防御性：环分量里不该出现（见上）
		}
		keys = append(keys, key)
		cur = next
	}

	ids := make([]string, 0, len(path)+1)
	for _, i := range path {
		ids = append(ids, g.nodes[i].id)
	}
	ids = append(ids, g.nodes[path[0]].id) // 合上环

	// 「谁在等谁」：path[i] 等的是走进它的那条边（上一条）上的 Key。走法保证了
	// len(keys) == len(path)（每前进一步就记下那条边），所以这里不必再兜底。
	waits := make([]string, 0, len(path))
	for i, idx := range path {
		prev := (i - 1 + len(path)) % len(path)
		waits = append(waits, fmt.Sprintf("%s requires %q", g.nodes[idx].id, keys[prev]))
	}
	return fmt.Errorf("pulse: dependency cycle: %s (%s)",
		strings.Join(ids, " -> "), strings.Join(waits, ", "))
}

// problemsStreamLocked 是流式糖（Produce / Consume / Tee）在 Start 的静态校验。
// 调用方持 g.mu。三条都是「静态可判定、运行期表现为挂死或静默丢数据」的病：
//
//  1. **名额是「同时活着」的约束，而糖在装配那一刻看不全整张图。** 一条流上
//     生产端 / 复制端堵在发送上、消费端堵在读上，链上每个节点都得同时在名额表
//     里；逐个 helper 的局部检查**证明不了组合图安全**——实测
//     `WithMaxRunning(2)` 下 `Produce → Tee([a]) → Consume(a)`（三个流节点）
//     装配全过，运行期直接死锁，只能等外部超时。这里按**整张流图**要名额：糖装
//     出来的流节点 + **每条流出口的读取者**——读取者可以是手写的 `NewNode`
//     （糖之外的那条路），它同样得拿到名额才进得去 Run、才读得到那条流：实测
//     `Produce → Tee([a]) → 手写 sink(a)` 在 `maxRun=2` 下也是装配全过、运行
//     期死锁，`maxRun=3` 才跑得通。偏保守（两条互不相关的流也会要求名额之和），
//     但引擎的名额是逐个申请的、没有 gang 调度，凑合的分配同样会死锁——宁可拒
//     得早。
//  2. **流节点的每条出口都得有人 Requires。** `problemsSourcesLocked` 只判「每个
//     Requires 有来源」，不判「每个 Provides 有消费者」；漏挂一个消费者（或
//     Tee 的某条出口没接下游）时，发送端会永久堵在那条无缓冲出口上——缓冲只把
//     死锁推到「缓冲写满」。
//  3. **一条流出口只喂一个下游**（或**一组** `FanOut` worker）。同一个出口被两个
//     各写各的节点 `Requires` 时，两边都会跑、各自只拿到一部分值：实测
//     `Tee([a])` 加两个 `Consume(a)` 装配全过，10 个值被 8/2 瓜分，而 Tee 的
//     承诺是「每个下游拿到完整、同序的数据」。瓜分是**静默**的，所以在 Start 拒。
//     「抢」是 `FanOut` 的用法（N 个 worker 共读一条流，本来就是显式声明），
//     同一组不算多消费者——否则会把文档点名推荐的玩法一起拒掉。
//
// 每条出口一条毛病（没人读 / 被几个各写各的读了各一条）；名额不足那条在这批
// 之先，与旧实现的先后一致。
func (g *Graph) problemsStreamLocked() []error {
	var stream []*Node
	outlets := make(map[string]struct{})
	for _, n := range g.nodes {
		if n.streamKind == "" {
			continue
		}
		stream = append(stream, n)
		for _, k := range n.provides {
			outlets[k.name] = struct{}{}
		}
	}
	if len(stream) == 0 {
		return nil
	}

	// 流图 = 流节点 + 读取流出口的节点（手写消费者也算：它同样要占一个名额才读得到）。
	readers := make(map[string][]*Node, len(outlets)) // 出口 Key 名 → 读取它的节点
	var live []*Node
	for _, n := range g.nodes {
		isStream, touched := n.streamKind != "", false
		seen := make(map[string]struct{}, len(n.requires))
		for _, k := range n.requires {
			if _, ok := outlets[k.name]; !ok {
				continue
			}
			if _, dup := seen[k.name]; dup {
				continue // 同一个 Key 声明两次仍只是一个读取者
			}
			seen[k.name] = struct{}{}
			readers[k.name] = append(readers[k.name], n)
			touched = true
		}
		if isStream || touched {
			live = append(live, n)
		}
	}

	var out []error
	if c := g.lim.capacity(); c > 0 && c < len(live) {
		ids := make([]string, 0, len(live))
		for _, n := range live {
			ids = append(ids, n.id)
		}
		out = append(out, fmt.Errorf("pulse: stream needs %d slots at the same time, but WithMaxRunning(%d): %s",
			len(live), c, strings.Join(ids, ", ")))
	}
	for _, n := range stream {
		for _, k := range n.provides {
			rs := readers[k.name]
			if len(rs) == 0 {
				out = append(out, fmt.Errorf("pulse: node %q provides %q but nothing consumes it: "+
					"a stream output nobody reads blocks the sender forever", n.id, k.name))
				continue
			}
			if len(rs) > 1 && !sameFanOut(rs) {
				ids := make([]string, 0, len(rs))
				for _, r := range rs {
					ids = append(ids, r.id)
				}
				out = append(out, fmt.Errorf("pulse: stream output %q is read by %d nodes (%s), "+
					"so its values would be split between them: one stream output feeds one downstream — "+
					"add a Tee output per downstream to broadcast, or make it one FanOut to compete",
					k.name, len(rs), strings.Join(ids, ", ")))
			}
		}
	}
	return out
}

// sameFanOut 判这一串读取者是不是**同一次 FanOut** 的 worker：几个 worker 共读
// 一条流本来就是 FanOut 的用法（「抢」），是显式声明；两个各写各的节点共读同一条
// 出口则是静默瓜分。调用方保证 rs 非空。
func sameFanOut(rs []*Node) bool {
	if rs[0].compete == "" {
		return false
	}
	for _, r := range rs[1:] {
		if r.compete != rs[0].compete {
			return false
		}
	}
	return true
}
