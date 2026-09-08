package llm

import (
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/observability"
)

// EventGenerateFinished 是 after_response 折叠出的观测记录事件名
// （<组件>.<事实> 点分约定）。
const EventGenerateFinished = "llm.generate_finished"

// Observe 把 llm 运行期事实的观测折叠挂到宿主传入的 scope：监听
// after_response 折成 Record 写 cfg.Sink——HostID/TraceID 取自 cfg
// （适配层从不自造序号），模型、token 用量与实例身份进 Attrs（key
// 契约见本包 Attr* 常量）。
//
// scope 必须与请求的派发作用域一致（llm.WithEventScope 注入的同一
// scope）：observed 层派发走 EmitLocal，只本 scope 可见，挂错 scope
// 什么也听不到。cfg.Sink 为 nil 返回哨兵错误。
//
// 重复调用：同一 scope 重复 Observe = 双监听双记录，属宿主装配
// 错误（不做幂等去重，语义靠本文档钉死）。
//
// Duration 计时：锚点 Started 由拦截包装在模型调用入口随事件携带
// （ResponseEvent.Started，waterfall 链之后、inner 调用之前），折叠
// 取 time.Since(ev.Started)——同 scope 并发 Generate 各自携带锚点，
// 无共享状态，并发安全（多实例互不串扰）。本包不订阅
// before_generate：原「计时起点例外」（D5）已由载荷锚点取代，
// waterfall 链保持纯净。
func Observe(scope *kernel.Context, cfg observability.ObserveConfig) error {
	if scope == nil {
		return observability.ErrNilScope
	}
	if cfg.Sink == nil {
		return observability.ErrNilSink
	}
	if _, err := kernel.On(scope, EventAfterResponse, func(ev *ResponseEvent) {
		rec := observability.Record{
			HostID:   cfg.HostID,
			TraceID:  cfg.TraceID,
			Source:   observability.SourceAdapter,
			Event:    EventGenerateFinished,
			Status:   string(ev.Response.FinishReason),
			Duration: time.Since(ev.Started),
		}
		observability.Set(&rec.Attrs, AttrModel, ev.Response.Model)
		observability.Set(&rec.Attrs, AttrInstance, ev.Instance)
		// TokenUsage 用 int；attrs 契约锁 ~int64，此处显式收窄。
		observability.Set(&rec.Attrs, AttrTokensIn, int64(ev.Response.Usage.InputTokens))
		observability.Set(&rec.Attrs, AttrTokensOut, int64(ev.Response.Usage.OutputTokens))
		observability.Set(&rec.Attrs, AttrTokensCached, int64(ev.Response.Usage.CachedInputTokens))
		cfg.Sink.Write(rec)
	}); err != nil {
		return err
	}
	return nil
}
