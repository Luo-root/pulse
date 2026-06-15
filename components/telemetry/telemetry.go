// Package telemetry 提供可选的可观测性集成
// 包含 Aspect 切面用于工作流节点追踪，和 Agent 结构化日志
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Luo-root/pulse/components/flowchart/node"
)

// ============================================================================
// Tracer 接口（可选 OTel 实现）
// ============================================================================

// Span 追踪跨度
type Span interface {
	// SetAttribute 设置属性
	SetAttribute(key string, value any)
	// SetStatus 设置状态
	SetStatus(code SpanCode, description string)
	// RecordError 记录错误
	RecordError(err error)
	// End 结束跨度
	End()
}

// SpanCode 跨度状态码
type SpanCode int

const (
	SpanOK SpanCode = iota
	SpanError
)

// Tracer 追踪器接口
type Tracer interface {
	// Start 开始一个新跨度
	Start(ctx context.Context, name string) (context.Context, Span)
}

// ============================================================================
// 内置简单 Tracer（基于 slog）
// ============================================================================

// SlogTracer 基于 slog 的简单追踪器
// 不需要外部依赖，适合开发和调试
type SlogTracer struct {
	Logger *slog.Logger
}

func NewSlogTracer(logger *slog.Logger) *SlogTracer {
	if logger == nil {
		logger = slog.Default()
	}
	return &SlogTracer{Logger: logger}
}

func (t *SlogTracer) Start(ctx context.Context, name string) (context.Context, Span) {
	return ctx, &slogSpan{tracer: t, name: name, start: time.Now()}
}

type slogSpan struct {
	tracer *SlogTracer
	name   string
	start  time.Time
	attrs  map[string]any
}

func (s *slogSpan) SetAttribute(key string, value any) {
	if s.attrs == nil {
		s.attrs = make(map[string]any)
	}
	s.attrs[key] = value
}

func (s *slogSpan) SetStatus(code SpanCode, description string) {
	s.attrs["_status"] = fmt.Sprintf("%d: %s", code, description)
}

func (s *slogSpan) RecordError(err error) {
	s.attrs["_error"] = err.Error()
}

func (s *slogSpan) End() {
	elapsed := time.Since(s.start)
	attrs := []any{
		slog.String("span", s.name),
		slog.Duration("duration", elapsed),
	}
	for k, v := range s.attrs {
		attrs = append(attrs, slog.Any(k, v))
	}
	s.tracer.Logger.Info("span", attrs...)
}

// ============================================================================
// NoOpTracer 空实现（默认，零开销）
// ============================================================================

type NoOpTracer struct{}

func (NoOpTracer) Start(ctx context.Context, name string) (context.Context, Span) {
	return ctx, &noOpSpan{}
}

type noOpSpan struct{}

func (*noOpSpan) SetAttribute(string, any)   {}
func (*noOpSpan) SetStatus(SpanCode, string) {}
func (*noOpSpan) RecordError(error)          {}
func (*noOpSpan) End()                       {}

// ============================================================================
// WorkflowAspect 工作流节点追踪切面
// ============================================================================

// WorkflowAspect 工作流节点追踪切面
// 在每个节点执行前后记录 span
type WorkflowAspect struct {
	tracer Tracer
}

func NewWorkflowAspect(tracer Tracer) *WorkflowAspect {
	return &WorkflowAspect{tracer: tracer}
}

func (a *WorkflowAspect) Around(ctx *node.AspectContext, n node.Node, next func() (map[string]any, error)) (map[string]any, error) {
	spanCtx, span := a.tracer.Start(ctx.Context(), "node."+n.ID())
	_ = spanCtx

	span.SetAttribute("node.id", n.ID())
	span.SetAttribute("node.inputs", n.Inputs())
	span.SetAttribute("node.outputs", n.Outputs())

	result, err := next()

	if err != nil {
		span.RecordError(err)
		span.SetStatus(SpanError, err.Error())
	} else {
		span.SetStatus(SpanOK, "ok")
	}

	span.End()
	return result, err
}

// ============================================================================
// AgentLogger Agent 结构化日志
// ============================================================================

// AgentLogger Agent 操作日志
type AgentLogger struct {
	Logger *slog.Logger
}

func NewAgentLogger(logger *slog.Logger) *AgentLogger {
	if logger == nil {
		logger = slog.Default()
	}
	return &AgentLogger{Logger: logger}
}

// LogToolCall 记录工具调用
func (l *AgentLogger) LogToolCall(toolName string, args map[string]any, duration time.Duration, err error) {
	attrs := []any{
		slog.String("tool", toolName),
		slog.Duration("duration", duration),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
		l.Logger.Warn("tool_call_failed", attrs...)
	} else {
		l.Logger.Info("tool_call", attrs...)
	}
}

// LogModelCall 记录模型调用
func (l *AgentLogger) LogModelCall(model string, tokens int, duration time.Duration, err error) {
	attrs := []any{
		slog.String("model", model),
		slog.Int("tokens", tokens),
		slog.Duration("duration", duration),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
		l.Logger.Warn("model_call_failed", attrs...)
	} else {
		l.Logger.Info("model_call", attrs...)
	}
}

// LogAgentRound 记录 Agent 循环轮次
func (l *AgentLogger) LogAgentRound(round int, toolCalls int, hasResponse bool) {
	l.Logger.Info("agent_round",
		slog.Int("round", round),
		slog.Int("tool_calls", toolCalls),
		slog.Bool("has_response", hasResponse),
	)
}
