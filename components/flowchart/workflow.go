package flowchart

import (
	"context"
	"fmt"
	"sync"

	"github.com/Luo-root/pulse/components/flowchart/flow"
	"github.com/Luo-root/pulse/components/flowchart/node"
	"github.com/panjf2000/ants/v2"
)

type Workflow struct {
	nodes      []node.Node
	ctx        *flow.FlowContext
	aspects    []node.Aspect
	mu         sync.Mutex
	running    bool
	closed     bool
	pool       *ants.Pool
	maxWorkers int
	doneCh     chan struct{}
}

func NewWorkflow(ctx context.Context, maxWorkers int) (*Workflow, error) {
	if maxWorkers <= 0 {
		maxWorkers = ants.DefaultAntsPoolSize
	}

	pool, err := ants.NewPool(maxWorkers, ants.WithPreAlloc(true))
	if err != nil {
		return nil, err
	}

	return &Workflow{
		nodes:      make([]node.Node, 0),
		ctx:        flow.NewFlowContext(ctx),
		aspects:    make([]node.Aspect, 0),
		pool:       pool,
		maxWorkers: maxWorkers,
		doneCh:     make(chan struct{}),
	}, nil
}

// AddNode 向工作流添加一个执行节点
func (w *Workflow) AddNode(n node.Node) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return ErrWorkflowClosed
	}

	w.nodes = append(w.nodes, n)
	return nil
}

// AddAspect 添加全局切面
func (w *Workflow) AddAspect(aspect node.Aspect) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return ErrWorkflowClosed
	}

	w.aspects = append(w.aspects, aspect)
	return nil
}

// Input 输入初始数据
func (w *Workflow) Input(key string, value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return ErrWorkflowClosed
	}

	w.ctx.Set(key, value)
	return nil
}

// Run 运行工作流（阻塞直到所有节点完成）
func (w *Workflow) Run(input map[string]any) error {
	if err := w.prepareRun(); err != nil {
		return err
	}
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	for k, v := range input {
		w.ctx.Set(k, v)
	}

	if err := w.submitAll(true); err != nil {
		return err
	}

	return w.ctx.Err()
}

// Start 异步启动工作流（立即返回，通过 Wait 等待完成）
func (w *Workflow) Start() error {
	if err := w.prepareRun(); err != nil {
		return err
	}

	if err := w.submitAll(false); err != nil {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		close(w.doneCh)
		return err
	}

	return nil
}

// Wait 阻塞等待工作流完成
func (w *Workflow) Wait() error {
	<-w.doneCh
	return w.ctx.Err()
}

// ============================================================================
// 内部方法
// ============================================================================

// prepareRun 统一的状态检查
func (w *Workflow) prepareRun() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return ErrWorkflowClosed
	}
	if w.running {
		return ErrWorkflowRunning
	}
	w.running = true
	w.doneCh = make(chan struct{})
	return nil
}

// submitAll 将所有节点提交到协程池，数据驱动执行顺序
// wait=true 时阻塞直到所有节点完成（Run 用），wait=false 时立即返回（Start 用）
func (w *Workflow) submitAll(wait bool) error {
	if len(w.nodes) == 0 {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		close(w.doneCh)
		return fmt.Errorf("workflow has no nodes")
	}

	var wg sync.WaitGroup

	for _, n := range w.nodes {
		n := n
		wg.Add(1)
		err := w.pool.Submit(func() {
			defer wg.Done()
			w.RunNode(n)
		})
		if err != nil {
			wg.Done()
			w.ctx.Cancel(ErrWorkflowSubmitNodeToPool)
			wg.Wait()
			w.mu.Lock()
			w.running = false
			w.mu.Unlock()
			close(w.doneCh)
			return ErrWorkflowSubmitNodeToPool
		}
	}

	go func() {
		wg.Wait()
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		close(w.doneCh)
	}()

	if wait {
		<-w.doneCh
	}
	return nil
}

// RunNode 执行单个节点（包含切面 + 拦截器调用链）
func (w *Workflow) RunNode(n node.Node) {
	// 收集切面（全局 + 节点级）
	var aspects []node.Aspect
	for _, a := range w.aspects {
		if a != nil {
			aspects = append(aspects, a)
		}
	}
	for _, a := range n.Aspects() {
		if a != nil {
			aspects = append(aspects, a)
		}
	}

	// 构建切面链：核心逻辑在最内层
	// core 接收 AspectContext，使用切面级 context 等待数据
	// 这样超时切面取消 ac.ctx 时，WaitAll 能立即感知
	core := func(ac *node.AspectContext) (map[string]any, error) {
		if err := w.ctx.Err(); err != nil {
			return nil, err
		}
		inputs, err := w.ctx.WaitAllWithContext(ac.Context(), n.Inputs()...)
		if err != nil {
			return nil, err
		}
		return n.Run(w.ctx, inputs)
	}

	chain := node.BuildNodeChain(aspects, n, core)

	// 创建切面上下文，执行切面链
	ac := node.NewAspectContext(w.ctx, w.ctx.GetContext())
	outputs, runErr := chain(ac)

	if runErr != nil {
		w.ctx.Cancel(runErr)
	}

	// 写入输出
	if runErr == nil && outputs != nil {
		for k, v := range outputs {
			w.ctx.Set(k, v)
		}
	}
}

// ============================================================================
// 生命周期
// ============================================================================

func (w *Workflow) Reset(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return ErrWorkflowClosed
	}
	if w.running {
		return ErrWorkflowResetRunning
	}

	w.ctx = flow.NewFlowContext(ctx)
	w.doneCh = make(chan struct{})
	return nil
}

func (w *Workflow) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return
	}
	w.closed = true
	w.running = false

	if w.pool != nil {
		w.pool.Release()
	}
}

func (w *Workflow) Err() error                  { return w.ctx.Err() }
func (w *Workflow) IsRunning() bool             { w.mu.Lock(); defer w.mu.Unlock(); return w.running }
func (w *Workflow) IsClosed() bool              { w.mu.Lock(); defer w.mu.Unlock(); return w.closed }
func (w *Workflow) Get(key string) (any, error) { return w.ctx.Get(key) }

func (w *Workflow) GetStats() map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	return map[string]any{
		"total_nodes":   len(w.nodes),
		"is_running":    w.running,
		"max_workers":   w.maxWorkers,
		"pool_capacity": w.pool.Cap(),
		"pool_free":     w.pool.Free(),
		"pool_waiting":  w.pool.Waiting(),
	}
}

func (w *Workflow) GetPoolInfo() map[string]any {
	return map[string]any{
		"capacity": w.pool.Cap(),
		"free":     w.pool.Free(),
		"waiting":  w.pool.Waiting(),
	}
}

func (w *Workflow) ResizePool(newSize int) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return ErrWorkflowClosed
	}
	if w.running {
		return ErrWorkflowRunning
	}
	if newSize <= 0 {
		newSize = ants.DefaultAntsPoolSize
	}
	w.pool.Tune(newSize)
	w.maxWorkers = newSize
	return nil
}
