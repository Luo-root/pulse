package telemetry

import (
	"context"
	"fmt"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newTestOTelTracer() (*OTelTracer, *tracetest.InMemoryExporter) {
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
	)
	otel.SetTracerProvider(tp)
	return NewOTelTracerWithProvider("test", tp), exporter
}

func TestOTelTracer_StartCreatesSpan(t *testing.T) {
	tracer, exporter := newTestOTelTracer()

	ctx := context.Background()
	spanCtx, span := tracer.Start(ctx, "test-operation")
	_ = spanCtx

	span.SetAttribute("key", "value")
	span.SetStatus(SpanOK, "success")
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	s := spans[0]
	if s.Name != "test-operation" {
		t.Errorf("expected span name 'test-operation', got %q", s.Name)
	}
	if len(s.Attributes) == 0 {
		t.Error("expected attributes to be set")
	}
}

func TestOTelTracer_RecordsError(t *testing.T) {
	tracer, exporter := newTestOTelTracer()

	_, span := tracer.Start(context.Background(), "error-op")
	span.RecordError(fmt.Errorf("something went wrong"))
	span.SetStatus(SpanError, "failed")
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}

	s := spans[0]
	if len(s.Events) == 0 {
		t.Error("expected error event to be recorded")
	}
}

func TestOTelTracer_NestedSpans(t *testing.T) {
	tracer, exporter := newTestOTelTracer()

	ctx, parent := tracer.Start(context.Background(), "parent")
	_, child := tracer.Start(ctx, "child")
	child.End()
	parent.End()

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(spans))
	}

	// Child should have parent's span context
	childSpan := spans[0]
	parentSpan := spans[1]
	if childSpan.Parent.SpanID() != parentSpan.SpanContext.SpanID() {
		t.Error("child span should reference parent")
	}
}

func TestOTelSpan_SetAttributeTypes(t *testing.T) {
	tracer, exporter := newTestOTelTracer()

	_, span := tracer.Start(context.Background(), "attrs")
	span.SetAttribute("str", "hello")
	span.SetAttribute("int", 42)
	span.SetAttribute("int64", int64(100))
	span.SetAttribute("float64", 3.14)
	span.SetAttribute("bool", true)
	span.SetAttribute("other", []int{1, 2, 3})
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if len(spans[0].Attributes) != 6 {
		t.Errorf("expected 6 attributes, got %d", len(spans[0].Attributes))
	}
}

func TestNoOpTracer(t *testing.T) {
	tracer := NoOpTracer{}
	ctx, span := tracer.Start(context.Background(), "noop")
	if ctx == nil {
		t.Error("expected non-nil context")
	}
	span.SetAttribute("key", "value")
	span.SetStatus(SpanOK, "ok")
	span.RecordError(fmt.Errorf("err"))
	span.End() // should not panic
}
