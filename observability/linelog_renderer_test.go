package observability

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// 本文件验收「宿主自带出口」这条缝（#198）：
//
//   - 导出的编码原语与内置版式**逐字节同形**（耗时 / attrs 组 / 显示列补齐）；
//   - 换渲染器只换**行体**——行首标识、结尾换行、缓冲、颜色判定仍归 sink；
//   - WithImmediate 让每条写完即落 writer。
//
// 版式口径见 LineSink godoc 的「宿主自带出口」一节。

// seamRecord 是接缝测试共用记录：四类标量属性、一个含空格的文本（引号规则）、
// 亚毫秒耗时（单位口径）、CJK 状态（显示列补齐）。
func seamRecord() Record {
	rec := Record{
		HostID:   "h1",
		TraceID:  "tr-1",
		Source:   SourceAdapter,
		Event:    "llm.generate_finished",
		Status:   "运行中",
		Duration: 585*time.Microsecond + 100*time.Nanosecond,
	}
	rec.Time = time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	Set(&rec.Attrs, "llm.model", "gpt-4o-mini")
	Set(&rec.Attrs, "llm.tokens_in", int64(42))
	Set(&rec.Attrs, "llm.cached", true)
	Set(&rec.Attrs, "app.note", "hello world") // 含空格 → 加引号
	return rec
}

// TestPrimitivesMatchBuiltinSegments 原语与内置版式同形。
//
// 整行不可能一致——列序与具名段压缩（`from`/`to` → `state=a→b`）是**版式
// 逻辑**，正是宿主换渲染器要自己决定的东西。能且必须一致的是三处**规则**：
// 耗时口径、attrs 组、按显示列补齐。谁改了这三处而不改原语（或反之），
// 这条测试就红。
func TestPrimitivesMatchBuiltinSegments(t *testing.T) {
	rec := seamRecord()

	var buf bytes.Buffer
	s := NewLineSink(&buf)
	s.Write(rec)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimRight(buf.String(), "\n")

	segs := strings.Split(line, lineSep)
	// 列序：标识 | 时间 | 状态 | 耗时 | 事件 | source | attrs | host | trace
	if len(segs) != 9 {
		t.Fatalf("段数变了（fixture 或版式动了）：%d 段\n%s", len(segs), line)
	}

	// 1) 耗时列：右对齐到 colDuration，去补齐后与 AppendDuration 同形
	//    （含亚毫秒：`585.1µs` 不是 `585µs`）
	dur := string(AppendDuration(nil, rec.Duration))
	if dur != "585.1µs" {
		t.Fatalf("AppendDuration 口径变了：%q", dur)
	}
	wantDur := strings.Repeat(" ", colDuration-DisplayWidth(dur)) + dur
	if segs[3] != wantDur {
		t.Fatalf("耗时列与 AppendDuration 不同形：\n got %q\nwant %q", segs[3], wantDur)
	}

	// 2) attrs 组：整组由 AppendAttrs 产出（插入序 + 按需引号）
	wantAttrs := string(AppendAttrs(nil, rec.Attrs))
	if wantAttrs != `llm.model=gpt-4o-mini llm.tokens_in=42 llm.cached=true app.note="hello world"` {
		t.Fatalf("AppendAttrs 口径变了：%q", wantAttrs)
	}
	if segs[6] != wantAttrs {
		t.Fatalf("attrs 段与 AppendAttrs 不同形：\n got %q\nwant %q", segs[6], wantAttrs)
	}

	// 3) 状态列：按**显示列**补齐（「运行中」3 rune / 6 列）
	if got, want := DisplayWidth(rec.Status), 6; got != want {
		t.Fatalf("DisplayWidth(%q) = %d, want %d", rec.Status, got, want)
	}
	wantStatus := rec.Status + strings.Repeat(" ", colStatus-DisplayWidth(rec.Status))
	if segs[2] != wantStatus {
		t.Fatalf("状态列补齐与 DisplayWidth 不同形：\n got %q\nwant %q", segs[2], wantStatus)
	}
}

// TestAppendTextValueQuoting 引号规则是公开契约（宿主直接用它渲染文本事实）。
func TestAppendTextValueQuoting(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{"/users/42", "/users/42"},
		{"hello world", `"hello world"`},
		{"a=b", `"a=b"`},
		{"say \"hi\"", `"say \"hi\""`},
		{"", `""`},
		{"line\nbreak", `"line\nbreak"`},
		{"运行中", "运行中"}, // 非 ASCII 不需要引号
	} {
		if got := string(AppendTextValue(nil, tc.in)); got != tc.want {
			t.Errorf("AppendTextValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestAppendPaddingNonPositive AppendPadding(n<=0) 什么都不做。
func TestAppendPaddingNonPositive(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		if got := string(AppendPadding(nil, n)); got != "" {
			t.Errorf("AppendPadding(%d) = %q, want 空", n, got)
		}
	}
	if got := string(AppendPadding([]byte("x"), 3)); got != "x   " {
		t.Errorf("AppendPadding(x, 3) = %q", got)
	}
}

// TestDisplayWidth 表驱动钉住宽度近似表。它是**导出即冻结**的口径（godoc 自己
// 写着「这张近似表是公开契约的一部分」），所以每类字符行为都得有断言，不能只靠
// 一个 CJK 用例。
//
// 最后两行的 emoji 是**已知近似**（本表按 1 列算，终端可能给 2 列）。把它们钉在
// 测试里是故意的：将来谁想把 emoji 改判 2 列，这条会先红，就不得不去动 Release
// notes 与冻结清单，而不是悄悄改了就走。
func TestDisplayWidth(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		why  string
	}{
		{"585.1µs", 7, "godoc 举的例子：µ 是 2 字节 1 列"},
		{"e\u0301", 1, "组合记号 0 列"},
		{"\u200b", 0, "零宽空格"},
		{"\u200d", 0, "零宽连接符"},
		{"\ufeff", 0, "零宽不换行空格（BOM）"},
		{"a\x01b", 2, "C0 控制字符不占列"},
		{"a\x7fb", 2, "C1（DEL）不占列"},
		{"Ａ", 2, "全角 ASCII"},
		{"运行中", 6, "CJK 2 列"},
		{"𠀀", 2, "CJK 扩展 B（U+20000）"},
		{"🪵", 1, "近似：emoji 按 1 列（终端可能 2 列）"},
		{"👨‍👩‍👧", 3, "近似：3 个 emoji 各 1 列 + 2 个 ZWJ 各 0 列"},
	} {
		if got := DisplayWidth(tc.in); got != tc.want {
			t.Errorf("DisplayWidth(%q) = %d, want %d（%s）", tc.in, got, tc.want, tc.why)
		}
	}
}

// TestWithRendererReplacesBody 换渲染器只换行体：标识与结尾换行仍由 sink 加，
// 渲染器拿到的 color 是 sink 解析好的结论（不需要自己判终端）。
func TestWithRendererReplacesBody(t *testing.T) {
	var buf bytes.Buffer
	calls, gotColor := 0, false
	s := NewLineSink(&buf,
		WithColor(true), // 显式开：渲染器应拿到 true
		WithRenderer(func(dst []byte, r Record, color bool) []byte {
			calls++
			gotColor = color
			dst = append(dst, "evt="...)
			return append(dst, r.Event...)
		}),
	)
	s.Write(Record{Event: "tool.finished"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("渲染器调用次数 = %d, want 1", calls)
	}
	if !gotColor {
		t.Error("渲染器没拿到 sink 的颜色判定（WithColor(true) 应为 true）")
	}
	// 行首标识（含它的暗淡上色）与结尾换行仍由 sink 负责——换渲染器不该
	// 影响它们；标识上色也仍然听 sink 的（WithColor(true) 下 dim）
	want := ansiDim + "PULSE" + ansiReset + " | evt=tool.finished\n"
	if got := buf.String(); got != want {
		t.Fatalf("行体之外的语义没保住：\n got %q\nwant %q", got, want)
	}
}

// TestWithRendererEmptyPrefix 标识可以关掉（多服务共用一个终端时才需要它）。
func TestWithRendererEmptyPrefix(t *testing.T) {
	var buf bytes.Buffer
	s := NewLineSink(&buf,
		WithPrefix(""),
		WithRenderer(func(dst []byte, r Record, color bool) []byte {
			return append(dst, "body"...)
		}),
	)
	s.Write(Record{})
	_ = s.Flush()
	if got, want := buf.String(), "body\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestWithRendererNilPanics 传 nil 是编程错误，设置时就 panic（不留到运行期）。
func TestWithRendererNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("WithRenderer(nil) 应当 panic")
		}
	}()
	WithRenderer(nil)
}

// TestWithImmediateWritesThrough 每条写完即落 writer（盯终端要即时行）。
func TestWithImmediateWritesThrough(t *testing.T) {
	var buf bytes.Buffer
	s := NewLineSink(&buf, WithImmediate())
	s.Write(Record{Event: "a"})
	if buf.Len() == 0 {
		t.Fatal("WithImmediate() 下写一条就应落到 writer，不必等 Flush")
	}
	s.Write(Record{Event: "b"})
	if got, want := strings.Count(buf.String(), "\n"), 2; got != want {
		t.Fatalf("行数 = %d, want %d", got, want)
	}
	// 默认（32 KiB 缓冲）下同一条记录不会立刻可见——两者的差别是这条 option 的全部意义
	var buf2 bytes.Buffer
	s2 := NewLineSink(&buf2)
	s2.Write(Record{Event: "a"})
	if buf2.Len() != 0 {
		t.Fatalf("默认缓冲下不该立刻可见，实测 %d 字节", buf2.Len())
	}
}

// TestAppendAttrsExceptMatchesAppendAttrs 两条 attrs 导出面是**同一实现**：
// 不跳过任何键时逐字节相同（四类标量与引号规则一并）。谁只改了其中一条，
// 这条先红——「宿主不必重写口径」靠的就是这份同一性。
func TestAppendAttrsExceptMatchesAppendAttrs(t *testing.T) {
	rec := seamRecord()
	want := string(AppendAttrs(nil, rec.Attrs))

	if got := string(AppendAttrsExcept(nil, rec.Attrs)); got != want {
		t.Fatalf("空 skip 与 AppendAttrs 不同形：\n got %q\nwant %q", got, want)
	}
	// 列了不存在的键 = no-op（宿主的列键集里有别的域的词是常态）
	if got := string(AppendAttrsExcept(nil, rec.Attrs, "not.there")); got != want {
		t.Fatalf("跳过不存在的键不该改变输出：\n got %q\nwant %q", got, want)
	}
}

// TestAppendAttrsExceptRenderRest 宿主拿它把「固定列盖不住的属性」补到行尾：
// 输出 = 未被跳过那部分按**插入序**渲染，首条仍不带前导空格。
func TestAppendAttrsExceptRenderRest(t *testing.T) {
	rec := seamRecord() // 插入序：llm.model / llm.tokens_in / llm.cached / app.note

	got := string(AppendAttrsExcept(nil, rec.Attrs, "llm.tokens_in", "llm.cached"))
	if want := `llm.model=gpt-4o-mini app.note="hello world"`; got != want {
		t.Fatalf("跳过中间两条：\n got %q\nwant %q", got, want)
	}
	// 全被跳过 → 0 字节（与空组同一口径）
	if got := string(AppendAttrsExcept(nil, rec.Attrs,
		"llm.model", "llm.tokens_in", "llm.cached", "app.note")); got != "" {
		t.Fatalf("全部被跳过应产出 0 字节，实得 %q", got)
	}
}

// TestAppendAttrsExceptSeparatorPattern 钉住 godoc 里那个**按产出长度**决定
// 要不要补分隔符的写法：attrs 全被固定列吃掉时整组不写，行尾不能留一段空列。
//
// 为什么不能按 `Attrs.Len() > 0` 判空：`Len()` 问的是「组非空」，而这里要问的
// 是「有可渲染项」——两者在全被固定列消费的记录上刚好相反。
func TestAppendAttrsExceptSeparatorPattern(t *testing.T) {
	colKeys := []string{"llm.model", "llm.tokens_in", "llm.cached", "app.note"}
	render := func(dst []byte, a Attrs, skip ...string) []byte {
		mark := len(dst)
		dst = append(dst, lineSep...)
		dst = AppendAttrsExcept(dst, a, skip...)
		if len(dst) == mark+len(lineSep) {
			dst = dst[:mark]
		}
		return dst
	}

	rec := seamRecord()
	if got := string(render(nil, rec.Attrs, colKeys...)); got != "" {
		t.Fatalf("全被列吃掉时应整组不写，实得 %q", got)
	}
	if got, want := string(render(nil, rec.Attrs, colKeys[:3]...)), ` | app.note="hello world"`; got != want {
		t.Fatalf("有剩余属性时应补上分隔符：\n got %q\nwant %q", got, want)
	}
}

// TestAppendAttrsFloatFormat 浮点是四类标量里**唯一有第二选择**的一类，godoc
// 因此把口径写死成 `'g', -1, 64`。这条钉住它：宿主按 `'f'` 拼会在同一个值上
// 写出 `0.000001`，域列于是与同一份 Record 的 attrs 段不同形。
func TestAppendAttrsFloatFormat(t *testing.T) {
	var a Attrs
	Set(&a, "llm.temperature", 1e-06)
	if got, want := string(AppendAttrs(nil, a)), "llm.temperature=1e-06"; got != want {
		t.Fatalf("浮点口径变了：got %q, want %q（'f' 会写成 0.000001）", got, want)
	}
}

// TestAppendAttrsExceptNoAlloc 变参键名不引入分配——宿主的列键集是静态的，
// 包级切片摊开即可复用。这是选变参而不是过滤闭包的直接原因（闭包版本要额外
// 约定「别捕获输出缓冲」）。
func TestAppendAttrsExceptNoAlloc(t *testing.T) {
	rec := seamRecord()
	colKeys := []string{"llm.model", "llm.tokens_in", "llm.cached"}
	buf := make([]byte, 0, 4096)

	if got := testing.AllocsPerRun(500, func() {
		buf = AppendAttrsExcept(buf[:0], rec.Attrs, colKeys...)
	}); got != 0 {
		t.Fatalf("AppendAttrsExcept（切片摊开）分配 = %v, want 0", got)
	}
	if got := testing.AllocsPerRun(500, func() {
		buf = AppendAttrsExcept(buf[:0], rec.Attrs, "llm.model", "llm.tokens_in")
	}); got != 0 {
		t.Fatalf("AppendAttrsExcept（字面量变参）分配 = %v, want 0", got)
	}
	// 对照：同形状的 AppendAttrs（口径相同、只差过滤）
	if got := testing.AllocsPerRun(500, func() {
		buf = AppendAttrs(buf[:0], rec.Attrs)
	}); got != 0 {
		t.Fatalf("AppendAttrs 分配 = %v, want 0", got)
	}
	if len(buf) == 0 {
		t.Fatal("探针没写出内容，量到的不算数")
	}
}
