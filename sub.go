package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// SubCtx 是 `Sub` 交给 build 的三样东西：**取消域**、建议的图 id、嵌套路径。
type SubCtx struct {
	rc   *RunCtx
	id   string
	path string
}

// Context 返回子图该用的 context：派生自本节点的 `rc.Context()`。
//
// **别用 `context.Background()`**：那样父图被取消时子图看不见，它会照常跑完——
// 取消被吞掉，而且没有任何报错。传 `sc.Context()` 才有这条链。
func (sc *SubCtx) Context() context.Context { return sc.rc.Context() }

// GraphID 是给子图的建议 id：`<父图 id>/<本节点 id>`。
//
// 它只是**建议**（`New` 的 graphID 由调用方定）——观测里「跑的是哪张图」看
// `pulse.graph`，而「这一步在嵌套的哪一层」看 `Path()`，两者分工不同。
func (sc *SubCtx) GraphID() string { return sc.id }

// Path 是这条嵌套的**节点 id 链**：根图为 `""`，一层子图是 `"step1"`，
// 再深一层是 `"step1/inner"`。不透明字符串，层级由出口自行拆分（#294 的口径）。
//
// 要让它进观测记录，就在 `build` 里把它交给**这一层**的出口
// （`observe.ObserveConfig.Path`）——继承父图的出口带的是父层的路径。
func (sc *SubCtx) Path() string { return sc.path }

// Observer 是父图挂的观察者（父图没挂时为 nil）。
//
// **通常不用调用它**：子图没挂观察者时 `Sub` 会自动继承父图的——而漏挂导致的
// 观测断链是静默的（父图照常跑完、`Run()` 照常返回 nil，只是子图的记录一条都
// 不出现），所以默认就给接上。需要它的是另一种用法：在子图上**再叠一层**自己的
// 出口，例如 `pulse.WithObserver(pulse.MultiObserver{sc.Observer(), mine})`。
func (sc *SubCtx) Observer() Observer {
	if sc.rc == nil || sc.rc.g == nil {
		return nil
	}
	return sc.rc.g.observer
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
// 编译期拿不到 `T`，所以「同一对必须同一个类型」只能在装图期对账：两端用
// **同一个 type 记号**去查注册表，对不上就报。运行期还有一道兜底：子图声明的
// 那条键与绑定的类型不符时，第一次跑就报（见 Sub 的运行体）。
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

// Sub 装一个「图即节点」的节点：`build` 每次运行都被调用一次，返回要跑的子图。
//
//	step1 := pulse.Sub(g, "step1",
//	    []pulse.SubBind{pulse.In(topic, childIn), pulse.Out(childOut, summary)},
//	    func(sc *pulse.SubCtx) (*pulse.Graph, error) {
//	        child, err := pulse.New(sc.Context(), sc.GraphID())
//	        if err != nil {
//	            return nil, err
//	        }
//	        // …给 child 装节点（读 childIn、写 childOut）…
//	        return child, nil
//	    })
//
// 父侧的 `Requires` / `Provides` 由 binds 推出来（`In` 的父侧端进 Requires、
// `Out` 的父侧端进 Provides），边界写在接线处——读父图一眼看清这个节点吃什么
// 吐什么；父图的静态校验（来源 / 环 / 流式名额）照常生效，子图自己的校验在子图
// `Run` 时跑。
//
// **它收掉的三处手工搭最容易错、而且错了没有声音的地方**：
//
//  1. 观察者——子图没挂观察者时自动继承父图的。手工搭漏挂的话，父图照常跑完、
//     `Run()` 照常返回 nil，只是子图的记录一条都不出现。
//     **但继承只保证「记录不丢」，不保证层级归因**：出口实例上的 `pulse.path`
//     是建它那一刻定下的，继承来的那份带的还是**父层**的路径——实测两层嵌套里
//     最内层那 4 条记录全被记成中间层的 `path`，归因错比缺更难查。要 `pulse.path`
//     就在 `build` 里按 `sc.Path()` 给这一层**各建一个出口**（见 `ObserveConfig.Path`）；
//  2. 取消域——`sc.Context()` 派生自本节点，父图 / 本节点被取消时子图看得见。
//     手工搭用 `context.Background()` 建子图，取消会被吞；
//  3. 桥接——父 → 子走 `Seed`，子 → 父由 `Sub` 读子图的槽写回。手工搭时子 →
//     父只能让子图节点把结果写进闭包变量（`Graph` 没有公开读槽 API）。
//
// 语义与手写嵌套**完全一致**（糖不改语义）：
//
//   - 子图成功 → 本节点成功，输出按槽位桥回父侧（就绪 → `Set`，跳过 → `Skip`）；
//   - **全部**输出都以跳过收尾 → 本节点 `NoValue()`（节点级跳过，与「缺项不是
//     失败」同一口径；已经 Set 过的输出不回滚）；
//   - 子图失败 → 本节点失败，**首错原样冒泡**（`errors.Is` 成立），首错取消父图；
//   - 取消 → 原样带出（子图 ctx 派生自本节点）。
//
// 槽位与运行名额**各自独立**，这条算术要记住：父图 `WithMaxRunning` 管的是
// **同时有几张子图在跑**（本节点在父图里占一个名额，占满整段子图运行），
// 子图内部的并发由子图自己的 `WithMaxRunning` 管——所以嵌套会突破父图的上限
// （实测：父 `maxRun=1` + 子 `maxRun=2` 时，同时进入 `Run` 的节点峰值 3）。
//
// **子图是一次性的**：`build` 每次运行都要造一张新图（一次性契约）。把子图建在
// 闭包外面复用 → `ErrGraphStarted`，这时报出来的是一句能照着改的话。认领是
// **原子**的（在改这张图的 `path` / observer / 种值**之前**就认掉），所以并发的
// 两次 `Sub` 复用同一张图也只会有一方拿到它，另一方当场拿到同一句话——不会两边
// 一起写同一张图。
//
// `aspects` 作用于**父侧这个节点**（例如 `pulse.Timeout(30*time.Second)` =
// 整张子图限时，是最自然的用法）。
func Sub(g *Graph, id string, binds []SubBind, build func(*SubCtx) (*Graph, error),
	aspects ...Aspect) error {
	if g == nil {
		return fmt.Errorf("pulse: Sub %q: nil graph", id)
	}
	if id == "" {
		return fmt.Errorf("pulse: Sub: empty node id")
	}
	// 这个 id 会成为 `SubCtx.Path()` 里的一段，而 path 的层与层之间正是用 `/`
	// 拼的：放行 `a/b` 的话，它与「`a` 里再嵌一个 `b`」拼出**同一条** path
	// （实测两条都是 `"a/b"`，连 `GraphID()` 也是），层级再也拆不回来。
	// 别的节点 id 含 `/` 无所谓——它们不进 path。
	if strings.ContainsRune(id, '/') {
		return fmt.Errorf("pulse: Sub %q: node id must not contain '/': "+
			"that is the separator between layers in a nested path, so this step would come out "+
			"indistinguishable from a subgraph nested one level deeper", id)
	}
	if build == nil {
		return fmt.Errorf("pulse: Sub %q: nil build", id)
	}
	if len(binds) == 0 {
		return fmt.Errorf("pulse: Sub %q: no binds: a step that exchanges nothing "+
			"with its parent cannot be ordered by it either", id)
	}

	// 装配期能判的都判掉：父侧键交给 Add 兜（重复 Requires、既读又写都会红），
	// 这里查子侧——同一个子侧键出现在两条绑定里，第二次种值会被幂等首写**静默忽略**。
	seenChild := make(map[string]struct{}, len(binds))
	var requires, provides []keyRef
	for i, b := range binds {
		if b.parent.typ == nil || b.child.typ == nil {
			return fmt.Errorf("pulse: Sub %q: bind %d is zero: build binds with In/Out", id, i)
		}
		if _, dup := seenChild[b.child.name]; dup {
			return fmt.Errorf("pulse: Sub %q: child key %q is bound twice: "+
				"the second seed would be silently ignored (idempotent first write)", id, b.child.name)
		}
		seenChild[b.child.name] = struct{}{}
		if b.in {
			requires = append(requires, b.parent)
			continue
		}
		provides = append(provides, b.parent)
	}

	n := NewNode(id, requires, provides, func(rc *RunCtx) error {
		return runSub(rc, id, binds, build)
	}, aspects...)
	if err := g.Add(n); err != nil {
		return fmt.Errorf("pulse: Sub %q: %w", id, err)
	}
	return nil
}

// runSub 是 Sub 节点的运行体：取值 → 建图 → 校验 → 种值 → 跑 → 桥回。
func runSub(rc *RunCtx, id string, binds []SubBind, build func(*SubCtx) (*Graph, error)) error {
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
		v, err := b.pull(rc)
		if err != nil {
			if errors.Is(err, ErrSkipped) {
				vals[i].skip = true
				continue
			}
			return err
		}
		vals[i].v = v
	}

	// 2) 建子图：ctx 派生自本节点（父取消子看得见），GraphID / Path 由 SubCtx 给。
	path := id
	if rc.g.path != "" {
		path = rc.g.path + "/" + id
	}
	child, err := build(&SubCtx{rc: rc, id: rc.g.id + "/" + id, path: path})
	if err != nil {
		return err
	}
	if child == nil {
		return fmt.Errorf("pulse: Sub %q: build returned a nil graph", id)
	}

	// 3) 先**原子认领**这张子图：复用同一张图是这里最容易犯、也最难从「已经启动过」
	//    四个字里看出该怎么改的错，所以放在任何改动与检查之前，别让后面任何一条
	//    检查抢了它的消息（种子冲突那条尤其会撞上来）。必须是原子的——否则并发的
	//    两次 Sub 会一起看到「没启动」，然后一起写下面这些字段（`-race` 实测：
	//    `child.path = path` 那一行就是一处 DATA RACE）。
	if !child.claimSub() {
		return subStartedErr(id, ErrGraphStarted)
	}

	// 4) 建完先校验绑定：子图没声明这条键、或类型对不上，都要**现在**说——
	//    别留到「种进去没人看」（Seed 对未知键会静默建槽，然后什么都不会发生）。
	//    认领之后没人能再动这张图，所以这些读也是安全的。
	for _, b := range binds {
		typ, ok := child.keys.typeOf(b.child.name)
		if !ok {
			return fmt.Errorf("pulse: Sub %q: the child graph does not declare %s: "+
				"the bind points at a key the child does not know", id, b.child)
		}
		if typ != b.child.typ {
			return fmt.Errorf("pulse: Sub %q: key %q type mismatch: child declares %s, bind declares %s",
				id, b.child.name, typ, b.child.typ)
		}
	}
	child.path = path // 更深一层嵌套靠它算 path

	// 5) 观察者：子图自己挂了就用它自己的；没挂则继承父图的——漏挂是**静默**的
	//    （父图照常跑完，子图的记录一条都不出现），所以默认接上。
	//
	//    **继承只保证「记录不丢」，不保证层级归因**：出口实例上的 `pulse.path` 是
	//    建它那一刻定下的，继承来的那份带的还是**父层**的 path。嵌套要在 `build`
	//    里按 `sc.Path()` 给每一层各建一个出口（见 `ObserveConfig.Path`）。
	if child.observer == nil {
		child.observer = rc.g.observer
	}

	// 6) 种父侧输入。种之前先确认这条键还是空的：子图自己在 build 里把它种上了
	//    的话（或标成了跳过），这次种值会被幂等首写**静默忽略**——「父侧传进去的
	//    值不见了」而没有任何报错，正是糖要消灭的那类错。想「父图给值、子图兜底
	//    默认值」在一次性槽位上是表达不出来的，所以这里吵出来，别猜。
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
			return subStartedErr(id, err)
		}
	}

	// 7) 跑子图。首错原样冒泡。
	if err := child.Run(); err != nil {
		return subStartedErr(id, err)
	}

	// 8) 桥回父侧：就绪 → Set，跳过 → Skip；**全部**输出都跳 → 本节点以跳过收尾。
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

// subStartedErr 把「复用同一张子图」这条最容易犯的错翻成一句能照着改的话。
//
// 保留 `%w`：冻结面里的哨兵不该被这句翻译吃掉，宿主照样 `errors.Is(err,
// ErrGraphStarted)` 判得出来（与 Join / FanOut 的 `%w` 同形）。
func subStartedErr(id string, err error) error {
	if errors.Is(err, ErrGraphStarted) {
		return fmt.Errorf("pulse: Sub %q: the child graph was already started: "+
			"a graph runs once, so build must return a new one on every run (%w)", id, err)
	}
	return err
}
