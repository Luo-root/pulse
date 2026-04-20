package flowchart

import (
	"Pulse/compoents/flowchart/node"
	"Pulse/compoents/schema"
)

// Workflow 工作流引擎
type Workflow struct {
	nodes   []node.Node
	ctx     *schema.FlowContext
	aspects []node.Aspect // 全局切面（所有节点生效）
}

func NewWorkflow() *Workflow {
	return &Workflow{
		ctx: schema.NewFlowContext(),
	}
}

// AddNode 添加节点
func (w *Workflow) AddNode(node node.Node) {
	w.nodes = append(w.nodes, node)
}

// AddAspect 添加全局切面（所有节点执行）
func (w *Workflow) AddAspect(aspect node.Aspect) {
	w.aspects = append(w.aspects, aspect)
}

// Start 启动所有节点（异步、自动等待依赖）
func (w *Workflow) Start() {
	for _, n := range w.nodes {
		go w.runNode(n)
	}
}

// runNode 执行单个节点（包含全局切面 + 节点切面）
func (w *Workflow) runNode(node node.Node) {
	// ==================== 执行所有切面 Before ====================
	for _, a := range w.aspects {
		a.Before(w.ctx, node)
	}
	for _, a := range node.Aspects() {
		a.Before(w.ctx, node)
	}

	// 执行业务
	inputs := w.ctx.WaitAll(node.Inputs()...)
	outputs, err := node.Run(w.ctx, inputs)

	// ==================== 执行所有切面 After ====================
	for _, a := range w.aspects {
		a.After(w.ctx, node, err)
	}
	for _, a := range node.Aspects() {
		a.After(w.ctx, node, err)
	}

	// 输出结果
	if err == nil && outputs != nil {
		for k, v := range outputs {
			w.ctx.Set(k, v)
		}
	}
}

// Input 输入初始数据，启动流程
func (w *Workflow) Input(key string, value any) {
	w.ctx.Set(key, value)
}
