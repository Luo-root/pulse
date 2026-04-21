package node

import (
	"github.com/Luo-root/pulse/compoents/schema"
)

// Node 工作流节点
type Node interface {
	ID() string
	Inputs() []string  // 依赖的输入 keys
	Outputs() []string // 输出 keys

	// Run 执行节点业务逻辑
	Run(ctx *schema.FlowContext, inputs map[string]any) (outputs map[string]any, err error)

	// Aspects 节点自己的切面（AOP）
	Aspects() []Aspect
}

// SimpleNode 通用节点
type SimpleNode struct {
	id      string
	inputs  []string
	outputs []string
	runFunc func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error)
	aspects []Aspect
}

// NewNode 🌟 最友好的节点初始化函数
// 只需要传：ID、输入列表、输出列表、执行逻辑
func NewNode(
	id string,
	inputs []string,
	outputs []string,
	runFunc func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error),
) *SimpleNode {
	return &SimpleNode{
		id:      id,
		inputs:  inputs,
		outputs: outputs,
		runFunc: runFunc,
		aspects: make([]Aspect, 0),
	}
}

// AddAspect 给节点追加切面
func (n *SimpleNode) AddAspect(aspect Aspect) {
	n.aspects = append(n.aspects, aspect)
}

func (n *SimpleNode) ID() string {
	return n.id
}

func (n *SimpleNode) Inputs() []string {
	return n.inputs
}

func (n *SimpleNode) Outputs() []string {
	return n.outputs
}

func (n *SimpleNode) Run(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
	return n.runFunc(ctx, inputs)
}

func (n *SimpleNode) Aspects() []Aspect {
	return n.aspects
}
