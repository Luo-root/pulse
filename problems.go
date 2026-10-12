package pulse

import (
	"fmt"
	"sort"
	"strings"
)

// 本文件是**静态校验**那一套的唯一实现：来源 → 出口 → 依赖环 → 流式，一次给全。
//
// 两个入口共用它：`Start` / `Validate` 只取第一条（`validateLocked`），
// `Topology().Problems` 取全部。所以「装配期提前报的那句」与「启动时拒的那句」
// 逐字同源——「一份判据」这条纪律在这里就是「一个函数」。

// problemsLocked 返回这张图**全部**静态可判定的毛病，顺序固定为
// 来源 → 出口 → 依赖环 → 流式。调用方持 g.mu。
func (g *Graph) problemsLocked() []error { return g.problemsAtLocked("", mountChain{g}) }

// problemsAt 是递归的**入口**：自己加锁，再交给递归体。
//
// 递归读子图必须走这里：父图持着自己的锁，子图那把锁由子图自己取（`topologyAt` 同理）。
// 沿挂载边一层层往下取，锁序恒为「父 → 子」；挂载边不可能成环（`Sub` 装配期就拒，且装配
// 那一步是全局串行的），所以不会两层互等对方的锁。万一真成了环，`chain` 会在那一层当场
// 报出来而不是一路递归下去（见 problemsSubAtLocked）。
func (g *Graph) problemsAt(at string, chain mountChain) []error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.problemsAtLocked(at, chain)
}

// problemsAtLocked 是递归体：`at` 是这张图**在本视图里**的位置（根为空串），`chain` 是从根
// 一路挂到这张图的图链（判环用）。调用方持 g.mu。
//
// 五类判据，顺序固定：来源 → 出口 → 依赖环 → 流式 → **静态子树**（每处挂载各报一条）。
func (g *Graph) problemsAtLocked(at string, chain mountChain) []error {
	var out []error
	out = append(out, g.problemsSourcesLocked()...)
	out = append(out, g.problemsOutputsLocked()...)
	out = append(out, g.problemsCycleLocked()...)
	out = append(out, g.problemsStreamLocked()...)
	out = append(out, g.problemsSubAtLocked(at, chain)...)
	if len(out) == 0 {
		return nil
	}
	return out
}

// problemsSubAtLocked 按**引用点**报静态子树里的毛病，一处挂载一条。
//
// 为什么要按引用点而不是按定义：同一张子图可以挂在好几处，你需要的不是「这张子图有病」，
// 而是**从哪一处的视角看它有问题**——报文里带上挂载位置（`at`），照着就能找到该改的那一处。
// 这与 #298 给 yaml 立的口径是同一条（那里报的是「哪一处引用」，这里报的是「哪一处挂载」）。
//
// 顺带把接线**重新核一遍**：子图的端口可能在挂载之后、它自己封口之前改过（挂载那一刻核过
// 一次，但那是当时的事实）。判据与 `Sub` 共用同一份 checkBinds，报文逐字相同。
//
// 往下一层是 `n.child.problemsAt`（**它自己加锁**），不是直接读字段：父图那把锁管不到子图，
// 而子图在装配期一直躺在宿主手里——那头一边 `Add`、这头一边读，就是一处竞态（用例
// TestSubChildAssemblyVsParentReadOnly）。
func (g *Graph) problemsSubAtLocked(at string, chain mountChain) []error {
	var out []error
	for _, n := range g.nodes {
		if n.child == nil {
			continue
		}
		here := joinPath(at, n.id)
		// 兜底判环：环本不该存在（`Sub` 装配期拒 + 装配全局串行），但一路展开碰上环的代价
		// 不是一句错话——是爆栈（单线程）或两层互等对方的锁（并发）。所以每往下一层都自己
		// 数一遍「这张子图是不是已经在这条链上」。
		if chain.has(n.child) {
			out = append(out, fmt.Errorf("pulse: subgraph cycle: %s -> %s "+
				"(mounting %q at %q would put graph %q inside its own subtree)",
				chain.String(), n.child.id, n.child.id, here, n.child.id))
			continue
		}
		if err := checkBinds(n.child, n.id, n.binds); err != nil {
			out = append(out, fmt.Errorf("pulse: subgraph mounted at %q: %w", here, err))
			continue
		}
		sub := chain.push(n.child)
		for _, err := range n.child.problemsAt(here, sub) {
			out = append(out, fmt.Errorf("pulse: subgraph %q (mounted at %q): %w",
				n.child.id, here, err))
		}
	}
	return out
}

// problemsSourcesLocked 报「每个 Requires 都有来源」。调用方持 g.mu。
//
// 判据是 producer 表里有没有这个 Key **且那条来源真的存在**：外部 Seed/SkipSeed
// 记为 seed，节点 Provides 记为 node + 节点 id；某种来源至多一个（see
// claimSource）。**声明过的输入端口也算有来源**（`DeclareInput`）——它表达的是
// 「这本来就是外部输入」，与「你忘了接线」是两件事，前者不该被报成毛病。
// Requires 本身不登记来源，所以「表里没有**且**没声明成入口」= 没人会写它。零值
// （`sourceNone`）按「没有来源」处理——它不该落表，万一落了也不能当有来源，
// 否则等它的节点会永远停在 pending。
//
// 一条缺来源的 `Requires` 一条毛病，按（节点声明序、Requires 出现顺序）给。
func (g *Graph) problemsSourcesLocked() []error {
	var out []error
	for _, n := range g.nodes {
		for _, k := range n.requires {
			if s, ok := g.producer[k.name]; ok && s.present() {
				continue
			}
			if _, ok := g.inPorts[k.name]; ok {
				continue
			}
			out = append(out, fmt.Errorf("pulse: node %q requires %q but nothing provides or seeds it",
				n.id, k.name))
		}
	}
	return out
}

// problemsOutputsLocked 报「声明过的出口都有人产出」。调用方持 g.mu。
//
// 与 problemsSourcesLocked 对称，判的是另一头：那条说「没人喂我」，这条说「我承诺了却没人
// 产出」。出口是**对外的承诺**（父图能用 `Out` 接出去），没人产出时那句 `never arrived`
// 要等子图跑完才出现——而它读一遍声明就能定，和入口那条一样属于装配期该报的毛病。
//
// 判据复用 sourceOfLocked（谁会写这条槽优先，没人写就看它是不是声明过的入口）：**入口与
// 出口同名是透传**——父侧种进来的那份就是这条槽的值，所以透传不算没人产出。只有零值来源
// （`sourceNone`：既没人 Provides、也没人 Seed、也没声明成入口）才算。
//
// 顺序按**名字**排：出口表是 map，迭代顺序随机，而这里是报文顺序（`Topology().Problems`
// 要用它、`Validate` 的首条也取它），同一张图两次问必须一模一样。
func (g *Graph) problemsOutputsLocked() []error {
	if len(g.outPorts) == 0 {
		return nil
	}
	var missing []string
	for name, ref := range g.outPorts {
		if g.sourceOfLocked(ref).kind == sourceNone {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	out := make([]error, 0, len(missing))
	for _, name := range missing {
		out = append(out, fmt.Errorf("pulse: declared output %q is never produced: "+
			"nothing in this graph provides or seeds it, so a parent binding it with Out could "+
			"only ever report \"never arrived\" (provide it with a node, or drop the DeclareOutput)",
			name))
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
// 整趟是 **O(V+E)**：Tarjan 单趟，每条见证只走自己分量里的边，成员判据按**该分量**的
// 规模开（不是每个分量各开一份全图数组——那样 k 个互不相连的小环会退化成 O(k·V)）。
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
	// 成员判据按**本分量**的规模开（`len(comp)`）。早先这里是一份 `len(g.nodes)` 的
	// `[]bool`：k 个互不相连的小环各自清零一遍全图，整趟就从 O(V+E) 掉到 O(k·V)
	// （实测 V=4000、k=2000 时多分配 7.4 MB——评审在 #308 上点的这条）。
	inComp := make(map[int]struct{}, len(comp))
	start := comp[0]
	for _, i := range comp {
		inComp[i] = struct{}{}
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
			if _, ok := inComp[e.to]; ok {
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
