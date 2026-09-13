package observability

import (
	"bytes"
	"errors"
	"log/slog"
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

// TestLineSinkParityWithSlogFields 与 SlogSink 的字段面**实测对照**：同一条
// 记录分别过两个出口，逐字段比对字段名序列（slog 侧跳过 handler 自带的
// time/level/msg 三件套）。值格式不逐字比对（引号细节见 LineSink godoc）。
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

	// LineSink 侧
	var lineBuf bytes.Buffer
	ls := NewLineSink(&lineBuf)
	ls.Write(rec)
	_ = ls.Flush()
	lineNames := fieldNames(strings.TrimSpace(lineBuf.String()))

	// SlogSink 侧（同一记录、同一 TextHandler）
	var slogBuf bytes.Buffer
	ss := SlogSink{Logger: slog.New(slog.NewTextHandler(&slogBuf, nil))}
	ss.Write(rec)
	slogNames := fieldNames(strings.TrimSpace(slogBuf.String()))

	const handlerPrefix = 3 // time / level / msg
	if len(slogNames) < handlerPrefix {
		t.Fatalf("slog 输出字段过少：%q", slogBuf.String())
	}
	slogAttrs := slogNames[handlerPrefix:]
	if len(slogAttrs) != len(lineNames) {
		t.Fatalf("字段数不符：line=%v slog(去前缀)=%v", lineNames, slogAttrs)
	}
	for i := range lineNames {
		if lineNames[i] != slogAttrs[i] {
			t.Fatalf("第 %d 个字段名不符：line=%q slog=%q\nline 全行 %q\nslog 全行 %q",
				i, lineNames[i], slogAttrs[i], lineBuf.String(), slogBuf.String())
		}
	}
}

// fieldNames 从 logfmt 风格行里取字段名（测试用：值不含空格与转义）。
func fieldNames(line string) []string {
	var out []string
	for _, tok := range strings.Fields(line) {
		if i := strings.IndexByte(tok, '='); i > 0 {
			out = append(out, tok[:i])
		}
	}
	return out
}

// TestLineSinkManyAttrsSorted >8 个 Attrs 走 sort.Strings 回退分支：仍按
// key 字典序输出、一条不少（乱序插入以真正校验排序）。
func TestLineSinkManyAttrsSorted(t *testing.T) {
	var buf bytes.Buffer
	s := NewLineSink(&buf)
	rec := Record{Source: SourceAdapter, Event: "evt"}
	rec.Time = time.Unix(0, 0).UTC()

	var wantKeys []string
	for i := 0; i < 12; i++ {
		k := "k." + string(rune('a'+i))
		wantKeys = append(wantKeys, k)
	}
	// 逆序插入：若回退分支没排序，输出会跟着逆序。
	for i := len(wantKeys) - 1; i >= 0; i-- {
		Set(&rec.Attrs, wantKeys[i], "v")
	}
	s.Write(rec)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	got := fieldNames(strings.TrimSpace(buf.String()))
	// 前缀字段：time/source/event（无 host_id/trace_id/status/error）。
	wantPrefix := []string{"time", "source", "event"}
	if len(got) != len(wantPrefix)+len(wantKeys) {
		t.Fatalf("字段数 = %d, want %d（%v）", len(got), len(wantPrefix)+len(wantKeys), got)
	}
	for i, w := range wantPrefix {
		if got[i] != w {
			t.Fatalf("前缀字段第 %d 位 = %q, want %q", i, got[i], w)
		}
	}
	for i, w := range wantKeys {
		if got[len(wantPrefix)+i] != w {
			t.Fatalf("Attrs 第 %d 位 = %q, want %q（应字典序）", i, got[len(wantPrefix)+i], w)
		}
	}
}

// TestLineSinkQuotesKeyWhenNeeded 键与值同规则：含空格/等号的 key 也加引号
// （与 slog.TextHandler 的 needsQuoting 口径一致）。
func TestLineSinkQuotesKeyWhenNeeded(t *testing.T) {
	var buf bytes.Buffer
	s := NewLineSink(&buf)
	rec := Record{Source: SourceAdapter, Event: "evt"}
	rec.Time = time.Unix(0, 0).UTC()
	Set(&rec.Attrs, "weird key", "v") // 含空格 → 加引号
	Set(&rec.Attrs, "plain.key", "v")
	s.Write(rec)
	_ = s.Flush()

	out := buf.String()
	if !strings.Contains(out, `"weird key"=v`) {
		t.Fatalf("含空格的 key 应加引号，got %q", out)
	}
	if !strings.Contains(out, "plain.key=v") {
		t.Fatalf("普通 key 不应加引号，got %q", out)
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
