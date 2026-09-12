package observability

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// 本文件是 #172 的速度口径：入队成本（空 Attrs / 含 Attrs 两类，按验收
// 要求分开报）与「突发」场景下生产者侧耗时（对照直写）。

func benchRecordN(attrs int) Record {
	r := Record{
		HostID:   "host-1",
		TraceID:  "tr-0123456789abcdef",
		Source:   SourceAdapter,
		Event:    "llm.generate_finished",
		Status:   "stop",
		Duration: 1234567,
	}
	for i := 0; i < attrs; i++ {
		Set(&r.Attrs, fmt.Sprintf("k.%d", i), "v")
	}
	return r
}

// BenchmarkAsyncSinkWrite 入队成本，空 Attrs（验收口径：0 alloc）。
func BenchmarkAsyncSinkWrite(b *testing.B) {
	as := NewAsyncSink(&MemorySink{}, WithCapacity(4096))
	defer as.Close(context.Background())
	rec := benchRecordN(0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		as.Write(rec)
	}
}

// BenchmarkAsyncSinkWrite3Attrs 入队成本，含 3 个 Attrs（验收口径：
// O(1) alloc / O(N) 拷贝——深拷 map）。
func BenchmarkAsyncSinkWrite3Attrs(b *testing.B) {
	as := NewAsyncSink(&MemorySink{}, WithCapacity(4096))
	defer as.Close(context.Background())
	rec := benchRecordN(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		as.Write(rec)
	}
}

// BenchmarkDirectWrite 对照：直写同一内存出口（同步路径）。
func BenchmarkDirectWrite(b *testing.B) {
	inner := &MemorySink{}
	rec := benchRecordN(0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		inner.Write(rec)
	}
}

// BenchmarkBurst1000_Async 突发口径：AsyncSink 包 SlogSink→无缓冲文件，
// 生产者连发 1000 条（容量 2048，不触发回压）——计时区只含入队，排空
// 放在计时区外（后台继续写）。
func BenchmarkBurst1000_Async(b *testing.B) {
	f, err := os.Create(filepath.Join(b.TempDir(), "obs-async.log"))
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		as := NewAsyncSink(SlogSink{Logger: slog.New(slog.NewTextHandler(f, nil))}, WithCapacity(2048))
		rec := benchRecordN(3)
		b.StartTimer()
		for j := 0; j < 1000; j++ {
			as.Write(rec)
		}
		b.StopTimer()
		_ = as.Close(context.Background()) // 排空：真实成本仍在，只是不在生产者路径上
		b.StartTimer()
	}
}

// BenchmarkBurst1000_Direct 对照：同一条数直写同一出口（同步路径）。
func BenchmarkBurst1000_Direct(b *testing.B) {
	f, err := os.Create(filepath.Join(b.TempDir(), "obs-direct.log"))
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	inner := SlogSink{Logger: slog.New(slog.NewTextHandler(f, nil))}
	rec := benchRecordN(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 1000; j++ {
			inner.Write(rec)
		}
	}
}

// BenchmarkAsyncSinkSustained 持续口径：内存出口 + 有界队列，生产者与
// worker 并行——摊到每条的生产者成本（队列满时会计入回压等待）。
func BenchmarkAsyncSinkSustained(b *testing.B) {
	as := NewAsyncSink(&MemorySink{}, WithCapacity(1024))
	defer as.Close(context.Background())
	rec := benchRecordN(3)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		as.Write(rec)
	}
}
