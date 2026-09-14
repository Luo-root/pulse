package observability

import (
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// SlogSink 把记录写为 slog 结构化日志（默认 stderr）——给机器读的出口：
// 接宿主既有 logger、要 JSON、或要喂采集器时用它。
//
// 与 LineSink 的口径对齐（同一批字段、同一顺序），差别只在呈现：
//   - **不自己输出 time**：handler 已经带一个时间字段，再写一个就是每行两个
//     `time=`（旧实现的实测缺陷，logfmt 解析器读到两个同名字段）。时间以
//     handler 的为准，要改格式就配 handler；
//   - **耗时是 `duration_ms` 数值，不截断**：亚毫秒写成 `0.585` 而不是 0
//     ——取整会让快慢看不出来。人读出口（LineSink）同一条记录写带单位的
//     `585.1µs`，两者是同一事实的机器面与人读面；
//   - 字段顺序与 LineSink 一致：status → duration_ms → event → source →
//     fiber → from/to → loader_kind/entry_id/plugin → Attrs → host_id →
//     error → trace_id；
//   - Attrs 按**插入序**（产生方语义序），不再按 key 字典序排序；
//   - 零值字段省略键（人读面用 `-` 占位，机器面省键——两边都不造假值）。
type SlogSink struct {
	Logger *slog.Logger
}

// Write 实现 Sink。
func (s SlogSink) Write(r Record) {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	attrs := make([]any, 0, 18)
	if r.Status != "" {
		attrs = append(attrs, "status", r.Status)
	}
	if r.Duration != 0 {
		attrs = append(attrs, "duration_ms", strconv.FormatFloat(float64(r.Duration)/float64(time.Millisecond), 'f', -1, 64))
	}
	attrs = append(attrs, "event", r.Event)
	if r.Source != "" {
		attrs = append(attrs, "source", string(r.Source))
	}
	// 装配专用段
	if r.FiberName != "" {
		attrs = append(attrs, "fiber", r.FiberName)
	}
	if r.From != "" || r.To != "" {
		attrs = append(attrs, "from", r.From, "to", r.To)
	}
	if r.EntryID != "" || r.LoaderKind != "" || r.PluginName != "" {
		attrs = append(attrs,
			"loader_kind", r.LoaderKind,
			"entry_id", r.EntryID,
			"plugin", r.PluginName,
		)
	}
	// Attrs 段：按插入序（产生方语义序），与 LineSink 同口径。
	if r.Attrs.Len() > 0 {
		r.Attrs.Range(func(k string, x any) {
			attrs = append(attrs, k, x)
		})
	}
	if r.HostID != "" {
		attrs = append(attrs, "host_id", r.HostID)
	}
	if r.Err != nil {
		attrs = append(attrs, "error", r.Err.Error())
	}
	if r.TraceID != "" {
		attrs = append(attrs, "trace_id", r.TraceID)
	}
	logger.Info("pulse.observability", attrs...)
}

// MemorySink 内存收集器：测试断言与演示用。并发安全。
type MemorySink struct {
	mu      sync.Mutex
	records []Record
}

// Write 实现 Sink。
func (s *MemorySink) Write(r Record) {
	r = stampTime(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
}

// Snapshot 返回已收记录的副本（浅拷贝切片；Record 为值类型）。
func (s *MemorySink) Snapshot() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, len(s.records))
	copy(out, s.records)
	return out
}

// Len 返回当前条数（零增量断言用）。
func (s *MemorySink) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}
