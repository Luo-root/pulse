package observability

import (
	"bufio"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// 本文件是观测出口的**常驻性能基线**：把「折叠构造 → Sink 写入 → 格式化
// → 落盘」的每一层拆开量，任何出口改动都应先跑它对照。
//
// 结论口径（i9-14900HX / Windows，跑法见 README「异步出口」节）：
// 瓶颈排序 = 无缓冲落盘 syscall >> slog 格式化 > 折叠/构造 >> kernel 派发；
// LineSink 把「格式化 + 缓冲落盘」压到与折叠同量级。

type benchNoopSink struct{}

func (benchNoopSink) Write(Record) {}

func benchLineRecord(attrs int) Record {
	r := Record{
		HostID:   "host-1",
		TraceID:  "tr-0123456789abcdef",
		Source:   SourceAdapter,
		Event:    "llm.generate_finished",
		Status:   "stop",
		Duration: 1234567,
	}
	for i := 0; i < attrs; i++ {
		Set(&r.Attrs, "k."+string(rune('a'+i)), "v")
	}
	return r
}

// --- 层 0：只构造 Record（折叠侧成本，不含写入） ---
//
// 注意：key/值用常量预置，避免把「基准自身的字符串拼装」算进 Record 构造
// ——否则会虚增 allocs（实测：动态拼 key 时 +3 allocs，全是拼装噪声）。
func BenchmarkLayer_RecordBuild(b *testing.B) {
	const (
		kModel = "llm.model"
		kInst  = "llm.instance"
		kTok   = "llm.tokens_in"
		vModel = "gpt-4o-mini"
		vInst  = "main"
	)
	var vTok int64 = 1234
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var r Record
		r.HostID = "host-1"
		r.TraceID = "tr-0123456789abcdef"
		r.Source = SourceAdapter
		r.Event = "llm.generate_finished"
		r.Status = "stop"
		r.Duration = 1234567
		Set(&r.Attrs, kModel, vModel)
		Set(&r.Attrs, kInst, vInst)
		Set(&r.Attrs, kTok, vTok)
	}
}

// --- 层 1：出口下限与内存出口 ---

func BenchmarkLayer_NoopSink(b *testing.B) {
	s := benchNoopSink{}
	r := benchLineRecord(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(r)
	}
}

func BenchmarkLayer_MemorySink(b *testing.B) {
	s := &MemorySink{}
	r := benchLineRecord(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(r)
	}
}

// --- 层 2：slog 格式化（丢弃输出，不含 I/O）与两种 handler ---

func BenchmarkLayer_SlogDiscardText(b *testing.B) {
	s := SlogSink{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := benchLineRecord(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(r)
	}
}

func BenchmarkLayer_SlogDiscardJSON(b *testing.B) {
	s := SlogSink{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	r := benchLineRecord(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(r)
	}
}

// --- 层 3：携带落盘（缓冲 vs 无缓冲） ---

func BenchmarkLayer_SlogFileBuffered(b *testing.B) {
	f, err := os.Create(filepath.Join(b.TempDir(), "slog-buf.log"))
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	bw := bufio.NewWriterSize(f, 64<<10)
	defer bw.Flush()
	s := SlogSink{Logger: slog.New(slog.NewTextHandler(bw, nil))}
	r := benchLineRecord(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(r)
	}
}

func BenchmarkLayer_SlogFileRaw(b *testing.B) {
	f, err := os.Create(filepath.Join(b.TempDir(), "slog-raw.log"))
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	s := SlogSink{Logger: slog.New(slog.NewTextHandler(f, nil))}
	r := benchLineRecord(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(r)
	}
}

// --- 层 4：LineSink（自带缓冲，不经 slog） ---

func BenchmarkLayer_LineDiscard(b *testing.B) {
	s := NewLineSink(io.Discard)
	r := benchLineRecord(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(r)
	}
}

func BenchmarkLayer_LineFile(b *testing.B) {
	f, err := os.Create(filepath.Join(b.TempDir(), "line.log"))
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	s := NewLineSink(f)
	r := benchLineRecord(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(r)
	}
	_ = s.Flush()
}

// --- 层 5：字段数敏感性：slog vs LineSink（同一条记录形态对比） ---

func BenchmarkScale_Slog_0(b *testing.B) {
	s := SlogSink{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := benchLineRecord(0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(r)
	}
}

func BenchmarkScale_Slog_3(b *testing.B) {
	s := SlogSink{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := benchLineRecord(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(r)
	}
}

func BenchmarkScale_Slog_10(b *testing.B) {
	s := SlogSink{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r := benchLineRecord(10)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(r)
	}
}

func BenchmarkScale_Line_0(b *testing.B) {
	s := NewLineSink(io.Discard)
	r := benchLineRecord(0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(r)
	}
}

func BenchmarkScale_Line_3(b *testing.B) {
	s := NewLineSink(io.Discard)
	r := benchLineRecord(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(r)
	}
}

func BenchmarkScale_Line_10(b *testing.B) {
	s := NewLineSink(io.Discard)
	r := benchLineRecord(10)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		s.Write(r)
	}
}
