package observability

import (
	"log/slog"
	"sync"
)

// SlogSink 把记录写为 slog 结构化日志（默认 stderr）。
// 装配记录与桥记录共用一个 Logger 实例即为「同一出口」。
type SlogSink struct {
	Logger *slog.Logger
}

// Write 实现 Sink。字段顺序固定，便于 grep 与日志聚合分组。
func (s SlogSink) Write(r Record) {
	logger := s.Logger
	if logger == nil {
		logger = slog.Default()
	}
	attrs := make([]any, 0, 16)
	if r.HostID != "" {
		attrs = append(attrs, "host_id", r.HostID)
	}
	if r.TraceID != "" {
		attrs = append(attrs, "trace_id", r.TraceID)
	}
	attrs = append(attrs,
		"source", string(r.Source),
		"event", r.Event,
	)
	if r.Duration != 0 {
		attrs = append(attrs, "duration_ms", r.Duration.Milliseconds())
	}
	if r.Status != "" {
		attrs = append(attrs, "status", r.Status)
	}
	if r.Err != nil {
		attrs = append(attrs, "error", r.Err.Error())
	}
	// 装配专用段
	if r.FiberName != "" {
		attrs = append(attrs,
			"fiber", r.FiberName,
			"from", r.From,
			"to", r.To,
		)
	}
	if r.EntryID != "" || r.LoaderKind != "" {
		attrs = append(attrs,
			"loader_kind", r.LoaderKind,
			"entry_id", r.EntryID,
			"plugin", r.PluginName,
		)
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
