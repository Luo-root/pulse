package node

import (
	"github.com/Luo-root/pulse/components/schema"
)

// Aspect 切面接口
// 对应你要的三种类型
type Aspect interface {
	// Before 节点执行前调用
	Before(ctx *schema.FlowContext, node Node)

	// After 节点执行后调用
	After(ctx *schema.FlowContext, node Node, err error)
}

// Interceptor 是能够拦截节点执行的切面（AOP 增强）
// 它包装了实际的节点执行逻辑，可实现重试、超时、熔断、兜底等高级控制。
// Interceptor 也是 Aspect，因此可以直接通过 AddAspect 添加到节点或工作流。
type Interceptor interface {
	Aspect
	// Around 包装节点执行，构建洋葱调用链
	// next: 调用下一个拦截器或实际节点执行
	// 返回: 节点输出和错误
	Around(ctx *schema.FlowContext, node Node, next func() (map[string]any, error)) (map[string]any, error)
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
