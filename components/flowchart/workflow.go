package flowchart

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

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
		ctx:        schema.NewFlowContext(ctx),
		aspects:    make([]node.Aspect, 0),
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
			w.RunNode(nodeCopy)
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

// RunNode 执行单个节点（包含全局切面 + 节点切面 + Interceptor 调用链）
func (w *Workflow) RunNode(n node.Node) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("runNode panic [node=%s]: %v", n.ID(), r)
			// panic 也应触发工作流级取消，防止其他节点无限等待
			w.ctx.Cancel(fmt.Errorf("panic in node %s: %v", n.ID(), r))
		}
	}()

	// 1. 收集所有切面（全局 + 节点私有）
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

	// 2. 分离传统 Before/After 切面 与 Interceptor（可拦截执行）
	var traditional []node.Aspect
	var interceptors []node.Interceptor
	for _, a := range aspects {
		if ic, ok := a.(node.Interceptor); ok {
			interceptors = append(interceptors, ic)
		} else {
			traditional = append(traditional, a)
		}
	}

	// 3. 传统切面 Before 阶段（如日志、监控埋点）
	for _, a := range traditional {
		a.Before(w.ctx, n)
	}

	// 4. 构建调用链：实际执行被 Interceptor 层层包裹（洋葱模型）
	invoker := func() (map[string]any, error) {
		// 新增：全局错误自检，实现快速失败
		// 若兄弟节点已失败并触发 Cancel，此处直接退出，避免无效执行与无效等待
		if err := w.ctx.Err(); err != nil {
			return nil, err
		}
		inputs, err := w.ctx.WaitAll(n.Inputs()...)
		if err != nil {
			return nil, err
		}
		return n.Run(w.ctx, inputs)
	}

	// 反向包裹：interceptors 数组后面的包在最外层
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

	// 新增：只要节点执行出错（且不是已被取消的连锁反应），立即触发工作流级取消
	// 这会通过 context 传播给所有正在 WaitAll/DataSlot.Get 中等待的节点，实现级联中断
	if runErr != nil {
		w.ctx.Cancel(runErr)
	}

	// 5. 传统切面 After 阶段
	for _, a := range traditional {
		a.After(w.ctx, n, runErr)
	}

	// 6. 输出结果（仅成功时写入上下文）
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
func (w *Workflow) Reset(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return schema.ErrWorkflowClosed
	}

	if w.running {
		return schema.ErrWorkflowResetRunning
	}

	w.ctx = schema.NewFlowContext(ctx)
	return nil
}

// Run 运行工作流（阻塞直到所有节点完成）
// 若任意节点执行失败（含超时、熔断、panic 等），返回该错误，实现错误透出
func (w *Workflow) Run(Input map[string]any) error {
	// 标记为运行中
	w.mu.Lock()
	w.running = true
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	for k, v := range Input {
		err := w.Input(k, v)
		if err != nil {
			return err
		}
	}
	// 启动所有节点
	var wg sync.WaitGroup

	for _, n := range w.nodes {
		nodeCopy := n
		wg.Add(1)

		err := w.pool.Submit(func() {
			defer wg.Done()
			w.RunNode(nodeCopy)
		})
		if err != nil {
			return schema.ErrWorkflowSubmitNodeToPool
		}
	}

	// 等待所有节点完成
	wg.Wait()

	// 新增：若工作流执行过程中任意节点出错，将首错返回给调用方
	if err := w.ctx.Err(); err != nil {
		return err
	}
	return nil
}

// Wait 阻塞等待工作流运行结束（适用于 Start 启动的异步模式），并返回工作流级错误（若存在）
func (w *Workflow) Wait() error {
	for w.IsRunning() {
		time.Sleep(10 * time.Millisecond)
	}
	return w.ctx.Err()
}

// Err 非阻塞查询当前工作流错误状态
func (w *Workflow) Err() error {
	return w.ctx.Err()
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

// Get 上下文中的数据
func (w *Workflow) Get(key string) (any, error) {
	return w.ctx.Get(key)
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
