package pulse

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// subMountMu 是 `Sub` 那把**全局**装配锁，包住「查一遍子树 + 真的挂上」这整段。
//
// 为什么必须是全局的：组合环是两张图各自动作的**合成**结果——A 里挂 B 与 B 里挂 A 各看各的
// 都对，只有放在一起才成环。单看任何一张图的锁都拦不住（两个 goroutine 各自先查过「没有环」
// 再各挂一步，环就进去了；此后递归展开是爆栈 / 互等锁）。装配是一次性动作、临界区里没有用户
// 代码，代价是零；运行期不碰它。
var subMountMu sync.Mutex

// Sub 把一张**现成的**子图挂成 `g` 上的一个节点（「图即节点」）——这是子图组合的**唯一**
// 公共入口。
//
// 它挂的是装配期就已经建好的定义，所以**父图在运行之前**就能递归展开整棵子树、把静态
// 毛病一次报全（`Topology()` / `Validate()`）。
//
// 接线只能接到子图**声明的端口**上：`In(父, 子)` 的 child 端必须是 `DeclareInput` 声明过的
// 入口，`Out(子, 父)` 的 child 端必须是 `DeclareOutput` 声明过的出口。接到子图内部的中间
// Key 是**静态错**——想让父图用那条 Key 就把它声明成出口，意图明确；不声明等于「这是我的
// 实现细节，不承诺」。父侧那两端由 binds 推出来（`In` 的父侧端进 Requires、`Out` 的父侧端
// 进 Provides），所以父图自己的静态校验（来源 / 出口 / 环 / 名额）照常生效。
//
// **组合环在装配期就拒**：`Sub(A, …, B)` 之后再 `Sub(B, …, A)`（或直接自挂）会让静态递归
// 永远展开不完，所以这里沿子图子树查一遍「父图是不是已经在它的子树里」，是就报
// `pulse: subgraph cycle: A -> B -> A`。判据只看**当前这条路径**（见 hasAncestor）——同一张
// 子图在**不同挂载位置**重复出现是合法的，不能用全局 visited 误拒。
//
// 「查一遍」与「真的挂上」在**同一个全局临界区**里（subMountMu）：对向挂载（A 里挂 B、同时
// B 里挂 A）是两张图各自动作的结果，单看任何一张图的锁都拦不住——两边都可能先看过「没有环」
// 再各挂一步，环就进去了，此后递归展开是爆栈或互等锁。装配是一次性动作、临界区里没有用户
// 代码，这把全局锁的代价是零。递归本身还有一道兜底判环（见 problemsSubAtLocked）。
//
// 运行语义与手写嵌套逐条一致（糖不改语义）：别名额（这一步不吃名额，见 WithMaxRunning）、
// 子图失败即本节点失败且首错原样冒泡、输出按槽位桥回（就绪 → `Set`、跳过 → `Skip`，全部跳过
// = 本节点 `NoValue()`）。两处要记住：
//
//  1. **一次性边界**：挂进父图的这张子图**只能跑一处**——同一张子图挂两处时，跑的那一步
//     会报一句能照着改的话（每个挂载点各跑一份要等 `Execution` 落地，那是下一步的事）。
//  2. **取消域**：子图那份 ctx 是它**建图时**带进来的（静态组合没有「建图时的父节点」可
//     派生），所以这一步把父节点这一层的取消**转发**进去。不转发的话子图会跑满整段、
//     把父图的取消吞掉——实测过：父图耗时 300ms 且报成功（见 runStaticSub 的第 4 步）。
//
// `aspects` 作用于**父侧这个节点**（例如 `pulse.Timeout(30*time.Second)` = 整张子图限时）。
func Sub(g *Graph, id, description string, binds []SubBind, child *Graph, aspects ...Aspect) error {
	if g == nil {
		return fmt.Errorf("pulse: Sub %q: nil graph", id)
	}
	if child == nil {
		return fmt.Errorf("pulse: Sub %q: nil child graph: pass the graph built by New/Add", id)
	}
	if id == "" {
		return fmt.Errorf("pulse: Sub: empty node id")
	}
	if strings.ContainsRune(id, '/') {
		return fmt.Errorf("pulse: Sub %q: node id must not contain '/': "+
			"that is the separator between layers in a nested path, so this step would come out "+
			"indistinguishable from a subgraph nested one level deeper", id)
	}
	if len(binds) == 0 {
		return fmt.Errorf("pulse: Sub %q: no binds: a step that exchanges nothing "+
			"with its parent cannot be ordered by it either", id)
	}
	// 组合环那条判据是**全局**不变式：「查一遍子树」与「挂上」必须一起做完（见上面那段）。
	subMountMu.Lock()
	defer subMountMu.Unlock()
	// 组合环：装配期能判的，别留到递归展开时才炸（那时是栈溢出）。
	if err := g.checkMountCycle(child); err != nil {
		return err
	}
	// 接线只能接端口：子侧两端都要是声明过的对应方向端口。
	requires, provides, err := subBinds(child, id, binds)
	if err != nil {
		return err
	}

	n := NewNode(id, description, requires, provides, func(rc *RunCtx) error {
		return runStaticSub(rc, id, binds, child)
	}, aspects...)
	n.sub = true // 这一步不吃运行名额：它整段都在等子图跑完（见 WithMaxRunning）
	n.child = child
	n.binds = binds
	if err := g.Add(n); err != nil {
		return fmt.Errorf("pulse: Sub %q: %w", id, err)
	}
	return nil
}

// subBinds 是接线检查 + 父侧推导：把 binds 翻成父侧的 Requires / Provides，并核子侧两端是
// 「声明过的、方向对的端口」。端口是**唯一**的接线面（见 object-model 第 3.1 节）。
func subBinds(child *Graph, id string, binds []SubBind) (requires, provides []keyRef, err error) {
	if err := checkBinds(child, id, binds); err != nil {
		return nil, nil, err
	}
	requires, provides = parentBinds(binds)
	return requires, provides, nil
}

// parentBinds 从 binds 推出父侧那两端：`In(父, 子)` 的父侧端进 Requires、
// `Out(子, 父)` 的父侧端进 Provides——边界写在接线处，读父图一眼看清这一步吃什么吐什么。
func parentBinds(binds []SubBind) (requires, provides []keyRef) {
	for _, b := range binds {
		if b.in {
			requires = append(requires, b.parent)
			continue
		}
		provides = append(provides, b.parent)
	}
	return requires, provides
}

// checkBinds 检查一批绑定：零值绑定、子侧键被绑两次、子侧两端是不是声明过的对应方向端口、
// 类型对不对。装配期一次判清。
func checkBinds(child *Graph, id string, binds []SubBind) error {
	seenChild := make(map[string]struct{}, len(binds))
	for i, b := range binds {
		if b.parent.typ == nil || b.child.typ == nil {
			return fmt.Errorf("pulse: Sub %q: bind %d is zero: build binds with In/Out", id, i)
		}
		if _, dup := seenChild[b.child.name]; dup {
			return fmt.Errorf("pulse: Sub %q: child key %q is bound twice: "+
				"the second seed would be silently ignored (idempotent first write)", id, b.child.name)
		}
		seenChild[b.child.name] = struct{}{}
		if err := child.checkPort(id, b); err != nil {
			return err
		}
	}
	return nil
}

// checkPort 判一条绑定的子侧端是不是**声明过的、方向对的**端口。
//
// 报文要能照着改，所以三种情形分开说：没声明成端口（点明「它不是这张图的端口」）、方向反了
// （声明过、但你绑的是另一头）、类型不符（端口声明的类型与绑定不一致）。
func (g *Graph) checkPort(stepID string, b SubBind) error {
	g.mu.Lock()
	inputs := g.inPorts
	outputs := g.outPorts
	g.mu.Unlock()

	want, other := outputs, inputs // 方向说明见下：`in` 绑的是子图的**入口**
	direction, otherDirection := "output", "input"
	if b.in {
		want, other = inputs, outputs
		direction, otherDirection = "input", "output"
	}
	if p, ok := want[b.child.name]; ok {
		if p.typ != b.child.typ {
			return fmt.Errorf("pulse: Sub %q: port %q type mismatch: the child declares it as %s, "+
				"the bind declares %s", stepID, b.child.name, typeName(p.typ), typeName(b.child.typ))
		}
		return nil
	}
	if _, ok := other[b.child.name]; ok {
		return fmt.Errorf("pulse: Sub %q: %q is a declared %s of the child graph, but this bind "+
			"treats it as the %s side: %s points from %s to %s",
			stepID, b.child.name, otherDirection, direction, bindName(b.in), bindFrom(b.in), bindTo(b.in))
	}
	return fmt.Errorf("pulse: Sub %q: %q is not a %s port of the child graph: declare it with "+
		"Declare%s (a key that only lives inside the child is its implementation detail), "+
		"or bind a different key", stepID, b.child.name, direction, portDeclMethod(b.in))
}

// bindName / bindFrom / bindTo / portDeclMethod 只负责把一条绑定说成人话（报错用）。
func bindName(in bool) string {
	if in {
		return "In(父, 子)"
	}
	return "Out(子, 父)"
}

func bindFrom(in bool) string {
	if in {
		return "父"
	}
	return "子"
}

func bindTo(in bool) string {
	if in {
		return "子"
	}
	return "父"
}

func portDeclMethod(in bool) string {
	if in {
		return "Input"
	}
	return "Output"
}

// checkMountCycle 是组合环的判据：把 child 挂进 g 之前，先看 g 是不是已经在 child 的子树里
// （自挂也算）。调用方只是在装配（且持着 subMountMu），所以这里逐个图加锁读一次，不跨图持锁。
func (g *Graph) checkMountCycle(child *Graph) error {
	if g == child {
		return fmt.Errorf("pulse: subgraph cycle: %s -> %s (a graph cannot be mounted inside itself)",
			g.id, g.id)
	}
	if path, ok := child.findInSubtree(g, []string{child.id}); ok {
		return fmt.Errorf("pulse: subgraph cycle: %s -> %s (mounting %q here would put %q inside its own subtree)",
			strings.Join(append([]string{g.id}, path...), " -> "), g.id, child.id, g.id)
	}
	return nil
}

// findInSubtree 沿**静态子图边**找 target：找到就返回从本图到 target 的 id 路径。
//
// 判据只看**当前这条路径**——递归进去的是子树，兄弟分支共用同一张子图不会互相影响；
// 用全局 visited 会把「同一张子图挂多处」这种合法用法误判成环（那是本模型要支持的用法）。
func (g *Graph) findInSubtree(target *Graph, path []string) ([]string, bool) {
	g.mu.Lock()
	kids := make([]*Graph, 0, len(g.nodes))
	for _, n := range g.nodes {
		if n.child != nil {
			kids = append(kids, n.child)
		}
	}
	g.mu.Unlock()

	for _, k := range kids {
		if k == target {
			return append(append([]string(nil), path...), k.id), true
		}
		if got, ok := k.findInSubtree(target, append(append([]string(nil), path...), k.id)); ok {
			return got, true
		}
	}
	return nil, false
}

// runStaticSub 是挂子图那一步的运行体：取值 → 认领 → 种值 → 跑 → 桥回。
func runStaticSub(rc *RunCtx, id string, binds []SubBind, child *Graph) error {
	// 1) 先取父侧的全部输入：跳过是**到达**，照跳种；其它错误（取消、未声明）直接返回。
	type inVal struct {
		v    any
		skip bool
	}
	vals := make([]inVal, len(binds))
	for i, b := range binds {
		if !b.in {
			continue
		}
		v, skip, err := readParentInput(rc, b)
		if err != nil {
			return err
		}
		vals[i] = inVal{v: v, skip: skip}
	}

	// 2) 原子认领这张子图。静态组合里最容易犯的错是「同一张子图挂两处」，那句话要说清
	//    静态形态只能跑一处、每个挂载点各跑一份要等 Execution。
	if !child.claimSub() {
		return fmt.Errorf("pulse: Sub %q: the child graph is already in use: a graph runs once, "+
			"so a statically mounted subgraph can run at one mount point only (running one copy per "+
			"mount point needs the execution tree, which is not in place yet) (%w)", id, ErrGraphStarted)
	}

	// 3) 挂接状态**一次写完**：观察者（子图没挂才继承父图的）、名额（子图自己声明过就用
	//    它自己那份，没声明则继承父图那份）、图级切面（默认继承：父图那批前置——继承的
	//    在外、子图自己声明的在内）。这一步**不能**少了：少了就是观察者断链（父图照常
	//    跑完、子图的记录一条不出现）加名额各算各的。
	//
	//    注意它**会写子图的字段**：这是阶段 1 明说的边界——把挂接状态收进「执行树创建时
	//    算有效值、不写共享定义」是阶段 2（`Execution`）的事，见 object-model 第 3.2 节。
	child.adopt(rc.g.observer, rc.g.lim, rc.g.aspects)

	// 4) 种父侧输入。种之前先确认这条键还是空的：子图自己把它种上了的话（或标成了跳过），
	//    这次种值会被幂等首写**静默忽略**——「父侧传进去的值不见了」而没有任何报错。
	for i, b := range binds {
		if !b.in {
			continue
		}
		if st, _ := child.slotOf(b.child).snapshot(); st != slotPending {
			return fmt.Errorf("pulse: Sub %q: the child graph already resolved key %s before the "+
				"binding seeded it, so the value from the parent would be silently ignored "+
				"(idempotent first write)", id, b.child)
		}
		if err := b.seed(child, vals[i].v, vals[i].skip); err != nil {
			return err
		}
	}

	// 5) 跑。**取消要传下去**：子图那份 ctx 是它建图时带进来的（静态组合没有「建图时的
	//    父节点」可派生），所以这里把父节点这一层的取消转发进子图的取消域。少了这一步，
	//    子图会跑满整段并把取消吞掉——实测 F.Run() 耗时 300ms 且返回 nil（父 ctx 50ms
	//    就被取消了）；转发之后是 51ms + context canceled。
	//    转发是**单向、一对一**的：只把父侧的取消带进子图，不动子图自己的生命周期。
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-rc.Context().Done():
			child.cancelRun()
		case <-stop:
		}
	}()

	if err := child.Run(); err != nil {
		return err
	}

	// 6) 桥回父侧：就绪 → Set，跳过 → Skip；**全部**输出都跳 → 本节点以跳过收尾。
	return bridgeOutputs(rc, id, binds, child)
}

// SubBind 是一对「父侧键 ↔ 子侧键」。用 `In` / `Out` 构造——**方向由构造器定**
// （写反了读不出来），两端必须同一个 `T`，否则编译期就红。
type SubBind struct {
	parent keyRef
	child  keyRef
	in     bool

	pull func(*RunCtx) (any, error)    // In：读父侧（跳过 → *SkipError）
	seed func(*Graph, any, bool) error // In：把值（或「跳过」）种进子图
	push func(*RunCtx, any) error      // Out：把子侧的值写回父侧
}

// In 声明一条父 → 子的绑定：父图读 `parent`，把它种进子图的 `child`。
//
// 两条绑定都是**箭头读法**：来源在前、去向在后——`In(父, 子)`、`Out(子, 父)`。
//
// 跳过的输入照跳：父侧那条槽以跳过到达时，子图对应的键用 `SkipSeed` 种成跳过
// （「跳过是到达」；子图自己的门按它自己的规则判）。
//
// 泛型参数在这里只做一件事：**逼两端用同一个 `T`**（编译期就红）。真正的实现
// 在 `InRef`，读写走的是同一条非泛型路径。
func In[T any](parent, child Key[T]) SubBind {
	return InRef(parent.asRef(), child.asRef())
}

// Out 声明一条子 → 父的绑定：子图产出 `child`，跑完之后写回父图的 `parent`。
//
// 与 `In` 同一条箭头读法（来源在前、去向在后）：`Out(子, 父)`。
func Out[T any](child, parent Key[T]) SubBind {
	return OutRef(child.asRef(), parent.asRef())
}

// InRef 是 `In` 的**动态版**：两端由已解析的 keyRef 给出。
//
// 给**声明式装配**（`pulse/yaml`）用——那里两端是运行期的 name+type 记号，
// 编译期拿不到 `T`，所以「同一对必须同一个类型」只能在**装配期**对账：两端用
// **同一个 type 记号**去查注册表与端口声明，对不上就报。
//
// 手写装配**优先用 `In` / `Out`**：那边编译期就能红，便宜得多。
func InRef(parent, child keyRef) SubBind {
	return SubBind{
		parent: parent,
		child:  child,
		in:     true,
		pull:   func(rc *RunCtx) (any, error) { return getRef(rc, parent) },
		seed: func(g *Graph, v any, skip bool) error {
			if skip {
				return g.seedRef(child, nil, true)
			}
			return g.seedRef(child, v, false)
		},
	}
}

// OutRef 是 `Out` 的动态版（见 `InRef` 的说明）：来源在前、去向在后
// （`OutRef(子, 父)`）。
func OutRef(child, parent keyRef) SubBind {
	return SubBind{
		parent: parent,
		child:  child,
		push:   func(rc *RunCtx, v any) error { return setRef(rc, parent, v) },
	}
}

// readParentInput 取父侧一条绑定的输入：**跳过是到达**，照跳种（skip=true）；
// 其它错误（取消、未声明）原样返回。
func readParentInput(rc *RunCtx, b SubBind) (any, bool, error) {
	v, err := b.pull(rc)
	if err != nil {
		if errors.Is(err, ErrSkipped) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return v, false, nil
}

// bridgeOutputs 把子图跑完之后的结果桥回父侧：就绪 → `Set`，跳过 → `Skip`；
// **全部**输出都以跳过收尾 → 这一步以节点级跳过收尾（`NoValue()`，未发布的输出由引擎自动跳过）。
func bridgeOutputs(rc *RunCtx, id string, binds []SubBind, child *Graph) error {
	hasOut, allSkipped := false, true
	for _, b := range binds {
		if b.in {
			continue
		}
		hasOut = true
		st, _ := child.slotOf(b.child).snapshot()
		switch st {
		case slotReady:
			allSkipped = false
		case slotSkipped:
		default:
			return fmt.Errorf("pulse: Sub %q: the child finished but its output key %s "+
				"never arrived (neither written nor skipped)", id, b.child)
		}
	}
	if hasOut && allSkipped {
		return NoValue() // 节点级跳过；未发布的输出由引擎自动跳过
	}
	for _, b := range binds {
		if b.in {
			continue
		}
		st, v := child.slotOf(b.child).snapshot()
		if st == slotSkipped {
			if err := skipRef(rc, b.parent); err != nil {
				return err
			}
			continue
		}
		if err := b.push(rc, v); err != nil {
			return err
		}
	}
	return nil
}

// adopt 把父图的挂接状态写进子图（观察者、名额、图级切面），**一次加锁写完**。
//
// 为什么不能各写各的：这几个字段是「认领之后不再变」的那批，而读它们的是
// `Validate()` / `Topology()` 这类只读入口（持 child.mu）。子图在**装配那一刻**就已经
// 在调用方手里（`Sub` 的入参就是它），宿主完全可以把它交给别的 goroutine；挂接期间那头
// 调只读入口，就是在读一个正在被写的字段——评审在 #304 上点的那条 race（用例
// `TestSubHookupRaceWithReadOnlyEntry` 复现）。
//
// 临界区里**不做任何会再锁 child.mu 的事**（种值走的是 slotsMu，留在调用点按原顺序做）；
// 也**不校验**——校验与报文都在调用点，这里只负责把字段写进去。
func (g *Graph) adopt(observer Observer, lim *limiter, aspects []Aspect) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.observer == nil {
		g.observer = observer
	}
	if !g.limSet {
		g.lim = lim
	}
	// 图级切面默认**继承**（把父图那批前置）；`WithoutAspects()` 切断。注意切断只解开
	// 「继承到子图节点上」这一段——父图那一步仍然被父图的图级切面包着，那是父图
	// 对自己节点做的事。前置用**新切片**，不写坏父图那批的底层数组。
	if !g.aspectsCut {
		g.aspects = append(append([]Aspect{}, aspects...), g.aspects...)
	}
}
