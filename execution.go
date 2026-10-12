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
	cancel context.CancelFunc

	// observer / lim / aspects 是**这一次运行的有效值**（本次运行的 ExecOption >
	// 定义声明 > 父执行环境）。它们只在创建时算一次，此后不写——定义因此保持只读。
	observer Observer
	lim      *limiter
	aspects  []Aspect

	slots   map[string]*slot
	slotsMu sync.Mutex

	mu      sync.Mutex
	started bool
	done    bool // Wait 返回过：此后 cancel 只是收尾，不再算运行结果
	err     error
	wg      sync.WaitGroup
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
//  2. **判启动条件**：这次运行的输入齐不齐——每一条**被节点 `Requires` 的进口**都要有
//     交代（`Seed` 给了值、`SkipSeed` 标了跳过、或由子图的 `In` 绑定喂），喂了没声明过
//     的键也报。这是**启动条件**，与 `Validate()` 那条静态判据是两份：`Validate` 问
//     「这张定义结构上成不成立」，这里问「这一次跑得起来吗」。
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
	return g.newExecution(ctx, "", "", nil, cfg, in, true)
}

// newChildExecution 为挂载点建一份**子执行**：ctx 从父节点那一层的 ctx 派生
// （所以取消天然传下去，不需要有人转发），path 由父执行的位置 + 这一步的节点 id 拼出来。
//
// `parentObserver` / `parentLim` / `parentAspects` 是**父执行环境**的有效值：子图没声明
// 什么就继承什么（切面切不切由子图自己的 `WithoutAspects` 决定）。
func (g *Graph) newChildExecution(parent *Execution, ctx context.Context, nodeID string, in Inputs) (*Execution, error) {
	path := nodeID
	if parent.site.Path != "" {
		path = parent.site.Path + "/" + nodeID
	}
	return g.newExecution(ctx, parent.site.Execution, path, parent, execConfig{}, in, false)
}

// newExecution 是两条入口的共用本体。`parent` 为 nil 表示这是根执行（没有可继承的
// 执行环境，也没有「被谁喂」这回事）。
func (g *Graph) newExecution(ctx context.Context, parentID, path string, parent *Execution, cfg execConfig, in Inputs, root bool) (*Execution, error) {
	if g == nil {
		return nil, fmt.Errorf("pulse: nil graph")
	}
	c, cancel := context.WithCancel(ctx)

	e := &Execution{
		graph:  g,
		site:   Site{Graph: g.id, Execution: nextExecID(g.id, parentID, path), Path: path},
		ctx:    c,
		cancel: cancel,
		slots:  make(map[string]*slot),
	}
	if parent != nil {
		e.observer = parent.observer
		e.lim = parent.lim
		e.aspects = parent.aspects
	}

	// 定义只读：拿一次锁把要用的东西读完（同一时刻别人正在 Add 的话，读到的是
	// 「要么全是新的、要么全是旧的」——不会撕成两半）。
	entries, err := g.prepareExecution(e, cfg, in)
	if err != nil {
		cancel()
		return nil, err
	}
	for _, en := range entries {
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
// 不同的值；节点 id 里不许出现 `/`，所以这个写法不会有歧义。
func nextExecID(graphID, parentID, path string) string {
	n := execSeq.Add(1)
	if parentID == "" {
		return fmt.Sprintf("%s#%d", graphID, n)
	}
	return fmt.Sprintf("%s/%s#%d", parentID, path, n)
}

// prepareExecution 在 `g.mu` 里做三件事：算有效配置、判这一次的输入、建槽位表。
// 返回可以种进槽位的那批输入（已经按 key 名排序，报文才确定）。
func (g *Graph) prepareExecution(e *Execution, cfg execConfig, in Inputs) ([]inputEntry, error) {
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
	required := make(map[string]keyRef, len(g.nodes))
	for _, n := range g.nodes {
		for _, k := range n.requires {
			if src, ok := g.producer[k.name]; ok && src.kind == sourceNode {
				continue // 内部有人产出，不需要外部喂
			}
			if _, ok := g.inPorts[k.name]; !ok {
				continue // 既没人产出、也不是声明的进口：那是静态错，Start 会报
			}
			required[k.name] = k
		}
	}

	names := make([]string, 0, len(in.entries))
	for name := range in.entries {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]inputEntry, 0, len(names))
	for _, name := range names {
		en := in.entries[name]
		if err := g.keys.check(en.ref); err != nil {
			return nil, err
		}
		// 「图内已有来源」这条**先判**，且与端口无关：一条键既有内部产出、又在
		// 端口表里时，喂进来的值会抢先占住槽位、那个节点的 `Set` 被幂等首写静默
		// 丢掉——「值不见了而没人报错」。声明成进口不改变「一个键一个来源」。
		if src, provided := g.producer[name]; provided && src.kind == sourceNode {
			return nil, fmt.Errorf("pulse: input %s: this key is provided by %s inside graph %q, "+
				"so a value fed from outside would be silently ignored (a key has exactly one "+
				"source)", en.ref, src, g.id)
		}
		if _, ok := g.inPorts[name]; !ok {
			if _, ok := g.outPorts[name]; ok {
				return nil, fmt.Errorf("pulse: input %s: this key is declared as an output, not an "+
					"input: declare it with DeclareInput if it should also come from outside", en.ref)
			}
			return nil, fmt.Errorf("pulse: input %s: not a declared input of graph %q: "+
				"declare it with DeclareInput before feeding it (an undeclared key would otherwise "+
				"look like \"no source\" to Validate)", en.ref, g.id)
		}
		delete(required, name)
		entries = append(entries, en)
	}

	if len(required) > 0 {
		missing := make([]string, 0, len(required))
		for name := range required {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		k := required[missing[0]]
		return nil, fmt.Errorf("pulse: input %s has no value for this run: node %s requires it, "+
			"and it is a declared input of graph %q - supply it with Seed, mark it with SkipSeed, "+
			"or (inside a parent graph) bind it with In", k, requiredByLocked(g, k.name), g.id)
	}
	return entries, nil
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
// 两条都是「运行时表现为挂死」的事：有 deadline 时是一句看不出病因的超时，
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
	// 校验与封口在**装配锁**里做完，之后才放出去发事件、建 goroutine（临界区里不跑用户代码）：
	// 与 Validate 同一条边界，见 assemblyMu。
	assemblyMu.Lock()
	g.mu.Lock()
	if err := g.validateLocked(e.lim.capacity()); err != nil {
		g.mu.Unlock()
		assemblyMu.Unlock()
		return err
	}
	// 通过校验地启动 = 这棵树的定义从此不再变化（**含挂着的子图**）：跑起来之后定义还在变，
	// 观测与拓扑看到的那张图、真正在跑的那张图就成了两张。封口放在校验**之后**，所以
	// 启动被拒的那次一层都不封——补上缺的那块重新 Start 正是要支持的用法（见 Validate）。
	g.sealTreeLocked(mountChain{g})
	nodes := append([]*Node(nil), g.nodes...)
	g.mu.Unlock()

	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		assemblyMu.Unlock()
		return ErrGraphStarted
	}
	e.started = true
	e.wg.Add(len(nodes) + 1) // +1：started 那一发落地前，其他 goroutine 的 Wait 不该溜过去
	e.mu.Unlock()
	assemblyMu.Unlock()

	e.notify(func(o Observer) { o.OnGraphStarted(e.site) })
	e.wg.Done()

	for _, n := range nodes {
		n := n
		go e.runNode(n)
	}
	return nil
}

// Wait 等待 Start 提交的节点全部终止，并释放这次运行的 ctx。
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
	e.done = true // 先定结果，再 cancel：收尾不该被读成运行结果
	e.mu.Unlock()
	err := e.Err()
	e.finishOnce.Do(func() {
		// 图级终态只会是 completed / failed / canceled：跳过是节点级的事实
		// （fail 不收跳过，Err 也不含它），升不到这一层。
		e.notify(func(o Observer) { o.OnGraphFinished(e.site, finishReason(err), err) })
	})
	e.cancel()
	return err
}

// Err 返回首个节点错误或取消原因。不含单纯的跳过。运行结束后
// （Wait 返回过）收尾的那次 cancel 不再算结果——否则干净跑完的图会
// 报 context.Canceled。
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
func (e *Execution) Cancel() { e.cancel() }

func (e *Execution) fail(err error) {
	if err == nil || isSkipped(err) {
		return
	}
	e.mu.Lock()
	if e.err == nil {
		e.err = err
		e.cancel()
	}
	e.mu.Unlock()
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

// acquire 占用这次运行的**有效**名额表里的一个名额（nil = 无限，直接放行）。
// 取消语义见 limiter.acquire。
func (e *Execution) acquire(ctx context.Context) error { return e.lim.acquire(ctx) }

// release 归还名额。没占过、或名额表无限，都安全。
func (e *Execution) release() { e.lim.release() }

func (e *Execution) runNode(n *Node) {
	defer e.wg.Done()

	rc := newRunCtx(e, n, e.ctx)
	defer rc.Cancel()

	// 每节点生命周期事件至多一次（防 Retry 双打点）。
	var waited, ran atomic.Bool
	emitWaiting := func() {
		if waited.CompareAndSwap(false, true) {
			e.notify(func(o Observer) { o.OnNodeWaiting(e.site, n.id) })
		}
	}
	emitRunning := func() {
		if ran.CompareAndSwap(false, true) {
			e.notify(func(o Observer) { o.OnNodeRunning(e.site, n.id) })
		}
	}

	// 切面覆盖「等输入 + 执行」整段，这样 Timeout 能打断 Wait。
	// MaxRunning 只在即将执行用户 Run 时占用。
	// 生命周期埋点在 innermost core：Retry 重入时靠门闩只发一次。
	chain := buildChain(append(append([]Aspect{}, e.aspects...), n.aspects...), func(rc *RunCtx) (err error) {
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
			if err := e.acquire(rc.ctx); err != nil {
				return err
			}
			defer e.release()
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
		e.skipAllOrUnwritten(n, rc, true)
		e.notify(func(o Observer) { o.OnNodeFinished(e.site, n.id, NodeSkipped, err) })
		return
	}
	if err != nil {
		e.fail(err)
		e.skipAllOrUnwritten(n, rc, true)
		e.notify(func(o Observer) { o.OnNodeFinished(e.site, n.id, finishReason(err), err) })
		return
	}
	e.skipAllOrUnwritten(n, rc, false)
	e.notify(func(o Observer) { o.OnNodeFinished(e.site, n.id, NodeCompleted, nil) })
}

func (e *Execution) skipAllOrUnwritten(n *Node, rc *RunCtx, allIfNoneWritten bool) {
	if allIfNoneWritten && len(rc.wrote) == 0 {
		e.skipAll(n.provides)
		return
	}
	e.skipUnwritten(n, rc)
}

func (e *Execution) skipAll(keys []keyRef) {
	for _, k := range keys {
		_ = e.slotOf(k).resolveSkip()
	}
}

func (e *Execution) skipUnwritten(n *Node, rc *RunCtx) {
	for _, k := range n.provides {
		if _, ok := rc.wrote[k.name]; ok {
			continue
		}
		_ = e.slotOf(k).resolveSkip()
	}
}

// Run 是**同步便捷入口**：为这一次调用新建一份执行，跑完并等到全部终止。
//
// 等价于 `NewExecution` + `Start` + `Wait`，返回值与 `Wait` 相同（首错或 ctx 取消；
// 不含跳过）。要拿出来看身份 / 分步控制就自己建执行：
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
	if err := e.Start(); err != nil {
		return err
	}
	return e.Wait()
}
