package flowchart

import (
	"context"
	"fmt"
	"log"
	"sort"
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

// Run 运行工作流（阻塞直到完成）
func (w *Workflow) Run(Input map[string]any) error {
	if err := w.prepareRun(); err != nil {
		return err
	}
	defer func() {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
	}()

	for k, v := range Input {
		w.ctx.Set(k, v)
	}

	// 预检 + 拓扑分层
	layers, err := w.buildDAG()
	if err != nil {
		return fmt.Errorf("workflow validation failed: %w", err)
	}

	// 按层执行
	for _, layer := range layers {
		if err := w.runLayer(layer); err != nil {
			return err
		}
	}

	return w.ctx.Err()
}

// Start 异步启动工作流
func (w *Workflow) Start() error {
	if err := w.prepareRun(); err != nil {
		return err
	}

	// 预检 + 拓扑分层
	layers, err := w.buildDAG()
	if err != nil {
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		return fmt.Errorf("workflow validation failed: %w", err)
	}

	go func() {
		for _, layer := range layers {
			if err := w.runLayer(layer); err != nil {
				break
			}
		}
		w.mu.Lock()
		w.running = false
		w.mu.Unlock()
		close(w.doneCh)
	}()

	return nil
}

// Wait 阻塞等待工作流完成（事件驱动，无轮询）
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

// runLayer 并行执行一层中的所有节点
func (w *Workflow) runLayer(layer []node.Node) error {
	var wg sync.WaitGroup

	for _, n := range layer {
		nodeCopy := n
		wg.Add(1)
		err := w.pool.Submit(func() {
			defer wg.Done()
			w.RunNode(nodeCopy)
		})
		if err != nil {
			wg.Done()
			w.ctx.Cancel(flow.ErrWorkflowSubmitNodeToPool)
			wg.Wait()
			return flow.ErrWorkflowSubmitNodeToPool
		}
	}

	wg.Wait()

	// 层间错误检查：任意节点失败，后续层不再执行
	if err := w.ctx.Err(); err != nil {
		return err
	}

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
	for _, a := range traditional {
		a.After(w.ctx, n, runErr)
	}

	// 写入输出
	if runErr == nil && outputs != nil {
		for k, v := range outputs {
			w.ctx.Set(k, v)
		}
	}
}

// ============================================================================
// DAG 构建与验证
// ============================================================================

// buildDAG 从节点声明的 Inputs/Outputs 构建依赖图，执行拓扑排序，返回分层结果
// 不声明 Inputs/Outputs 的节点不参与排序，直接放入第一层（它们通过 FlowContext.Wait 自行等待）
func (w *Workflow) buildDAG() ([][]node.Node, error) {
	if len(w.nodes) == 0 {
		return nil, fmt.Errorf("workflow has no nodes")
	}

	// 区分两类节点：
	//  1. 声明了依赖的节点：参与 DAG 排序
	//  2. 未声明依赖的节点：直接放入初始层
	var dagNodes []node.Node
	var freeNodes []node.Node

	for _, n := range w.nodes {
		if len(n.Inputs()) == 0 && len(n.Outputs()) == 0 {
			freeNodes = append(freeNodes, n)
		} else {
			dagNodes = append(dagNodes, n)
		}
	}

	// 如果没有声明依赖的节点，直接返回所有节点并行执行
	if len(dagNodes) == 0 {
		return [][]node.Node{w.nodes}, nil
	}

	// --- 构建生产者映射 ---
	producers := make(map[string]string) // output key → node ID
	for _, n := range dagNodes {
		for _, out := range n.Outputs() {
			if existing, exists := producers[out]; exists {
				return nil, fmt.Errorf(
					"duplicate output key %q: produced by both %q and %q",
					out, existing, n.ID(),
				)
			}
			producers[out] = n.ID()
		}
	}

	// --- 验证依赖 ---
	for _, n := range dagNodes {
		for _, in := range n.Inputs() {
			_, hasProducer := producers[in]
			isReady := w.ctx.IsReady(in) // 已通过 Input() 设置
			if !hasProducer && !isReady {
				log.Printf("[workflow] warning: node %q depends on key %q, but no node produces it and it's not a provided input", n.ID(), in)
			}
		}
	}

	// --- 构建邻接表和入度表 ---
	nodeMap := make(map[string]node.Node)
	inDegree := make(map[string]int)
	adjMap := make(map[string][]string) // node ID → 依赖的 node IDs

	for _, n := range dagNodes {
		nodeMap[n.ID()] = n
		inDegree[n.ID()] = 0
		adjMap[n.ID()] = []string{}
	}

	for _, n := range dagNodes {
		for _, in := range n.Inputs() {
			producerID, hasProducer := producers[in]
			if hasProducer && producerID != n.ID() {
				adjMap[n.ID()] = append(adjMap[n.ID()], producerID)
				inDegree[n.ID()]++
			}
		}
	}

	// --- 拓扑排序（Kahn 算法）---
	var queue []string
	for _, n := range dagNodes {
		if inDegree[n.ID()] == 0 {
			queue = append(queue, n.ID())
		}
	}

	var sorted []string
	for len(queue) > 0 {
		sort.Strings(queue) // 保证确定性
		id := queue[0]
		queue = queue[1:]
		sorted = append(sorted, id)

		for _, n := range dagNodes {
			if n.ID() == id {
				continue
			}
			for _, dep := range adjMap[n.ID()] {
				if dep == id {
					inDegree[n.ID()]--
					if inDegree[n.ID()] == 0 {
						queue = append(queue, n.ID())
					}
				}
			}
		}
	}

	if len(sorted) != len(dagNodes) {
		return nil, fmt.Errorf("cycle detected in node dependencies")
	}

	// --- 分层 ---
	depth := make(map[string]int)
	for _, id := range sorted {
		if len(adjMap[id]) == 0 {
			depth[id] = 0
			continue
		}
		maxDep := -1
		for _, dep := range adjMap[id] {
			if d, ok := depth[dep]; ok && d > maxDep {
				maxDep = d
			}
		}
		depth[id] = maxDep + 1
	}

	maxDepth := 0
	for _, d := range depth {
		if d > maxDepth {
			maxDepth = d
		}
	}

	layers := make([][]node.Node, maxDepth+1)
	for id, d := range depth {
		layers[d] = append(layers[d], nodeMap[id])
	}

	// freeNodes 放入第一层（并行执行，自行等待）
	if len(freeNodes) > 0 {
		layers[0] = append(layers[0], freeNodes...)
	}

	// 每层内按 ID 排序，保证确定性
	for i := range layers {
		sort.Slice(layers[i], func(a, b int) bool {
			return layers[i][a].ID() < layers[i][b].ID()
		})
	}

	return layers, nil
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
