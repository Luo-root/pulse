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
//
// `capacity` 是**根这一层**的有效名额上限：`Validate` / `Topology` 只有定义，给
// `declaredCapacity()`；某次运行 `Start()` 给这次执行的有效名额（可能被 `WithExecMaxRunning`
// 改过、也可能继承父执行环境那一份）。往下每一层按同一条优先级自己解析（见 effectiveCapacity）。
func (g *Graph) problemsLocked(capacity int) []error {
	return g.problemsAtLocked("", mountChain{g}, capacity)
}

// problemsAtLocked 是递归体：`at` 是这张图**在本视图里**的位置（根为空串），`chain` 是从根
// 一路挂到这张图的图链（判环用），`capacity` 是**这一层的有效名额上限**。调用方持 g.mu。
//
// 五类判据，顺序固定：来源 → 出口 → 依赖环 → 流式 → **静态子树**（每处挂载各报一条）。
func (g *Graph) problemsAtLocked(at string, chain mountChain, capacity int) []error {
	// 流的别名、额度池与完成等待关系要看整棵挂载树，不能在每层重新从零判断。
	// 只复制定义；子层各自取锁，不读取 Execution，也不创建槽位。
	streams := g.problemsStreamLocked(at, chain, capacity)
	return g.problemsWithStreamsAtLocked(at, chain, capacity, streams)
}

// validateRunLocked 与定义检查共用判据，只额外提供这次已确认的外部 skip。
// 子执行的 Start 也走这里，不能在递归运行时重新丢失已供给的输入状态。
func (g *Graph) validateRunLocked(capacity int, known streamKnownInputs) error {
	if g.validation != nil {
		return g.validation.check(capacity, known, false)
	}
	if g.canReusePlainValidationLocked() {
		return nil
	}
	chain := mountChain{g}
	streams := g.problemsStreamLocked("", chain, capacity, known)
	if problems := g.problemsWithStreamsAtLocked("", chain, capacity, streams); len(problems) != 0 {
		return problems[0]
	}
	return nil
}

// canReusePlainValidationLocked 只复用已成功封口、没有 Sub 或流的普通图。
// 这类定义的来源、出口和依赖环此后都不再变化；每轮输入仍由 Start 独立核验。
// 流与 Sub 的判据还依赖本轮额度、skip、宿主订阅及 checkpoint，不能只看 sealed。
// 调用方持 g.mu；needsWaitTreeLocked 仍只读扫描声明，不引入执行状态或缓存失效协议。
func (g *Graph) canReusePlainValidationLocked() bool {
	return g.sealed && !g.needsWaitTreeLocked()
}

// problemsWithStreamsAt 是递归子图的加锁入口：父图持有自己的锁，子图自己取锁。
// streams 复用根检查已算好的整树流事实，不在每一层重新计算；capacity 已按该层优先级解析。
// 锁序沿挂载边由父到子；装配期拒绝组合环，递归时 chain 再兜底判环。
func (g *Graph) problemsWithStreamsAt(at string, chain mountChain, capacity int, streams map[string][]error) []error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.problemsWithStreamsAtLocked(at, chain, capacity, streams)
}

func (g *Graph) problemsWithStreamsAtLocked(at string, chain mountChain, capacity int, streams map[string][]error) []error {
	out := g.problemsLocalWithStreamsAtLocked(at, streams)
	out = append(out, g.problemsSubAtLocked(at, chain, capacity, streams)...)
	return out
}

// problemsLocalWithStreamsAtLocked 只判当前层，子视图由调用方聚合。调用方持 g.mu。
func (g *Graph) problemsLocalWithStreamsAtLocked(at string, streams map[string][]error) []error {
	var out []error
	out = append(out, g.problemsSourcesLocked()...)
	out = append(out, g.problemsOutputsLocked()...)
	// 结构组合已在事件图中展开。把 Sub 的全部父 Requires 压成一个原子
	// 节点，会误拒实时流反馈，或把可选且无人读取的 In 变成整个 child 的门。
	// 没有挂载的简单图保留原 DAG 判据、顺序与报文。
	expanded := false
	if streams != nil {
		for _, n := range g.nodes {
			if n.child != nil {
				expanded = true
				break
			}
		}
	}
	if !expanded {
		out = append(out, g.problemsCycleLocked()...)
	}
	out = append(out, streams[at]...)
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
// 往下一层是 `n.child.problemsWithStreamsAt`（**它自己加锁**），不是直接读字段：父图那把锁管不到子图，
// 而子图在装配期一直躺在宿主手里——那头一边 `Add`、这头一边读，就是一处竞态（用例
// TestSubChildAssemblyVsParentReadOnly）。
//
// **名额跟着递归往下走**：`capacity` 是这一层的有效上限，子图那一层按同一条优先级自己解析
// （`effectiveCapacity`）。不传递的话子图会被按「自己没声明 = 不限」判，而它真跑起来继承的
// 正是父图这一份——于是「父图限额 2、子图有一条要 3 个名额的流」这种图整树校验会放行，
// 等子图自己的 `Start` 才拒，那时父图别的节点可能已经跑完并留下副作用了。子图的
// `limSet` / `maxRunning` 只在 `New` 的选项里写一次、此后不再变，所以不持它的锁读也安全。
func (g *Graph) problemsSubAtLocked(at string, chain mountChain, capacity int, streams map[string][]error) []error {
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
		for _, err := range n.child.problemsWithStreamsAt(here, sub, n.child.effectiveCapacity(capacity), streams) {
			out = append(out, fmt.Errorf("pulse: subgraph %q (mounted at %q): %w",
				n.child.id, here, err))
		}
	}
	return out
}

// problemsSourcesLocked 报「每个 Requires 都有来源」。调用方持 g.mu。
//
// 判据是 producer 表里有没有这个 Key **且那条来源真的存在**：节点 Provides 记为
// node + 节点 id，一条 Key 至多一个来源（见 sourceConflict）。**声明过的输入端口也算有来源**（`DeclareInput`）——它表达的是
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
			out = append(out, fmt.Errorf("pulse: node %q requires %q but nothing provides it "+
				"(declare it with DeclareInput if the value comes from outside)", n.id, k.name))
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
// （`sourceNone`：既没人 Provides、也没声明成入口）才算。
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
			"nothing in this graph provides it, and it is not a declared input of the same name "+
			"(a pass-through), so a parent binding it with Out could only ever report "+
			"\"never arrived\" (provide it with a node, or drop the DeclareOutput)", name))
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
