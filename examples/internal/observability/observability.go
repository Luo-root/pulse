// Package observability 为 examples 提供基于 kernel 事件和 flow 切面的最小观测插件。
//
// 它只依赖 Pulse 当前公开 API，作为未来正式 observability 包的验证原型。
// 本包不进入 kernel 正式 API。
package observability

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/kernel/flow"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
)

// ServiceKey 让装配完成后的 demo 可以通过 kernel 获取 Reporter。
var ServiceKey = kernel.NewServiceKey[*Reporter]("pulse.examples.observability")

// Record 是安全最小化的结构化诊断记录。
// 它不承载 prompt、附件字节、密钥或思维链。
type Record struct {
	Time       time.Time
	TraceID    string
	Layer      string
	Event      string
	Duration   time.Duration
	NodeID     string
	Step       int
	ToolName   string
	Status     string
	Attributes map[string]any
	Err        error
}

// Sink 是日志/指标后端边界。demo 默认使用 SlogSink；测试或后续 exporter 可以替换。
type Sink interface {
	Write(context.Context, Record)
}

// SlogSink 把记录输出为 slog 结构化日志。
type SlogSink struct{ Logger *slog.Logger }

// Write 实现 Sink。
func (s SlogSink) Write(_ context.Context, r Record) {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []any{
		"trace_id", r.TraceID,
		"layer", r.Layer,
		"event", r.Event,
		"duration_ms", r.Duration.Milliseconds(),
	}
	if r.NodeID != "" {
		attrs = append(attrs, "node", r.NodeID)
	}
	if r.Step != 0 {
		attrs = append(attrs, "step", r.Step)
	}
	if r.ToolName != "" {
		attrs = append(attrs, "tool", r.ToolName)
	}
	if r.Status != "" {
		attrs = append(attrs, "status", r.Status)
	}
	if r.Err != nil {
		attrs = append(attrs, "error", r.Err.Error())
	}
	for key, value := range r.Attributes {
		attrs = append(attrs, key, value)
	}
	logger.LogAttrs(context.Background(), slog.LevelInfo, "pulse demo trace", pairsToAttrs(attrs)...)
}

func pairsToAttrs(values []any) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(values)/2)
	for i := 0; i+1 < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok {
			continue
		}
		attrs = append(attrs, slog.Any(key, values[i+1]))
	}
	return attrs
}

// MemorySink 把记录保存在内存中，供示例测试断言。
type MemorySink struct {
	mu      sync.Mutex
	Records []Record
}

// Write 实现 Sink。
func (s *MemorySink) Write(_ context.Context, r Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Records = append(s.Records, r)
}

// Snapshot 返回当前记录副本。
func (s *MemorySink) Snapshot() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.Records))
	copy(out, s.Records)
	return out
}

// MultiSink 把同一条记录写到多个 Sink。
type MultiSink []Sink

// Write 实现 Sink。
func (s MultiSink) Write(ctx context.Context, r Record) {
	for _, sink := range s {
		if sink != nil {
			sink.Write(ctx, r)
		}
	}
}

// Reporter 负责记录跨 llm / loop / flow 的一次请求观测。
type Reporter struct {
	traceID string
	sink    Sink

	mu          sync.Mutex
	turnStarted time.Time
	steps       map[int]time.Time
	requests    []time.Time

	nodeRunning atomic.Int32
	nodeMax     atomic.Int32
}

// NewReporter 构造一个 trace Reporter。
func NewReporter(traceID string, sink Sink) *Reporter {
	if traceID == "" {
		traceID = "trace"
	}
	return &Reporter{traceID: traceID, sink: sink, steps: make(map[int]time.Time)}
}

// TraceID 返回此 Reporter 所属的请求标识。
func (r *Reporter) TraceID() string { return r.traceID }

func (r *Reporter) write(ctx context.Context, record Record) {
	record.Time = time.Now()
	record.TraceID = r.traceID
	if r.sink != nil {
		r.sink.Write(ctx, record)
	}
}

// FlowAspect 返回 Graph 可安装的观测切面。
// 现有 flow seam 覆盖等待输入与节点执行，因此这里记录的是 node_total_ms。
func (r *Reporter) FlowAspect() flow.Aspect {
	return flow.AspectFunc(func(rc *flow.RunCtx, next func(*flow.RunCtx) error) error {
		started := time.Now()
		current := r.nodeRunning.Add(1)
		for {
			old := r.nodeMax.Load()
			if current <= old || r.nodeMax.CompareAndSwap(old, current) {
				break
			}
		}
		defer r.nodeRunning.Add(-1)

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
		r.write(rc.Context(), Record{
			Layer:    "flow",
			Event:    "node_finished",
			NodeID:   rc.NodeID(),
			Status:   status,
			Duration: time.Since(started),
			Err:      err,
		})
		return err
	})
}

// AliveNodesPeak 返回切面内同时存活的节点峰值（含仍在等待输入的节点）。
// 注意：这不是「正在执行用户 Run」的并发上限——执行并发由 WithMaxRunning 控制，
// 切面在 WaitAll 之外，等待输入同样计入存活。
func (r *Reporter) AliveNodesPeak() int32 { return r.nodeMax.Load() }

// Plugin 订阅 llm 与 loop 的公开事件，并将 Reporter 作为服务提供。
func Plugin(traceID string, sink Sink) kernel.Plugin {
	return kernel.Func(func(c *kernel.Context) error {
		reporter := NewReporter(traceID, sink)
		if _, err := kernel.Provide(c, ServiceKey, reporter); err != nil {
			return err
		}
		if _, err := kernel.OnWaterfall(c, llm.EventBeforeGenerate,
			func(req *llm.GenerateRequest, next func(*llm.GenerateRequest) *llm.GenerateRequest) *llm.GenerateRequest {
				reporter.mu.Lock()
				reporter.requests = append(reporter.requests, time.Now())
				reporter.mu.Unlock()
				reporter.write(context.Background(), Record{
					Layer:      "llm",
					Event:      "generate_started",
					Attributes: InputSummary(req.Messages),
				})
				return next(req)
			}); err != nil {
			return err
		}
		if _, err := kernel.On(c, llm.EventAfterResponse, func(resp *llm.Response) {
			started := reporter.popRequestStart()
			reporter.write(context.Background(), Record{
				Layer:    "llm",
				Event:    "generate_finished",
				Duration: time.Since(started),
				Status:   string(resp.FinishReason),
				Attributes: map[string]any{
					"input_tokens":        resp.Usage.InputTokens,
					"output_tokens":       resp.Usage.OutputTokens,
					"cached_input_tokens": resp.Usage.CachedInputTokens,
				},
			})
		}); err != nil {
			return err
		}
		if _, err := kernel.On(c, loop.EventTurnStart, func(turn *loop.TurnStart) {
			reporter.mu.Lock()
			reporter.turnStarted = time.Now()
			reporter.mu.Unlock()
			reporter.write(context.Background(), Record{
				Layer: "loop",
				Event: "turn_started",
				Attributes: map[string]any{
					"history_messages": len(turn.History),
					"input_messages":   len(turn.Input),
				},
			})
		}); err != nil {
			return err
		}
		if _, err := kernel.On(c, loop.EventStepStart, func(step *loop.StepStart) {
			reporter.mu.Lock()
			reporter.steps[step.Step] = time.Now()
			reporter.mu.Unlock()
			reporter.write(context.Background(), Record{Layer: "loop", Event: "step_started", Step: step.Step})
		}); err != nil {
			return err
		}
		if _, err := kernel.On(c, loop.EventAfterModel, func(after *loop.AfterModel) {
			started := reporter.popStepStart(after.Step)
			status := ""
			if after.Response != nil {
				status = string(after.Response.FinishReason)
			}
			reporter.write(context.Background(), Record{
				Layer:    "loop",
				Event:    "model_finished",
				Step:     after.Step,
				Duration: time.Since(started),
				Status:   status,
			})
		}); err != nil {
			return err
		}
		if _, err := kernel.On(c, loop.EventAfterToolCall, func(after *loop.AfterToolCall) {
			status := "completed"
			if after.Rejected {
				status = "rejected"
			} else if after.Err != nil {
				status = "failed"
			}
			reporter.write(context.Background(), Record{
				Layer:    "tool",
				Event:    "finished",
				ToolName: after.Call.Name,
				Duration: after.Duration,
				Status:   status,
				Err:      after.Err,
			})
		}); err != nil {
			return err
		}
		if _, err := kernel.On(c, loop.EventTurnEnd, func(end *loop.TurnEnd) {
			reporter.mu.Lock()
			started := reporter.turnStarted
			reporter.mu.Unlock()
			reporter.write(context.Background(), Record{
				Layer:    "loop",
				Event:    "turn_finished",
				Duration: time.Since(started),
				Status:   string(end.StoppedBy),
				Attributes: map[string]any{
					"steps":         end.Steps,
					"input_tokens":  end.Usage.InputTokens,
					"output_tokens": end.Usage.OutputTokens,
				},
			})
		}); err != nil {
			return err
		}
		return nil
	})
}

func (r *Reporter) popRequestStart() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		return time.Now()
	}
	started := r.requests[0]
	r.requests = r.requests[1:]
	return started
}

func (r *Reporter) popStepStart(step int) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	started, ok := r.steps[step]
	if !ok {
		return time.Now()
	}
	delete(r.steps, step)
	return started
}

// InputSummary 只统计模态数量与内联字节数，不读取内容。
func InputSummary(messages []*llm.Message) map[string]any {
	var text, image, custom, bytes int
	for _, message := range messages {
		if message == nil {
			continue
		}
		for _, part := range message.Parts {
			switch part.Kind {
			case llm.PartText:
				text++
			case llm.PartImage:
				image++
				if part.Image != nil {
					bytes += len(part.Image.Data)
				}
			case llm.PartCustom:
				custom++
				if part.Media != nil {
					bytes += len(part.Media.Data)
				}
			}
		}
	}
	return map[string]any{
		"messages":           len(messages),
		"text_parts":         text,
		"image_parts":        image,
		"custom_parts":       custom,
		"inline_media_bytes": bytes,
	}
}
