package pulse

import (
	"fmt"
	"strings"
)

// 本文件是**静态组合**那一套：`Sub`（挂一张现成的子图）+ 接线检查 + 挂子图那一步的运行体。
// 装配锁 `assemblyMu` 声明在 graph.go（它同时管着 `Add` / `DeclareInput` / `Validate`）。

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
// 同一批 binds 的形状判据（都在装配期报，报文点着子侧键名）：
//
//   - **同一条子侧进口只能绑一次**：两次种同一条槽，第二次会被幂等首写静默忽略；
//   - **子图需要的进口都得给值**（`In` / `Fix` / `FixSkip` 都算）：有节点 `Requires` 它，或它是
//     这一处绑出去的透传出口的唯一来源 → 漏绑在这一步就报，别留到运行期才从「建子执行」或
//     `bridgeOutputs` 那里冒出来（见 checkInletsFedLocked）。没被任何节点 `Requires`、也没被
//     这一处绑出去的进口是**可选**的，不逼调用方编个值出来；
//   - **一条 `In` 绑的进口，在子图里不可能已经有来源**：这个状态已经建不出来了——
//     「一条键既是声明过的进口、又有节点产出它」在**声明那一刻**就被拒（`DeclareInput` 与
//     `Add` 两个方向同一句判据，都撞 `ErrDuplicateSource`，见 key.go / graph.go）；
//   - **透传**（同一条键既是入口又是出口）是合法的：`In(父, k)` + `Out(k, 父)` 两条都写，
//     父侧喂进去的值就是子图产出的那条值。出口**不去重**——同一条出口推给两个父侧键没有
//     静默丢值的问题（推给同一个父侧键早在 `Add` 那层就被「declares ... twice in Provides」拒了）。
//
// **组合环在装配期就拒**：`Sub(A, …, B)` 之后再 `Sub(B, …, A)`（或直接自挂）会让静态递归
// 永远展开不完，所以这里沿子图子树查一遍「父图是不是已经在它的子树里」，是就报
// `pulse: subgraph cycle: A -> B -> A`。判据只看**当前这条路径**（见 hasAncestor）——同一张
// 子图在**不同挂载位置**重复出现是合法的，不能用全局 visited 误拒。
//
// 「查一遍」与「真的挂上」在**同一个全局临界区**里（assemblyMu）：对向挂载（A 里挂 B、同时
// B 里挂 A）是两张图各自动作的结果，单看任何一张图的锁都拦不住——两边都可能先看过「没有环」
// 再各挂一步，环就进去了，此后递归展开是爆栈或互等锁。装配是一次性动作、临界区里没有用户
// 代码，这把全局锁的代价是零。递归本身还有一道兜底判环（见 problemsSubAtLocked）。
//
// Sub 是结构组合，不等待推导的全部父 Requires，也不因父输入全 skip 而跳过整张子图。
// In 保留真实来源，内部节点各自按自己的 Requires 等待和判 skip；实时流 Out 在子执行启动前
// 建立稳定槽位别名，普通值仍在 child.Wait 成功后桥回。失败与取消原样冒泡，全部实际出口
// 都 skipped 时本节点以 NoValue 收尾。Sub 自身不吃运行名额。三处要记住：
//
//  1. **每个挂载位置一份子执行**：挂在这里的每一步为子图建一份**自己的**子执行
//     （自己的 ctx / 槽位 / 状态 / 首错），所以同一张子图挂两处时两处都能跑，各自的
//     输入输出互不干扰。子图是**定义**，可以在别处照样挂、照样跑（见 Graph 的 godoc
//     与 TestSealedChildCanBeReusedElsewhere）。
//  2. **取消域**：子执行的 ctx 从父节点这一层的 ctx 派生，所以取消天然传下去（含这一步
//     上的 `Timeout` 切面）。不派生的话子图会跑满整段、把父图的取消吞掉——实测过：父图
//     耗时 300ms 且报成功。取消**只影响这一次运行**：子图定义与别处的运行不受影响。
//  3. **普通值出口的 Sub 可重试**：每次尝试各建一份新的子执行，第一次尝试的桥回随
//     Retry 的暂存一起作废，父侧只拿到成功那次的普通值。内部流最终只向外给普通值也合法；
//     直接向外暴露实时流时，发布无法撤回，写暂存会在子执行启动前被拒。**没有边界的重入仍然被拒**：
//     判据在最内层的推进闸上（所有节点共用一处，见 `RunCtx.beginAdvance`），用户自己写的
//     重试切面开不了边界，第二次推进会连第一次的失败原因一起报出来。`pulse/yaml` 对
//     `graph:` 节点上的 `retry:` 是**照装**（与 Go 侧同一个语义），不再在 `Load` 期拒。
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
	// 组合环那条判据是**全局**不变式：「查一遍子树」与「挂上」必须一起做完（见 assemblyMu）。
	assemblyMu.Lock()
	defer assemblyMu.Unlock()
	// 组合环：装配期能判的，别留到递归展开时才炸（那时是栈溢出）。
	if err := g.checkMountCycle(child); err != nil {
		return err
	}
	// 这份接线从此属于这张定义：**复制一份收下**。不复制的话，调用方之后改自己那份切片
	// （`binds[0] = …`）就能改运行期接线——而父侧的 `Requires` / `Provides` 早按挂载那一刻
	// 推导完了，于是跑起来的那张图与 `Topology()` / 校验看到的那张成了两张，装配锁与封口
	// 也一起被绕开。
	binds = append([]SubBind(nil), binds...)

	// 接线只能接端口：子侧两端都要是声明过的对应方向端口。
	requires, provides, err := subBinds(child, id, binds)
	if err != nil {
		return err
	}

	// 这一步的体就是「跑子图 + 桥回」，**没有自己的重入账**：一个节点在同一次执行里
	// 只允许成功推进一次，那条判据在最内层的推进闸上、所有节点共用一处（见
	// `RunCtx.beginAdvance`）。`Retry` 每次尝试都开一条新的写暂存，于是它的重入是
	// 「有边界的重入」——每一次尝试各建一份**新的子执行**，第一次尝试的桥回随它的
	// 暂存一起作废，父侧只拿得到成功那一次的输出版本。用户自己写的重试切面开不了
	// 边界，第二次推进在那里当场被拒（报文连第一次的失败原因一起报出来）。
	body := func(rc *RunCtx) error {
		return runStaticSub(rc, id, binds, child)
	}
	n := NewNode(id, description, requires, provides, body, aspects...)
	n.sub = true // 这一步不吃运行名额：它整段都在等子图跑完（见 WithMaxRunning）
	n.child = child
	n.binds = binds
	if err := g.addAllLocked([]*Node{n}); err != nil {
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
// `Fix` / `FixSkip` 两边都不进：它不牵涉父侧任何键。
// 同一父值可以送往多个不同的子入口，父侧依赖只记首次；按名字 + 真实类型去重，
// 同名跨型仍留给 Add 拒绝。binds 保留每条接线，子入口重绑仍逐条核；依赖去重不代表读者合并。
func parentBinds(binds []SubBind) (requires, provides []keyRef) {
	seen := make(map[keyRef]struct{}, len(binds))
	for _, b := range binds {
		if b.fix != nil {
			continue
		}
		if b.in {
			if _, dup := seen[b.parent]; !dup {
				requires = append(requires, b.parent)
				seen[b.parent] = struct{}{}
			}
			continue
		}
		provides = append(provides, b.parent)
	}
	return requires, provides
}

// checkBinds 检查一批绑定，**两个方向都判**：① 传进来的每条绑定自己成不成立（零值绑定、
// 同一条子侧进口绑两次、子侧两端是不是声明过的对应方向端口、类型对不对）；② **子图需要的
// 进口有没有漏**（见 checkInletsFedLocked）。装配期一次判清，`Sub` 装配时与 `Validate` /
// `Topology` 递归时共用同一份。
//
// 整批在**子图那把锁**里判完：端口表是子图的字段，而子图在装配那一刻就在宿主手里，
// 解锁之后再查它那张表就是读一张别人能改的 map（`DeclareInput` 往同一张表里写）——
// `-race` 实测报过一处 DATA RACE（declarePorts ↔ 这里）。所以这里持着锁调 `checkPortLocked`；
// **持着子图锁时不许再调会自己加锁的变体**（`checkPort` 已经不在了，理由就是它）。
//
// 「这条进口在子图里已经有来源」**不在这里**，而且今天已经没有那个状态可判：「一条键既是
// 声明过的进口、又有节点产出它」在声明那一刻就被拒（见 declarePorts / addAllLocked，两个方向
// 同一句 `ErrDuplicateSource`）。它历史上还有另一种形状——「子图自己 `Seed` 过这条键」——
// 那条随 `Seed` 搬进**一次运行**而消失：输入属于一次运行，不再能往定义里垫默认值。要「子图
// 自己有一个默认值」就在子图里放一个产出它的节点（那是产出，不是外部来源）；要「这一处挂载
// 用另一个值」就用 `Fix` / `FixSkip`。
//
// 这里剩下的都是「挂载那一刻核过、但那是当时的事实」那类：子图的端口可能在挂载之后、它自己
// 封口之前改过（例如挂完之后又 `DeclareInput` 了一条，或又 `Add` 了一个要新进口的节点），
// 所以每次重新判一遍，报文与 `Sub` 装配时逐字相同。
func checkBinds(child *Graph, id string, binds []SubBind) error {
	seenIn := make(map[string]struct{}, len(binds))
	var outs []keyRef
	child.mu.Lock()
	defer child.mu.Unlock()
	for i, b := range binds {
		if b.fix != nil {
			// `Fix` / `FixSkip`：载荷是一份（可能不止一条的）输入，逐条走同一条进口判据。
			if b.fix.bad != nil {
				return fmt.Errorf("pulse: Sub %q: %w", id, b.fix.bad)
			}
			for _, ref := range b.fixKeys() {
				if err := checkInletLocked(child, id, ref, seenIn); err != nil {
					return err
				}
			}
			continue
		}
		if !b.child.valid() || !b.parent.valid() {
			return fmt.Errorf("pulse: Sub %q: bind %d is zero: build binds with In/Out/Fix", id, i)
		}
		// 动态 Ref 绕过泛型的同 T 约束，必须比较真实类型，不能只比较显示用的 String。
		if b.parent.typ != b.child.typ {
			return fmt.Errorf("pulse: Sub %q: bind %d type mismatch: parent key %s and child key %s "+
				"must have the same Go type", id, i, b.parent, b.child)
		}
		if b.in {
			if err := checkInletLocked(child, id, b.child, seenIn); err != nil {
				return err
			}
			continue
		}
		if err := child.checkPortRefLocked(id, b.child, false); err != nil {
			return err
		}
		outs = append(outs, b.child)
	}
	// 反向那一遍：上面判的是「传进来的每条绑定对不对」，这一遍判「子图需要的进口有没有漏」。
	return child.checkInletsFedLocked(id, seenIn, outs)
}

// checkInletsFedLocked 是**反向**那一遍：子图**自己需要**的那些进口，这一处挂载都得给值
// （`In` / `Fix` / `FixSkip` 都算，`fed` 就是上面收下来的那批）。调用方持 g.mu（`checkBinds`
// 里持的就是子图那把锁）。
//
// 两种「需要」，都是读一遍声明就能定的：
//
//  1. **子图里有节点 `Requires` 它**（而它是声明过的进口）：挂载点不给值，子图这次跑不起来
//     ——`newChildExecution` 会在**运行期**报「has no value for this run」，那时父图别的节点
//     可能已经跑完并留下副作用，而 `Validate()` 还已经把这个定义封了口；
//  2. **它是这一处绑出去的出口的唯一来源**：同一条键既是进口又是出口 = 透传，而声明过的进口
//     不可能再被节点产出（见 declarePorts / addAllLocked），所以它的唯一来源就是这个挂载点。
//     不喂的话，`bridgeOutputs` 只能等子图跑完才报一句「its output key … never arrived」。
//
// 两个边界都不是随手划的：
//
//   - **没被任何节点 `Requires`、也没被这一处绑出去**的进口是**可选**的：它可能是留给别的
//     挂载点的接口，也可能是这一处用不到的透传端——不逼调用方编个值出来（与「没人 `Requires`
//     的进口不逼调用方编值」同一条纪律，见 requiredInputsLocked）；
//   - 义务由**承诺**定：`Out` 是「这一步会给你这条值」的承诺，所以第 2 种只看**绑出去的**出口。
//     没绑出去的出口谁也没承诺，硬要它喂一个没人读的值只是噪音（一个多口透传组件挂在只用
//     其中一部分口的地方，是正常用法）。
//
// 判据只读子图的定义（端口表 + 节点声明）与这一处的绑定，不看任何一次运行留下的痕迹；
// 两个循环都按声明序 / 绑定序走，报文不会因 map 迭代顺序而变。
func (g *Graph) checkInletsFedLocked(stepID string, fed map[string]struct{}, outs []keyRef) error {
	for _, n := range g.nodes {
		for _, k := range n.requires {
			if _, isIn := g.inPorts[k.name]; !isIn {
				continue // 不是声明过的进口：那是子图自己的来源问题，它的静态校验会报
			}
			if _, ok := fed[k.name]; ok {
				continue
			}
			return fmt.Errorf("pulse: Sub %q: child inlet %s has no value for this mount: "+
				"node %q inside the child requires it, and it is a declared input of graph %q - "+
				"bind it with In, or give this mount site a value with Fix / FixSkip",
				stepID, k, n.id, g.id)
		}
	}
	for _, k := range outs {
		if _, isIn := g.inPorts[k.name]; !isIn {
			continue // 出口由子图内部产出（或它自己报「没人产出」），不干挂载点的事
		}
		if _, ok := fed[k.name]; ok {
			continue
		}
		return fmt.Errorf("pulse: Sub %q: child outlet %s is bound here, but it is also a "+
			"declared input of graph %q and nothing inside the child produces it, so this mount "+
			"site has to feed it - bind it with In, or give this mount site a value with "+
			"Fix / FixSkip", stepID, k, g.id)
	}
	return nil
}

// checkInletLocked 是「往子图某条进口送一个值」的两条判据（`In` 与 `Fix` / `FixSkip` 共用）：
// 方向对（是声明过的**入口**）、这一次只喂一条。调用方持 child.mu；`seenIn` 由调用方跨 bind
// 累积（同一个挂载点上两条绑定喂同一条进口 = 第二条被静默吞掉）。
//
// 早先这里还有第三条——「子图内部已经有节点产出它」。它已经不在这里了：那个状态在声明那一刻
// 就被拒（见 declarePorts / addAllLocked），所以走到这里的子图一定没有那个毛病。
func checkInletLocked(child *Graph, id string, ref keyRef, seenIn map[string]struct{}) error {
	if err := child.checkPortRefLocked(id, ref, true); err != nil {
		return err
	}
	if _, dup := seenIn[ref.name]; dup {
		return fmt.Errorf("pulse: Sub %q: child inlet %q is bound twice: "+
			"the second value would be silently ignored (idempotent first write)", id, ref.name)
	}
	seenIn[ref.name] = struct{}{}
	return nil
}

// checkPortRefLocked 判一条绑定的子侧端是不是**声明过的、方向对的**端口。调用方持 g.mu。
//
// 报文要能照着改成：三种情形分开说——没声明成端口（点明「它不是这张图的端口」）、方向反了
// （声明过、但你绑的是另一头）、类型不符（端口声明的类型与绑定不一致）。
func (g *Graph) checkPortRefLocked(stepID string, ref keyRef, intoChild bool) error {
	want, other := g.outPorts, g.inPorts // 方向说明见下：`in` 绑的是子图的**入口**
	direction, otherDirection := "output", "input"
	bind, from, to, decl := "Out(子, 父)", "子", "父", "Output"
	if intoChild {
		want, other = g.inPorts, g.outPorts
		direction, otherDirection = "input", "output"
		bind, from, to, decl = "In(父, 子)", "父", "子", "Input"
	}
	if p, ok := want[ref.name]; ok {
		if p.typ != ref.typ {
			return fmt.Errorf("pulse: Sub %q: port %q type mismatch: the child declares it as %s, "+
				"the bind declares %s", stepID, ref.name, typeName(p.typ), typeName(ref.typ))
		}
		return nil
	}
	if _, ok := other[ref.name]; ok {
		return fmt.Errorf("pulse: Sub %q: %q is a declared %s of the child graph, but this bind "+
			"treats it as the %s side: %s points from %s to %s",
			stepID, ref.name, otherDirection, direction, bind, from, to)
	}
	return fmt.Errorf("pulse: Sub %q: %q is not a %s port of the child graph: declare it with "+
		"Declare%s (a key that only lives inside the child is its implementation detail), "+
		"or bind a different key", stepID, ref.name, direction, decl)
}

// checkMountCycle 是组合环的判据：把 child 挂进 g 之前，先看 g 是不是已经在 child 的子树里
// （自挂也算）。调用方只是在装配（且持着 assemblyMu），所以这里逐个图加锁读一次，不跨图持锁。
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

// SubBind 是一对「父侧键 ↔ 子侧键」——或者一条「固定值 ↔ 子侧键」。用 `In` / `Out` /
// `Fix` / `FixSkip` 构造——**方向由构造器定**（写反了读不出来），`In` / `Out` 两端必须
// 同一个 `T`，否则编译期就红。
type SubBind struct {
	parent keyRef
	child  keyRef
	in     bool
	// fix 非 nil 表示这条绑定是**固定值 → 子**：这一步给子图那几条进口常量（或「跳过」），
	// 不牵涉父侧任何键。载荷是一份完整的输入（而不是单个值），因为**声明式装配**那条路
	// 要用 `SeedByName` 做类型对齐（`coerceSeedValue`），而那个函数写的就是 `Inputs`。
	fix *fixBind

	push func(*RunCtx, any) error // 普通值 Out：把子侧的值写回父侧；In / 实时 Out 直接绑定槽位
}

// fixBind 是一条（批）固定值绑定的载荷。`bad` 兜住「值本身就不合法」的那条路
// （nil 值），留到 `Sub` / `Validate` 报出来——构造器保持不含 error 的形状。
type fixBind struct {
	in  Inputs
	bad error
}

// intoChild 报这条绑定是不是「往子图的进口送一个值」——`In` 与 `Fix` / `FixSkip` 都是。
// 端口方向、进口去重、「这条进口在子图里已经有来源」三条判据都按它分流。
func (b SubBind) intoChild() bool { return b.in || b.fix != nil }

// fixKeys 是一条固定值绑定喂的那几条子侧键（名字有序），`Fix` 与 `In` 共用同一套判据时用。
func (b SubBind) fixKeys() []keyRef {
	if b.fix == nil {
		return nil
	}
	return b.fix.in.refs()
}

// Fix 声明一条「固定值 → 子图」的绑定：**这一步**给子图的 `child` 这条进口一个常量，
// 父侧不参与。
//
// 什么时候要它：同一张子图挂在两处、两处要的默认值不同——那个值属于**挂载点**，
// 不属于子图定义，所以不能写进子图里（写进去就成了「共享定义被一次运行写脏」，
// 也正是旧 `pulse.Seed(child, k, v)` 被删掉的原因）。子图**谁都这么用**的默认值仍然
// 建议放进子图自己（一个产出它的节点）——两种是不同的事。
//
// 与 `In` 同一条方向语义（箭头朝子图），所以端口判据、进口去重与「这条进口在子图里
// 已经有来源」三条判据都是共用的。
func Fix[T any](child Key[T], v T) SubBind {
	var b fixBind
	if err := b.in.set(child.asRef(), any(v), false); err != nil {
		b.bad = err
	}
	return SubBind{child: child.asRef(), fix: &b}
}

// FixSkip 与 `Fix` 同形：给子图的某条进口一个**固定的跳过**。
//
// 跳过是到达，不是失败——「这一步的子图这次没有这条值，但你照常按没有值走」。
func FixSkip[T any](child Key[T]) SubBind {
	var b fixBind
	if err := b.in.set(child.asRef(), nil, true); err != nil {
		b.bad = err
	}
	return SubBind{child: child.asRef(), fix: &b}
}

// FixByName / FixSkipByName 是声明式装配（`pulse/yaml`）用的那条路：按登记表解析键名与
// 类型记号，值再按目标类型做**形状对齐**（与 `SeedByName` 完全同一条判据——子图的
// `seeds:` 从 YAML 解出来是 `[]any` / `map[string]any`，不 coerce 就对不上具体类型）。
//
// 两条都要把解析出来的 `child` 记进绑定本身：`bridgeOutputs` 按它分流「往子图送」与
// 「从子图取」——只填 `fix` 不填 `child` 的话，这条绑定会被当成一条子侧键为空的
// `Out`（实测报出 `its output key  never arrived`，键名是空的）。
func FixByName(reg *Registry, name, typeTag string, v any) (SubBind, error) {
	ref, err := reg.ResolveKey(name, typeTag)
	if err != nil {
		return SubBind{}, err
	}
	var b fixBind
	if err := SeedByName(&b.in, reg, name, typeTag, v); err != nil {
		return SubBind{}, err
	}
	return SubBind{child: ref, fix: &b}, nil
}

// FixSkipByName 见 `FixByName`。
func FixSkipByName(reg *Registry, name, typeTag string) (SubBind, error) {
	ref, err := reg.ResolveKey(name, typeTag)
	if err != nil {
		return SubBind{}, err
	}
	var b fixBind
	if err := SkipSeedByName(&b.in, reg, name, typeTag); err != nil {
		return SubBind{}, err
	}
	return SubBind{child: ref, fix: &b}, nil
}

// In 声明一条父 → 子的绑定：子图的 child 沿父侧 parent 的真实槽位等待到达。
//
// 两条绑定都是**箭头读法**：来源在前、去向在后——`In(父, 子)`、`Out(子, 父)`。
//
// 跳过的输入照跳：父侧那条槽以跳过到达时，子图对应的键用「跳过」种进去
// （「跳过是到达」；子图自己的门按它自己的规则判）。
//
// 泛型参数在这里只做一件事：**逼两端用同一个 `T`**（编译期就红）。真正的实现
// 在 `InRef`，读写走的是同一条非泛型路径。
func In[T any](parent, child Key[T]) SubBind {
	return InRef(parent.asRef(), child.asRef())
}

// Out 声明一条子 → 父的绑定：实时流 child 到达即向父侧 parent 可见；
// 普通值在子执行成功完成后写回。绑定只传递端点身份，不消费或确认消息。
//
// 与 `In` 同一条箭头读法（来源在前、去向在后）：`Out(子, 父)`。
func Out[T any](child, parent Key[T]) SubBind {
	return OutRef(child.asRef(), parent.asRef())
}

// InRef 是 `In` 的**动态版**：两端由已解析的 keyRef 给出。
//
// 给**声明式装配**（`pulse/yaml`）用——那里两端是运行期的 name+type 记号，
// 编译期拿不到 `T`，所以「同一对必须同一个类型」只能在**装配期**对账：两端用
// **真实 reflect.Type**与子图端口声明对账（显示用的短名不能代替类型身份），对不上就报。
//
// 手写装配**优先用 `In` / `Out`**：那边编译期就能红，便宜得多。
func InRef(parent, child keyRef) SubBind {
	return SubBind{
		parent: parent,
		child:  child,
		in:     true,
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
