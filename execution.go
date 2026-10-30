package pulse

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

// 本文件是**一次运行**：它自己的 ctx / 取消域、输入值、槽位、首错、waitgroup、有效配置。
// `Graph` 只留定义（见 graph.go）——「图纸」与「这一次开动机器」从此是两个对象。

// execSeq 给每次运行发一个**进程内唯一**的序号。
//
// 为什么不用「父 id + path」的纯确定性派生：同一挂载点重试第 n 次重建出来的子执行是
// **不同的实例**，序号必须进 id——否则同一个挂载点重试几次的观测记录会合并成一份。
// 序号不必跨进程可比，`Execution` 的身份是「这一次运行」，不是「可复现的指纹」。
var execSeq atomic.Uint64

// Execution 是**一次运行**：节点集合（读 `Graph` 的定义）、这次运行的 ctx 与取消域、
// 输入值、槽位、每节点状态、首错、waitgroup、有效观察者 / 名额 / 图级切面。
//
// 它是**一次性的**：`Start()` 只能成功一次，槽位不会重开。想再跑一遍就再建一个
// （`Run` 每次调用都新建一份）——于是同一张定义可以顺序跑很多次、并发跑很多次，
// 一次运行的取消 / 失败 / 完成不污染定义，也不污染别的运行。
//
// 别把一个 `Execution` 交给两处当「同一个运行」用：它的身份、槽位与终态都属于这一次。
type Execution struct {
	graph *Graph
	site  Site

	ctx    context.Context
	cancel context.CancelFunc // 业务 fail-fast，只取消业务等待。
	// controlCancel 与宿主、父节点显式控制共用可靠取消链；内部业务失败不触发它。
	controlCtx    context.Context
	controlCancel context.CancelFunc

	// observer / lim / aspects 是**这一次运行的有效值**（本次运行的 ExecOption >
	// 定义声明 > 父执行环境）。它们只在创建时算一次，此后不写——定义因此保持只读。
	observer   Observer
	lim        *limiter
	aspects    []Aspect
	checkpoint *checkpointConfig
	// restore 是尚未灌槽的完整恢复计划；restored 在 Start 成功后只读。
	restore  []NodeCheckpoint
	restored map[*Node]NodeCheckpoint

	slots   map[string]*slot
	slotsMu sync.Mutex
	// 已恢复的同一逻辑流只建一个端点，多个 Out 别名不能各造一份确认账。
	checkpointStreams map[CheckpointStreamID]any // 受 slotsMu 保护
	// seeded 是创建时收下的输入来源键名：Seed / SkipSeed，以及合法静态 In 的 pending 承诺。
	// 它在创建那一刻一次写完、此后只读，`Start` 拿它按「就要封口的那张定义」再判一次
	// 启动条件（见 checkStartInputsLocked）。
	seeded map[string]struct{}
	// mounted 的定义已由父执行在整树装配边界内检查并封口；不能把它从挂载环境
	// 拆出来重判流读者，否则父侧真实消费者会在 child.Start 时凭空消失。
	mounted     bool
	hostReaders map[string]keyRef // 仅在 assemblyMu + graph.mu + e.mu 内登记根宿主读取者

	mu      sync.Mutex
	started bool
	done    bool // Wait 返回过：此后 cancel 只是收尾，不再算运行结果
	err     error
	// errReason 与 err 一起锁存；Wait/Snapshot 不再遍历用户 error 的方法。
	errReason NodeFinishReason
	// states 是本次运行的节点读数，受 mu 保护；输出顺序由 graph.nodes 决定。
	states map[*Node]nodeRuntime
	wg     sync.WaitGroup
	// 流共享初始化和已接受的宿主可靠操作；入口与封口同用 mu，封口后不再 Add。
	streamWork    sync.WaitGroup
	streamClosing bool
	// finishOnce 让「发一次 OnGraphFinished」与「所有 Wait 都等到它落地」同时
	// 成立：sync.Once 会阻塞后来的调用者直到第一个调用返回（比自己的 done
	// 门闩强——那个只保证不重发，先返回的 Wait 会把出口先关掉）。
	finishOnce sync.Once
}

// NewExecution 为**这一次运行**建一个执行：ctx 是这次运行的取消域，in 是这一次的输入。
//
// 它做两件事，都不改定义：
//
//  1. **算有效配置**：观察者 / 名额 / 图级切面按「本次运行的 `ExecOption` > 这张定义的
//     声明 > 父执行环境」定下来。所以同一张定义被两处挂载时，两处各有各的有效值
//     （旧版本是运行期把这些值回写进子图字段，两处会互相踩）。
//
//  2. **判启动条件**：这次运行的输入齐不齐——每一条**被节点 `Requires` 的进口**都要有
//     交代（`Seed` 给了值、`SkipSeed` 标了跳过、或由子图的 `In` 绑定喂），喂了没声明过
//     的键也报。这是**启动条件**，与 `Validate()` 那条静态判据是两份：`Validate` 问
//     「这张定义结构上成不成立」，这里问「这一次跑得起来吗」。
//
//     这一判在 `Start` 那一刻**再判一次**（同一份判据、同一句报文）：从创建到启动之间
//     定义还能长（`Add` 一条要新进口的节点就行），而 `Start` 紧接着就把那张定义封上并跑它
//     ——「判过的定义」得是「封上并跑的那张」。缺输入的那次启动**不算启动过**（与静态校验
//     被拒同一条线性化规则），补上输入换一份执行即可。
//
// 静态结构（无来源 / 有环 / 出口没人产出）仍在 `Start()` 报——那是定义的问题，
// 不该等某一次运行才说。
func (g *Graph) NewExecution(ctx context.Context, in Inputs, opts ...ExecOption) (*Execution, error) {
	cfg := execConfig{}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return g.newExecution(ctx, ctx, "", "", nil, cfg, in, nil)
}

// newChildExecution 为挂载点建一份**子执行**：ctx 从父节点那一层的 ctx 派生
// （所以取消天然传下去，不需要有人转发），path 由父执行的位置 + 这一步的节点 id 拼出来。
//
// `parentObserver` / `parentLim` / `parentAspects` 是**父执行环境**的有效值：子图没声明
// 什么就继承什么（切面切不切由子图自己的 `WithoutAspects` 决定）。
func (g *Graph) newChildExecution(rc *RunCtx, in Inputs, bound map[keyRef]*slot) (*Execution, error) {
	parent, nodeID := rc.exec, rc.node.id
	path := nodeID
	if parent.site.Path != "" {
		path = parent.site.Path + "/" + nodeID
	}
	return g.newExecution(rc.ctx, rc.controlCtx, parent.site.Execution, path, parent, execConfig{}, in, bound)
}

// newExecution 是两条入口的共用本体。`parent` 为 nil 表示这是根执行（没有可继承的
// 执行环境，也没有「被谁喂」这回事）。
func (g *Graph) newExecution(ctx, control context.Context, parentID, path string, parent *Execution, cfg execConfig, in Inputs, bound map[keyRef]*slot) (*Execution, error) {
	if g == nil {
		return nil, fmt.Errorf("pulse: nil graph")
	}
	c, cancel := context.WithCancel(ctx)
	controlCtx, controlCancel := c, cancel
	if cfg.checkpoint != nil || (parent != nil && parent.checkpoint != nil) {
		controlCtx, controlCancel = context.WithCancel(control)
	}

	e := &Execution{
		graph:         g,
		site:          Site{Graph: g.id, Execution: nextExecID(g.id, parentID, path), Path: path},
		ctx:           c,
		cancel:        cancel,
		controlCtx:    controlCtx,
		controlCancel: controlCancel,
		slots:         make(map[string]*slot),
		mounted:       parent != nil,
	}
	if parent != nil {
		e.observer = parent.observer
		e.lim = parent.lim
		e.aspects = parent.aspects
	}
	if cfg.checkpoint != nil {
		c := *cfg.checkpoint
		c.failure = &checkpointFailure{root: e}
		e.checkpoint = &c
	}

	// 定义只读：拿一次锁把要用的东西读完（同一时刻别人正在 Add 的话，读到的是
	// 「要么全是新的、要么全是旧的」——不会撕成两半）。
	entries, err := g.prepareExecution(e, cfg, in, bound)
	if err != nil {
		e.Cancel()
		return nil, err
	}
	e.seeded = make(map[string]struct{}, len(entries)+len(bound))
	for ref, source := range bound {
		if err := e.slotOf(ref).bindTo(source); err != nil {
			e.Cancel()
			return nil, fmt.Errorf("pulse: input %s: %w", ref, err)
		}
		e.seeded[ref.name] = struct{}{}
	}
	for _, en := range entries {
		e.seeded[en.ref.name] = struct{}{}
		s := e.slotOf(en.ref)
		if en.skip {
			_ = s.resolveSkip()
			continue
		}
		_ = s.resolveValue(en.value)
	}
	return e, nil
}

// nextExecID 拼运行身份：根执行是 `<图 id>#<序号>`，子执行是 `<父执行>/<一步的节点 id>#<序号>`。
// 序号来自进程内唯一的计数器，所以「同一张图并发跑两次」「同一挂载点重试两次」都拿得到
// 不同的值；子图挂载节点 id 里不许出现 `/`，所以这个写法不会有歧义。
func nextExecID(graphID, parentID, path string) string {
	n := execSeq.Add(1)
	if parentID == "" {
		return fmt.Sprintf("%s#%d", graphID, n)
	}
	return fmt.Sprintf("%s/%s#%d", parentID, path, n)
}

// prepareExecution 在 `g.mu` 里做三件事：算有效配置、判这一次的输入、建槽位表。
// 返回可以种进槽位的那批输入（已经按 key 名排序，报文才确定）。
func (g *Graph) prepareExecution(e *Execution, cfg execConfig, in Inputs, bound map[keyRef]*slot) ([]inputEntry, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// —— 有效配置：本次运行 > 定义声明 > 父执行环境 ——
	if cfg.observerSet {
		e.observer = cfg.observer
	} else if g.observer != nil {
		e.observer = g.observer
	}
	base := e.aspects
	if g.aspectsCut {
		base = nil
	}
	e.aspects = append(append(append([]Aspect{}, base...), g.aspects...), cfg.aspects...)
	if cfg.quotaSet {
		e.lim = newLimiter(cfg.maxRunning)
	} else if g.limSet {
		e.lim = newLimiter(g.maxRunning)
	}

	// —— 槽位表：这次运行里可能被读写的每一条键 ——
	for _, n := range g.nodes {
		for _, k := range n.requires {
			e.slotOfLocked(k)
		}
		for _, k := range n.provides {
			e.slotOfLocked(k)
		}
	}
	for _, k := range g.inPorts {
		e.slotOfLocked(k)
	}
	for _, k := range g.outPorts {
		e.slotOfLocked(k)
	}

	// —— 判这一次的输入 ——
	required := g.requiredInputsLocked()
	seeded := make(map[string]struct{}, len(in.entries))

	names := make([]string, 0, len(in.entries))
	for name := range in.entries {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]inputEntry, 0, len(names))
	for _, name := range names {
		en := in.entries[name]
		if err := g.checkExecutionInputLocked(en.ref); err != nil {
			return nil, err
		}
		seeded[name] = struct{}{}
		entries = append(entries, en)
	}

	// 静态 In 是来源承诺，可以尚未到达；端口、类型和双源仍用与 Seed 相同的判据。
	refs := make([]keyRef, 0, len(bound))
	for ref := range bound {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	for _, ref := range refs {
		if err := g.checkExecutionInputLocked(ref); err != nil {
			return nil, err
		}
		if bound[ref] == nil {
			return nil, fmt.Errorf("pulse: input %s: nil binding source", ref)
		}
		if _, duplicate := seeded[ref.name]; duplicate {
			return nil, fmt.Errorf("pulse: input %s is supplied twice: %w", ref, ErrDuplicateSource)
		}
		seeded[ref.name] = struct{}{}
	}
	if err := g.missingInputErrorLocked(required, seeded); err != nil {
		return nil, err
	}
	return entries, nil
}

// checkExecutionInputLocked 只核输入声明，不要求绑定来源已经到达。调用方持 g.mu。
func (g *Graph) checkExecutionInputLocked(ref keyRef) error {
	if err := g.keys.check(ref); err != nil {
		return err
	}
	if src, provided := g.producer[ref.name]; provided && src.kind == sourceNode {
		return fmt.Errorf("pulse: input %s: this key is provided by %s inside graph %q, "+
			"so a value fed from outside would be silently ignored (a key has exactly one "+
			"source)", ref, src, g.id)
	}
	if _, ok := g.inPorts[ref.name]; !ok {
		if _, ok := g.outPorts[ref.name]; ok {
			return fmt.Errorf("pulse: input %s: this key is declared as an output, not an "+
				"input: declare it with DeclareInput if it should also come from outside", ref)
		}
		return fmt.Errorf("pulse: input %s: not a declared input of graph %q: "+
			"declare it with DeclareInput before feeding it (an undeclared key would otherwise "+
			"look like \"no source\" to Validate)", ref, g.id)
	}
	return nil
}

// requiredInputsLocked 是「这一次运行**必须交代**的进口」：被某个节点 `Requires`、图内没人
// 产出、且声明成了进口的那些键。调用方持 g.mu。
//
// 判据是**这一次运行**的，不是定义的：没人 `Requires` 的进口不逼调用方编个值出来（留作公共
// 接口是合法的）。「既没人产出、也不是声明过的进口」那种键归静态校验（`Validate` / `Start`
// 报 nothing provides it），这里不重复。
func (g *Graph) requiredInputsLocked() map[string]keyRef {
	// 只有声明过的进口才可能成为本轮必需输入；无进口时无需扫描节点或分配大表。
	if len(g.inPorts) == 0 {
		return nil
	}
	required := make(map[string]keyRef, len(g.inPorts))
	for _, n := range g.nodes {
		for _, k := range n.requires {
			if src, ok := g.producer[k.name]; ok && src.kind == sourceNode {
				continue // 内部有人产出，不需要外部喂
			}
			if _, ok := g.inPorts[k.name]; !ok {
				continue // 既没人产出、也不是声明的进口：那是静态错，静态校验会报
			}
			required[k.name] = k
		}
	}
	return required
}

// missingInputErrorLocked 报「被 `Requires` 的进口还有谁没交代」。调用方持 g.mu。
//
// **一份报文两个入口共用**：`NewExecution`（创建那一刻）与 `Start`（封口之前再审一次，见
// checkStartInputsLocked），所以同一份缺失在两个时刻报出来逐字相同——`supplied` 是这份执行
// 已经收下的键名，`required` 按**当时那张定义**算。
func (g *Graph) missingInputErrorLocked(required map[string]keyRef, supplied map[string]struct{}) error {
	missing := make([]string, 0, len(required))
	for name := range required {
		if _, ok := supplied[name]; ok {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	k := required[missing[0]]
	return fmt.Errorf("pulse: input %s has no value for this run: node %s requires it, "+
		"and it is a declared input of graph %q - supply it with Seed, mark it with SkipSeed, "+
		"or (inside a parent graph) bind it with In", k, requiredByLocked(g, k.name), g.id)
}

// checkStartInputsLocked 在**封口之前**按当前定义再判一次这份执行的启动条件（调用方持 g.mu
// 与 assemblyMu）。
//
// 为什么要第二眼：这份执行的输入是**创建那一刻**按当时的定义收下的（那是早反馈），此后定义
// 还长得进去——`Add` 一条「`Requires` 这条声明过的进口」的节点就能让这份旧执行缺一条值。判据
// 本身与创建时完全相同（同一个 missingInputErrorLocked），只是判的时刻挪到「就要封口」的
// 那一刻：**判过的定义 = 封上的定义 = 真正要跑的那张**，三者必须是同一个版本。少了这一眼，
// 那个节点会等在一条永远不来的槽上，直到 ctx 到期——一句看不出病因的超时。
func (g *Graph) checkStartInputsLocked(supplied map[string]struct{}) error {
	return g.missingInputErrorLocked(g.requiredInputsLocked(), supplied)
}

// requiredByLocked 找出图中第一个 Requires 这条键的节点 id（报文点名用）。调用方持 g.mu。
func requiredByLocked(g *Graph, name string) string {
	for _, n := range g.nodes {
		for _, k := range n.requires {
			if k.name == name {
				return fmt.Sprintf("%q", n.id)
			}
		}
	}
	return "(unknown)"
}

// Graph 返回这张执行读的**定义**（不可变）。
func (e *Execution) Graph() *Graph { return e.graph }

// ID 是**这一次运行**的身份，对每个执行实例唯一。观测里它是 `pulse.AttrExecution`：
// 同一张图跑两次得到两个不同的值，两份记录因此分得开（只看图 id 与路径会折叠成一份）。
func (e *Execution) ID() string { return e.site.Execution }

// Site 是「谁在哪里」：定义身份 + 本次运行身份 + 挂载位置。根执行的 Path 是 `""`。
func (e *Execution) Site() Site { return e.site }

// Context 是这次运行的取消域。取消它 = 拆掉这一次运行（定义与别的运行不受影响）。
func (e *Execution) Context() context.Context { return e.ctx }

// Start 异步提交这张图的全部节点。
//
// 提交前做一次**只读校验**（与 `Validate` 同一套判据：装配期可以先问一遍，
// 别等启动时才知道），两条判据都能在这一刻静态判定：
//
//   - 每个 `Requires` 都必须有来源（声明过的进口，或某个节点的 `Provides`）。
//     没有来源的槽永远不会被写入；
//   - 依赖关系**无环**。有来源不等于能满足：环里每条 `Requires` 都有生产者，
//     但没有任何节点能先进入 `Run`（门要等全部输入到达），所有槽永远停在 pending。
//
// 第三眼判的是**这份执行**而不是定义：它收下的输入里，有没有哪条被 `Requires` 的进口是空的
// ——定义在创建之后还能长出新要求（`Add` 一条要新进口的节点）。判据与 `NewExecution` 那一刻
// 完全同一份、报文逐字相同，判的时刻在封口**之前**（见 checkStartInputsLocked）。
//
// 三条都是「运行时表现为挂死」的事：有 deadline 时是一句看不出病因的超时，
// 没有时进程会被 runtime 判为 fatal deadlock。校验不过时这次运行**仍未启动**
// （`started` 保持 false）：补上生产者之后可以重新 `Start`；含环的图则要改装配
// ——引擎没有 Remove，环只能靠**重新装一张图**消除。
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
func (e *Execution) Start() error {
	g := e.graph
	// 开放定义的最终校验与封口仍在装配边界内；冻结树只需根锁，
	// 宿主登记也取根锁，故检查期间本轮订阅事实不会变化。
	assembly := g.lockValidation()
	known := streamKnownInputs{
		skipped: e.streamInputSkips(), hasAspects: len(e.aspects) != 0,
		hostReads: e.hostStreamReads(), checkpoint: e.checkpoint != nil,
	}
	// 子执行来自已通过整树检查并封口的挂载位置。流的父侧消费者和兄弟池都属于
	// 那次检查的上下文，不能在这里把子树当作独立根重新判。每次运行的输入仍在下方核验。
	if !e.mounted {
		if err := g.validateRunLocked(e.lim.capacity(), known); err != nil {
			g.unlockValidation(assembly)
			return err
		}
	}
	// 启动条件再判一眼（与创建那一刻同一份判据、同一句报文）：这份执行的输入是**创建时**
	// 按当时那张定义收下的，而从那之后定义还长得进去——这一眼之后紧接着就是封口，于是
	// 「判过的定义」与「封上的定义」是同一个版本（见 checkStartInputsLocked）。
	if err := g.checkStartInputsLocked(e.seeded); err != nil {
		g.unlockValidation(assembly)
		return err
	}
	if err := e.checkHostStreamInputsLocked(); err != nil {
		g.unlockValidation(assembly)
		return err
	}
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		g.unlockValidation(assembly)
		return ErrGraphStarted
	}
	if e.checkpoint != nil {
		// 整批恢复与声明按即将封口的同一版本核验，失败零灌槽、零事件、零封口。
		err := g.checkCheckpointLocked(e.checkpoint, mountChain{g})
		if err == nil {
			err = e.checkCheckpointInputsLocked()
		}
		if err == nil {
			err = g.checkRestoreLocked(e.restore)
		}
		if err != nil {
			e.mu.Unlock()
			g.unlockValidation(assembly)
			return err
		}
		if err := e.applyRestoreLocked(); err != nil {
			e.mu.Unlock()
			g.unlockValidation(assembly)
			return e.stopCheckpoint("restore streams", err)
		}
	}
	// 全部校验通过才封整棵树；挂载执行已由根检查并冻结，不为每个
	// child.Start 另编一份脱离父侧读者环境的子树模型。
	if !e.mounted {
		g.freezeValidationLocked()
	}
	nodes := append([]*Node(nil), g.nodes...)
	hostReaders := make([]keyRef, 0, len(e.hostReaders))
	for _, ref := range e.hostReaders {
		hostReaders = append(hostReaders, ref)
	}
	e.started = true
	// 宿主读取组的必需义务也属于执行终态；尾部未确认时不能提前成功并关闭持久会话。
	e.wg.Add(len(nodes) + len(hostReaders) + 1) // +1：started 那一发落地前，Wait 不该溜过去
	e.mu.Unlock()
	g.unlockValidation(assembly)

	e.notify(func(o Observer) { o.OnGraphStarted(e.site) })
	e.wg.Done()

	for _, ref := range hostReaders {
		go e.runHostStreamReader(ref) // 只等待义务终结，不消费消息，也不产生节点事件。
	}
	for _, n := range nodes {
		n := n
		go e.runNode(n)
	}
	return nil
}

// streamInputSkips 只读创建时供给的外部输入，不采信节点运行留下的输出。
// 不创建槽位、不发事件；根与 child.Start 都把相同的确定事实交给流检查。
func (e *Execution) streamInputSkips() map[string]bool {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	var skipped map[string]bool
	for name := range e.seeded {
		if s := e.slots[name]; s != nil {
			if state, _ := s.snapshot(); state == slotSkipped {
				if skipped == nil {
					skipped = make(map[string]bool)
				}
				skipped[name] = true
			}
		}
	}
	return skipped
}

// Wait 等待 Start 提交的节点全部终止、已登记的必需宿主读取组完成或显式停止，
// 并会合已接纳的可靠流工作，然后确定整轮终态并释放这次运行的 ctx。
// 可靠模式优先返回此时已锁存的树级可靠首错（CheckpointError），归为 failed；
// 否则保留首个节点错误或真实取消。各子执行在自己的收尾边界采用同一规则，
// 已确定的终态不会被随后另一执行的失败改写。
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
// 同一个执行的 `Start` / `Wait`**：那是同一个 goroutine 等自己，会死等。
func (e *Execution) Wait() error {
	e.mu.Lock()
	started := e.started
	e.mu.Unlock()
	if !started {
		return ErrGraphNotStarted
	}
	e.wg.Wait()
	e.mu.Lock()
	e.streamClosing = true
	e.mu.Unlock()
	e.streamWork.Wait()
	e.mu.Lock()
	if !e.done {
		// 可靠工作已会合；树级可靠首错优先于先到的业务失败或取消，直接采用
		// 已知的 failed 归因，不再遍历用户 error。锁顺序与 Start 一致：e.mu → f.mu；
		// stopCheckpoint 释放 f.mu 后才调用 root.fail，不反向持锁。
		if failure := e.checkpointErr(); failure != nil {
			e.err, e.errReason = failure, NodeFailed
		} else if e.err == nil {
			// 没有节点捕获的真实取消也属于本轮结果；先锁存，再标记结束。
			e.err = e.ctx.Err()
			e.errReason = NodeCompleted
			if e.err != nil {
				e.errReason = NodeCanceled
			}
		}
		e.done = true
	}
	// 并发 / 重复 Wait 只读已锁存结果，不能重新捕获前一次的收尾 cancel。
	err, reason := e.err, e.errReason
	e.mu.Unlock()
	e.finishOnce.Do(func() {
		// 图级终态只会是 completed / failed / canceled：跳过是节点级的事实
		// （fail 不收跳过，Err 也不含它），升不到这一层。
		e.notify(func(o Observer) { o.OnGraphFinished(e.site, reason, err) })
	})
	e.Cancel()
	return err
}

// beginStreamWork 在执行封口前登记可靠流工作；不能把可能调用用户钩子的工作放在 mu 内。
func (e *Execution) beginStreamWork() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.streamClosing {
		return false
	}
	e.streamWork.Add(1)
	return true
}

// Err 在 Wait 收尾前返回首个节点错误或当前取消原因，不含单纯的跳过。
// Wait 收尾时优先锁存当时已知的树级可靠首错，否则保留首个节点错误或真实取消；
// 此后 Err 与 Wait 返回同一最终结果。随后释放资源的 cancel 不再算结果，
// 并发或重复 Wait 以及其他执行稍后的可靠失败都不改变已确定的结果。
func (e *Execution) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	if e.done {
		return nil
	}
	return e.ctx.Err()
}

// Cancel 取消这次运行（等价于取消宿主给 `NewExecution` 的那个 ctx）。包内入口，
// 也给宿主一个明确的「拆掉它」动作——定义与别的运行不受影响。
func (e *Execution) Cancel() {
	e.controlCancel()
	e.cancel()
}

// slotOf 取（必要时建）这次运行里某条键的槽位。
func (e *Execution) slotOf(k keyRef) *slot {
	e.slotsMu.Lock()
	defer e.slotsMu.Unlock()
	return e.slotOfLocked(k)
}

func (e *Execution) slotOfLocked(k keyRef) *slot {
	if s, ok := e.slots[k.name]; ok {
		return s
	}
	s := newSlot()
	e.slots[k.name] = s
	return s
}

// Run 是**同步便捷入口**：为这一次调用新建一份执行，跑完并等到全部终止。
//
// 等价于 `NewExecution` + `Start` + `Wait`，返回值与 `Wait` 相同（可靠首错优先，
// 否则首个节点错误或 ctx 取消；不含跳过）。要拿出来看身份 / 分步控制就自己建执行：
//
//	e, err := g.NewExecution(ctx, in)
//	... e.Start(); e.Wait()
//
// 每次调用都是**独立的一次运行**：同一张图反复 `Run` 互不影响，并发 `Run` 也互不影响
// （各自的槽位与取消域）。
func (g *Graph) Run(ctx context.Context, in Inputs, opts ...ExecOption) error {
	e, err := g.NewExecution(ctx, in, opts...)
	if err != nil {
		return err
	}
	// Run 独占这份执行；Start 拒绝时调用方拿不到它，也必须解除宿主取消注册。
	defer e.Cancel()
	if err := e.Start(); err != nil {
		return err
	}
	return e.Wait()
}

// 本文件是**一次运行的运行期配置**：`WithExec*` 一族覆盖这张定义声明的默认值。
//
// 分工：`Option`（`With*`）是**定义**的声明，每次运行都生效；`ExecOption`（`WithExec*`）
// 只作用于**这一次**运行。优先级从高到低：本次运行 > 定义声明 > 父执行环境。

// ExecOption 配置**某一次运行**，在 `NewExecution` / `Run` 时给。
type ExecOption func(*execConfig)

// execConfig 是一次运行的覆盖项；每项都带一个「给了没有」的标记，
// 否则零值（nil 出口、0 名额）分不出「没给」与「给了零值」。
type execConfig struct {
	observer    Observer
	observerSet bool
	maxRunning  int
	quotaSet    bool
	aspects     []Aspect
	checkpoint  *checkpointConfig
}

// WithExecObserver 让**这一次运行**用这个观察者，压过定义上 `WithObserver` 声明的那一个。
//
// 它与定义声明的关系是「覆盖」而不是「叠加」：要组合两个出口就自己用 `MultiObserver`
// 拼好再传进来。子图那一层没声明出口时会继承父执行环境的（这次覆盖后的）出口——
// 继承走的是**有效值**，不是定义上的声明。
func WithExecObserver(o Observer) ExecOption {
	return func(c *execConfig) {
		c.observer = o
		c.observerSet = true
	}
}

// WithExecMaxRunning 让**这一次运行**用这个名额上限，压过定义上 `WithMaxRunning` 声明的那个。
//
// `n<=0` = 这一次不限（与定义声明同一条规则：`<=0` 也是「声明过」，不是「没声明」）。
// 这次声明的名额同样会往下继承给子执行环境（同一棵执行树共用一个计数）。
func WithExecMaxRunning(n int) ExecOption {
	return func(c *execConfig) {
		c.maxRunning = n
		c.quotaSet = true
	}
}

// WithExecAspects 给**这一次运行**追加图级切面：它们排在定义声明的那批**之后**
// （更靠内），并照样继承给子执行环境。
//
// **是追加不是替换**：替换会让「这一次多加一个超时」变成「把这张图的图级切面全撤掉」，
// 那不是调用方想要的。要撤掉继承来的那批，用定义上的 `WithoutAspects()`。
func WithExecAspects(as ...Aspect) ExecOption {
	return func(c *execConfig) { c.aspects = append(c.aspects, as...) }
}
