package llm

import (
	"sync"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/observability"
)

// EventGenerateFinished 是 after_response 折叠出的观测记录事件名
// （<组件>.<事实> 点分约定）。
const EventGenerateFinished = "llm.generate_finished"

// Observe 把 llm 运行期事实的观测折叠挂到宿主传入的 scope：监听
// 自身 before_generate（只计时，waterfall 透传）与 after_response，
// 折成 Record 写 cfg.Sink——HostID/TraceID 取自 cfg（适配层从不自造
// 序号），模型与 token 用量进 Attrs（key 契约见本包 Attr* 常量）。
//
// scope 必须与请求的派发作用域一致（llm.WithEventScope 注入的同一
// scope）：observed 层派发走 EmitLocal/WaterfallLocal，只本 scope
// 可见，挂错 scope 什么也听不到。cfg.Sink 为 nil 返回哨兵错误。
//
// 重复调用：同一 scope 重复 Observe = 双监听双记录，属宿主装配
// 错误（不做幂等去重，语义靠本文档钉死）。
//
// before_generate 计时起点说明：after_response 不携带耗时，Waterfall
// 回调是唯一可行的计时起点；恒 next 且不改参数的观察者不改变
// Waterfall 语义（D5 计量起点例外，见 observability-v1-design.md）。
// genStart 为本函数闭包状态，同 scope 并发发起 Generate 时计时不
// 定义（ReAct 循环内串行，常规用法不受影响）。
func Observe(scope *kernel.Context, cfg observability.ObserveConfig) error {
	if scope == nil {
		return observability.ErrNilScope
	}
	if cfg.Sink == nil {
		return observability.ErrNilSink
	}
	var mu sync.Mutex
	var genStart time.Time
	if _, err := kernel.OnWaterfall(scope, EventBeforeGenerate,
		func(req *GenerateRequest, next func(*GenerateRequest) *GenerateRequest) *GenerateRequest {
			mu.Lock()
			genStart = time.Now()
			mu.Unlock()
			return next(req)
		}); err != nil {
		return err
	}
	if _, err := kernel.On(scope, EventAfterResponse, func(resp *Response) {
		mu.Lock()
		started := genStart
		mu.Unlock()
		rec := observability.Record{
			HostID:   cfg.HostID,
			TraceID:  cfg.TraceID,
			Source:   observability.SourceAdapter,
			Event:    EventGenerateFinished,
			Status:   string(resp.FinishReason),
			Duration: time.Since(started),
		}
		observability.Set(&rec.Attrs, AttrModel, resp.Model)
		// TokenUsage 用 int；attrs 契约锁 ~int64，此处显式收窄。
		observability.Set(&rec.Attrs, AttrTokensIn, int64(resp.Usage.InputTokens))
		observability.Set(&rec.Attrs, AttrTokensOut, int64(resp.Usage.OutputTokens))
		observability.Set(&rec.Attrs, AttrTokensCached, int64(resp.Usage.CachedInputTokens))
		cfg.Sink.Write(rec)
	}); err != nil {
		return err
	}
	return nil
}
