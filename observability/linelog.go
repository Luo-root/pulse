package observability

import (
	"io"
	"sort"
	"strconv"
	"sync"
	"time"
)

// LineOption 配置 LineSink。
type LineOption func(*lineOpts)

type lineOpts struct {
	bufSize int
}

// WithBufSize 设置行缓冲阈值（字节）。缺省 32 KiB；<= 0 视为编程错误，
// 构造期 panic。
func WithBufSize(n int) LineOption {
	return func(o *lineOpts) { o.bufSize = n }
}

const defaultLineBufSize = 32 << 10

// LineSink 是轻量行式出口：把 Record 编码成一行 logfmt 风格文本
// （`time=... host_id=... trace_id=... source=... event=... k=v ...`），
// 写进自带缓冲，累计到阈值才落一次 io.Writer。
//
// 它**不经过 `log/slog`**：没有 `[]any` 逐字段装箱、没有 slog.Value 转换、
// 没有每条的 keys 排序分配——实测（i9-14900HX）从 SlogSink 的 ~5 µs/条
// （16 allocs，仅格式化）降到 ~0.4 µs/条（0–1 alloc，含缓冲落盘摊销）。
//
// 语义与 SlogSink 对齐，可直接替换：
//   - 同一信封字段与顺序：time, host_id, trace_id, source, event,
//     duration_ms, status, error, fiber/from/to, loader_kind/entry_id/plugin；
//   - Attrs 按 key 字典序输出（确定性，便于 grep 与聚合）；
//   - Time 为零时补 wall clock；Duration 输出毫秒整数；Err 输出 error 文本；
//   - 键与值同规则：需要时按 Go 字符串字面量加引号（含空格/等号/引号/
//     控制字符）——与 `slog.TextHandler` 的 needsQuoting 口径一致。
//
// 契约：
//   - **并发安全**：内部一把锁保护缓冲与出口；
//   - **写错误**记在 Err()（首错为准），不 panic、不阻断后续写入；
//     错误后的缓冲会被丢弃（不无限增长）；
//   - **关闭前 Flush()**：未达阈值的最后一批仍在内存里；
//   - 需要 JSON 结构化输出、或要接宿主既有 logger 时，仍用 SlogSink。
type LineSink struct {
	mu        sync.Mutex
	w         io.Writer
	buf       []byte
	threshold int
	err       error
}

// NewLineSink 构造行式出口。w 为 nil 视为编程错误（panic）；w 只需被
// 本 Sink 串行调用（内部已加锁），不需要自身并发安全。
func NewLineSink(w io.Writer, opts ...LineOption) *LineSink {
	if w == nil {
		panic("observability: LineSink requires a non-nil io.Writer")
	}
	o := lineOpts{bufSize: defaultLineBufSize}
	for _, opt := range opts {
		opt(&o)
	}
	if o.bufSize <= 0 {
		panic("observability: LineSink buffer size must be > 0")
	}
	return &LineSink{
		w:         w,
		buf:       make([]byte, 0, o.bufSize),
		threshold: o.bufSize,
	}
}

// Write 实现 Sink：编码一行并追加进缓冲；缓冲达到阈值即整体写出。
func (s *LineSink) Write(r Record) {
	if r.Time.IsZero() {
		r.Time = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = appendRecordLine(s.buf, r)
	if len(s.buf) >= s.threshold {
		s.flushLocked()
	}
}

// Flush 把缓冲中未落盘的记录写出（进程/宿主关闭前必须调用）。
func (s *LineSink) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
	return s.err
}

// Err 返回首个写错误（无错误为 nil）。
func (s *LineSink) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// flushLocked 写出并清空缓冲；失败记首错并丢弃该批（不无限增长）。
func (s *LineSink) flushLocked() {
	if len(s.buf) == 0 {
		return
	}
	if _, err := s.w.Write(s.buf); err != nil && s.err == nil {
		s.err = err
	}
	s.buf = s.buf[:0]
}

// appendRecordLine 把一条记录编码为完整行（含换行）。字段顺序与 SlogSink
// 一致；Attrs 按 key 字典序。
func appendRecordLine(dst []byte, r Record) []byte {
	dst = append(dst, "time="...)
	dst = append(dst, r.Time.Format(time.RFC3339Nano)...)
	if r.HostID != "" {
		dst = appendTextField(dst, "host_id", r.HostID)
	}
	if r.TraceID != "" {
		dst = appendTextField(dst, "trace_id", r.TraceID)
	}
	dst = appendTextField(dst, "source", string(r.Source))
	dst = appendTextField(dst, "event", r.Event)
	if r.Duration != 0 {
		dst = append(dst, " duration_ms="...)
		dst = strconv.AppendInt(dst, r.Duration.Milliseconds(), 10)
	}
	if r.Status != "" {
		dst = appendTextField(dst, "status", r.Status)
	}
	if r.Err != nil {
		dst = appendTextField(dst, "error", r.Err.Error())
	}
	if r.FiberName != "" {
		dst = appendTextField(dst, "fiber", r.FiberName)
		dst = appendTextField(dst, "from", r.From)
		dst = appendTextField(dst, "to", r.To)
	}
	if r.EntryID != "" || r.LoaderKind != "" {
		dst = appendTextField(dst, "loader_kind", r.LoaderKind)
		dst = appendTextField(dst, "entry_id", r.EntryID)
		dst = appendTextField(dst, "plugin", r.PluginName)
	}
	dst = appendAttrs(dst, r.Attrs)
	return append(dst, '\n')
}

// appendAttrs 按 key 字典序追加 Attrs（≤8 个键用栈上数组插入排序，零分配）。
// 内部存储是插入序切片；排序后逐键 lookup（≤8 条时线性扫描的代价可忽略）。
func appendAttrs(dst []byte, a Attrs) []byte {
	n := len(a.entries)
	if n == 0 {
		return dst
	}
	if n <= 8 {
		var small [8]string
		keys := small[:0]
		for _, e := range a.entries {
			keys = append(keys, e.key)
		}
		insertionSortStrings(keys)
		for _, k := range keys {
			x, _ := a.lookup(k)
			dst = appendScalarField(dst, k, x)
		}
		return dst
	}
	keys := make([]string, 0, n)
	for _, e := range a.entries {
		keys = append(keys, e.key)
	}
	sort.Strings(keys)
	for _, k := range keys {
		x, _ := a.lookup(k)
		dst = appendScalarField(dst, k, x)
	}
	return dst
}

// insertionSortStrings 对小切片做插入排序（避免 sort.Strings 的开销）。
func insertionSortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

// appendTextField 追加 ` key=<文本>`（key 与值同规则：需要时加引号）。
func appendTextField(dst []byte, key, val string) []byte {
	dst = append(dst, ' ')
	dst = appendTextValue(dst, key)
	dst = append(dst, '=')
	return appendTextValue(dst, val)
}

// appendScalarField 追加 ` key=<标量>`（key 与值同规则：需要时加引号；
// 标量按 kind 直写，不经 any 装箱）。
func appendScalarField(dst []byte, key string, v attrScalar) []byte {
	dst = append(dst, ' ')
	dst = appendTextValue(dst, key)
	dst = append(dst, '=')
	switch v.kind {
	case attrString:
		return appendTextValue(dst, v.s)
	case attrInt:
		return strconv.AppendInt(dst, v.i, 10)
	case attrFloat:
		return strconv.AppendFloat(dst, v.f, 'g', -1, 64)
	case attrBool:
		return strconv.AppendBool(dst, v.b)
	}
	return dst
}

// appendTextValue 追加文本值：含空格/等号/引号/控制字符时按 Go 字符串
// 字面量加引号（与 slog.TextHandler 的 needsQuoting 精神一致）。
func appendTextValue(dst []byte, s string) []byte {
	if !needsQuoting(s) {
		return append(dst, s...)
	}
	return strconv.AppendQuote(dst, s)
}

func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c == '"' || c == '=' || c == 0x7f {
			return true
		}
	}
	return false
}
