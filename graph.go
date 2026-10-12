package pulse

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Graph 今天同时戴着两顶帽子：**定义**（节点集合 / 来源表 / Key 登记 / 端口）与**这一次运行**
// （ctx / 取消 / 槽位 / started / claimed / 首错 / waitgroup）。
//
// **目标模型（#311）把它们拆开**：`Graph` = 可复用的**静态定义**，一次运行是 `Execution`
// （自己的 ctx / 输入 / 槽位 / 节点状态 / 首错 / 取消域）。阶段 1（静态组合）已落地，
// `Execution` 还没拆，所以今天有一条必须记住的推论：
//
//	**一个 `Graph` 实例在一次运行里只能产生一个运行实例。**
//
// 同一张子图挂两处时，跑的那一步会原子认领它（`claimSub`），另一处拿到的是「这张图已经在用」
// 那句；`Start()` 成功过一次的图也不能重跑（`ErrGraphStarted`）。要「每个挂载点各跑一份」得等
// 阶段 2 的 `Execution`——**在那之前别往 `Graph` 上继续堆运行状态**：新加的运行期字段都属于
// 未来的 `Execution`（见 docs/design/object-model.md 第 3.2 节与 AGENTS.md 的 one-shot 那条）。
//
// 另一条与「定义」有关、最容易被记反的边界：`Validate()` 通过 / `Start()` 成功会把**整棵子树**
// 的定义封口（此后 `Add` / `DeclareInput` / `DeclareOutput` / `Sub` 都被拒），但**封口不等于
// 用过它**——一张已封口的子图照样能被别处挂载，并在那一处（如果还没跑过）正常跑起来。
// 前者看 `sealed`，后者看 `claimed` / `started`，两个问题、两套判据（用例
// `TestSealedChildCanBeReusedElsewhere`）。
//
// id 是图身份（New 的 graphID，必填）：不同业务用不同图组装（node 可跨图复用），Observer 回调
// 随事实携带它，供观测折叠区分「运行的是哪张图」（观测 key 见 pulse.AttrGraph）。
type Graph struct {
	id string
	// desc 是 New 时给的说明（`""` = 没写）。只由构造函数写入，没有 setter。
	desc   string
	ctx    context.Context
	cancel context.CancelFunc

	keys     keyRegistry
	producer map[string]source // key → 来源（外部 seed 或某个节点）；每种来源至多一个。见 source.go
	slots    map[string]*slot
	slotsMu  sync.Mutex

	// inPorts / outPorts 是**声明**的端口表（名字 → 声明）。端口是静态接口：入口声明过
	// 之后那条 `Requires` 不再算「无来源」（见 DeclareInput）；出口是组合这张图时 `Out`
	// 允许绑定的那批（见 DeclareOutput）。两张表都由声明写入，**封口后不再变**。
	inPorts  map[string]keyRef
	outPorts map[string]keyRef

	nodes []*Node
	// aspects 是这张图的**有效**图级切面（继承来的排在前面）。见 WithAspects。
	aspects []Aspect
	// aspectsCut 表示这一层声明了 `WithoutAspects`：不继承父图那批（自己声明的照挂）。
	// 只在运行期被读一次，见 runSub 的切面继承点。
	aspectsCut bool
	observer   Observer
	// lim 是这张图的**有效**名额表（nil = 无限）。默认自己一份；作为子图被挂载起来
	// 跑起来、而自己没声明过 WithMaxRunning 时，继承父图那一份（同一份计数）——
	// 于是「一棵图树同时有几个节点在干活」由根图决定。见 limiter。
	lim *limiter
	// limSet 记录这张图**自己**声明过名额。声明了就用自己的、不再继承；
	// n<=0 也算声明（那是「这一子树不限额」的口子）。
	limSet  bool
	mu      sync.Mutex
	started bool
	// sealed 表示这张**定义**已经封口：`Validate()` 通过过一次，或这张图 `Start()` 成功过。
	// 此后 `Add` / `DeclareInput` / `DeclareOutput` / 挂子图都被拒（`ErrGraphStarted` 的第二个
	// 触发点，见 errSealed）。校验**失败**不封口——那只说明图还没装配完，补上缺的那块再校验
	// 正是要支持的用法。
	//
	// 封口是**整棵子树**的事：`Validate()` 通过 / `Start()` 成功时连同它挂着的子图一起封
	// （见 Validate / Start / sealTreeLocked）——子图是这张定义的一部分，只封自己那句话就成了
	// 半句。反过来不成立：子图自己 `Validate()` 通过只封它自己与它的子树，不回头封父图。
	sealed bool
	// claimed 是「这张图已经被一次挂子图运行认领」。与 started 分开是因为认领必须
	// 早于挂载对它的任何改动（挂 observer、继承名额与图级切面、种值），而 started 要到
	// Start 才置上——两者之间那一整段，并发的两次挂载会一起写同一批字段。
	claimed bool
	done    bool // Wait 返回过：此后 cancel 只是收尾，不再算运行结果
	err     error
	wg      sync.WaitGroup
	// finishOnce 让「发一次 OnGraphFinished」与「所有 Wait 都等到它落地」同时
	// 成立：sync.Once 会阻塞后来的调用者直到第一个调用返回（比自己的 done
	// 门闩强——那个只保证不重发，先返回的 Wait 会把出口先关掉）。
	finishOnce sync.Once
}

// Option 配置 Graph。
type Option func(*Graph)

// WithMaxRunning 限制**这棵图树里**同时在 Run 里**干活**的节点数。n<=0 表示无限
// （默认）。
//
// 名额是**整棵树共享一份计数**（除非哪一层自己声明了，见下）：作为子图跑起来、且自己
// 没声明过 WithMaxRunning 时，它继承父图那一份；挂子图那一步**不吃名额**（它整段都在
// 等子图跑完，占着就等于把父侧的额度锁在「等」上）。所以**在没有人自己声明过的图树
// 里**，「根图声明 n」= 整棵树同时最多 n 个节点在干活。
//
// 子图自己声明 `WithMaxRunning(m)` 不是「在父表之上再卡一道更紧的上限」，而是**这一支
// 退出父表、自己另算**（`m<=0` = 这一子树不限，同样是退出）。所以父图 `n=1`、子图
// `m=2` 时，这一支内部仍能同时跑 2 个——整棵树的瞬时并发可以是 1 + 2。要的是「硬
// 上限」就得让子节点同时占用父表与自己的表，那是另一套语义，本版不做。
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
func WithMaxRunning(n int) Option {
	return func(g *Graph) {
		g.lim = newLimiter(n)
		g.limSet = true
	}
}

// WithAspects 安装**图级**切面：这一层的每个节点都被它包住（先于节点自己的切面，
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
func WithAspects(as ...Aspect) Option {
	return func(g *Graph) { g.aspects = append(g.aspects, as...) }
}

// WithoutAspects 声明**这一层不继承**父图的图级切面（自己 `WithAspects` 写的照挂；
// 只写它 = 这一层什么图级切面都不要）。
//
// 它只管「继承到子图节点上」这一段：父图那一步**仍然**被父图的图级切面包着
// ——那是父图对自己节点做的事，不是继承。所以「这棵子树要长跑」不能只靠它，得让父图
// 别把那个切面挂在图级（见 `WithAspects` 的说明）。
func WithoutAspects() Option {
	return func(g *Graph) { g.aspectsCut = true }
}

// New 构造空图。graphID 是图身份（必填，空串报错）：观测记录靠它
// 区分「运行的是哪张图」。graphID 不做唯一性约束：同 id 的图并存
// （含并发）时观测记录同键，需要区分请用不同 id。ctx 取消会打断
// 所有等待。
//
// `description` 是人可读的说明（`""` = 没写，合法）：不参与任何语义、校验与拓扑结果，
// 只出现在 `Topology()` / `Snapshot()` 这类**给人看**的出口上；入图后不再变（没有
// setter）。**它与 `graphID` 都是 string，写反了照样编译**——而写反的后果比节点那处
// 更重：观测记录里的 `pulse.graph` 带的正是图 id，写反会让归因整片错（同样只有
// 使用风险，测试兜不住调用点）。见 `NewNode` 的同一段说明。
func New(ctx context.Context, graphID, description string, opts ...Option) (*Graph, error) {
	if graphID == "" {
		return nil, errors.New("pulse: graph id is required (observe instance identity)")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c, cancel := context.WithCancel(ctx)
	g := &Graph{
		id:       graphID,
		desc:     description,
		ctx:      c,
		cancel:   cancel,
		slots:    make(map[string]*slot),
		producer: make(map[string]source),
	}
	for _, o := range opts {
		o(g)
	}
	return g, nil
}

// ID 返回图身份（New 的 graphID）。
func (g *Graph) ID() string { return g.id }

// Description 是 New 时给的说明（`""` = 没写）。
func (g *Graph) Description() string { return g.desc }

// claimSub 把这张图**原子**地认领给一次挂子图运行：
// 返回 false 表示它已经用过（被别处认领过，或已经启动过）。供挂子图那一步在跑子图之前
// 拦「复用同一张图」。
//
// 为什么不是「看一眼 started」：看一眼与随后的写（挂载要挂 observer、继承名额与
// 图级切面、再种值）之间没有原子性。两个并发的挂载复用同一张尚未启动的图时，两边都会看到
// 「没启动」，然后一起写同一批字段——`-race` 实测就是一处 DATA RACE。认领放在
// 所有改动**之前**，抢不到的一方当场拿到 `ErrGraphStarted` 走人：既不再碰这张图，
// 也照样拿得到那句「怎么改」的翻译。
//
// 认领**比 started 严**：它是一次性消费的标记——图交出去一次就用掉了，即使这一轮
// 因为校验没过而没真正启动，也不还回来。一次性契约下正确写法本来就是「一图一处」
// （挂进父图的那张只能跑一处），留一个「失败了还能拿回来重来」的缝，只会让复用更难被发现。
//
// 实现上它取代了早先那个只读的 `isStarted()`：只读一眼拦不住并发，而认领顺带
// 覆盖了「已经启动过」那一半（`g.started`）。
func (g *Graph) claimSub() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.claimed || g.started {
		return false
	}
	g.claimed = true
	return true
}

// assemblyMu 是**装配锁**：所有「改这张图的定义」的入口（`Add` / `DeclareInput` /
// `DeclareOutput` / `Sub`）与「判完整棵树再封口」的入口（`Validate` / `Start`）共用它。
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
// 运行期不碰它。锁序**只有一种**：先 `assemblyMu`，再图自己的锁（`g.mu` / `g.slotsMu`），
// 从不反过来——所以它不是「另一把图锁」，而是压在所有图锁之上的一道装订线。
var assemblyMu sync.Mutex

// Add 登记节点。图启动后拒绝。
//
// 两段式：先全量校验（只读），全部通过才提交。半途失败必须什么都不留——
// 否则失败那次 Add 的 Provides 会占住 producer，真正提供该 Key 的节点此后
// 一直吃 ErrDuplicateSource，等它的节点则永远停在 pending（父 ctx 不取消时
// Run 不返回）。
func (g *Graph) Add(n *Node) error {
	if n == nil {
		return fmt.Errorf("pulse: nil node")
	}
	return g.addAll([]*Node{n})
}

// addAll 批量登记节点，**整批 all-or-nothing**：先对「现有图 + 本批次」全量
// 校验，再一次性提交。任一节点不过，图里不留这一批的任何痕迹（producer / 槽位 /
// 节点表都不动）——装配糖（FanOut）要的正是这条：它一次装 N 个 worker，返回
// 错误时不能留下半批节点在跑。
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
	if g.started {
		return ErrGraphStarted
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
			g.slotOfLocked(k)
		}
		for _, k := range n.provides {
			_ = g.keys.register(k)
			_ = g.claimSource(k.name, nodeSource(n.id))
			g.slotOfLocked(k)
		}
		g.nodes = append(g.nodes, n)
	}
	return nil
}

// Seed 在运行前写入初始值（幂等首写）。
func Seed[T any](g *Graph, k Key[T], v T) error {
	return g.seedRef(k.asRef(), v, false)
}

// SkipSeed 在运行前将某 Key 标为跳过。
func SkipSeed[T any](g *Graph, k Key[T]) error {
	return g.seedRef(k.asRef(), nil, true)
}

// seedRef 是 Seed / SkipSeed / SeedByName 的共用路径。与 Add 同一条纪律：
// 先只读校验，再提交——失败的 Seed 不该在图里留下登记表或来源占位。
func (g *Graph) seedRef(ref keyRef, v any, skip bool) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started {
		return ErrGraphStarted
	}
	if err := g.keys.check(ref); err != nil {
		return err
	}
	if err := g.sourceConflict(ref.name, seedSource); err != nil {
		return err
	}
	_ = g.keys.register(ref)
	_ = g.claimSource(ref.name, seedSource)
	if skip {
		return g.slotOfLocked(ref).resolveSkip()
	}
	return g.slotOfLocked(ref).resolveValue(v)
}

// claimSource 保证每个 Key 只有一种来源：外部 Seed/SkipSeed，或恰好一个节点。
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

func (g *Graph) slotOf(k keyRef) *slot {
	g.slotsMu.Lock()
	defer g.slotsMu.Unlock()
	return g.slotOfLocked(k)
}

func (g *Graph) slotOfLocked(k keyRef) *slot {
	if s, ok := g.slots[k.name]; ok {
		return s
	}
	s := newSlot()
	g.slots[k.name] = s
	return s
}

// Run 提交全部节点并阻塞到全部终止。返回首错或 ctx 取消；不含 ErrSkipped。
func (g *Graph) Run() error {
	if err := g.Start(); err != nil {
		return err
	}
	return g.Wait()
}

// Start 异步提交全部节点。
//
// 提交前做一次**只读校验**（与 `Validate` 同一套判据：装配期可以先问一遍，
// 别等启动时才知道），两条判据都能在这一刻静态判定：
//
//   - 每个 `Requires` 都必须有来源（外部 `Seed`/`SkipSeed`，或某个节点的
//     `Provides`）。没有来源的槽永远不会被写入；
//   - 依赖关系**无环**。有来源不等于能满足：环里每条 `Requires` 都有生产者，
//     但没有任何节点能先进入 `Run`（门要等全部输入到达），所有槽永远停在
//     pending。
//
// 两条都是「运行时表现为挂死」的事：有 deadline 时是一句看不出病因的超时，
// 没有时进程会被 runtime 判为 fatal deadlock。校验不过时图**仍未启动**
// （`started` 保持 false）：补上生产者 / Seed 之后可以重新 `Start`；含环的图
// 则要改装配——引擎没有 Remove，环只能靠**重新装一张图**消除。
//
// **通过校验的启动会把整棵子树封口**（与 `Validate` 同一条规则，见那里）：挂着的子图
// 也从此不改。少了这一步，持有子图指针的调用方能在父图已经跑起来、子图还没开始之间改它
// ——`Add` / `DeclareInput` 都进得去（实测），真正跑的就不是观察与拓扑看到的那张图了。
//
// 通过校验后发出一次 `Observer.OnGraphStarted`——在提交**任何**节点 goroutine
// 之前，所以它先于本轮的全部节点事件。空图也发（它的两条件同样齐）。
//
// WaitGroup 的计数在**放锁之前**登记，且把 started 那一发自身也计进去
// （`+1`）：另一 goroutine 只要看见 `started == true`，就一定看见非零计数——
// 否则它会在节点还没提交时就从 `wg.Wait()` 返回、提前发出 finished（慢的
// started 回调把窗口拉到几十毫秒量级，这不是理论竞态）。
func (g *Graph) Start() error {
	// 校验与封口在**装配锁**里做完，之后才放出去发事件、建 goroutine（临界区里不跑用户代码）：
	// 与 Validate 同一条边界，见 assemblyMu。
	assemblyMu.Lock()
	g.mu.Lock()
	if g.started {
		g.mu.Unlock()
		assemblyMu.Unlock()
		return ErrGraphStarted
	}
	if err := g.validateLocked(); err != nil {
		g.mu.Unlock()
		assemblyMu.Unlock()
		return err
	}
	// 通过校验地启动 = 这棵树的定义从此不再变化（**含挂着的子图**）：跑起来之后定义还在变，
	// 观测与拓扑看到的那张图、真正在跑的那张图就成了两张。封口放在校验**之后**，所以
	// 启动被拒的那次一层都不封——补上缺的那块重新 Start 正是要支持的用法（见 Validate）。
	g.sealTreeLocked(mountChain{g})
	g.started = true
	nodes := append([]*Node(nil), g.nodes...)
	g.wg.Add(len(nodes) + 1) // +1：started 那一发落地前，其他 goroutine 的 Wait 不该溜过去
	g.mu.Unlock()
	assemblyMu.Unlock()

	g.notify(func(o Observer) { o.OnGraphStarted(g.id) })
	g.wg.Done()

	for _, n := range nodes {
		n := n
		go g.runNode(n)
	}
	return nil
}

// Validate 校验这张图跑不跑得起来：判据与 Start 完全同一套（每个
// `Requires` 都有来源 / 声明的出口都有人产出 / 依赖无环 / 流式名额与出口）。
//
// **通过即封口，封的是整棵子树**：这张定义连同它挂着的子图从此都不再变化——`Add` /
// `DeclareInput` / `DeclareOutput` / 挂子图都被拒（`ErrGraphStarted`，见 errSealed）。
// 为什么连子图一起封：挂上去的子图是这张定义的一部分，只封父图的话，子图还能接着 `Add`，
// 刚校验过的那棵树会变——「通过即封口」就成了半句话。子图**自己**那份定义的封口也不冲突：
// 同一张子图挂多处时，第一处校验通过就把它封上，别处照样挂得进去（挂载不改子图定义，见 Sub）。
//
// 校验**失败不封口，一层都不封**：那只说明图还没装配完，补上缺的那块再校验正是要支持的
// 用法（与 `Start` 失败后仍可 `Start` 同一条线性化规则）。所以这里是两趟：先判完整棵树，
// 干净了再逐层封（`sealTreeLocked`）——判到一半就封的话，失败的校验会留下封口。
//
// **两趟之间没有窗口**：改定义的入口与这里共用装配锁 `assemblyMu`，所以第二趟（封口）
// 拿到的树一定是第一趟判过的那一棵——不是「大概率没变」，而是「别人改不动」。少了这把锁，
// 窗口里给子图加一个「要一条没人提供的输入」的节点，`Validate()` 会返回 nil 却把带毛病的
// 子图封上（实测），随后既改不了、`Start()` 又重新报错。
//
// 封口之外它仍然是只读的：不置 `started`、不建 goroutine、不碰槽位，所以校验完照样能
// 接着 `Seed` / `Start`——`Seed` 是**本次运行**的值，不属于定义。想「装配完立刻冻结」
// 就调它一次，不需要另一个 `Seal()`（那与「成功即封口」职责重叠）。
//
// 给**装配期**用：声明式装配（`pulse/yaml`）在装图那一刻就把每张子图的静态
// 毛病报出来，而不是等它第一次运行到才炸——那些毛病里「没人提供、没人 seed」
// 和「依赖成环」都是读一遍声明就能定的，留到运行期只会表现成挂死或一句
// 看不出病因的超时。
//
// 已经启动的图直接通过：它在 `Start` 那一刻就过了同一套判据（并封了整棵树），此后
// `Add` / `Seed` 都不再被接受，图的内容没变过。
func (g *Graph) Validate() error {
	// 与 Start 同一条同步边界：判 + 封在装配锁里一次做完（见 assemblyMu 与上面的说明）。
	assemblyMu.Lock()
	defer assemblyMu.Unlock()
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.validateLocked(); err != nil {
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

// validateLocked 是 Start 与 Validate 共用的那一套判据：**一份判据，两个入口**，
// 所以「装配期提前报的那句」与「启动时拒的那句」逐字相同。调用方持 g.mu。
//
// 判据本体在 problemsLocked（problems.go）：它一次给全，`Topology().Problems`
// 拿的就是它；这里只取**第一条**，维持 `Start` / `Validate` 报首错的既有形状
// 与文案（顺序也固定为来源 → 依赖环 → 流式，与旧实现一致）。
func (g *Graph) validateLocked() error {
	if p := g.problemsLocked(); len(p) > 0 {
		return p[0]
	}
	return nil
}

// Wait 等待 Start 提交的节点全部终止，并释放图自己的 ctx：它到这一步不再
// 挂在父 ctx 的 children 上（`New` 派生的子 ctx 若不 cancel，父 ctx 是
// Background 或长生命周期时会一直持有它）。
//
// 返回前发出一次 `Observer.OnGraphFinished`（reason 取运行终态，err 即本次
// 返回值）——在**全部**节点终止之后，所以它晚于本轮的全部节点事件。
// 只 `Start` 不 `Wait` 的宿主收不到它。
//
// **并发与重复 `Wait` 都在这一发返回之后才返回**（finishOnce）：任何一个
// `Wait` 返回时，本轮的 finished 都已在出口落地——不会出现「先返回的那个把
// 出口收尾了，记录还留在另一个 goroutine 里」。
//
// 回调在 `Start` / `Wait` 的调用路径上同步执行，所以**别在 Observer 回调里调
// 本图的 `Start` / `Wait`**：那是同一个 goroutine 等自己，会死等。
func (g *Graph) Wait() error {
	g.mu.Lock()
	started := g.started
	g.mu.Unlock()
	if !started {
		return ErrGraphNotStarted
	}
	g.wg.Wait()
	g.mu.Lock()
	g.done = true // 先定结果，再 cancel：收尾不该被读成运行结果
	g.mu.Unlock()
	err := g.Err()
	g.finishOnce.Do(func() {
		// 图级终态只会是 completed / failed / canceled：跳过是节点级的事实
		// （fail 不收跳过，Err 也不含它），升不到这一层。
		g.notify(func(o Observer) { o.OnGraphFinished(g.id, finishReason(err), err) })
	})
	g.cancel()
	return err
}

// cancelRun 取消这张图的**运行**上下文（`New` 派生出来的那一份）。包内入口，给静态组合
// 转发父侧的取消用（见 runStaticSub 第 4 步）：静态子图那份 ctx 是它建图时带进来的，
// 父节点这一层的取消只能靠转发进去。`sync.Once` 语义由 `context.CancelFunc` 自己保证。
func (g *Graph) cancelRun() { g.cancel() }

// Err 返回首个节点错误或取消原因。不含单纯的跳过。运行结束后
// （Wait 返回过）收尾的那次 cancel 不再算结果——否则干净跑完的图会
// 报 context.Canceled。
func (g *Graph) Err() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return g.err
	}
	if g.done {
		return nil
	}
	return g.ctx.Err()
}

func (g *Graph) fail(err error) {
	if err == nil || isSkipped(err) {
		return
	}
	g.mu.Lock()
	if g.err == nil {
		g.err = err
		g.cancel()
	}
	g.mu.Unlock()
}

// acquire 占用这张图**有效**名额表里的一个名额（nil = 无限，直接放行）。
// 取消语义见 limiter.acquire。
func (g *Graph) acquire(ctx context.Context) error { return g.lim.acquire(ctx) }

// release 归还名额。没占过、或名额表无限，都安全。
func (g *Graph) release() { g.lim.release() }

func (g *Graph) runNode(n *Node) {
	defer g.wg.Done()

	rc := newRunCtx(g, n, g.ctx)
	defer rc.Cancel()

	// 每节点生命周期事件至多一次（防 Retry 双打点）。
	var waited, ran atomic.Bool
	emitWaiting := func() {
		if waited.CompareAndSwap(false, true) {
			g.notify(func(o Observer) { o.OnNodeWaiting(g.id, n.id) })
		}
	}
	emitRunning := func() {
		if ran.CompareAndSwap(false, true) {
			g.notify(func(o Observer) { o.OnNodeRunning(g.id, n.id) })
		}
	}

	// 切面覆盖「等输入 + 执行」整段，这样 Timeout 能打断 Wait。
	// MaxRunning 只在即将执行用户 Run 时占用。
	// 生命周期埋点在 innermost core：Retry 重入时靠门闩只发一次。
	chain := buildChain(append(append([]Aspect{}, g.aspects...), n.aspects...), func(rc *RunCtx) (err error) {
		emitWaiting()
		skipped, err := awaitAll(rc, n.requires)
		if err != nil {
			return err
		}
		if rc.ctx.Err() != nil {
			return rc.ctx.Err()
		}
		// 到几个收几个：**只要有一条输入真的到了值**，就带着到了的那些进入
		// Run；一条值都没到才轮到本节点自己跳过。判据是「有没有值」，不是
		// 「有没有跳过」——上游某一路没有值，不该让手里还有数据的下游跟着停，
		// 也不该让已经到达的值作废。一条 Requires 都没有的源节点不适用此
		// 判据，照常执行。
		if len(n.requires) > 0 && len(skipped) == len(n.requires) {
			return skipErr(skipped...)
		}
		// 挂子图那一步**不吃名额**：它整段（取值 + 子图 + 桥回）都在等子图跑完，
		// 占着名额就等于把父侧额度锁在「等」上——名额共享之后那就是死锁
		// （限额 ≤ 嵌套深度时必现，实测见 .workbase/probe-sharedsem）。
		if !n.sub {
			if err := g.acquire(rc.ctx); err != nil {
				return err
			}
			defer g.release()
		}
		emitRunning()
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("pulse: panic in node %s: %v", n.id, rec)
			}
		}()
		if n.run != nil {
			return n.run(rc)
		}
		return nil
	})

	err := chain(rc)
	if isSkipped(err) {
		g.skipAllOrUnwritten(n, rc, true)
		g.notify(func(o Observer) { o.OnNodeFinished(g.id, n.id, NodeSkipped, err) })
		return
	}
	if err != nil {
		g.fail(err)
		g.skipAllOrUnwritten(n, rc, true)
		g.notify(func(o Observer) { o.OnNodeFinished(g.id, n.id, finishReason(err), err) })
		return
	}
	g.skipAllOrUnwritten(n, rc, false)
	g.notify(func(o Observer) { o.OnNodeFinished(g.id, n.id, NodeCompleted, nil) })
}

func (g *Graph) skipAllOrUnwritten(n *Node, rc *RunCtx, allIfNoneWritten bool) {
	if allIfNoneWritten && len(rc.wrote) == 0 {
		g.skipAll(n.provides)
		return
	}
	g.skipUnwritten(n, rc)
}

func (g *Graph) skipAll(keys []keyRef) {
	for _, k := range keys {
		_ = g.slotOf(k).resolveSkip()
	}
}

func (g *Graph) skipUnwritten(n *Node, rc *RunCtx) {
	for _, k := range n.provides {
		if _, ok := rc.wrote[k.name]; ok {
			continue
		}
		_ = g.slotOf(k).resolveSkip()
	}
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
