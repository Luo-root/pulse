package observability

import (
	"io"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// LineOption 配置 LineSink。
type LineOption func(*lineOpts)

type lineOpts struct {
	bufSize  int
	prefix   string
	color    bool
	colorAt  bool // 显式设过 color；未设 = 按目的地自动判断
	renderer LineRenderer
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

// WithImmediate 让每条记录写完即落 io.Writer（不攒批）。盯终端时要用它——
// 缺省 32 KiB 缓冲意味着最后一批可能长时间停在内存里。代价是每条一次
// writer 调用；缓冲仍复用，热路径的零分配承诺不变。
//
// 等价于 WithBufSize(1)（阈值 1 字节 ⇒ 每条都触发写出）：后者是这条语义
// 的通用旋钮，本选项只是把「行即写」这件事写成名字。
func WithImmediate() LineOption {
	return func(o *lineOpts) { o.bufSize = 1 }
}

// LineRenderer 渲染**行体**：把一条记录追加到 dst 并返回新的 dst。
//
// 契约：
//   - 只产行体——行首标识（WithPrefix，含它的暗淡上色）与结尾换行由
//     LineSink 负责，它们属于 sink 语义，不属于版式；
//   - color 是 sink 已经解析好的「当前是否上色」（TTY 判定 + WithColor
//     覆盖的结果）。渲染器不必自己判断目的地，也不会把 ANSI 写进重定向
//     到文件的日志里；
//   - 不缓冲、不写 w——写出时机由 sink 决定（缺省攒批，WithImmediate 每条
//     即写）；
//   - **在 sink 的内部锁内被调用**：别在渲染器里回调 sink 自己的方法
//     （`Write` / `Flush` / `Err`——`sync.Mutex` 不可重入，会死锁，而且表现
//     是安静挂住不是报错），也别长时间阻塞，那会挡住所有并发写入；
//   - 零分配由渲染器自己负责：热路径上每次 Write 都会调它一次，分配一次
//     就是每条一次。
//
// 想让**域事实进列**（HTTP 的方法 / 路径 / 客户端，LLM 的模型 / 用量……）
// 时提供自己的实现，用本包导出的编码原语（AppendDuration /
// AppendTextValue / AppendAttrs / AppendAttrsExcept / AppendPadding /
// DisplayWidth）保证与内置版式同形——出口本身不认识任何业务语义，域语义
// 留在宿主手里。默认渲染器的用法见包文档「宿主自带出口」一节。
type LineRenderer func(dst []byte, r Record, color bool) []byte

// WithRenderer 替换行体渲染器（缺省是内置列式版式）。传 nil 视为编程错误，
// 立即 panic。
func WithRenderer(fn LineRenderer) LineOption {
	if fn == nil {
		panic("observability: WithRenderer requires a non-nil LineRenderer")
	}
	return func(o *lineOpts) { o.renderer = fn }
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
// 配色等于把业务语义搬进基座。要按**自己域的**语义上色（或让域事实进列）时，
// 用 WithRenderer 换行体渲染器，见下面「宿主自带出口」。
//
// # 宿主自带出口
//
// 上面这套列序是**默认**版式，不是唯一版式。宿主想让域事实进列（HTTP 的
// 方法 / 路径 / 客户端、LLM 的模型 / 用量……）时用 `WithRenderer` 换掉行体
// 渲染器，用本包导出的编码原语拼自己的列——出口仍然不认识任何业务语义，
// 域语义留在宿主手里；`color` 由 sink 解析好交给渲染器，宿主不必自己判断
// 终端、也不会把 ANSI 写进重定向到文件的日志里。缓冲、Flush、写错误、行首
// 标识、结尾换行这些**出口语义**由 sink 统一负责，与换不换渲染器无关。
// 完整示例见包文档与 README 的「宿主自带出口」一节。
//
// # 成本
//
// 不经 `log/slog`：没有 `[]any` 逐字段装箱、没有 slog.Value 转换、没有每条的
// 键排序分配——数字走 `strconv.Append*`，时间走 `AppendFormat`，属性直接读
// 内部条目（不经 `Range` 的闭包与 `native()` 装箱）。实测（`sink_bench_test.go`，
// 同一会话）渲染成本约为 `SlogSink` 的 **1/5**，且**零分配**（`SlogSink`
// 每条 14 allocs）——这是它当默认出口的底气。换渲染器不改变这些：热路径仍是
// 「追加进缓冲」，是否分配取决于渲染器自己（默认渲染器 0 allocs）。
//
// # 契约
//
//   - **并发安全**：内部一把锁保护缓冲与出口；
//   - **写错误**记在 Err()（首错为准），不 panic、不阻断后续写入；
//     错误后的缓冲会被丢弃（不无限增长）；
//   - **关闭前 Flush()**：未达阈值的最后一批仍在内存里；要每条即时可见用
//     WithImmediate()；
//   - 需要 JSON 结构化输出、或要接宿主既有 logger 时，用 SlogSink。
type LineSink struct {
	mu        sync.Mutex
	w         io.Writer
	buf       []byte
	threshold int
	prefix    string
	color     bool
	renderer  LineRenderer
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
		renderer:  o.renderer,
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

// appendLine 编码一条记录为完整行（含换行）：行首标识 + 行体 + 换行。
//
// 行体交给渲染器（缺省 appendBody，即内置列式版式；WithRenderer 可换）。
// 标识与换行留在 sink：前者是「哪个服务在说话」的分栏锚点，后者是行式出口
// 的定义——两者都与版式无关，换渲染器不该影响它们。
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

	if s.renderer != nil {
		dst = s.renderer(dst, r, s.color)
	} else {
		dst = s.appendBody(dst, r)
	}
	return append(dst, '\n')
}

// appendBody 编码内置版式的**行体**（不含标识与换行）。列序见 LineSink
// godoc：时间 → 状态 → 耗时 → 事件 → 具名段 → attrs → host → err → trace。
func (s *LineSink) appendBody(dst []byte, r Record) []byte {
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

	return s.appendNamedFields(dst, r)
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
		return AppendPadding(dst, colStatus-DisplayWidth(emptyCol))
	}
	dst = append(dst, r.Status...)
	return AppendPadding(dst, colStatus-DisplayWidth(r.Status))
}

// appendDurationCol 耗时列：右对齐、带单位。零值（未计时）渲染 `-`——写
// `0ns` 会被读成「测出来是 0」，与「没有这个事实」不是一件事。
func (s *LineSink) appendDurationCol(dst []byte, r Record) []byte {
	if r.Duration == 0 {
		dst = AppendPadding(dst, colDuration-len(emptyCol))
		return append(dst, emptyCol...)
	}
	var scratch [16]byte
	text := AppendDuration(scratch[:0], r.Duration)
	code := ""
	if r.Err != nil {
		code = ansiRed
	} else if r.Duration >= time.Second {
		code = ansiYellow
	}
	painted := false
	dst, painted = s.paint(dst, code)
	// 耗时文本只有 ASCII 数字与 `µ`（两者都是 1 列宽），rune 数 == 显示列数。
	dst = AppendPadding(dst, colDuration-utf8.RuneCount(text))
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
		dst = AppendTextValue(dst, string(r.Source))
	}
	if r.FiberName != "" {
		dst = append(dst, lineSep...)
		dst = append(dst, "fiber="...)
		dst = AppendTextValue(dst, r.FiberName)
	}
	if r.From != "" || r.To != "" {
		dst = append(dst, lineSep...)
		dst = append(dst, "state="...)
		dst = AppendTextValue(dst, r.From)
		dst = append(dst, "→"...)
		dst = AppendTextValue(dst, r.To)
	}
	if r.LoaderKind != "" {
		dst = append(dst, lineSep...)
		dst = append(dst, "loader="...)
		dst = AppendTextValue(dst, r.LoaderKind)
	}
	if r.EntryID != "" {
		dst = append(dst, lineSep...)
		dst = append(dst, "entry="...)
		dst = AppendTextValue(dst, r.EntryID)
	}
	if r.PluginName != "" {
		dst = append(dst, lineSep...)
		dst = append(dst, "plugin="...)
		dst = AppendTextValue(dst, r.PluginName)
	}
	return s.appendTail(dst, r)
}

// appendTail 追加尾段：attrs（插入序）→ host → err → trace。
// 顺序与 SlogSink 一致（错误在关联 id 之前：读日志先看失败原因，trace 是
// 辅助定位，也是线上最不常读的一段，所以放在最后）。
func (s *LineSink) appendTail(dst []byte, r Record) []byte {
	if r.Attrs.Len() > 0 {
		dst = append(dst, lineSep...)
		dst = AppendAttrs(dst, r.Attrs)
	}
	if r.HostID != "" {
		dst = append(dst, lineSep...)
		dst = append(dst, "host="...)
		dst = AppendTextValue(dst, r.HostID)
	}
	if r.Err != nil {
		dst = append(dst, lineSep...)
		painted := false
		dst, painted = s.paint(dst, ansiRed)
		dst = append(dst, "err="...)
		dst = AppendTextValue(dst, r.Err.Error())
		dst = s.unpaint(dst, painted)
	}
	if r.TraceID != "" {
		dst = append(dst, lineSep...)
		painted := false
		dst, painted = s.paint(dst, ansiDim)
		dst = append(dst, "trace="...)
		// 与 host= / err= 同一条引号口径：TraceID 不只来自
		// NewTraceID——宿主可填入外部 trace id（W3C traceparent 之类），
		// 原样追加会让含空格 / 引号 / 换行的值打乱列宽甚至注入额外行。
		dst = AppendTextValue(dst, r.TraceID)
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

// AppendAttrs 按**插入序**追加整组属性（`k=v k=v`，组内单空格分隔，首条不带
// 前导空格——调用方已经补过 ` | ` 分隔符）。
//
// 宿主自带出口时直接用它渲染 attrs 段，不必重写「四类标量 + 按需引号」这套
// 规则；输出与内置版式的 attrs 段逐字节同形。要挑单个事实进域列用
// `Get[T](a, key)` 取类型化值，再按同一口径拼——四类标量各只有一行：
//
//	string   AppendTextValue(dst, s)
//	int64    strconv.AppendInt(dst, i, 10)
//	float64  strconv.AppendFloat(dst, f, 'g', -1, 64)
//	bool     strconv.AppendBool(dst, b)
//
// 浮点是唯一有第二选择的：`'f'` 更顺手，但同一个值会写出不同字节
// （`1e-06` → `0.000001`），同一个域列值与内置 attrs 段就不同形了。
//
// **空 Attrs（Len() == 0）产出 0 字节**：组间分隔符由调用方按需加——内置版式
// 的写法是 `if r.Attrs.Len() > 0 { dst = append(dst, lineSep...); ... }`。
// 直接「先补分隔符再调它」会在无属性记录上多出一段空列。
//
// **口径稳定**：插入序、引号规则与标量渲染是各出口共用的一致性资产，改动随
// minor 发布（见 README 的冻结清单）。
func AppendAttrs(dst []byte, a Attrs) []byte {
	return appendAttrsSkipping(dst, a, nil)
}

// AppendAttrsExcept 与 AppendAttrs **同一口径**（插入序 + 四类标量 + 按需
// 引号），但跳过 `skip` 里列出的键。宿主把已经渲染成固定列的键名传进来，剩下
// 的就是「固定列盖不住、但**不该丢**」的属性——这条不变式内置版式自己就在守
// （固定列盖不住的属性照样出现在行尾），导出原语只是把它变成宿主可复用的形态。
//
// 键名走变参而不是过滤闭包：宿主的列键集是**静态**的，包级一张切片
// `colKeys...` 摊开即可；回调式过滤还要额外约定「别在闭包捕获输出缓冲」。
//
// skip 里列不存在的键是 no-op；skip 为空等价于 AppendAttrs。
//
// **全被跳过时同样产出 0 字节**（与空组一致），所以分隔符不能先写、也不能拿
// `Attrs.Len() > 0` 判空——`Len()` 问的是「组非空」，不是「有可渲染项」。按
// **产出长度**决定：
//
//	mark := len(dst)
//	dst = append(dst, " | "...)
//	dst = AppendAttrsExcept(dst, r.Attrs, colKeys...)
//	if len(dst) == mark+len(" | ") {
//		dst = dst[:mark] // 全被固定列吃掉，这一组不写
//	}
//
// **口径稳定**：与 AppendAttrs 同属冻结面，改动随 minor 发布（见 README 的
// 冻结清单）。
func AppendAttrsExcept(dst []byte, a Attrs, skip ...string) []byte {
	return appendAttrsSkipping(dst, a, skip)
}

// appendAttrsSkipping 是 AppendAttrs / AppendAttrsExcept 的**同一实现**：两条
// 导出面只差「跳过哪些键」，顺序 / 标量 / 引号这套口径不存在第二份。
//
// 首条不带前导空格按 `first` 判定而不是下标 `i > 0`——有跳过时下标不再是
// 「第几个写出的」。
//
// 旧实现按 key 字典序输出并为此排序（≤8 个键走栈上插入排序）——但字典序不是
// 阅读序，出口也排不出来：`http.request.method` 该排在 `http.response.body.size`
// 前面是 HTTP 知识，出口不认识。Attrs 内部本来就是插入序切片（#179），产生方
// 的写的顺序就是它想被读到的顺序，直接照抄即可，还省掉一次排序。
//
// 不走 `Attrs.Range`：回调把值交出去时是 `any`（`native()`），省不省得掉那份
// 装箱取决于调用点形状（闭包能否内联、值会不会转手给不可内联的辅助函数）——
// 出口是热路径，这里不赌编译器。
func appendAttrsSkipping(dst []byte, a Attrs, skip []string) []byte {
	first := true
	for i := range a.entries {
		e := &a.entries[i]
		if len(skip) > 0 && slices.Contains(skip, e.key) {
			continue
		}
		if !first {
			dst = append(dst, ' ')
		}
		first = false
		dst = AppendTextValue(dst, e.key)
		dst = append(dst, '=')
		switch e.val.kind {
		case attrString:
			dst = AppendTextValue(dst, e.val.s)
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

// DisplayWidth 返回 s 在终端里占的**显示列**数：CJK / 假名 / 韩文音节 /
// 全角形式按 2 列，组合记号与零宽字符按 0 列，C0/C1 控制字符不占列，
// 其余按 1 列。宿主自带出口时用它算列补齐——配 AppendPadding。
//
// 为什么不用 utf8.RuneCountInString：中文状态（「运行中」）是 3 rune 但
// 占 6 列，按 rune 补齐会把该行后续列整体右推——而状态正是宿主可配的
// 自由文本（本包 godoc 自己举了「宿主装配横幅那种一整句」的例子）。
//
// 表是常见区段的**近似**（东亚宽度 East Asian Width 的子集）：只为列补齐而引
// golang.org/x/text/width 不值当。覆盖不到的（emoji 变体选择符、罕用宽字符）
// 按 1 列算，与终端可能差 1 列。
//
// **口径稳定**：这张近似表是公开契约的一部分——修正它（比如把 emoji 改判 2 列）
// 会改变宿主的列对齐结果，因此随 minor 发布。
func DisplayWidth(s string) int {
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

// AppendDuration 追加带单位的耗时：`820ns` / `585.1µs` / `7.62ms` / `1.23s`。
// 宿主自带出口时用它渲染耗时列——与内置版式同一条口径，不会出现一个出口
// 写 `7.62ms`、另一个写 `7ms`。
//
// 刻意**不取整到毫秒**：旧的 `duration_ms` 把亚毫秒记录写成 0，快慢全看不出来。
// 单位按量级选，µs 保留一位小数，ms / s 保留两位。
//
// 全程整数运算（取整 + 补零），**不用 strconv.AppendFloat**：`'f'` 定点格式化走
// strconv 的十进制大数路径，实测每条多花约 120 ns（占整行渲染成本的三分之一），
// 换来的只是「999.999µs 进位成 1000.0µs」这种假进位。整数版更快也更老实。
//
// **口径稳定**：单位选择与小数位数是各出口同形的依据，改动随 minor 发布。
func AppendDuration(dst []byte, d time.Duration) []byte {
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

// AppendTextValue 追加文本值：含空格 / 等号 / 引号 / 控制字符时按 Go 字符串
// 字面量加引号（与 slog.TextHandler 的 needsQuoting 精神一致）；空串也算需要
// 引号，渲染成 `""`。
//
// 注意**空串渲染成 `""` 是有值的样子**：「没有这个事实」与「值就是空串」是
// 两件事，缺列的占位属版式选择、由宿主决定——内置版式对缺列用 `-`。取属性时
// 用 `Get[T]` 的 ok 位区分，别把零值当缺值。
//
// 宿主自带出口时用它渲染文本事实与键名，保证与内置版式同一条引号规则——
// 同一段含空格的错误文本不会在一个出口加引号、在另一个不加。
//
// **口径稳定**：改动随 minor 发布。
func AppendTextValue(dst []byte, s string) []byte {
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

// AppendPadding 追加 n 个空格（n <= 0 时什么都不做）。与 DisplayWidth 配对做
// 列补齐：`AppendPadding(dst, width-DisplayWidth(s))`。
//
// **口径稳定**：改动随 minor 发布。
func AppendPadding(dst []byte, n int) []byte {
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
