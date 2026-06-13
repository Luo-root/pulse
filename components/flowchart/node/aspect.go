package node

import (
	"context"
	"fmt"

	"github.com/Luo-root/pulse/components/flowchart/flow"
)

// AspectContext 切面执行上下文
// 每层拦截器拥有独立的可取消 context，实现超时、熔断等节点级控制
type AspectContext struct {
	Flow   *flow.FlowContext // 工作流共享数据
	ctx    context.Context   // 本层可取消的 context
	cancel context.CancelFunc
}

// NewAspectContext 创建切面上下文
func NewAspectContext(flowCtx *flow.FlowContext, parent context.Context) *AspectContext {
	ctx, cancel := context.WithCancel(parent)
	return &AspectContext{Flow: flowCtx, ctx: ctx, cancel: cancel}
}

// Context 获取本层的可取消 context（用于超时、取消控制）
func (ac *AspectContext) Context() context.Context {
	return ac.ctx
}

// Done 返回本层 context 的 Done channel
func (ac *AspectContext) Done() <-chan struct{} {
	return ac.ctx.Done()
}

// Cancel 取消本层 context
func (ac *AspectContext) Cancel(err error) {
	ac.cancel()
}

// Aspect 统一切面接口
// 所有切面（Before/After/Around/Retry/Timeout/CircuitBreaker）都实现此接口
type Aspect interface {
	Around(ctx *AspectContext, node Node, next func() (map[string]any, error)) (map[string]any, error)
}

// ============================================================================
// 便捷构造器
// ============================================================================

// BeforeFunc 创建前置切面：执行 fn 后调用 next
func BeforeFunc(fn func(ctx *AspectContext, node Node)) Aspect {
	return &beforeAspect{fn: fn}
}

type beforeAspect struct {
	fn func(ctx *AspectContext, node Node)
}

func (a *beforeAspect) Around(ctx *AspectContext, node Node, next func() (map[string]any, error)) (map[string]any, error) {
	a.fn(ctx, node)
	return next()
}

// AfterFunc 创建后置切面：调用 next 后执行 fn
func AfterFunc(fn func(ctx *AspectContext, node Node, err error)) Aspect {
	return &afterAspect{fn: fn}
}

type afterAspect struct {
	fn func(ctx *AspectContext, node Node, err error)
}

func (a *afterAspect) Around(ctx *AspectContext, node Node, next func() (map[string]any, error)) (map[string]any, error) {
	out, err := next()
	a.fn(ctx, node, err)
	return out, err
}

// AroundFunc 创建环绕切面
func AroundFunc(fn func(ctx *AspectContext, node Node, next func() (map[string]any, error)) (map[string]any, error)) Aspect {
	return &aroundAspect{fn: fn}
}

type aroundAspect struct {
	fn func(ctx *AspectContext, node Node, next func() (map[string]any, error)) (map[string]any, error)
}

func (a *aroundAspect) Around(ctx *AspectContext, node Node, next func() (map[string]any, error)) (map[string]any, error) {
	return a.fn(ctx, node, next)
}

// ============================================================================
// 切面链构建
// ============================================================================

// BuildChain 将切面列表构建为洋葱调用链
// 最外层的切面最先执行（index 0 在最外层）
// core 是最内层的实际执行逻辑
func BuildChain(aspects []Aspect, core func() (map[string]any, error)) func(ac *AspectContext) (map[string]any, error) {
	invoker := func(ac *AspectContext) (map[string]any, error) {
		return core()
	}

	for i := len(aspects) - 1; i >= 0; i-- {
		aspect := aspects[i]
		next := invoker
		invoker = func(ac *AspectContext) (map[string]any, error) {
			return aspect.Around(ac, nil, func() (map[string]any, error) {
				return next(ac)
			})
		}
	}

	return invoker
}

// BuildNodeChain 为节点构建切面调用链
// aspects: 全局切面 + 节点切面
// node: 当前节点
// runFunc: 节点执行逻辑，接收 AspectContext 以使用切面级 context 等待数据
func BuildNodeChain(aspects []Aspect, node Node, runFunc func(ac *AspectContext) (map[string]any, error)) func(ac *AspectContext) (map[string]any, error) {
	invoker := func(ac *AspectContext) (map[string]any, error) {
		return runFunc(ac)
	}

	for i := len(aspects) - 1; i >= 0; i-- {
		aspect := aspects[i]
		next := invoker
		invoker = func(ac *AspectContext) (map[string]any, error) {
			return aspect.Around(ac, node, func() (map[string]any, error) {
				return next(ac)
			})
		}
	}

	return invoker
}

// ============================================================================
// 内置切面：ErrorSwallow
// ============================================================================

// ErrorSwallowAspect 拦截 error，执行降级逻辑
// 作为最外层切面使用，确保节点级别的错误不会传播到工作流层
type ErrorSwallowAspect struct {
	FallbackFunc func(ctx *AspectContext, node Node, err error) (map[string]any, error)
}

func NewErrorSwallowAspect(fallback func(ctx *AspectContext, node Node, err error) (map[string]any, error)) *ErrorSwallowAspect {
	return &ErrorSwallowAspect{FallbackFunc: fallback}
}

func (e *ErrorSwallowAspect) Around(ctx *AspectContext, node Node, next func() (map[string]any, error)) (map[string]any, error) {
	out, err := next()
	if err != nil && e.FallbackFunc != nil {
		return e.FallbackFunc(ctx, node, err)
	}
	return out, err
}

// ============================================================================
// 内置切面：Recovery（panic 捕获）
// ============================================================================

// RecoveryAspect 捕获 panic 并执行兜底逻辑
type RecoveryAspect struct {
	FallbackFunc func(ctx *AspectContext, node Node, recoverVal any) (map[string]any, error)
}

func NewRecoveryAspect(fallback func(ctx *AspectContext, node Node, recoverVal any) (map[string]any, error)) *RecoveryAspect {
	return &RecoveryAspect{FallbackFunc: fallback}
}

func (r *RecoveryAspect) Around(ctx *AspectContext, node Node, next func() (map[string]any, error)) (outputs map[string]any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			if r.FallbackFunc != nil {
				outputs, err = r.FallbackFunc(ctx, node, rec)
			} else {
				err = fmt.Errorf("panic in node %s: %v", node.ID(), rec)
			}
		}
	}()
	return next()
}
