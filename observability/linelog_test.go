package observability

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件是 LineSink 的契约验收面：格式（字段序 + Attrs 排序 + 引号）、
// 缓冲与 Flush、并发安全、错误捕获。

func TestLineSinkFormatContract(t *testing.T) {
	var buf bytes.Buffer
	s := NewLineSink(&buf)
	rec := Record{
		HostID:   "h1",
		TraceID:  "tr-1",
		Source:   SourceAdapter,
		Event:    "llm.generate_finished",
		Duration: 2500 * time.Millisecond,
		Status:   "stop",
		Err:      errors.New("boom x"),
	}
	rec.Time = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	Set(&rec.Attrs, "llm.model", "gpt-4o-mini")
	Set(&rec.Attrs, "llm.tokens_in", int64(42))
	Set(&rec.Attrs, "llm.temp", 0.7)
	Set(&rec.Attrs, "llm.cached", true)
	Set(&rec.Attrs, "app.note", "hello world") // 含空格 → 加引号
	Set(&rec.Attrs, "app.plain", "plain")      // 不需引号

	s.Write(rec)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	want := `time=2026-09-12T12:00:00Z host_id=h1 trace_id=tr-1 source=bridge event=llm.generate_finished duration_ms=2500 status=stop error="boom x" app.note="hello world" app.plain=plain llm.cached=true llm.model=gpt-4o-mini llm.temp=0.7 llm.tokens_in=42` + "\n"
	if got := buf.String(); got != want {
		t.Fatalf("line mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestLineSinkBuffersUntilFlushOrThreshold(t *testing.T) {
	var buf bytes.Buffer
	s := NewLineSink(&buf, WithBufSize(64))

	s.Write(Record{Event: "small"})
	if buf.Len() != 0 {
		t.Fatalf("未达阈值不应落盘，got %q", buf.String())
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "event=small") {
		t.Fatalf("Flush 后应在出口里，got %q", buf.String())
	}

	// 超过阈值自动落盘。
	buf.Reset()
	for i := 0; i < 20; i++ {
		s.Write(Record{Event: "evt-with-some-length"})
	}
	if buf.Len() == 0 {
		t.Fatal("超过阈值应自动落盘")
	}
}

func TestLineSinkConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	s := NewLineSink(&buf, WithBufSize(128))

	const writers, per = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				s.Write(Record{Event: "evt"})
			}
		}(w)
	}
	wg.Wait()
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != writers*per {
		t.Fatalf("lines = %d, want %d（并发写不得丢行）", len(lines), writers*per)
	}
	for _, ln := range lines {
		if !strings.HasPrefix(ln, "time=") || !strings.Contains(ln, " event=evt") {
			t.Fatalf("行内容错乱（交错写？）：%q", ln)
		}
	}
}

type failWriter struct{ calls int }

func (f *failWriter) Write(p []byte) (int, error) {
	f.calls++
	return 0, errors.New("disk on fire")
}

func TestLineSinkCapturesWriteError(t *testing.T) {
	fw := &failWriter{}
	s := NewLineSink(fw, WithBufSize(32))

	for i := 0; i < 50; i++ {
		s.Write(Record{Event: "evt"}) // 不 panic、不阻断
	}
	if err := s.Flush(); err == nil {
		t.Fatal("写失败应可通过 Err()/Flush() 观察")
	}
	if s.Err() == nil {
		t.Fatal("Err() 应返回首错")
	}
}

// TestLineSinkParityWithSlogFields 与 SlogSink 的字段面保持同序同集：
// 同一条记录两边输出的字段名序列一致（值域格式不做逐字比对）。
func TestLineSinkParityWithSlogFields(t *testing.T) {
	rec := Record{
		HostID:  "h1",
		TraceID: "tr-1",
		Source:  SourceAdapter,
		Event:   "evt",
		Status:  "ok",
	}
	Set(&rec.Attrs, "b.key", "v")
	Set(&rec.Attrs, "a.key", "v")

	rec.Time = time.Unix(0, 0).UTC()
	var buf bytes.Buffer
	s := NewLineSink(&buf)
	s.Write(rec)
	_ = s.Flush()

	got := strings.Fields(strings.TrimSpace(buf.String()))
	wantOrder := []string{"time=", "host_id=", "trace_id=", "source=", "event=", "status=", "a.key=", "b.key="}
	for i, prefix := range wantOrder {
		if !strings.HasPrefix(got[i], prefix) {
			t.Fatalf("字段序第 %d 位 = %q, want 前缀 %q（全行 %q）", i, got[i], prefix, buf.String())
		}
	}
	if len(got) != len(wantOrder) {
		t.Fatalf("字段数 = %d, want %d", len(got), len(wantOrder))
	}
}

func TestLineSinkConstructorValidation(t *testing.T) {
	assertPanics := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s: want panic", name)
			}
		}()
		fn()
	}
	assertPanics("nil writer", func() { NewLineSink(nil) })
	assertPanics("zero buf", func() { NewLineSink(&bytes.Buffer{}, WithBufSize(0)) })
}
