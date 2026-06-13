package node

import (
	"fmt"
	"sort"
	"sync"

	"github.com/Luo-root/pulse/components/flowchart/flow"
)

// TopologicalNode 拓扑排序节点包装器
// 将一组有依赖关系的任务节点按拓扑序组织，确保依赖先执行
// 核心设计理念保留：依旧通过任务等待机制（ctx.Wait/ctx.WaitAll）控制执行
// 执行策略：按拓扑分层，同层节点并行执行（通过goroutine），层间串行等待
type TopologicalNode struct {
	id       string
	nodes    []Node
	adjMap   map[string][]string // 依赖图: nodeID -> 它所依赖的节点ID列表（用于构建拓扑序）
	inDegree map[string]int      // 入度表
	order    []string            // 拓扑排序结果（节点ID列表）
	layers   [][]string          // 分层结果：每层包含的节点ID列表
	outputs  []string
	aspects  []Aspect
}

// NewTopologicalNode 创建拓扑排序节点
// id: 节点ID
// nodes: 一组有依赖关系的任务节点
// outputKeys: 最终输出的key列表
func NewTopologicalNode(id string, nodes []Node, outputKeys []string) (*TopologicalNode, error) {
	tn := &TopologicalNode{
		id:       id,
		nodes:    nodes,
		adjMap:   make(map[string][]string),
		inDegree: make(map[string]int),
		outputs:  outputKeys,
		aspects:  make([]Aspect, 0),
	}

	if err := tn.buildGraph(); err != nil {
		return nil, err
	}

	if err := tn.topologicalSort(); err != nil {
		return nil, err
	}

	tn.buildLayers()

	return tn, nil
}

// buildGraph 从节点的 Inputs/Outputs 构建依赖图
// 规则：如果节点A的某个input key匹配节点B的某个output key，则A依赖B
func (tn *TopologicalNode) buildGraph() error {
	// 收集所有output key -> 生产该output的节点ID
	outputProducers := make(map[string]string)
	for _, n := range tn.nodes {
		for _, out := range n.Outputs() {
			if producer, exists := outputProducers[out]; exists {
				return fmt.Errorf("output key %q is produced by both %s and %s", out, producer, n.ID())
			}
			outputProducers[out] = n.ID()
		}
	}

	// 构建依赖关系和入度
	for _, n := range tn.nodes {
		nodeID := n.ID()
		tn.inDegree[nodeID] = 0
		tn.adjMap[nodeID] = []string{}

		for _, in := range n.Inputs() {
			// 跳过外部输入（如 user_goal, react_planner_plan 等）
			if producer, exists := outputProducers[in]; exists && producer != nodeID {
				tn.adjMap[nodeID] = append(tn.adjMap[nodeID], producer)
				tn.inDegree[nodeID]++
			}
		}
	}

	return nil
}

// topologicalSort 执行拓扑排序（Kahn算法）
func (tn *TopologicalNode) topologicalSort() error {
	inDegree := make(map[string]int)
	for k, v := range tn.inDegree {
		inDegree[k] = v
	}

	// 找到所有入度为0的节点
	var queue []string
	for _, n := range tn.nodes {
		if inDegree[n.ID()] == 0 {
			queue = append(queue, n.ID())
		}
	}

	var result []string
	for len(queue) > 0 {
		// 按ID排序保证确定性
		sort.Strings(queue)
		nodeID := queue[0]
		queue = queue[1:]
		result = append(result, nodeID)

		// 找到所有依赖当前节点的节点，减少入度
		for _, n := range tn.nodes {
			if n.ID() == nodeID {
				continue
			}
			for _, dep := range tn.adjMap[n.ID()] {
				if dep == nodeID {
					inDegree[n.ID()]--
					if inDegree[n.ID()] == 0 {
						queue = append(queue, n.ID())
					}
				}
			}
		}
	}

	if len(result) != len(tn.nodes) {
		return fmt.Errorf("cycle detected in task dependencies")
	}

	tn.order = result
	return nil
}

// buildLayers 将拓扑排序结果分层
// 每层包含的节点之间没有依赖关系，可以并行执行
func (tn *TopologicalNode) buildLayers() {
	if len(tn.order) == 0 {
		return
	}

	nodeMap := make(map[string]Node)
	for _, n := range tn.nodes {
		nodeMap[n.ID()] = n
	}

	// 计算每个节点的"深度"（从入度为0的节点开始的最长路径长度）
	depth := make(map[string]int)
	for _, nodeID := range tn.order {
		// 入度为0的节点深度为0
		if tn.inDegree[nodeID] == 0 {
			depth[nodeID] = 0
			continue
		}
		// 其他节点的深度 = max(依赖节点的深度) + 1
		maxDepDepth := -1
		for _, dep := range tn.adjMap[nodeID] {
			if d, ok := depth[dep]; ok && d > maxDepDepth {
				maxDepDepth = d
			}
		}
		depth[nodeID] = maxDepDepth + 1
	}

	// 按深度分组
	maxDepth := 0
	for _, d := range depth {
		if d > maxDepth {
			maxDepth = d
		}
	}

	tn.layers = make([][]string, maxDepth+1)
	for nodeID, d := range depth {
		tn.layers[d] = append(tn.layers[d], nodeID)
	}

	// 每层内按ID排序保证确定性
	for i := range tn.layers {
		sort.Strings(tn.layers[i])
	}
}

// ID 实现 Node 接口
func (tn *TopologicalNode) ID() string {
	return tn.id
}

// Inputs 实现 Node 接口：返回所有外部输入（不被任何节点生产的input）
func (tn *TopologicalNode) Inputs() []string {
	// 收集所有节点生产的output
	produced := make(map[string]bool)
	for _, n := range tn.nodes {
		for _, out := range n.Outputs() {
			produced[out] = true
		}
	}

	// 收集所有不被生产的input（即外部输入）
	externalInputs := make(map[string]bool)
	for _, n := range tn.nodes {
		for _, in := range n.Inputs() {
			if !produced[in] {
				externalInputs[in] = true
			}
		}
	}

	result := make([]string, 0, len(externalInputs))
	for in := range externalInputs {
		result = append(result, in)
	}
	sort.Strings(result)
	return result
}

// Outputs 实现 Node 接口
func (tn *TopologicalNode) Outputs() []string {
	return tn.outputs
}

// Aspects 实现 Node 接口
func (tn *TopologicalNode) Aspects() []Aspect {
	return tn.aspects
}

// AddAspect 添加切面
func (tn *TopologicalNode) AddAspect(aspect Aspect) {
	tn.aspects = append(tn.aspects, aspect)
}

// Run 按拓扑分层执行所有子节点
// 核心设计理念保留：依旧通过 ctx.WaitAll 等待机制控制执行
// 执行策略：
//   - 按拓扑分层，每层内部节点并行执行（通过 goroutine）
//   - 层间串行：前一层全部完成后，后一层才能开始
//   - 节点通过 ctx.WaitAll 等待自己的输入就绪（来自前一层节点的输出或外部输入）
func (tn *TopologicalNode) Run(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
	for k, v := range inputs {
		ctx.SetOrUpdate(k, v)
	}

	nodeMap := make(map[string]Node)
	for _, n := range tn.nodes {
		nodeMap[n.ID()] = n
	}

	for layerIdx, layer := range tn.layers {
		var wg sync.WaitGroup
		var mu sync.Mutex
		var errs []error

		for _, nodeID := range layer {
			n := nodeMap[nodeID]
			wg.Add(1)
			go func(node Node) {
				defer wg.Done()

				// 快速失败：检查是否已被取消
				if err := ctx.Err(); err != nil {
					return
				}

				nodeInputs, err := ctx.WaitAll(node.Inputs()...)
				if err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("node %s wait inputs: %w", node.ID(), err))
					mu.Unlock()
					return
				}

				outputs, err := node.Run(ctx, nodeInputs)
				if err != nil {
					ctx.Cancel(err) // 通知其他节点
					mu.Lock()
					errs = append(errs, fmt.Errorf("node %s run: %w", node.ID(), err))
					mu.Unlock()
					return
				}

				if outputs != nil {
					for k, v := range outputs {
						ctx.SetOrUpdate(k, v)
					}
				}
			}(n)
		}

		wg.Wait()

		if len(errs) > 0 {
			return nil, fmt.Errorf("layer %d execution failed: %w", layerIdx, errs[0])
		}
	}

	// 收集输出
	result := make(map[string]any)
	for _, outKey := range tn.outputs {
		val, err := ctx.Get(outKey)
		if err != nil {
			return nil, fmt.Errorf("output key %q not found: %w", outKey, err)
		}
		result[outKey] = val
	}

	return result, nil
}

// GetExecutionOrder 获取拓扑排序后的执行顺序（用于调试）
func (tn *TopologicalNode) GetExecutionOrder() []string {
	return tn.order
}

// GetLayers 获取分层结果（用于调试）
func (tn *TopologicalNode) GetLayers() [][]string {
	result := make([][]string, len(tn.layers))
	for i, layer := range tn.layers {
		result[i] = append([]string{}, layer...)
	}
	return result
}

// GetDependencyGraph 获取依赖图（用于调试）
func (tn *TopologicalNode) GetDependencyGraph() map[string][]string {
	result := make(map[string][]string)
	for k, v := range tn.adjMap {
		result[k] = append([]string{}, v...)
	}
	return result
}
