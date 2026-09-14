package observability

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// SlogSink 的 Attrs 段按**插入序**输出（产生方语义序），不再按 key 字典序；
// 具名字段恒在 Attrs 之前，尾段是 host_id → error → trace_id。
func TestSlogSinkAttrsInsertionOrder(t *testing.T) {
	var buf bytes.Buffer
	sink := SlogSink{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	rec := Record{
		HostID:  "h1",
		TraceID: "tr-1",
		Source:  SourceAdapter,
		Event:   "llm.after_response",
	}
	// 故意乱序插入：若还残留按 key 排序，输出会变成 model → tokens_in → tokens_out。
	Set(&rec.Attrs, "llm.tokens_out", int64(20))
	Set(&rec.Attrs, "llm.model", "m1")
	Set(&rec.Attrs, "llm.tokens_in", int64(10))
	Set(&rec.Attrs, "loop.tool", "search")
	sink.Write(rec)
	line := buf.String()

	tokensOut := strings.Index(line, "llm.tokens_out")
	model := strings.Index(line, "llm.model")
	tokensIn := strings.Index(line, "llm.tokens_in")
	tool := strings.Index(line, "loop.tool")
	if tokensOut < 0 || model < 0 || tokensIn < 0 || tool < 0 {
		t.Fatalf("fields missing in output: %q", line)
	}
	if !(tokensOut < model && model < tokensIn && tokensIn < tool) {
		t.Fatalf("attrs not in insertion order: %q", line)
	}
	// 尾段在 attrs 之后：host_id → trace_id（无 Err 时）。
	host := strings.Index(line, "host_id")
	trace := strings.Index(line, "trace_id")
	if host < tool || trace < host {
		t.Fatalf("named fields must follow attrs: %q", line)
	}
}

// SlogSink 不得自己再输出一个时间字段：handler 已经带一个，重复的 `time=`
// 会让 logfmt 解析器读到两个同名字段（票面第 1 条）。
func TestSlogSinkNoDuplicateTime(t *testing.T) {
	var buf bytes.Buffer
	sink := SlogSink{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	rec := Record{Source: SourceAdapter, Event: "evt", Status: "ok"}
	rec.Time = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	sink.Write(rec)

	line := buf.String()
	if n := strings.Count(line, "time="); n != 1 {
		t.Fatalf("time= 出现 %d 次，want 1（handler 的那一个）：%q", n, line)
	}
}

// 耗时是浮点毫秒，**不截断**：亚毫秒记录写成 0.585 而不是 0（票面第 5 条）。
func TestSlogSinkDurationNotTruncated(t *testing.T) {
	var buf bytes.Buffer
	sink := SlogSink{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	sink.Write(Record{Source: SourceAdapter, Event: "evt", Duration: 585 * time.Microsecond})

	if line := buf.String(); !strings.Contains(line, "duration_ms=0.585") {
		t.Fatalf("亚毫秒耗时应保留小数：%q", line)
	}
}

// 零值字段省键（不造假值）：人读面用 `-` 占位，机器面直接不输出这个键。
func TestSlogSinkOmitsZeroFields(t *testing.T) {
	var buf bytes.Buffer
	sink := SlogSink{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	sink.Write(Record{Source: SourceAdapter, Event: "evt"})

	line := buf.String()
	for _, absent := range []string{"duration_ms=", "status=", "host_id=", "trace_id=", "fiber=", "from=", "to="} {
		if strings.Contains(line, absent) {
			t.Fatalf("零值字段 %q 不应输出：%q", absent, line)
		}
	}
}

// 底层类型命中约束的命名标量（~int64 别名）按插入序输出且不丢内容。
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
		t.Fatalf("attrs not in insertion order: %q", line)
	}
}

// 装配期的 from/to 只在真有值时输出（旧实现在只给了 fiber 时写出 `from= to=`）。
func TestSlogSinkAssemblySegment(t *testing.T) {
	var buf bytes.Buffer
	sink := SlogSink{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	sink.Write(Record{Source: SourceKernel, Event: EventFiberState, FiberName: "f#1"})
	if line := buf.String(); strings.Contains(line, "from=") || strings.Contains(line, "to=") {
		t.Fatalf("空 from/to 不应输出：%q", line)
	}

	buf.Reset()
	sink.Write(Record{Source: SourceKernel, Event: EventFiberState, From: "loading", To: "active"})
	if line := buf.String(); !strings.Contains(line, "from=loading") || !strings.Contains(line, "to=active") {
		t.Fatalf("from/to 应输出：%q", line)
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
