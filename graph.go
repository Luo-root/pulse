package pulse

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Graph 是一张**可复用的静态定义**：节点、由 Key 推出的依赖、端口（对外接受的输入 /
// 产出的输出）、静态挂载的子图、描述，以及**默认**的运行策略声明（观察者 / 名额 / 图级切面）。
//
// 它**不持有**任何一次运行的东西：ctx、输入值、槽位、节点状态、首错、waitgroup、取消域
// 全在 `Execution` 里（见 `NewExecution` / `Run`）。所以同一张 Graph 可以顺序跑很多次、
// 并发跑很多次、被多个父图在各处挂载——一次运行的取消 / 失败 / 完成不污染定义，
// 也不污染别的运行。
//
// 装配 → 封口 → 进程结束：`Validate()` 通过、或某次运行 `Start()` 成功，会把**整棵子树**
// 的定义封口（此后 `Add` / `DeclareInput` / `DeclareOutput` / `Sub` 都被拒，
// `ErrGraphStarted`）。封口说的是「这张定义不再变」，**不是**「用过它」——一张已封口的
// 子图照样能被别处挂载，并在那一处正常跑起来（用例 `TestSealedChildCanBeReusedElsewhere`）。
// 校验失败一层都不封：那只说明图还没装配完，补上缺的那块再校验正是要支持的用法。
//
// id 是**定义**身份（`New` 的 graphID，必填）：不同业务用不同图组装（node 可跨图复用），
// 观测靠它区分「运行的是哪张图」（`pulse.AttrGraph`）。**一次运行**的身份是
// `Execution.ID()`（观测里是 `pulse.AttrExecution`）——同一张图跑两次的记录靠它才分得开，
// 只看图 id 与路径会把两次运行折叠成一份。
type Graph struct {
	id string
	// desc 是 New 时给的说明（`""` = 没写）。只由构造函数写入，没有 setter。
	desc string

	keys     keyRegistry
	producer map[string]source // key → 来源（外部输入端口或某个节点）；每种来源至多一个。见 source.go

	// inPorts / outPorts 是**声明**的端口表（名字 → 声明）。端口是静态接口：入口声明过
	// 之后那条 `Requires` 不再算「无来源」（见 DeclareInput）；出口是组合这张图时 `Out`
	// 允许绑定的那批（见 DeclareOutput）。两张表都由声明写入，**封口后不再变**。
	inPorts  map[string]keyRef
	outPorts map[string]keyRef

	nodes []*Node

	// aspects / observer / maxRunning 是这张图**自己声明**的默认运行策略。
	// 它们只是声明：某一次运行的**有效值**在创建 `Execution` 时算出来
	// （本次运行的 ExecOption > 这里的声明 > 父执行环境），所以同一张定义被两处挂载
	// 不会再互相踩——旧版本是运行期把这些值回写进子图字段，见 sub.go 的历史注释。
	aspects []Aspect
	// aspectsCut 表示这一层声明了 `WithoutAspects`：不继承父执行环境那批（自己声明的照挂）。
	aspectsCut bool
	observer   Observer
	// maxRunning 是自己声明的名额（`<=0` = 这一子树不限）；limSet 记录「声明过」。
	// 没声明时继承父执行环境那一份（同一棵执行树共用一个计数）。
	maxRunning int
	limSet     bool

	mu sync.Mutex
	// sealed 表示这张**定义**已经封口：`Validate()` 通过过一次，或挂着它的某次运行
	// `Start()` 成功过。此后 `Add` / `DeclareInput` / `DeclareOutput` / 挂子图都被拒
	// （`ErrGraphStarted` 的第二个触发点，见 errSealed）。校验**失败**不封口——那只说明
	// 图还没装配完，补上缺的那块再校验正是要支持的用法。
	//
	// 封口是**整棵子树**的事：`Validate()` 通过 / `Start()` 成功时连同它挂着的子图一起封
	// （见 Validate / sealTreeLocked）——子图是这张定义的一部分，只封自己那句话就成了
	// 半句。反过来不成立：子图自己 `Validate()` 通过只封它自己与它的子树，不回头封父图。
	sealed bool
}

// Option 配置 Graph 的**定义**（默认运行策略与身份），在 `New` 时给。
//
// 与 `ExecOption` 的分工：`With*` 一族是**这张定义的声明**，每次运行都生效；
// `WithExec*` 一族是**某一次运行**的覆盖，只作用于那一次。优先级：
// 本次运行 > 定义声明 > 父执行环境。
type Option func(*Graph)

// WithMaxRunning 声明**这棵图树里**同时在 `Run` 里**干活**的节点数上限（默认无限）。
//
// 名额是**执行树**的属性：没声明的层继承父执行环境那一份（同一份计数），所以「一棵图树里
// 同时有几个节点在干活」由最外层声明的那一次决定。子图自己声明 `WithMaxRunning(m)`
// 不是「在父表之上再卡一道更紧的上限」，而是**这一支退出父表、自己另算**（`m<=0` = 这一子树
// 不限，同样是退出）。所以父图 `n=1`、子图 `m=2` 时，这一支内部仍能同时跑 2 个——整棵树的
// 瞬时并发可以是 1 + 2。要的是「硬上限」就得让子节点同时占用父表与自己的表，那是另一套
// 语义，本版不做。
//
// 父图的节点与子图的节点是**同一份额度里的竞争者，互不为对方让路**：额度够就并发，
// 不够才排队（实测同一张图：额度 4 时父图 1 个节点 + 子图 2 个节点一起跑完 201ms；
// 额度 2 时同样三个活儿要 402ms 分两批）。额度管的是「干活」——观察者眼里 `running`
// 的行数可以比 n 多，多出来的就是各层的挂子图步骤在等子图（那不是并发度超了）。
//
// 与流式糖的关系：流式校验按**有效容量**判（继承来的也算），所以「这一层的流节点数
// 超过容量」会在那一层 `Start` 就被拒。但那只是**必要条件**，不是「运行时一定拿得
// 到」的保证：引擎的名额是逐个申请的、没有 gang 调度，别的节点（同图的，或祖先的）
// 占着名额时，满足容量的流照样可能等不到——那条路只有等到取消/超时。
//
// 等数据不占名额；排队等名额期间 ctx 取消同样打断（节点不进入 Run，
// 终态为 canceled），不会把名额交给一个已取消的节点。
//
// 某一次运行想改这个上限，用 `WithExecMaxRunning`。
func WithMaxRunning(n int) Option {
	return func(g *Graph) {
		g.maxRunning = n
		g.limSet = true
	}
}

// WithAspects 声明**图级**切面：这一层的每个节点都被它包住（先于节点自己的切面，
// 外层先跑），并且会**继承给子图**——挂子图那一步跑子图时把这批**前置**到子图的切面链上，
// 子图自己声明的那批排在其后（**叠加**，不是替换）。不想要就 `WithoutAspects()`。
//
// **图级切面慎用，尽量用节点级切面。** 它一次落在这一层**所有**节点上，包括挂子图
// 那一步（它背后是一整张子图），而且还会继续往子图里的每个节点上叠——量级不同的
// 东西被同一个参数管住，很容易跑出不在预期的行为。`Timeout` 是最容易被咬到的那个：
// 一个图级 `Timeout(d)` 同时是「父图普通节点各自的时限」「挂子图那一步（= 整张子图）
// 的时限」「子图里每个节点的时限」，而**子图天生比普通节点更容易超时**。要限时就写在
// 该限时的那一个节点上：`NewNode(..., pulse.Timeout(...))`；整张子图限时就用挂子图那
// 一步的 `aspects`（`pulse.Timeout(...)`）。图级留给「全树都要一致施加」的那类（追踪 / 指标），
// 并且心里清楚它会往下叠。
//
// 两条容易算错的账，写在这里省得再推一遍：
//
//  1. **子图的「总时间」先被挂子图那一步上的 `d` 掐死**，不会变成「每个节点各 `d`、加起来
//     `N*d`」——继承下来的那个 `d` 虽然套在子图每个节点上，但它只是更外层那个 `d`
//     里面的一段；
//  2. 名额紧时，**超时可能发生在子图拿到名额之前**：挂子图那一步不吃名额，可它的 `d`
//     从进入 `Run` 就开始计时，而此刻子图的节点还在排队等额度。
//
// 零参调用 = **没声明**（照旧继承）；`WithAspects(list...)` 传进空切片也是没声明。
//
// 某一次运行想追加，用 `WithExecAspects`（追加在声明之后，更靠内）。
func WithAspects(as ...Aspect) Option {
	return func(g *Graph) { g.aspects = append(g.aspects, as...) }
}

// WithoutAspects 声明**这一层不继承**父执行环境的图级切面（自己 `WithAspects` 写的照挂；
// 只写它 = 这一层什么图级切面都不要）。
//
// 它只管「继承到子图节点上」这一段：父图那一步**仍然**被父图的图级切面包着
// ——那是父图对自己节点做的事，不是继承。所以「这棵子树要长跑」不能只靠它，得让父图
// 别把那个切面挂在图级（见 `WithAspects` 的说明）。
func WithoutAspects() Option {
	return func(g *Graph) { g.aspectsCut = true }
}

// New 构造一张空图（**定义**，不带任何一次运行的东西）。
//
// graphID 是**定义**身份（必填，空串报错）：观测记录靠它区分「运行的是哪张图」。
// graphID 不做唯一性约束：同 id 的图并存（含并发）时观测记录同键，需要区分请用不同
// id——同一张图**多次运行**的区分不靠它，靠 `Execution.ID()`。
//
// ctx 不在这里：一次运行在谁的取消域里跑，是 `Run` / `NewExecution` 那一刻决定的
// （同一个执行上下文驱动这张图的这一次运行，以及它下面每一层子执行）。
//
// `description` 是人可读的说明（`""` = 没写，合法）：不参与任何语义、校验与拓扑结果，
// 只出现在 `Topology()` / 快照这类**给人看**的出口上；入图后不再变（没有 setter）。
// **它与 `graphID` 都是 string，写反了照样编译**——而写反的后果比节点那处更重：观测记录
// 里的 `pulse.graph` 带的正是图 id，写反会让归因整片错（同样只有使用风险，测试兜不住
// 调用点）。见 `NewNode` 的同一段说明。
func New(graphID, description string, opts ...Option) (*Graph, error) {
	if graphID == "" {
		return nil, errors.New("pulse: graph id is required (observe instance identity)")
	}
	g := &Graph{
		id:       graphID,
		desc:     description,
		producer: make(map[string]source),
	}
	for _, o := range opts {
		o(g)
	}
	return g, nil
}

// ID 返回图身份（New 的 graphID）——**定义**身份，不是某一次运行的。
func (g *Graph) ID() string { return g.id }

// Description 是 New 时给的说明（`""` = 没写）。
func (g *Graph) Description() string { return g.desc }

// assemblyMu 是**装配锁**：所有「改这张图的定义」的入口（`Add` / `DeclareInput` /
// `DeclareOutput` / `Sub`）与「判完整棵树再封口」的入口（`Validate` / `Execution.Start`）共用它。
//
// 两件事各自都要它，合起来才成立：
//
//  1. **组合环是两张图各自动作的合成结果**：A 里挂 B 与 B 里挂 A 各看各的都对，只有放在
//     一起才成环。单看任何一张图的锁都拦不住——两个 goroutine 各自先查过「没有环」再各挂
//     一步，环就进去了，此后递归展开是爆栈或互等锁。
//  2. **「判完整棵树」与「封上这棵树」是两个动作**。判的每一层各自取放锁，两趟之间那些锁
//     是空的；窗口里别人改一下子图，封住的就成了**没校验过**的那份——`Validate()` 返回 nil
//     却封了一张跑不起来的定义，既改不了，`Start()` 又会重新报错（实测：窗口里给子图加一个
//     「要一条没人提供的输入」的节点，`Validate()` 返回 nil，而封住的子图带 1 条毛病）。
//     把「改定义」与「判 + 封」放进同一个临界区，封上的就一定是刚判过的那份。
//
// 装配是一次性动作、临界区里没有用户代码（跑图的 goroutine 在放锁之后才建），代价是零；
// 运行期不碰它。锁序**只有一种**：先 `assemblyMu`，再图自己的锁（`g.mu`），从不反过来
// ——所以它不是「另一把图锁」，而是压在所有图锁之上的一道装订线。
var assemblyMu sync.Mutex

// Add 登记节点。定义封口后拒绝（见 errSealed）。
//
// 两段式：先全量校验（只读），全部通过才提交。半途失败必须什么都不留——
// 否则失败那次 Add 的 Provides 会占住 producer，真正提供该 Key 的节点此后
// 一直吃 ErrDuplicateSource，等它的节点则永远停在 pending。
func (g *Graph) Add(n *Node) error {
	if n == nil {
		return fmt.Errorf("pulse: nil node")
	}
	return g.addAll([]*Node{n})
}

// addAll 批量登记节点，**整批 all-or-nothing**：先对「现有图 + 本批次」全量
// 校验，再一次性提交。任一节点不过，图里不留这一批的任何痕迹（producer / 节点表都不动）
// ——装配糖（FanOut）要的正是这条：它一次装 N 个 worker，返回错误时不能留下半批节点在跑。
//
// Add 是它的单元素退化情形，判据与错误文案同源，所以两者行为一致。
//
// 两个入口分了名字：`addAll` 自己取装配锁（给包外与包内的一次性调用），`addAllLocked`
// 假定调用方已经持着它（`Sub` 就是这样——它必须在同一个临界区里做完「查环 + 接线 + 挂上」，
// 见 assemblyMu）。
func (g *Graph) addAll(nodes []*Node) error {
	assemblyMu.Lock()
	defer assemblyMu.Unlock()
	return g.addAllLocked(nodes)
}

// addAllLocked 是 addAll 的本体：**调用方持 assemblyMu**。
func (g *Graph) addAllLocked(nodes []*Node) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sealed {
		return errSealed("Add")
	}

	// —— 校验段：只读。批次内先出现的节点先占（同名 Key 同批出现两次也算冲突）——
	batchIDs := make(map[string]struct{}, len(nodes))
	batchClaims := make(map[string]source, len(nodes)) // key → 本批次里声明它的节点
	for _, n := range nodes {
		if n == nil {
			return fmt.Errorf("pulse: nil node")
		}
		if n.id == "" {
			return fmt.Errorf("pulse: empty node id")
		}
		if _, dup := batchIDs[n.id]; dup {
			return fmt.Errorf("pulse: duplicate node id %q", n.id)
		}
		for _, existing := range g.nodes {
			if existing.id == n.id {
				return fmt.Errorf("pulse: duplicate node id %q", n.id)
			}
		}

		seenReq := make(map[string]struct{}, len(n.requires))
		for _, k := range n.requires {
			if _, ok := seenReq[k.name]; ok {
				return fmt.Errorf("pulse: node %s declares %q twice in Requires", n.id, k.name)
			}
			seenReq[k.name] = struct{}{}
			if err := g.keys.check(k); err != nil {
				return err
			}
		}
		seenProv := make(map[string]struct{}, len(n.provides))
		for _, k := range n.provides {
			if _, ok := seenReq[k.name]; ok {
				return fmt.Errorf("pulse: node %s both requires and provides %q", n.id, k.name)
			}
			if _, ok := seenProv[k.name]; ok {
				return fmt.Errorf("pulse: node %s declares %q twice in Provides", n.id, k.name)
			}
			seenProv[k.name] = struct{}{}
			if err := g.keys.check(k); err != nil {
				return err
			}
			claim := nodeSource(n.id)
			if prev, ok := batchClaims[k.name]; ok && !prev.same(claim) {
				return fmt.Errorf("%w: %q already sourced by %s", ErrDuplicateSource, k.name, prev)
			}
			if err := g.sourceConflict(k.name, claim); err != nil {
				return err
			}
			batchClaims[k.name] = claim
		}
		batchIDs[n.id] = struct{}{}
	}

	// —— 提交段：到这一步不会再失败 ——
	for _, n := range nodes {
		for _, k := range n.requires {
			_ = g.keys.register(k)
		}
		for _, k := range n.provides {
			_ = g.keys.register(k)
			_ = g.claimSource(k.name, nodeSource(n.id))
		}
		g.nodes = append(g.nodes, n)
	}
	return nil
}

// claimSource 保证每个 Key 只有一种来源：一个声明过的输入端口，或恰好一个节点。
// 来源按**结构化**比较（见 source.same），不按字符串——一个真叫 `seed` 的节点
// 与外部来源是两种来源，撞不出「同源」。
func (g *Graph) claimSource(name string, owner source) error {
	if err := g.sourceConflict(name, owner); err != nil {
		return err
	}
	g.producer[name] = owner
	return nil
}

// sourceConflict 是 claimSource 的**只读预演**：判据与错误文案同源，但不写表。
func (g *Graph) sourceConflict(name string, owner source) error {
	prev, ok := g.producer[name]
	if !ok || !prev.present() || prev.same(owner) {
		return nil // 表里没有、或落了个零值（按没有来源处理）、或本来就是同一个来源
	}
	return fmt.Errorf("%w: %q already sourced by %s", ErrDuplicateSource, name, prev)
}

// Validate 校验这张图跑不跑得起来：判据与 `Execution.Start` 完全同一套（每个
// `Requires` 都有来源 / 声明的出口都有人产出 / 依赖无环 / 流式名额与出口）。
//
// **通过即封口，封的是整棵子树**：这张定义连同它挂着的子图从此都不再变化——`Add` /
// `DeclareInput` / `DeclareOutput` / 挂子图都被拒（`ErrGraphStarted`，见 errSealed）。
// 为什么连子图一起封：挂上去的子图是这张定义的一部分，只封父图的话，子图还能接着 `Add`，
// 刚校验过的那棵树会变——「通过即封口」就成了半句话。子图**自己**那份定义的封口也不冲突：
// 同一张子图挂多处时，第一处校验通过就把它封上，别处照样挂得进去（挂载不改子图定义，见 Sub）。
//
// 校验**失败不封口，一层都不封**：那只说明图还没装配完，补上缺的那块再校验正是要支持的
// 用法（与 `Start` 失败后仍可再 `Start` 同一条线性化规则）。所以这里是两趟：先判完整棵树，
// 干净了再逐层封（`sealTreeLocked`）——判到一半就封的话，失败的校验会留下封口。
//
// **两趟之间没有窗口**：改定义的入口与这里共用装配锁 `assemblyMu`，所以第二趟（封口）
// 拿到的树一定是第一趟判过的那一棵——不是「大概率没变」，而是「别人改不动」。少了这把锁，
// 窗口里给子图加一个「要一条没人提供的输入」的节点，`Validate()` 会返回 nil 却把带毛病的
// 子图封上（实测），随后既改不了、`Start()` 又重新报错。
//
// 封口之外它仍然是只读的：不建 goroutine、不碰槽位、不产生执行——所以校验完照样能
// `Run` / `NewExecution`（输入是**某一次运行**的值，不属于定义）。想「装配完立刻冻结」
// 就调它一次，不需要另一个 `Seal()`（那与「成功即封口」职责重叠）。
//
// 给**装配期**用：声明式装配（`pulse/yaml`）在装图那一刻就把每张子图的静态
// 毛病报出来，而不是等它第一次运行到才炸——那些毛病里「没人提供、也没声明成端口」
// 和「依赖成环」都是读一遍声明就能定的，留到运行期只会表现成挂死或一句
// 看不出病因的超时。
//
// 已经封口的图直接通过（它在封口那一刻就过了同一套判据，此后定义没变过）。
func (g *Graph) Validate() error {
	// 与 Start 同一条同步边界：判 + 封在装配锁里一次做完（见 assemblyMu 与上面的说明）。
	assemblyMu.Lock()
	defer assemblyMu.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.validateLocked(g.declaredCapacity()); err != nil {
		return err
	}
	g.sealTreeLocked(mountChain{g})
	return nil
}

// sealTreeLocked 把**整棵子树**一起封口（自己 + 挂着的子图，递归）。调用方持 g.mu；
// 往下一层靠 `child.sealTree` 自己取那把锁——与校验那一趟同一个顺序（父 → 子）。
//
// 与校验分两趟（而不是边走边封）：校验**失败**不能留下任何封口，包括子图的。两趟之间
// 逐层重新加锁，但**不存在窗口**——改定义的入口与这里共用装配锁 `assemblyMu`，那一瞬
// 不可能有别的 goroutine 正在装配这棵树里的任何一张图（见 assemblyMu 第 2 条）。
//
// `chain` 上出现过的图不再重复进去（同一张子图挂多处是合法用法）；它同时兜住环——
// 环本不该存在（见 mountChain），这里保证递归一定收敛。
func (g *Graph) sealTreeLocked(chain mountChain) {
	g.sealed = true
	for _, n := range g.nodes {
		if n.child == nil || chain.has(n.child) {
			continue
		}
		n.child.sealTree(chain.push(n.child))
	}
}

// sealTree 是 sealTreeLocked 的自加锁入口（收口：谁持锁进哪个函数写在名字上）。
func (g *Graph) sealTree(chain mountChain) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sealTreeLocked(chain)
}

// validateLocked 是 `Validate` 与 `Execution.Start` 共用的那一套判据：**一份判据，两个入口**，
// 所以「装配期提前报的那句」与「启动时拒的那句」逐字相同。调用方持 g.mu。
//
// `capacity` 是**这一层**要用哪个名额上限去判流式（0 = 不限）。两个入口给的不是同一个值，
// 而且这是有意的：`Validate()` 只有定义，给 `declaredCapacity()`；某次运行 `Start()` 给
// **执行的有效名额**（本次运行可能 `WithExecMaxRunning` 改过、也可能继承父执行环境那一份）。
// 「这一层的流节点数超过容量」正是那种「运行时表现为挂死」的病，判在真正要跑的那一刻最准。
//
// 判据本体在 problemsLocked（problems.go）：它一次给全，`Topology().Problems`
// 拿的就是它；这里只取**第一条**，维持 `Validate` / `Start` 报首错的既有形状
// 与文案（顺序也固定为来源 → 依赖环 → 流式，与旧实现一致）。
func (g *Graph) validateLocked(capacity int) error {
	if p := g.problemsLocked(capacity); len(p) > 0 {
		return p[0]
	}
	return nil
}

// declaredCapacity 是这张**定义自己声明**的名额上限（0 = 没声明 / 不限）。
//
// 装配期（`Validate` / `Topology`）只能知道这个：继承是运行期的事，那时还没有执行树。
// 运行期问执行要（`Execution` 的有效名额），见 validateLocked。
func (g *Graph) declaredCapacity() int {
	if !g.limSet || g.maxRunning <= 0 {
		return 0
	}
	return g.maxRunning
}

func isSkipped(err error) bool {
	return errors.Is(err, ErrSkipped)
}

// isCanceled 判「这一轮被从外面拆了」：主动取消与父 ctx 的截止时间到期同属
// 一类——对节点而言两者都是「不是我算错了」，在观测里都该是 canceled 而不是
// failed（宿主按 reason 分流，把一个没有 bug 的节点报成失败会指错方向）。
//
// 节点自己的 Timeout 不走这里：它返回一句带节点名与时限的 timeout 错误，
// 归 failed（那是这个节点没在时限内完成，是它的失败）。
func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
