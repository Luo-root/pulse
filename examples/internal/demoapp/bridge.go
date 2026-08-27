// bridge.go 是装配层观测桥：订阅 llm/loop 的公开事件并把运行期
// 事实折进 observability.Record 写同一 Sink。
//
// 边界（docs/design/observability-v1-design.md §2）：
//   - 允许 import llm/loop/flow——本文件属于装配层；
//   - TraceID/Duration/Status 由这里填充；token 数走 slog 附加键；
//   - 官方 Record 的装配专用字段保持零值。
package demoapp

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/kernel/flow"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/observability"
)

// Bridge 聚合一个请求的运行期观测状态。
type Bridge struct {
	Sink    observability.Sink
	TraceID string

	mu          sync.Mutex
	genStart    time.Time // 最近一次模型调用开始时刻
	lastTurnEnd time.Time
}

// InstallBridge 订阅 llm/loop 公开事件。观察语义：On 与「仅透传」的
// Waterfall（llm 层 hook 本身是 waterfall，桥必须委托 next）。
func InstallBridge(scope *kernel.Context, sink observability.Sink, traceID string) (*Bridge, error) {
	b := &Bridge{Sink: sink, TraceID: traceID}

	if _, err := kernel.OnWaterfall(scope, llm.EventBeforeGenerate,
		func(req *llm.GenerateRequest, next func(*llm.GenerateRequest) *llm.GenerateRequest) *llm.GenerateRequest {
			b.mu.Lock()
			b.genStart = time.Now()
			b.mu.Unlock()
			return next(req)
		}); err != nil {
		return nil, err
	}
	if _, err := kernel.On(scope, llm.EventAfterResponse, func(resp *llm.Response) {
		b.mu.Lock()
		started := b.genStart
		b.mu.Unlock()
		sink.Write(observability.Record{
			TraceID:  b.TraceID,
			Source:   observability.SourceBridge,
			Event:    "llm.generate_finished",
			Duration: time.Since(started),
			Status:   string(resp.FinishReason),
		})
		slog.Debug("token usage",
			"trace_id", b.TraceID,
			"input_tokens", resp.Usage.InputTokens,
			"output_tokens", resp.Usage.OutputTokens,
			"cached_input_tokens", resp.Usage.CachedInputTokens,
		)
	}); err != nil {
		return nil, err
	}
	if _, err := kernel.On(scope, loop.EventAfterToolCall, func(after *loop.AfterToolCall) {
		status := "completed"
		switch {
		case after.Rejected:
			status = "rejected"
		case after.Err != nil:
			status = "failed"
		}
		sink.Write(observability.Record{
			TraceID:  b.TraceID,
			Source:   observability.SourceBridge,
			Event:    "loop.tool_finished",
			Status:   status,
			Duration: after.Duration,
			Err:      after.Err,
		})
	}); err != nil {
		return nil, err
	}
	if _, err := kernel.On(scope, loop.EventTurnEnd, func(end *loop.TurnEnd) {
		b.mu.Lock()
		started := b.lastTurnEnd
		b.mu.Unlock()
		_ = started // turn 总耗时不在此拼接；起点由 turn_start 记录于调用侧日志
		slog.Debug("turn summary",
			"trace_id", b.TraceID,
			"steps", end.Steps,
			"stopped_by", end.StoppedBy,
			"input_tokens", end.Usage.InputTokens,
			"output_tokens", end.Usage.OutputTokens,
		)
	}); err != nil {
		return nil, err
	}
	return b, nil
}

// FlowPeak 记录 flow 切面观测到的并发存活峰值（原子）。
type FlowPeak struct{ v atomic.Int32 }

// Peak 返回当前峰值。
func (p *FlowPeak) Peak() int32 { return p.v.Load() }

// FlowAspect 返回 Graph 观测切面：node_total_ms + 并发存活峰值。
// wait/run 分段等待 flow E1 落地后接入（flow 设计稿·演进路线 E1）。
func FlowAspect(sink observability.Sink, traceID string, peak *FlowPeak) flow.Aspect {
	return flow.AspectFunc(func(rc *flow.RunCtx, next func(*flow.RunCtx) error) error {
		cur := peak.v.Add(1)
		for {
			old := peak.v.Load()
			if cur <= old || peak.v.CompareAndSwap(old, cur) {
				break
			}
		}
		defer peak.v.Add(-1)

		started := time.Now()
		err := next(rc)
		status := "completed"
		switch {
		case err == nil:
		case errors.Is(err, flow.ErrSkipped):
			status = "skipped"
		case rc.Context().Err() != nil:
			status = "canceled"
		default:
			status = "failed"
		}
		sink.Write(observability.Record{
			TraceID:  traceID,
			Source:   observability.SourceBridge,
			Event:    "flow.node_finished",
			Status:   status,
			Duration: time.Since(started),
			Err:      err,
		})
		return err
	})
}
