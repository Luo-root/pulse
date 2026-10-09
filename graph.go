package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

// Graph 是一次运行的世界：节点集合 + 数据槽 + 首错 + 取消。
//
// id 是图身份（New 的 graphID，必填）：不同业务用不同图组装（node
// 可跨图复用），Observer 回调随事实携带它，供观测折叠区分「运行的
// 是哪张图」（观测 key 见 pulse.AttrGraph）。
type Graph struct {
	id     string
	ctx    context.Context
	cancel context.CancelFunc

	keys     keyRegistry
	producer map[string]string // key → "seed" 或 node id；每种来源至多一个
	slots    map[string]*slot
	slotsMu  sync.Mutex

	nodes    []*Node
	aspects  []Aspect
	observer Observer
	maxRun   int // <=0 无限

	mu      sync.Mutex
	started bool
	done    bool // Wait 返回过：此后 cancel 只是收尾，不再算运行结果
	err     error
	sem     chan struct{}
	wg      sync.WaitGroup
}

// Option 配置 Graph。
type Option func(*Graph)

// WithMaxRunning 限制同时进入 Run 的节点数。n<=0 表示无限（默认）。
// 等数据不占名额；排队等名额期间 ctx 取消同样打断（节点不进入 Run，
// 终态为 canceled），不会把名额交给一个已取消的节点。
func WithMaxRunning(n int) Option {
	return func(g *Graph) { g.maxRun = n }
}

// WithAspects 安装全局切面（先于节点切面，外层先跑）。
func WithAspects(as ...Aspect) Option {
	return func(g *Graph) { g.aspects = append(g.aspects, as...) }
}

// New 构造空图。graphID 是图身份（必填，空串报错）：观测记录靠它
// 区分「运行的是哪张图」。graphID 不做唯一性约束：同 id 的图并存
// （含并发）时观测记录同键，需要区分请用不同 id。ctx 取消会打断
// 所有等待。
func New(ctx context.Context, graphID string, opts ...Option) (*Graph, error) {
	if graphID == "" {
		return nil, errors.New("pulse: graph id is required (observe instance identity)")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c, cancel := context.WithCancel(ctx)
	g := &Graph{
		id:       graphID,
		ctx:      c,
		cancel:   cancel,
		slots:    make(map[string]*slot),
		producer: make(map[string]string),
	}
	for _, o := range opts {
		o(g)
	}
	if g.maxRun > 0 {
		g.sem = make(chan struct{}, g.maxRun)
	}
	return g, nil
}

// ID 返回图身份（New 的 graphID）。
func (g *Graph) ID() string { return g.id }

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
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started {
		return ErrGraphStarted
	}
	if n.id == "" {
		return fmt.Errorf("pulse: empty node id")
	}
	for _, existing := range g.nodes {
		if existing.id == n.id {
			return fmt.Errorf("pulse: duplicate node id %q", n.id)
		}
	}

	// —— 校验段：只读 ——
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
		if err := g.sourceConflict(k.name, n.id); err != nil {
			return err
		}
	}

	// —— 提交段：到这一步不会再失败 ——
	for _, k := range n.requires {
		_ = g.keys.register(k)
		g.slotOfLocked(k)
	}
	for _, k := range n.provides {
		_ = g.keys.register(k)
		_ = g.claimSource(k.name, n.id)
		g.slotOfLocked(k)
	}
	g.nodes = append(g.nodes, n)
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
	if err := g.sourceConflict(ref.name, "seed"); err != nil {
		return err
	}
	_ = g.keys.register(ref)
	_ = g.claimSource(ref.name, "seed")
	if skip {
		return g.slotOfLocked(ref).resolveSkip()
	}
	return g.slotOfLocked(ref).resolveValue(v)
}

// claimSource 保证每个 Key 只有一种来源：外部 Seed/SkipSeed，或恰好一个节点。
func (g *Graph) claimSource(name, owner string) error {
	if err := g.sourceConflict(name, owner); err != nil {
		return err
	}
	g.producer[name] = owner
	return nil
}

// sourceConflict 是 claimSource 的**只读预演**：判据与错误文案同源，但不写表。
func (g *Graph) sourceConflict(name, owner string) error {
	if prev, ok := g.producer[name]; ok && prev != owner {
		return fmt.Errorf("%w: %q already sourced by %s", ErrDuplicateSource, name, prev)
	}
	return nil
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
// 提交前做一次**只读校验**，两条判据都能在这一刻静态判定：
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
func (g *Graph) Start() error {
	g.mu.Lock()
	if g.started {
		g.mu.Unlock()
		return ErrGraphStarted
	}
	if err := g.checkSourcesLocked(); err != nil {
		g.mu.Unlock()
		return err
	}
	if err := g.checkAcyclicLocked(); err != nil {
		g.mu.Unlock()
		return err
	}
	if len(g.nodes) == 0 {
		g.started = true
		g.mu.Unlock()
		return nil
	}
	g.started = true
	nodes := append([]*Node(nil), g.nodes...)
	g.mu.Unlock()

	g.wg.Add(len(nodes))
	for _, n := range nodes {
		n := n
		go g.runNode(n)
	}
	return nil
}

// checkSourcesLocked 判「每个 Requires 都有来源」。调用方持 g.mu。
//
// 判据是 producer 表里有没有这个 Key：外部 Seed/SkipSeed 记为 "seed"，
// 节点 Provides 记为节点 id；某种来源至多一个（see claimSource）。Requires
// 本身不登记来源，所以「表里没有」= 没人会写它。
func (g *Graph) checkSourcesLocked() error {
	for _, n := range g.nodes {
		for _, k := range n.requires {
			if _, ok := g.producer[k.name]; !ok {
				return fmt.Errorf("pulse: node %q requires %q but nothing provides or seeds it", n.id, k.name)
			}
		}
	}
	return nil
}

// checkAcyclicLocked 判「依赖关系无环」。调用方持 g.mu。
//
// 与 checkSourcesLocked 互补：**有来源不等于能满足**。环里的槽谁也等不来谁，
// 门（等全部输入到达）就永远不开——这不是可执行的反馈环，而是不可完成的拓扑。
//
// 建图：N 的某条 Requires 由节点 M 提供 → 边 M → N（M 必须先跑完）。来源是
// "seed" 的 Key 不建边——Seed/SkipSeed 在 Start 之前就已经到达了。自环不可能
// 出现：Requires 与 Provides 同名的节点在 Add 就被拒。
//
// 判据用 Kahn 剥叶（O(V+E)，只在 Start 跑一次）：剥得完 = 无环；剥不完 =
// 剩下的节点要么在环里，要么在环的下游。再从剩下的节点里走出一条**具体**的环
// 报出去——比只报节点集合可操作。遍历按声明序取第一条出边，所以报错稳定。
func (g *Graph) checkAcyclicLocked() error {
	if len(g.nodes) < 2 {
		return nil
	}
	index := make(map[string]int, len(g.nodes))
	for i, n := range g.nodes {
		index[n.id] = i
	}

	type edge struct {
		to  int    // 下游节点下标
		key string // 下游 Requires 的那条 Key
	}
	succ := make([][]edge, len(g.nodes))
	indeg := make([]int, len(g.nodes))
	linked := make(map[[2]int]struct{})
	for ci, n := range g.nodes {
		for _, k := range n.requires {
			pi, ok := index[g.producer[k.name]]
			if !ok {
				continue // 来自 seed（缺来源的那种已被 checkSourcesLocked 拦下）
			}
			pair := [2]int{pi, ci}
			if _, dup := linked[pair]; dup {
				continue // 同一对节点被多条 Requires 依赖：只留一条边
			}
			linked[pair] = struct{}{}
			succ[pi] = append(succ[pi], edge{to: ci, key: k.name})
			indeg[ci]++
		}
	}

	// Kahn 剥叶。
	queue := make([]int, 0, len(g.nodes))
	for i, d := range indeg {
		if d == 0 {
			queue = append(queue, i)
		}
	}
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		for _, e := range succ[i] {
			indeg[e.to]--
			if indeg[e.to] == 0 {
				queue = append(queue, e.to)
			}
		}
	}
	start := -1
	for i, d := range indeg {
		if d > 0 {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}

	// 走出一条环：indeg 不为 0 的节点必有出边（否则它会被剥掉），节点数有限，
	// 所以沿 succ 一直走必然撞回走过的节点。
	var path []int
	var keys []string // keys[i] 是 path[i] → path[i+1] 那条边
	pos := make(map[int]int, len(g.nodes))
	cur := start
	for {
		p, seen := pos[cur]
		if seen {
			path, keys = path[p:], keys[p:]
			break
		}
		pos[cur] = len(path)
		path = append(path, cur)
		if len(succ[cur]) == 0 { // 防御性出口：理论上到不了
			keys = keys[:len(path)-1]
			break
		}
		keys = append(keys, succ[cur][0].key)
		cur = succ[cur][0].to
	}

	ids := make([]string, 0, len(path)+1)
	for _, i := range path {
		ids = append(ids, g.nodes[i].id)
	}
	ids = append(ids, g.nodes[path[0]].id) // 合上环

	// 「谁在等谁」：path[i] 等的是走进它的那条边（上一条）上的 Key。
	waits := make([]string, 0, len(path))
	for i, idx := range path {
		prev := (i - 1 + len(path)) % len(path)
		if prev >= len(keys) {
			break
		}
		waits = append(waits, fmt.Sprintf("%s requires %q", g.nodes[idx].id, keys[prev]))
	}
	return fmt.Errorf("pulse: dependency cycle: %s (%s)",
		strings.Join(ids, " -> "), strings.Join(waits, ", "))
}

// Wait 等待 Start 提交的节点全部终止，并释放图自己的 ctx：它到这一步不再
// 挂在父 ctx 的 children 上（`New` 派生的子 ctx 若不 cancel，父 ctx 是
// Background 或长生命周期时会一直持有它）。
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
	g.cancel()
	return err
}

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

// acquire 占用一个运行名额。ctx 取消时不再排队，**拿到名额后再看一次**：
// 「名额空出」与「取消」同时就绪时 select 会随机挑一个分支，少了这一眼就会
// 在取消之后仍进入 Run（实测这条路径并不罕见）。留给调用方的名额不该用在
// 一个已取消的节点上。
func (g *Graph) acquire(ctx context.Context) error {
	if g.sem == nil {
		return nil
	}
	select {
	case g.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-g.sem // 放回名额
		return err
	}
	return nil
}

func (g *Graph) release() {
	if g.sem != nil {
		<-g.sem
	}
}

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
		if err := WaitAll(rc, n.requires...); err != nil {
			return err
		}
		if rc.ctx.Err() != nil {
			return rc.ctx.Err()
		}
		if err := g.acquire(rc.ctx); err != nil {
			return err
		}
		defer g.release()
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
