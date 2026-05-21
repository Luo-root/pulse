package flowchart

import (
	"context"
	"fmt"
	"sync"

	"github.com/Luo-root/pulse/components/flow"
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
		return flow.ErrWorkflowClosed
	}

	w.nodes = append(w.nodes, n)
	return nil
}

// AddAspect 添加全局切面
func (w *Workflow) AddAspect(aspect node.Aspect) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return flow.ErrWorkflowClosed
	}

	w.aspects = append(w.aspects, aspect)
	return nil
}

// Input 输入初始数据
func (w *Workflow) Input(key string, value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return flow.ErrWorkflowClosed
	}

	w.ctx.Set(key, value)
	return nil
}

// Run 运行工作流（阻塞直到所有节点完成）
// 所有节点同时提交到协程池，通过 FlowContext.Wait 自行等待依赖就绪
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

	if err := w.submitAll(); err != nil {
		return err
	}

	return w.ctx.Err()
}

// Start 异步启动工作流
func (w *Workflow) Start() error {
	if err := w.prepareRun(); err != nil {
		return err
	}

	if err := w.submitAll(); err != nil {
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
		return flow.ErrWorkflowClosed
	}
	if w.running {
		return flow.ErrWorkflowRunning
	}
	w.running = true
	w.doneCh = make(chan struct{})
	return nil
}

// submitAll 将所有节点提交到协程池，数据驱动执行顺序
func (w *Workflow) submitAll() error {
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
			w.ctx.Cancel(flow.ErrWorkflowSubmitNodeToPool)
			wg.Wait()
			w.mu.Lock()
			w.running = false
			w.mu.Unlock()
			close(w.doneCh)
			return flow.ErrWorkflowSubmitNodeToPool
		}
	}

	// 异步等待完成后关闭 doneCh（Start 需要），Run 直接阻塞在这里
	go func() {
		wg.Wait()
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		close(w.doneCh)
	}()

	// Start 调用时不阻塞，Run 调用时外部通过 Wait 或直接返回前等待
	// 但 Run 需要同步返回，所以这里阻塞
	<-w.doneCh
	return nil
}

// RunNode 执行单个节点（包含切面 + 拦截器调用链）
func (w *Workflow) RunNode(n node.Node) {
	// 收集切面
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

	// 分离传统切面和拦截器
	var traditional []node.Aspect
	var interceptors []node.Interceptor
	for _, a := range aspects {
		if ic, ok := a.(node.Interceptor); ok {
			interceptors = append(interceptors, ic)
		} else {
			traditional = append(traditional, a)
		}
	}

	// Before 阶段
	for _, a := range traditional {
		a.Before(w.ctx, n)
	}

	// 构建调用链
	invoker := func() (map[string]any, error) {
		if err := w.ctx.Err(); err != nil {
			return nil, err
		}
		inputs, err := w.ctx.WaitAll(n.Inputs()...)
		if err != nil {
			return nil, err
		}
		return n.Run(w.ctx, inputs)
	}

	for i := len(interceptors) - 1; i >= 0; i-- {
		ic := interceptors[i]
		next := invoker
		invoker = func(ic node.Interceptor, next func() (map[string]any, error)) func() (map[string]any, error) {
			return func() (map[string]any, error) {
				return ic.Around(w.ctx, n, next)
			}
		}(ic, next)
	}

	outputs, runErr := invoker()

	if runErr != nil {
		w.ctx.Cancel(runErr)
	}

	// After 阶段
	for i := len(traditional) - 1; i >= 0; i-- {
		traditional[i].After(w.ctx, n, runErr)
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
		return flow.ErrWorkflowClosed
	}
	if w.running {
		return flow.ErrWorkflowResetRunning
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
		return flow.ErrWorkflowClosed
	}
	if w.running {
		return flow.ErrWorkflowRunning
	}
	if newSize <= 0 {
		newSize = ants.DefaultAntsPoolSize
	}
	w.pool.Tune(newSize)
	w.maxWorkers = newSize
	return nil
}
