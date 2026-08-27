package flow

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Graph 是一次运行的世界：节点集合 + 数据槽 + 首错 + 取消。
type Graph struct {
	ctx    context.Context
	cancel context.CancelFunc

	keys     keyRegistry
	producer map[string]string // key → "seed" 或 node id；每种来源至多一个
	slots    map[string]*slot
	slotsMu  sync.Mutex

	nodes   []*Node
	aspects []Aspect
	maxRun  int // <=0 无限

	mu      sync.Mutex
	started bool
	err     error
	sem     chan struct{}
	wg      sync.WaitGroup
}

// Option 配置 Graph。
type Option func(*Graph)

// WithMaxRunning 限制同时进入 Run 的节点数。n<=0 表示无限（默认）。
// 等数据不占名额。
func WithMaxRunning(n int) Option {
	return func(g *Graph) { g.maxRun = n }
}

// WithAspects 安装全局切面（先于节点切面，外层先跑）。
func WithAspects(as ...Aspect) Option {
	return func(g *Graph) { g.aspects = append(g.aspects, as...) }
}

// New 构造空图。ctx 取消会打断所有等待。
func New(ctx context.Context, opts ...Option) *Graph {
	if ctx == nil {
		ctx = context.Background()
	}
	c, cancel := context.WithCancel(ctx)
	g := &Graph{
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
	return g
}

// Add 登记节点。图启动后拒绝。
func (g *Graph) Add(n *Node) error {
	if n == nil {
		return fmt.Errorf("flow: nil node")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started {
		return ErrGraphStarted
	}
	if n.id == "" {
		return fmt.Errorf("flow: empty node id")
	}
	for _, existing := range g.nodes {
		if existing.id == n.id {
			return fmt.Errorf("flow: duplicate node id %q", n.id)
		}
	}
	seenReq := make(map[string]struct{}, len(n.requires))
	for _, k := range n.requires {
		if _, ok := seenReq[k.name]; ok {
			return fmt.Errorf("flow: node %s declares %q twice in Requires", n.id, k.name)
		}
		seenReq[k.name] = struct{}{}
		if err := g.keys.register(k); err != nil {
			return err
		}
		g.slotOfLocked(k)
	}
	seenProv := make(map[string]struct{}, len(n.provides))
	for _, k := range n.provides {
		if _, ok := seenReq[k.name]; ok {
			return fmt.Errorf("flow: node %s both requires and provides %q", n.id, k.name)
		}
		if _, ok := seenProv[k.name]; ok {
			return fmt.Errorf("flow: node %s declares %q twice in Provides", n.id, k.name)
		}
		seenProv[k.name] = struct{}{}
		if err := g.keys.register(k); err != nil {
			return err
		}
		if err := g.claimSource(k.name, n.id); err != nil {
			return err
		}
		g.slotOfLocked(k)
	}
	g.nodes = append(g.nodes, n)
	return nil
}

// Seed 在运行前写入初始值（幂等首写）。
func Seed[T any](g *Graph, k Key[T], v T) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started {
		return ErrGraphStarted
	}
	ref := k.asRef()
	if err := g.keys.register(ref); err != nil {
		return err
	}
	if err := g.claimSource(ref.name, "seed"); err != nil {
		return err
	}
	return g.slotOfLocked(ref).resolveValue(v)
}

// SkipSeed 在运行前将某 Key 标为跳过。
func SkipSeed[T any](g *Graph, k Key[T]) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.started {
		return ErrGraphStarted
	}
	ref := k.asRef()
	if err := g.keys.register(ref); err != nil {
		return err
	}
	if err := g.claimSource(ref.name, "seed"); err != nil {
		return err
	}
	return g.slotOfLocked(ref).resolveSkip()
}

// claimSource 保证每个 Key 只有一种来源：外部 Seed/SkipSeed，或恰好一个节点。
func (g *Graph) claimSource(name, owner string) error {
	if prev, ok := g.producer[name]; ok && prev != owner {
		return fmt.Errorf("%w: %q already sourced by %s", ErrDuplicateSource, name, prev)
	}
	g.producer[name] = owner
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
func (g *Graph) Start() error {
	g.mu.Lock()
	if g.started {
		g.mu.Unlock()
		return ErrGraphStarted
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

// Wait 等待 Start 提交的节点全部终止。
func (g *Graph) Wait() error {
	g.mu.Lock()
	started := g.started
	g.mu.Unlock()
	if !started {
		return ErrGraphNotStarted
	}
	g.wg.Wait()
	return g.Err()
}

// Err 返回首个节点错误或取消原因。不含单纯的跳过。
func (g *Graph) Err() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err != nil {
		return g.err
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

func (g *Graph) acquire() {
	if g.sem != nil {
		g.sem <- struct{}{}
	}
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

	// 切面覆盖「等输入 + 执行」整段，这样 Timeout 能打断 Wait。
	// MaxRunning 只在即将执行用户 Run 时占用。
	chain := buildChain(append(append([]Aspect{}, g.aspects...), n.aspects...), func(rc *RunCtx) (err error) {
		if err := WaitAll(rc, n.requires...); err != nil {
			return err
		}
		if rc.ctx.Err() != nil {
			return rc.ctx.Err()
		}
		g.acquire()
		defer g.release()
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("flow: panic in node %s: %v", n.id, rec)
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
		return
	}
	if err != nil {
		g.fail(err)
		g.skipAllOrUnwritten(n, rc, true)
		return
	}
	g.skipAllOrUnwritten(n, rc, false)
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
