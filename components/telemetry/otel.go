package telemetry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// ============================================================================
// OTel Tracer — 包装 OpenTelemetry trace.Tracer
// ============================================================================

// OTelTracer 包装 OpenTelemetry 的 trace.Tracer，实现 Tracer 接口
type OTelTracer struct {
	tracer trace.Tracer
}

// NewOTelTracer 创建基于 OpenTelemetry 的追踪器
// name: instrumentation name，如 "github.com/Luo-root/pulse"
func NewOTelTracer(name string) *OTelTracer {
	return &OTelTracer{
		tracer: otel.Tracer(name),
	}
}

// NewOTelTracerWithProvider 使用指定 TracerProvider 创建追踪器
func NewOTelTracerWithProvider(name string, provider trace.TracerProvider) *OTelTracer {
	return &OTelTracer{
		tracer: provider.Tracer(name),
	}
}

func (t *OTelTracer) Start(ctx context.Context, name string) (context.Context, Span) {
	ctx, span := t.tracer.Start(ctx, name)
	return ctx, &OTelSpan{span: span}
}

// OTelSpan 包装 OpenTelemetry 的 trace.Span，实现 Span 接口
type OTelSpan struct {
	span trace.Span
}

func (s *OTelSpan) SetAttribute(key string, value any) {
	switch v := value.(type) {
	case string:
		s.span.SetAttributes(attribute.String(key, v))
	case int:
		s.span.SetAttributes(attribute.Int(key, v))
	case int64:
		s.span.SetAttributes(attribute.Int64(key, v))
	case float64:
		s.span.SetAttributes(attribute.Float64(key, v))
	case bool:
		s.span.SetAttributes(attribute.Bool(key, v))
	default:
		s.span.SetAttributes(attribute.String(key, fmt.Sprintf("%v", v)))
	}
}

func (s *OTelSpan) SetStatus(code SpanCode, description string) {
	switch code {
	case SpanOK:
		s.span.SetStatus(codes.Ok, description)
	case SpanError:
		s.span.SetStatus(codes.Error, description)
	}
}

func (s *OTelSpan) RecordError(err error) {
	s.span.RecordError(err)
}

func (s *OTelSpan) End() {
	s.span.End()
}

// ============================================================================
// OTel Metrics — 工具调用/模型调用/Agent 循环的指标
// ============================================================================

// OTelMetrics 收集 Agent 操作指标
type OTelMetrics struct {
	toolCalls    metric.Int64Counter
	toolDuration metric.Float64Histogram
	toolErrors   metric.Int64Counter
	modelCalls   metric.Int64Counter
	modelTokens  metric.Int64Counter
	modelErrors  metric.Int64Counter
	agentRounds  metric.Int64Counter
}

// NewOTelMetrics 创建 OTel 指标收集器
func NewOTelMetrics(name string) (*OTelMetrics, error) {
	meter := otel.Meter(name)

	toolCalls, err := meter.Int64Counter("pulse.tool.calls",
		metric.WithDescription("Total tool calls"),
	)
	if err != nil {
		return nil, fmt.Errorf("create tool.calls counter: %w", err)
	}

	toolDuration, err := meter.Float64Histogram("pulse.tool.duration_ms",
		metric.WithDescription("Tool call duration in milliseconds"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return nil, fmt.Errorf("create tool.duration histogram: %w", err)
	}

	toolErrors, err := meter.Int64Counter("pulse.tool.errors",
		metric.WithDescription("Total tool call errors"),
	)
	if err != nil {
		return nil, fmt.Errorf("create tool.errors counter: %w", err)
	}

	modelCalls, err := meter.Int64Counter("pulse.model.calls",
		metric.WithDescription("Total model calls"),
	)
	if err != nil {
		return nil, fmt.Errorf("create model.calls counter: %w", err)
	}

	modelTokens, err := meter.Int64Counter("pulse.model.tokens",
		metric.WithDescription("Total tokens used"),
	)
	if err != nil {
		return nil, fmt.Errorf("create model.tokens counter: %w", err)
	}

	modelErrors, err := meter.Int64Counter("pulse.model.errors",
		metric.WithDescription("Total model call errors"),
	)
	if err != nil {
		return nil, fmt.Errorf("create model.errors counter: %w", err)
	}

	agentRounds, err := meter.Int64Counter("pulse.agent.rounds",
		metric.WithDescription("Total agent loop rounds"),
	)
	if err != nil {
		return nil, fmt.Errorf("create agent.rounds counter: %w", err)
	}

	return &OTelMetrics{
		toolCalls:    toolCalls,
		toolDuration: toolDuration,
		toolErrors:   toolErrors,
		modelCalls:   modelCalls,
		modelTokens:  modelTokens,
		modelErrors:  modelErrors,
		agentRounds:  agentRounds,
	}, nil
}

// RecordToolCall 记录工具调用指标
func (m *OTelMetrics) RecordToolCall(ctx context.Context, toolName string, duration time.Duration, err error) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("tool", toolName))
	m.toolCalls.Add(ctx, 1, attrs)
	m.toolDuration.Record(ctx, float64(duration.Milliseconds()), attrs)
	if err != nil {
		m.toolErrors.Add(ctx, 1, attrs)
	}
}

// RecordModelCall 记录模型调用指标
func (m *OTelMetrics) RecordModelCall(ctx context.Context, model string, tokens int, err error) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("model", model))
	m.modelCalls.Add(ctx, 1, attrs)
	m.modelTokens.Add(ctx, int64(tokens), attrs)
	if err != nil {
		m.modelErrors.Add(ctx, 1, attrs)
	}
}

// RecordAgentRound 记录 Agent 循环轮次
func (m *OTelMetrics) RecordAgentRound(ctx context.Context) {
	if m == nil {
		return
	}
	m.agentRounds.Add(ctx, 1)
}

// ============================================================================
// Unified AgentLogger — 同时输出日志 + OTel 追踪 + 指标
// ============================================================================

// OTelAgentLogger 将 Agent 操作同时输出到 slog 日志、OTel span 和 metrics
type OTelAgentLogger struct {
	*AgentLogger
	tracer  *OTelTracer
	metrics *OTelMetrics
}

// NewOTelAgentLogger 创建统一日志器
func NewOTelAgentLogger(tracer *OTelTracer, metrics *OTelMetrics) *OTelAgentLogger {
	return &OTelAgentLogger{
		AgentLogger: NewAgentLogger(nil),
		tracer:      tracer,
		metrics:     metrics,
	}
}

// LogToolCall 记录工具调用（日志 + span + metrics）
func (l *OTelAgentLogger) LogToolCall(ctx context.Context, toolName string, args map[string]any, duration time.Duration, err error) {
	// 结构化日志
	l.AgentLogger.LogToolCall(toolName, args, duration, err)

	// OTel metrics
	if l.metrics != nil {
		l.metrics.RecordToolCall(ctx, toolName, duration, err)
	}
}

// LogModelCall 记录模型调用（日志 + metrics）
func (l *OTelAgentLogger) LogModelCall(ctx context.Context, model string, tokens int, duration time.Duration, err error) {
	l.AgentLogger.LogModelCall(model, tokens, duration, err)

	if l.metrics != nil {
		l.metrics.RecordModelCall(ctx, model, tokens, err)
	}
}

// LogAgentRound 记录 Agent 循环轮次（日志 + metrics）
func (l *OTelAgentLogger) LogAgentRound(ctx context.Context, round int, toolCalls int, hasResponse bool) {
	l.AgentLogger.LogAgentRound(round, toolCalls, hasResponse)

	if l.metrics != nil {
		l.metrics.RecordAgentRound(ctx)
	}
}
