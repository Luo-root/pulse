// Package demoapp bridge.go 是装配层观测桥：订阅 llm/loop 的公开事件并把运行期
// 事实折进 observability.Record 写同一 Sink。
//
// 边界（docs/design/observability-v1-design.md §2、§6）：
//   - 允许 import llm/loop/flow——本文件属于装配层；
//   - D3 两层标识在桥处合流：每条运行期记录同时携带
//     HostID（宿主稳定）与当次请求的 TraceID；
//   - 官方 Record 的装配专用字段保持零值；token 数走 slog 附加键。
package demoapp

import (
	"errors"
	"fmt"
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

// Bridge 聚合一次请求的运行期观测状态。每个用户请求创建一个实例
// （TraceID 独立），生命周期 = 该请求。
type Bridge struct {
	Sink    observability.Sink
	HostID  string
	TraceID string

	mu       sync.Mutex
	genStart time.Time // 最近一次模型调用开始时刻
	turnEnd  time.Time // 最近一次回合结束时刻（流式时长差值基数）
}

var bridgeSeq atomic.Uint64

// InstallBridge 为一次请求安装运行期桥，返回带独立 trace_id 的桥实例。
func InstallBridge(scope *kernel.Context, sink observability.Sink, hostID string) (*Bridge, error) {
	b := &Bridge{
		Sink:    sink,
		HostID:  hostID,
		TraceID: fmt.Sprintf("%s-trace-%d", hostID, bridgeSeq.Add(1)),
	}

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
			HostID:   b.HostID,
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
			HostID:   b.HostID,
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
		prev := b.turnEnd
		now := time.Now()
		b.turnEnd = now
		b.mu.Unlock()
		var dur time.Duration
		if !prev.IsZero() && now.After(prev) {
			dur = now.Sub(prev)
		}
		slog.Info("turn summary",
			"host_id", b.HostID,
			"trace_id", b.TraceID,
			"steps", end.Steps,
			"stopped_by", end.StoppedBy,
			"since_prev_turn_ms", dur.Milliseconds(),
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
func (b *Bridge) FlowAspect(peak *FlowPeak) flow.Aspect {
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
		b.Sink.Write(observability.Record{
			HostID:   b.HostID,
			TraceID:  b.TraceID,
			Source:   observability.SourceBridge,
			Event:    "flow.node_finished",
			Status:   status,
			Duration: time.Since(started),
			Err:      err,
		})
		return err
	})
}

// Write 让桥可以直接把自定义事实写进同一出口（runGraph 汇总行等）。
func (b *Bridge) Write(event, status string) {
	b.Sink.Write(observability.Record{
		HostID:  b.HostID,
		TraceID: b.TraceID,
		Source:  observability.SourceBridge,
		Event:   event,
		Status:  status,
	})
}
