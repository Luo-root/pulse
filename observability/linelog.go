package observability

import (
	"io"
	"os"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// LineOption 配置 LineSink。
type LineOption func(*lineOpts)

type lineOpts struct {
	bufSize int
	prefix  string
	color   bool
	colorAt bool // 显式设过 color；未设 = 按目的地自动判断
}

// WithBufSize 设置行缓冲阈值（字节）。缺省 32 KiB；<= 0 视为编程错误，
// 构造期 panic。
func WithBufSize(n int) LineOption {
	return func(o *lineOpts) { o.bufSize = n }
}

// WithPrefix 设置行首标识（缺省 `PULSE`）。传空串关闭标识——多服务共用
// 一个终端时它是有用的分栏锚点，独占日志文件时它只是每行多几个字符。
func WithPrefix(text string) LineOption {
	return func(o *lineOpts) { o.prefix = text }
}

// WithColor 强制开 / 关 ANSI 颜色。缺省按目的地自动判断：w 是 *os.File 且为
// 字符设备（终端）才上色；重定向到文件或管道时不上色——上色等于往日志里混
// 转义序列。
func WithColor(on bool) LineOption {
	return func(o *lineOpts) { o.color, o.colorAt = on, true }
}

const (
	defaultLineBufSize = 32 << 10

	// DefaultLinePrefix 是行首标识的缺省值。
	DefaultLinePrefix = "PULSE"

	// lineTimeLayout 定宽 25 列（含毫秒）。不用 RFC3339Nano：它会吃掉小数
	// 末尾的 0，「12:42:03.531」与「12:42:04.12」宽度不一，列就跳了。
	lineTimeLayout = "2006/01/02 - 15:04:05.000"

	lineSep     = " | " // 组间分隔
	colStatus   = 10    // 状态列：左对齐，宽度**下限**（超长不截断）
	colDuration = 9     // 耗时列：右对齐
	emptyCol    = "-"   // 列空值占位：让事件列起点恒定
)

// ANSI 颜色码。只在目的地是终端时使用（见 isTerminal）。
const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[90m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
)

// LineSink 是**默认出口**：把 Record 渲染成一行给人读的文本，写进自带缓冲，
// 累计到阈值才落一次 io.Writer。
//
// # 版式
//
// 列序固定、组间 ` | ` 分隔、组内以空格分隔（一组 = 一类事实）：
//
//	PULSE | 2026/09/14 - 12:42:03.531 | completed  |   585.0µs | llm.generate_finished | source=bridge | llm.model=gpt-4o-mini llm.tokens_in=42 | host=pulse-web | trace=6504f73f
//	PULSE | 2026/09/14 - 12:42:03.100 | -          |         - | pulse.kernel.fiber_state | source=kernel | fiber=llmAdapter#3 state=loading→active
//	PULSE | 2026/09/14 - 12:42:03.110 | ok         |         - | pulse.kernel.loader_action | source=kernel | loader=mount entry=llm-adapter
//
// 口径：
//   - **固定列**：标识（dim）→ 时间 → 状态 → 耗时 → 事件。时间定宽 25 列
//     （`2006/01/02 - 15:04:05.000`，含毫秒）；不用 RFC3339Nano——它会吃掉
//     小数末尾的 0，「12:42:03.531」与「12:42:04.12」宽度不一，列就跳了。
//     状态左对齐（状态是词不是数字：completed / stop / active，**补齐按
//     显示列**，全角字符按 2 列），耗时列右对齐
//     且**带单位、不取整**：`820ns` / `585.1µs` / `7.62ms` / `1.23s`——旧的
//     `duration_ms=0` 把亚毫秒记录写成 0，快慢全看不出来；
//   - **列缺值渲染 `-`**：装配期记录没有状态与耗时，占位保证事件列起点恒定
//     （「规整」的全部意义就是扫读时眼球不用重新找列）。长状态按原样输出，
//     列宽是下限不是截断；
//   - **具名字段组**：source → fiber → state=from→to → loader → entry → plugin。
//     key 名比机器面短（`from`/`to` 合成 `state=a→b`、`loader_kind`→`loader`、
//     `entry_id`→`entry`）——人读面优先阅读序，事实不丢（值原样出现）；
//   - **attrs 组按插入序**输出，不按 key 排序：Attrs 是产生方的语义顺序
//     （各包折叠函数按「模型 → 用量」的顺序 Set），出口不认识业务语义，
//     排序只会把它打乱；插入序同样是确定性的，且省掉一次排序；
//   - **不丢字段**：固定列盖不住的属性全部按插入序跟在列后面；
//   - **组内 k=v 按需加引号**（含空格 / 等号 / 引号 / 控制字符），与旧实现和
//     slog.TextHandler 的 needsQuoting 口径一致；**列**（状态 / 事件）按原样
//     输出不加引号——状态可能是一整句话（宿主装配横幅）。
//
// # 颜色
//
// 只在目的地是终端时上色：标识与时间暗淡（扫读时不抢注意力）、trace 暗淡、
// 有 Err 的耗时红色、≥1s 的耗时黄色。**不按 Status 字符串猜语义**——Status 是
// 各事实归属包自己的词表（`completed` / `stop` / `inactive`…），出口替它们
// 配色等于把业务语义搬进基座。需要更丰富的上色时，宿主自带 Sink 实现即可。
//
// # 成本
//
// 不经 `log/slog`：没有 `[]any` 逐字段装箱、没有 slog.Value 转换、没有每条的
// 键排序分配——数字走 `strconv.Append*`，时间走 `AppendFormat`，属性直接读
// 内部条目（不经 `Range` 的闭包与 `native()` 装箱）。实测（`sink_bench_test.go`，
// 同一会话）渲染成本约为 `SlogSink` 的 **1/5**，且**零分配**（`SlogSink`
// 每条 14 allocs）——这是它当默认出口的底气。
//
// # 契约
//
//   - **并发安全**：内部一把锁保护缓冲与出口；
//   - **写错误**记在 Err()（首错为准），不 panic、不阻断后续写入；
//     错误后的缓冲会被丢弃（不无限增长）；
//   - **关闭前 Flush()**：未达阈值的最后一批仍在内存里；
//   - 需要 JSON 结构化输出、或要接宿主既有 logger 时，用 SlogSink。
type LineSink struct {
	mu        sync.Mutex
	w         io.Writer
	buf       []byte
	threshold int
	prefix    string
	color     bool
	err       error
}

// NewLineSink 构造行式出口。w 为 nil 视为编程错误（panic）；w 只需被本 Sink
// 串行调用（内部已加锁），不需要自身并发安全。
func NewLineSink(w io.Writer, opts ...LineOption) *LineSink {
	if w == nil {
		panic("observability: LineSink requires a non-nil io.Writer")
	}
	o := lineOpts{bufSize: defaultLineBufSize, prefix: DefaultLinePrefix}
	for _, opt := range opts {
		opt(&o)
	}
	if o.bufSize <= 0 {
		panic("observability: LineSink buffer size must be > 0")
	}
	color := isTerminal(w)
	if o.colorAt {
		color = o.color
	}
	return &LineSink{
		w:         w,
		buf:       make([]byte, 0, o.bufSize),
		threshold: o.bufSize,
		prefix:    o.prefix,
		color:     color,
	}
}

// Write 实现 Sink：编码一行并追加进缓冲；缓冲达到阈值即整体写出。
func (s *LineSink) Write(r Record) {
	r = stampTime(r)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = s.appendLine(s.buf, r)
	if len(s.buf) >= s.threshold {
		s.flushLocked()
	}
}

// Flush 把缓冲中未落盘的记录写出（进程 / 宿主关闭前必须调用）。
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

// appendLine 编码一条记录为完整行（含换行）。列序见 LineSink godoc。
func (s *LineSink) appendLine(dst []byte, r Record) []byte {
	if s.prefix != "" {
		// 注意 `painted` 必须先声明再做 `=` 赋值：写成
		// `dst, painted := s.paint(...)` 会在 if 块里**连 dst 一起遮蔽**，
		// 追加落到内层切片上、外层长度不变（前缀被后续 append 覆盖，
		// 段内的字段直接消失）。go vet 默认不开 shadow 检查，抓不到。
		painted := false
		dst, painted = s.paint(dst, ansiDim)
		dst = append(dst, s.prefix...)
		dst = s.unpaint(dst, painted)
		dst = append(dst, lineSep...)
	}

	painted := false
	dst, painted = s.paint(dst, ansiDim)
	dst = r.Time.AppendFormat(dst, lineTimeLayout)
	dst = s.unpaint(dst, painted)

	dst = append(dst, lineSep...)
	dst = s.appendStatusCol(dst, r)

	dst = append(dst, lineSep...)
	dst = s.appendDurationCol(dst, r)

	dst = append(dst, lineSep...)
	if r.Event != "" {
		dst = append(dst, r.Event...)
	} else {
		dst = append(dst, emptyCol...)
	}

	dst = s.appendNamedFields(dst, r)
	return append(dst, '\n')
}

// appendStatusCol 状态列：左对齐、宽度下限 colStatus。状态是词不是数字
// （completed / stop / failed / active），左对齐符合阅读；超长状态（宿主
// 装配横幅那种一整句）原样输出——列宽是下限，不是截断。
//
// 补齐按**显示列**而不是 rune 数：状态是宿主可配的自由文本，「运行中」是
// 3 rune 却占 6 列，按 rune 补会让该行的后续列整体右推 3 列。
func (s *LineSink) appendStatusCol(dst []byte, r Record) []byte {
	if r.Status == "" {
		dst = append(dst, emptyCol...)
		return appendPadding(dst, colStatus-displayWidth(emptyCol))
	}
	dst = append(dst, r.Status...)
	return appendPadding(dst, colStatus-displayWidth(r.Status))
}

// appendDurationCol 耗时列：右对齐、带单位。零值（未计时）渲染 `-`——写
// `0ns` 会被读成「测出来是 0」，与「没有这个事实」不是一件事。
func (s *LineSink) appendDurationCol(dst []byte, r Record) []byte {
	if r.Duration == 0 {
		dst = appendPadding(dst, colDuration-len(emptyCol))
		return append(dst, emptyCol...)
	}
	var scratch [16]byte
	text := appendDuration(scratch[:0], r.Duration)
	code := ""
	if r.Err != nil {
		code = ansiRed
	} else if r.Duration >= time.Second {
		code = ansiYellow
	}
	painted := false
	dst, painted = s.paint(dst, code)
	// 耗时文本只有 ASCII 数字与 `µ`（两者都是 1 列宽），rune 数 == 显示列数。
	dst = appendPadding(dst, colDuration-utf8.RuneCount(text))
	dst = append(dst, text...)
	return s.unpaint(dst, painted)
}

// appendNamedFields 追加具名字段组（信封与装配事实，各自成组、组间 ` | `）：
// source → fiber → state=from→to → loader → entry → plugin。
//
// 与 SlogSink 同序；key 名在人读面压缩（`from`/`to` → `state=a→b`，
// `loader_kind` → `loader`，`entry_id` → `entry`）——事实不丢，值原样出现。
func (s *LineSink) appendNamedFields(dst []byte, r Record) []byte {
	if r.Source != "" {
		dst = append(dst, lineSep...)
		dst = append(dst, "source="...)
		dst = appendTextValue(dst, string(r.Source))
	}
	if r.FiberName != "" {
		dst = append(dst, lineSep...)
		dst = append(dst, "fiber="...)
		dst = appendTextValue(dst, r.FiberName)
	}
	if r.From != "" || r.To != "" {
		dst = append(dst, lineSep...)
		dst = append(dst, "state="...)
		dst = appendTextValue(dst, r.From)
		dst = append(dst, "→"...)
		dst = appendTextValue(dst, r.To)
	}
	if r.LoaderKind != "" {
		dst = append(dst, lineSep...)
		dst = append(dst, "loader="...)
		dst = appendTextValue(dst, r.LoaderKind)
	}
	if r.EntryID != "" {
		dst = append(dst, lineSep...)
		dst = append(dst, "entry="...)
		dst = appendTextValue(dst, r.EntryID)
	}
	if r.PluginName != "" {
		dst = append(dst, lineSep...)
		dst = append(dst, "plugin="...)
		dst = appendTextValue(dst, r.PluginName)
	}
	return s.appendTail(dst, r)
}

// appendTail 追加尾段：attrs（插入序）→ host → err → trace。
// 顺序与 SlogSink 一致（错误在关联 id 之前：读日志先看失败原因，trace 是
// 辅助定位，也是线上最不常读的一段，所以放在最后）。
func (s *LineSink) appendTail(dst []byte, r Record) []byte {
	if r.Attrs.Len() > 0 {
		dst = append(dst, lineSep...)
		dst = appendAttrs(dst, r.Attrs)
	}
	if r.HostID != "" {
		dst = append(dst, lineSep...)
		dst = append(dst, "host="...)
		dst = appendTextValue(dst, r.HostID)
	}
	if r.Err != nil {
		dst = append(dst, lineSep...)
		painted := false
		dst, painted = s.paint(dst, ansiRed)
		dst = append(dst, "err="...)
		dst = appendTextValue(dst, r.Err.Error())
		dst = s.unpaint(dst, painted)
	}
	if r.TraceID != "" {
		dst = append(dst, lineSep...)
		painted := false
		dst, painted = s.paint(dst, ansiDim)
		dst = append(dst, "trace="...)
		dst = append(dst, r.TraceID...)
		dst = s.unpaint(dst, painted)
	}
	return dst
}

// paint 前置颜色码；返回是否真的上了色（没上色时不用补 reset）。
func (s *LineSink) paint(dst []byte, code string) ([]byte, bool) {
	if !s.color || code == "" {
		return dst, false
	}
	return append(dst, code...), true
}

func (s *LineSink) unpaint(dst []byte, painted bool) []byte {
	if !painted {
		return dst
	}
	return append(dst, ansiReset...)
}

// appendAttrs 按**插入序**追加属性组（`k=v k=v`，组内单空格分隔，首条不带
// 前导空格——调用方已经补过 ` | ` 分隔符）。
//
// 旧实现按 key 字典序输出并为此排序（≤8 个键走栈上插入排序）——但字典序不是
// 阅读序，出口也排不出来：`http.request.method` 该排在 `http.response.body.size`
// 前面是 HTTP 知识，出口不认识。Attrs 内部本来就是插入序切片（#179），产生方
// 的写的顺序就是它想被读到的顺序，直接照抄即可，还省掉一次排序。
//
// 直接遍历内部条目：`Attrs.Range` 的回调是闭包，捕获 dst 会让缓冲逃逸到堆，
// 且 `native()` 会逐值装箱——两条都足以把「1 alloc/条」变成「每字段 1 alloc」。
func appendAttrs(dst []byte, a Attrs) []byte {
	for i := range a.entries {
		if i > 0 {
			dst = append(dst, ' ')
		}
		e := &a.entries[i]
		dst = appendTextValue(dst, e.key)
		dst = append(dst, '=')
		switch e.val.kind {
		case attrString:
			dst = appendTextValue(dst, e.val.s)
		case attrInt:
			dst = strconv.AppendInt(dst, e.val.i, 10)
		case attrFloat:
			dst = strconv.AppendFloat(dst, e.val.f, 'g', -1, 64)
		case attrBool:
			dst = strconv.AppendBool(dst, e.val.b)
		}
	}
	return dst
}

// displayWidth 返回 s 在终端里占的**显示列**数：CJK / 假名 / 韩文音节 /
// 全角形式按 2 列，组合记号与零宽字符按 0 列，C0/C1 控制字符不占列，
// 其余按 1 列。
//
// 为什么不用 utf8.RuneCountInString：中文状态（「运行中」）是 3 rune 但
// 占 6 列，按 rune 补齐会把该行后续列整体右推——而状态正是宿主可配的
// 自由文本（本包 godoc 自己举了「宿主装配横幅那种一整句」的例子）。
//
// 表是常见区段的近似（东亚宽度 East Asian Width 的子集）：只为给状态列
// 补齐而引 golang.org/x/text/width 不值当。覆盖不到的（emoji 变体选择符、
// 罕用宽字符）按 1 列算，与终端可能差 1 列。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r < 0x20 || (r >= 0x7f && r < 0xa0):
			// C0 / C1 控制字符：不占列
		case r >= 0x0300 && r <= 0x036f, // 组合附加符号
			r >= 0x200b && r <= 0x200f, // 零宽 / 方向标记
			r == 0xfeff:                // 零宽不换行空格（BOM）
			// 0 列
		case r >= 0x1100 && r <= 0x115f, // 韩文字母
			r >= 0x2e80 && r <= 0xa4cf,   // CJK 部首 / 假名 / 注音 / 韩文兼容 / 彝文
			r >= 0xac00 && r <= 0xd7a3,   // 韩文音节
			r >= 0xf900 && r <= 0xfaff,   // CJK 兼容表意
			r >= 0xfe30 && r <= 0xfe6f,   // CJK 兼容形式
			r >= 0xff00 && r <= 0xff60,   // 全角 ASCII
			r >= 0xffe0 && r <= 0xffe6,   // 全角符号
			r >= 0x20000 && r <= 0x3fffd: // CJK 扩展 B 及以后
			w += 2
		default:
			w++
		}
	}
	return w
}

// appendDuration 追加带单位的耗时：`820ns` / `585.1µs` / `7.62ms` / `1.23s`。
//
// 刻意**不取整到毫秒**：旧的 `duration_ms` 把亚毫秒记录写成 0，快慢全看不出来。
// 单位按量级选，µs 保留一位小数，ms / s 保留两位。
//
// 全程整数运算（取整 + 补零），**不用 strconv.AppendFloat**：`'f'` 定点格式化走
// strconv 的十进制大数路径，实测每条多花约 120 ns（占整行渲染成本的三分之一），
// 换来的只是「999.999µs 进位成 1000.0µs」这种假进位。整数版更快也更老实。
func appendDuration(dst []byte, d time.Duration) []byte {
	switch {
	case d >= time.Second:
		dst = strconv.AppendInt(dst, int64(d/time.Second), 10)
		dst = appendFraction(dst, int64(d%time.Second/(time.Second/100)))
		return append(dst, 's')
	case d >= time.Millisecond:
		dst = strconv.AppendInt(dst, int64(d/time.Millisecond), 10)
		dst = appendFraction(dst, int64(d%time.Millisecond/(time.Millisecond/100)))
		return append(dst, "ms"...)
	case d >= time.Microsecond:
		dst = strconv.AppendInt(dst, int64(d/time.Microsecond), 10)
		dst = append(dst, '.', byte('0'+d%time.Microsecond/(time.Microsecond/10)))
		return append(dst, "µs"...)
	default:
		dst = strconv.AppendInt(dst, d.Nanoseconds(), 10)
		return append(dst, "ns"...)
	}
}

// appendFraction 追加两位小数（n ∈ [0,100)，定宽补零；单位由调用方追加）。
func appendFraction(dst []byte, n int64) []byte {
	if n < 10 {
		return append(dst, '.', '0', byte('0'+n))
	}
	return append(dst, '.', byte('0'+n/10), byte('0'+n%10))
}

// appendTextValue 追加文本值：含空格 / 等号 / 引号 / 控制字符时按 Go 字符串
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

// appendPadding 追加 n 个空格（n <= 0 时什么都不做）。
func appendPadding(dst []byte, n int) []byte {
	for ; n > 0; n-- {
		dst = append(dst, ' ')
	}
	return dst
}

// isTerminal 判断目的地是不是终端：只有 *os.File 且为字符设备才算。
// 重定向到文件 / 管道时为 false（不上色）。
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
