package loop

import (
	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/observability"
)

// 观测记录事件名（<组件>.<事实> 点分约定）。
const (
	// EventToolFinished 是 after_tool_call 折叠出的观测记录事件名。
	EventToolFinished = "loop.tool_finished"
	// EventTurnFinished 是 turn_end 折叠出的观测记录事件名。
	EventTurnFinished = "loop.turn_finished"
)

// 工具结果状态（tool_finished 的 Status 取值）：结果语义是本包知识，
// 三态判定（Rejected 优先于 Err）归折叠函数。
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusRejected  = "rejected"
)

// Observe 把 loop 运行期事实的观测折叠挂到宿主传入的 scope：监听
// 自身 after_tool_call 与 turn_end，折成 Record 写 cfg.Sink——
// HostID/TraceID 取自 cfg，工具名与步数进 Attrs（key 契约见本包
// AttrTool / AttrSteps）。
//
// scope 必须与 Agent 的 llm.WithEventScope 相同（Local 派发只本 scope
// 可见）。cfg.Sink 为 nil 返回哨兵错误。同一 scope 重复调用 = 双监听
// 双记录，属宿主装配错误（不做幂等去重）。
//
// before_tool_call 刻意不订阅：AfterToolCall 已自带 Duration/Err，
// 订阅无观测增益，少一份与 HITL 审批链的顺序耦合——与
// llm.Observe 订阅 before_generate 的差异是「有无替代计时手段」
// （after_response 不携带耗时），不是 Waterfall 本身。
//
// token 用量不在 turn_end 重复记录：以 llm.generate_finished 的单次
// 口径为准，本包不做累计（避免同 key 双口径）。
func Observe(scope *kernel.Context, cfg observability.ObserveConfig) error {
	if scope == nil {
		return observability.ErrNilScope
	}
	if cfg.Sink == nil {
		return observability.ErrNilSink
	}
	if _, err := kernel.On(scope, EventAfterToolCall, func(after *AfterToolCall) {
		rec := observability.Record{
			HostID:   cfg.HostID,
			TraceID:  cfg.TraceID,
			Source:   observability.SourceAdapter,
			Event:    EventToolFinished,
			Status:   toolStatus(after),
			Duration: after.Duration,
			Err:      after.Err,
		}
		observability.Set(&rec.Attrs, AttrTool, after.Call.Name)
		cfg.Sink.Write(rec)
	}); err != nil {
		return err
	}
	if _, err := kernel.On(scope, EventTurnEnd, func(end *TurnEnd) {
		rec := observability.Record{
			HostID:  cfg.HostID,
			TraceID: cfg.TraceID,
			Source:  observability.SourceAdapter,
			Event:   EventTurnFinished,
			Status:  string(end.StoppedBy),
		}
		observability.Set(&rec.Attrs, AttrSteps, int64(end.Steps))
		cfg.Sink.Write(rec)
	}); err != nil {
		return err
	}
	return nil
}

// toolStatus 是工具结果的三态判定（Rejected 优先于 Err > 完成）——
// 工具结果语义是本包知识，这里是其唯一事实源，Observe 折叠调用它。
func toolStatus(after *AfterToolCall) string {
	switch {
	case after.Rejected:
		return StatusRejected
	case after.Err != nil:
		return StatusFailed
	}
	return StatusCompleted
}
