package observability

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// SlogSink 的 Attrs 段按 key 字典序输出：内部 map 无序，出口负责
// 确定性。具名字段（time/host_id/trace_id/source/event/...）恒在
// Attrs 之前。
func TestSlogSinkAttrsSorted(t *testing.T) {
	var buf bytes.Buffer
	sink := SlogSink{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	rec := Record{
		HostID:  "h1",
		TraceID: "tr-1",
		Source:  SourceAdapter,
		Event:   "llm.after_response",
	}
	Set(&rec.Attrs, "llm.tokens_out", int64(20))
	Set(&rec.Attrs, "llm.model", "m1")
	Set(&rec.Attrs, "llm.tokens_in", int64(10))
	Set(&rec.Attrs, "loop.tool", "search")
	sink.Write(rec)
	line := buf.String()

	trace := strings.Index(line, "trace_id")
	model := strings.Index(line, "llm.model")
	in := strings.Index(line, "llm.tokens_in")
	out := strings.Index(line, "llm.tokens_out")
	tool := strings.Index(line, "loop.tool")
	if trace < 0 || model < 0 || in < 0 || out < 0 || tool < 0 {
		t.Fatalf("fields missing in output: %q", line)
	}
	if !(model < in && in < out && out < tool) {
		t.Fatalf("attrs not in key order: %q", line)
	}
	if trace > model {
		t.Fatalf("named fields must precede attrs: %q", line)
	}
}

// 底层类型命中约束的命名标量（~int64 别名）也按排序输出且不丢内容。
func TestSlogSinkAttrsNamedTypes(t *testing.T) {
	type step int64

	var buf bytes.Buffer
	sink := SlogSink{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	rec := Record{Source: SourceAdapter, Event: "loop.turn_end"}
	Set(&rec.Attrs, "loop.steps", step(3))
	Set(&rec.Attrs, "loop.tool", "calculator")
	sink.Write(rec)
	line := buf.String()
	if !strings.Contains(line, "loop.steps=3") || !strings.Contains(line, "loop.tool=calculator") {
		t.Fatalf("named-type attrs lost: %q", line)
	}
	steps := strings.Index(line, "loop.steps")
	tool := strings.Index(line, "loop.tool")
	if steps > tool {
		t.Fatalf("attrs not in key order: %q", line)
	}
}

// 无 Attrs 的记录照常输出（零值段跳过）。
func TestSlogSinkNoAttrs(t *testing.T) {
	var buf bytes.Buffer
	sink := SlogSink{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	sink.Write(Record{Source: SourceKernel, Event: EventFiberState})
	line := buf.String()
	if !strings.Contains(line, "event="+EventFiberState) {
		t.Fatalf("missing event field: %q", line)
	}
}
