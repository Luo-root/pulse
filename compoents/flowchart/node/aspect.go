package node

import (
	"github.com/Luo-root/pulse/compoents/schema"
)

// Aspect 切面接口
// 对应你要的三种类型
type Aspect interface {
	// Before 节点执行前调用
	Before(ctx *schema.FlowContext, node Node)

	// After 节点执行后调用
	After(ctx *schema.FlowContext, node Node, err error)
}

// BeforeAspect 简易实现：只执行 Before
type BeforeAspect struct {
	Fn func(ctx *schema.FlowContext, node Node)
}

func (a *BeforeAspect) Before(ctx *schema.FlowContext, node Node) {
	a.Fn(ctx, node)
}
func (a *BeforeAspect) After(ctx *schema.FlowContext, node Node, err error) {}

// AfterAspect 简易实现：只执行 After
type AfterAspect struct {
	Fn func(ctx *schema.FlowContext, node Node, err error)
}

func (a *AfterAspect) Before(ctx *schema.FlowContext, node Node)           {}
func (a *AfterAspect) After(ctx *schema.FlowContext, node Node, err error) { a.Fn(ctx, node, err) }

// AroundAspect 简易实现：前后都执行
type AroundAspect struct {
	BeforeFn func(ctx *schema.FlowContext, node Node)
	AfterFn  func(ctx *schema.FlowContext, node Node, err error)
}

func (a *AroundAspect) Before(ctx *schema.FlowContext, node Node) {
	if a.BeforeFn != nil {
		a.BeforeFn(ctx, node)
	}
}
func (a *AroundAspect) After(ctx *schema.FlowContext, node Node, err error) {
	if a.AfterFn != nil {
		a.AfterFn(ctx, node, err)
	}
}
