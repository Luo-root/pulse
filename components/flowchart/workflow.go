package flowchart

import (
	"context"
	"log"
	"sync"

	"github.com/Luo-root/pulse/components/flowchart/node"
	"github.com/Luo-root/pulse/components/schema"
	"github.com/panjf2000/ants/v2"
)

// Workflow 工作流引擎
type Workflow struct {
	nodes      []node.Node
	ctx        *schema.FlowContext
	aspects    []node.Aspect // 全局切面（所有节点生效）
	mu         sync.Mutex    // 保护运行状态
	running    bool          // 是否正在运行
	closed     bool          // 是否已关闭
	pool       *ants.Pool    // 线程池
	maxWorkers int           // 最大工作协程数
}

// NewWorkflow 创建工作流实例
// maxWorkers: 最大并发节点数（建议根据 CPU 核心数和业务特性调整）
func NewWorkflow(maxWorkers int) (*Workflow, error) {
	if maxWorkers <= 0 {
		maxWorkers = ants.DefaultAntsPoolSize
	}

	pool, err := ants.NewPool(maxWorkers, ants.WithPreAlloc(true))
	if err != nil {
		return nil, err
	}

	return &Workflow{
		ctx:        schema.NewFlowContext(),
		pool:       pool,
		maxWorkers: maxWorkers,
		closed:     false,
	}, nil
}

// AddNode 向工作流添加一个执行节点
func (w *Workflow) AddNode(node node.Node) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return schema.ErrWorkflowClosed
	}

	w.nodes = append(w.nodes, node)
	return nil
}

// AddAspect 添加全局切面，作用于所有节点
func (w *Workflow) AddAspect(aspect node.Aspect) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return schema.ErrWorkflowClosed
	}
	w.aspects = append(w.aspects, aspect)
	return nil
}

// Start 启动所有节点（异步、自动等待依赖）
func (w *Workflow) Start() error {
	w.mu.Lock()

	if w.closed {
		w.mu.Unlock()
		return schema.ErrWorkflowClosed
	}

	if w.running {
		w.mu.Unlock()
		return schema.ErrWorkflowRunning
	}

	w.running = true
	w.mu.Unlock()

	var wg sync.WaitGroup

	for _, n := range w.nodes {
		nodeCopy := n // 避免闭包问题
		wg.Add(1)
		err := w.pool.Submit(func() {
			// 执行完成后自动标记运行状态（所有节点执行完才置false）
			defer wg.Done()
			w.runNode(context.Background(), nodeCopy)
		})
		if err != nil {
			w.mu.Lock()
			w.running = false
			w.mu.Unlock()
			return schema.ErrWorkflowSubmitNodeToPool
		}
	}

	go func() {
		// 阻塞，直到所有节点执行完毕
		wg.Wait()

		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()
	return nil
}

// runNode 执行单个节点（包含全局切面 + 节点切面）
func (w *Workflow) runNode(ctx context.Context, node node.Node) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("runNode panic [node=%s]: %v", node.ID(), r)
		}
	}()

	// 切面 Before
	for _, a := range w.aspects {
		a.Before(w.ctx, node)
	}
	for _, a := range node.Aspects() {
		a.Before(w.ctx, node)
	}

	// 执行业务
	inputs, err := w.ctx.WaitAll(ctx, node.Inputs()...)
	if err != nil {
		log.Printf("wait input failed: %v", err)
		return
	}
	outputs, runErr := node.Run(w.ctx, inputs)

	// 切面 After
	for _, a := range w.aspects {
		a.After(w.ctx, node, runErr)
	}
	for _, a := range node.Aspects() {
		a.After(w.ctx, node, runErr)
	}

	// 输出结果
	if runErr == nil && outputs != nil {
		for k, v := range outputs {
			w.ctx.Set(k, v)
		}
	}
}

// Input 输入初始数据，启动流程
func (w *Workflow) Input(key string, value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return schema.ErrWorkflowClosed
	}

	w.ctx.Set(key, value)
	return nil
}

// Reset 重置工作流上下文，安全可重入
func (w *Workflow) Reset() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return schema.ErrWorkflowClosed
	}

	if w.running {
		return schema.ErrWorkflowResetRunning
	}

	w.ctx = schema.NewFlowContext()
	return nil
}

// Run 运行工作流（阻塞直到所有节点完成）
// 每次调用前会自动重置上下文，支持多次运行
func (w *Workflow) Run(ctx context.Context) error {
	// 重置上下文以清除旧数据
	if err := w.Reset(); err != nil {
		return err
	}

	// 标记为运行中
	w.mu.Lock()
	w.running = true
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	// 启动所有节点
	var wg sync.WaitGroup

	for _, n := range w.nodes {
		nodeCopy := n
		wg.Add(1)

		err := w.pool.Submit(func() {
			defer wg.Done()
			w.runNode(ctx, nodeCopy)
		})
		if err != nil {
			return schema.ErrWorkflowSubmitNodeToPool
		}
	}

	// 等待所有节点完成
	wg.Wait()

	return nil
}

// Close 关闭工作流，释放线程池资源
// 调用后工作流将无法再使用
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

// IsRunning 检查工作流是否正在运行
func (w *Workflow) IsRunning() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

// IsClosed 检查工作流是否已关闭
func (w *Workflow) IsClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

// GetContext 获取当前上下文（只读）
func (w *Workflow) GetContext() schema.ReadOnlyFlowContext {
	return w.ctx
}

// GetStats 获取工作流和线程池统计信息
func (w *Workflow) GetStats() map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()

	stats := map[string]any{
		"total_nodes":   len(w.nodes),
		"is_running":    w.running,
		"max_workers":   w.maxWorkers,
		"pool_capacity": w.pool.Cap(),
		"pool_free":     w.pool.Free(),
		"pool_waiting":  w.pool.Waiting(),
	}

	return stats
}

// GetPoolInfo 获取线程池详细信息
func (w *Workflow) GetPoolInfo() map[string]any {
	return map[string]any{
		"capacity": w.pool.Cap(),     // 总容量
		"free":     w.pool.Free(),    // 空闲协程数
		"waiting":  w.pool.Waiting(), // 等待执行的任务数
	}
}

// ResizePool 动态调整线程池大小
// newSize: 新的最大并发数
func (w *Workflow) ResizePool(newSize int) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return schema.ErrWorkflowClosed
	}

	if w.running {
		return schema.ErrWorkflowRunning
	}

	if newSize <= 0 {
		newSize = ants.DefaultAntsPoolSize
	}

	w.pool.Tune(newSize)

	w.maxWorkers = newSize
	return nil
}
