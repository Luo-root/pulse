package llm

import (
	"context"

	"github.com/Luo-root/pulse/kernel"
)

// eventScopeKey 是 context.Context 中请求级事件作用域的键。
// loop 在调用 Generate/Stream 前用 WithEventScope 注入；observed
// 从 ctx 取出后对该 scope 做 Local 派发——挂在 reqScope 的 Bridge
// 才能只听到本请求。
type eventScopeKey struct{}

// WithEventScope 把请求级 kernel 作用域写入 ctx。
// scope 为 nil 时返回原 ctx（调用方可省略）。
func WithEventScope(ctx context.Context, scope *kernel.Context) context.Context {
	if scope == nil {
		return ctx
	}
	return context.WithValue(ctx, eventScopeKey{}, scope)
}

// EventScopeFrom 取出请求级事件作用域；没有则返回 nil。
func EventScopeFrom(ctx context.Context) *kernel.Context {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(eventScopeKey{}).(*kernel.Context)
	return s
}
