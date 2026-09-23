package observability

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// 本文件是 LineSink 的契约验收面：版式（列 / 组 / 属性插入序 / 引号 / 占位）、
// 颜色开关、缓冲与 Flush、并发安全、错误捕获。
//
// 版式口径见 LineSink godoc。断言写成**逐字全文比对**而不是 Contains——列宽、
// 分隔符、字段顺序都是契约的一部分，松断言盖不住回归。

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

	// 列：标识 | 时间(25) | 状态(左对齐 10) | 耗时(右对齐 9) | 事件 | 具名 | attrs | host | err | trace
	want := `PULSE | 2026/09/12 - 12:00:00.000 | stop       |     2.50s | llm.generate_finished | source=bridge | ` +
		`llm.model=gpt-4o-mini llm.tokens_in=42 llm.temp=0.7 llm.cached=true app.note="hello world" app.plain=plain | ` +
		`host=h1 | err="boom x" | trace=tr-1` + "\n"
	if got := buf.String(); got != want {
		t.Fatalf("line mismatch:\n got %q\nwant %q", got, want)
	}
}

// TestLineSinkFormatAssemblyRecord 装配期记录（无状态 / 无耗时）走同一版式：
// 空列渲染 `-`，事件列起点与运行期记录一致。
func TestLineSinkFormatAssemblyRecord(t *testing.T) {
	var buf bytes.Buffer
	s := NewLineSink(&buf)
	rec := Record{
		HostID:    "h1",
		Source:    SourceKernel,
		Event:     EventFiberState,
		FiberName: "llmAdapter#3",
		From:      "loading",
		To:        "active",
	}
	rec.Time = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

	s.Write(rec)
	_ = s.Flush()

	want := "PULSE | 2026/09/12 - 12:00:00.000 | -          |         - | pulse.kernel.fiber_state | " +
		"source=kernel | fiber=llmAdapter#3 | state=loading→active | host=h1\n"
	if got := buf.String(); got != want {
		t.Fatalf("line mismatch:\n got %q\nwant %q", got, want)
	}
}

// TestLineSinkColumnsAligned 「规整」的可测形式：状态 / 耗时列宽度不随内容变化，
// 事件列在每行的同一列起始。空状态（`-`）与长耗时同样不破列。
func TestLineSinkColumnsAligned(t *testing.T) {
	var buf bytes.Buffer
	s := NewLineSink(&buf)

	records := []Record{
		{Source: SourceAdapter, Event: "evt", Status: "ok", Duration: 820 * time.Nanosecond},
		{Source: SourceAdapter, Event: "evt", Status: "completed", Duration: 585100 * time.Nanosecond},
		{Source: SourceAdapter, Event: "evt", Duration: 2500 * time.Millisecond},
		{Source: SourceAdapter, Event: "evt", Status: "stop"},
	}
	for _, r := range records {
		s.Write(r)
	}
	_ = s.Flush()

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != len(records) {
		t.Fatalf("lines = %d, want %d", len(lines), len(records))
	}
	at := runeIndex(lines[0], "| evt |")
	if at < 0 {
		t.Fatalf("事件列未按 `| evt |` 成形：%q", lines[0])
	}
	for i, ln := range lines[1:] {
		if got := runeIndex(ln, "| evt |"); got != at {
			t.Fatalf("第 %d 行事件列起点 = %d, want %d\n行：%q", i+1, got, at, ln)
		}
	}

	// 长状态（宿主装配横幅那种一整句）原样输出：列宽是下限不是截断，也不加引号。
	buf.Reset()
	s.Write(Record{
		Source: SourceKernel,
		Event:  EventHostReady,
		Status: "active=3 failed=0 waiting=0 idle=1 total=4",
	})
	_ = s.Flush()
	if !strings.Contains(buf.String(), "| active=3 failed=0 waiting=0 idle=1 total=4 |         - | observability.host_ready |") {
		t.Fatalf("长状态应原样保留：%q", buf.String())
	}
}

// TestLineSinkNoAllocOnHotPath 热路径**零分配**是本出口的公开承诺（README 出口表
// 与 godoc 都写着 0 allocs）。用 AllocsPerRun 锁住它：谁再引入 `Format` 出字符串、
// 经 `Attrs.Range` 装箱（`native()` 逐值装箱）、或按 key 排序（>8 条要 make
// []string），这条立刻变红——分配计数是整数、跨运行确定，不像 ns 会漂。
func TestLineSinkNoAllocOnHotPath(t *testing.T) {
	// 默认 32 KiB 缓存的覆盖情况（实测单行字节数）：3 属性档 200 条约 28 KB，
	// 走纯渲染；10 属性档约 37 KB，会跨一次阈值、把 flush 路径一并覆盖
	// （`io.Discard.Write` 本身不分配，两条路径上的 0 alloc 断言都成立）。
	s := NewLineSink(io.Discard)
	for _, attrs := range []int{3, 10} {
		rec := Record{
			HostID:   "h1",
			TraceID:  "tr-1",
			Source:   SourceAdapter,
			Event:    "llm.generate_finished",
			Status:   "stop",
			Duration: 2500 * time.Millisecond,
		}
		for i := 0; i < attrs; i++ {
			Set(&rec.Attrs, "k."+strconv.Itoa(i), int64(i))
		}
		if got := testing.AllocsPerRun(200, func() { s.Write(rec) }); got != 0 {
			t.Fatalf("%d 个属性时热路径分配 = %v, want 0", attrs, got)
		}
	}
}

// runeIndex 返回 sub 在 s 里的 rune 下标。本用例的语料全是 ASCII（耗时里的
// `µ` 也是 1 列宽），故 rune 下标 == 显示列；含全角字符的场景按显示列逐字比对，
// 见 TestLineSinkCJKStatusAligned。
func runeIndex(s, sub string) int {
	i := strings.Index(s, sub)
	if i < 0 {
		return -1
	}
	return utf8.RuneCountInString(s[:i])
}

// TestLineSinkDurationUnits 耗时带单位且**不取整到毫秒**：旧的
// `duration_ms=0` 让亚毫秒记录看不出快慢（票面第 5 条）。
func TestLineSinkDurationUnits(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string // 9 列定宽的耗时列
	}{
		{0, "        -"},
		{820 * time.Nanosecond, "    820ns"},
		{585100 * time.Nanosecond, "  585.1µs"},
		{7620 * time.Microsecond, "   7.62ms"},
		{1230 * time.Millisecond, "    1.23s"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		s := NewLineSink(&buf)
		rec := Record{Source: SourceAdapter, Event: "evt", Status: "ok", Duration: c.d}
		rec.Time = time.Unix(0, 0).UTC()
		s.Write(rec)
		_ = s.Flush()

		line := strings.TrimSuffix(buf.String(), "\n")
		if !strings.Contains(line, lineSep+c.want+lineSep) {
			t.Fatalf("耗时列 %v 不符：\n got %q\nwant 含 %q", c.d, line, lineSep+c.want+lineSep)
		}
		if strings.Contains(line, "duration_ms=0") {
			t.Fatalf("亚毫秒耗时不得被写成 0：%q", line)
		}
	}
}

// TestLineSinkColor 颜色只在显式开启时出现，且只落在结构信号上
// （标识 / 时间 / trace 暗淡，错误耗时红、≥1s 耗时黄）。
func TestLineSinkColor(t *testing.T) {
	rec := Record{Source: SourceAdapter, Event: "evt", Status: "ok", Duration: 1500 * time.Millisecond}
	rec.Time = time.Unix(0, 0).UTC()

	t.Run("off by default on non-file writer", func(t *testing.T) {
		var buf bytes.Buffer
		s := NewLineSink(&buf)
		s.Write(rec)
		_ = s.Flush()
		if strings.Contains(buf.String(), "\x1b[") {
			t.Fatalf("非终端目的地不得上色：%q", buf.String())
		}
	})

	t.Run("explicit off on colored sink", func(t *testing.T) {
		var buf bytes.Buffer
		s := NewLineSink(&buf, WithColor(false))
		s.Write(rec)
		_ = s.Flush()
		if strings.Contains(buf.String(), "\x1b[") {
			t.Fatalf("WithColor(false) 不得上色：%q", buf.String())
		}
	})

	t.Run("on", func(t *testing.T) {
		var buf bytes.Buffer
		s := NewLineSink(&buf, WithColor(true))
		s.Write(rec)
		_ = s.Flush()
		out := buf.String()
		for _, want := range []struct{ name, code string }{
			{"标识暗淡", ansiDim + DefaultLinePrefix + ansiReset},
			{"时间暗淡", ansiDim + "1970/01/01 - 00:00:00.000" + ansiReset},
			{"≥1s 耗时黄色", ansiYellow},
		} {
			if !strings.Contains(out, want.code) {
				t.Fatalf("%s 未上色：%q", want.name, out)
			}
		}
		if strings.Contains(out, ansiRed) {
			t.Fatalf("无错误不应出现红色：%q", out)
		}
	})

	t.Run("error duration painted red", func(t *testing.T) {
		var buf bytes.Buffer
		s := NewLineSink(&buf, WithColor(true))
		bad := rec
		bad.Duration = time.Microsecond
		bad.Err = errors.New("boom")
		s.Write(bad)
		_ = s.Flush()
		if !strings.Contains(buf.String(), ansiRed) {
			t.Fatalf("有 Err 的耗时应变红：%q", buf.String())
		}
	})
}

// TestLineSinkPrefixOption WithPrefix("") 关闭标识列（多服务共用终端时才需要它），
// 且不影响其余列。票面第 2 条（每行固定 `level=INFO msg=…` 前缀）在人读面的解法。
func TestLineSinkPrefixOption(t *testing.T) {
	var buf bytes.Buffer
	s := NewLineSink(&buf, WithPrefix(""))
	rec := Record{Source: SourceAdapter, Event: "evt", Status: "ok"}
	rec.Time = time.Unix(0, 0).UTC()
	s.Write(rec)
	_ = s.Flush()

	out := buf.String()
	if strings.Contains(out, DefaultLinePrefix) {
		t.Fatalf("WithPrefix(\"\") 后不应有标识：%q", out)
	}
	if !strings.HasPrefix(out, "1970/01/01 - 00:00:00.000 | ok") {
		t.Fatalf("标识关闭后时间列应行首对齐：%q", out)
	}

	buf.Reset()
	s2 := NewLineSink(&buf, WithPrefix("API"))
	s2.Write(rec)
	_ = s2.Flush()
	if !strings.HasPrefix(buf.String(), "API | 1970/01/01") {
		t.Fatalf("自定义标识未生效：%q", buf.String())
	}
}

// TestLineSinkNoFieldLost 固定列盖不住的字段一个不丢：把 Record 的每个字段都填上
// 唯一标记值，逐个断言出现在输出里。装配期与运行期两条路各来一遍。
func TestLineSinkNoFieldLost(t *testing.T) {
	rec := Record{
		HostID:     "mk-host",
		TraceID:    "mk-trace",
		Source:     Source("mk-source"),
		Event:      "mk-event",
		Status:     "mk-status",
		Duration:   time.Second,
		Err:        errors.New("mk-error"),
		FiberName:  "mk-fiber",
		From:       "mk-from",
		To:         "mk-to",
		LoaderKind: "mk-loader",
		EntryID:    "mk-entry",
		PluginName: "mk-plugin",
	}
	Set(&rec.Attrs, "mk.key", "mk-value")

	var buf bytes.Buffer
	s := NewLineSink(&buf)
	s.Write(rec)
	_ = s.Flush()

	line := buf.String()
	for _, want := range []string{
		"mk-host", "mk-trace", "mk-source", "mk-event", "mk-status", "1.00s", "mk-error",
		"mk-fiber", "mk-from", "mk-to", "mk-loader", "mk-entry", "mk-plugin", "mk.key=mk-value",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("字段 %q 未出现在输出里：%q", want, line)
		}
	}
}

// TestLineSinkCJKStatusAligned 状态列按**显示列**补齐：中文状态不能把该行
// 后续列右推。CJK 全角字符 1 rune 占 2 列——`运行中` 是 3 rune 却 6 列，
// 补 4 个空格到 10 列；按 rune 补会补成 7 个空格、事件列右推 3 列。
func TestLineSinkCJKStatusAligned(t *testing.T) {
	cases := []struct {
		status string
		want   string // 10 列定宽的状态列
	}{
		{"ok", "ok        "},        // 2 列 + 8
		{"completed", "completed "}, // 9 列 + 1
		{"运行", "运行      "},          // 4 列 + 6
		{"运行中", "运行中    "},          // 6 列 + 4
	}
	for _, c := range cases {
		var buf bytes.Buffer
		s := NewLineSink(&buf)
		rec := Record{Source: SourceAdapter, Event: "evt", Status: c.status}
		rec.Time = time.Unix(0, 0).UTC()
		s.Write(rec)
		_ = s.Flush()

		line := strings.TrimSuffix(buf.String(), "\n")
		if !strings.Contains(line, lineSep+c.want+lineSep) {
			t.Fatalf("状态 %q 的补齐不符：\n got %q\nwant 含 %q", c.status, line, lineSep+c.want+lineSep)
		}
	}
}

func TestLineSinkBuffersUntilFlushOrThreshold(t *testing.T) {
	var buf bytes.Buffer
	// 阈值要放得下一条完整行（约 70 字节）：太小则首次 Write 就落盘，测不出缓冲。
	s := NewLineSink(&buf, WithBufSize(256))

	s.Write(Record{Event: "small"})
	if buf.Len() != 0 {
		t.Fatalf("未达阈值不应落盘，got %q", buf.String())
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "| small") {
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
		if !strings.HasPrefix(ln, DefaultLinePrefix+" | ") || !strings.Contains(ln, "| evt") {
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

// TestLineSinkFactParityWithSlogSink 两个出口的**事实面**对照：同一条记录分别
// 过 LineSink 与 SlogSink，每个事实都必须在，且**出现的先后完全一致**。
//
// 比的是事实（标记值）而不是字段名字面——人读面把状态 / 耗时 / 事件做成列，
// 把 host_id / error / trace_id 压成 host / err / trace，把 from/to 合成
// state=a→b，把 duration_ms 换成带单位的 1.50s；机器面保持原 key。两边
// 一旦有人改了顺序或漏了字段，这里就红。
func TestLineSinkFactParityWithSlogSink(t *testing.T) {
	rec := Record{
		HostID:     "hostMK",
		TraceID:    "traceMK",
		Source:     Source("srcMK"),
		Event:      "eventMK",
		Status:     "statusMK",
		Duration:   1500 * time.Millisecond,
		Err:        errors.New("errMK"),
		FiberName:  "fiberMK",
		From:       "fromMK",
		To:         "toMK",
		LoaderKind: "loaderMK",
		EntryID:    "entryMK",
		PluginName: "pluginMK",
	}
	Set(&rec.Attrs, "b.key", "attrBMK")
	Set(&rec.Attrs, "a.key", "attrAMK") // 逆序插入：两边都必须照插入序输出

	var lineBuf bytes.Buffer
	ls := NewLineSink(&lineBuf)
	ls.Write(rec)
	_ = ls.Flush()

	var slogBuf bytes.Buffer
	SlogSink{Logger: slog.New(slog.NewTextHandler(&slogBuf, nil))}.Write(rec)

	line, slogLine := lineBuf.String(), slogBuf.String()

	facts := []struct{ name, line, slog string }{
		{"status", "statusMK", "status=statusMK"},
		{"duration", "1.50s", "duration_ms=1500"},
		{"event", "eventMK", "event=eventMK"},
		{"source", "source=srcMK", "source=srcMK"},
		// 具名段是两出口 key 名唯一不同的地方（人读面压缩），必须逐项对照。
		{"fiber", "fiber=fiberMK", "fiber=fiberMK"},
		{"state ← from/to", "state=fromMK→toMK", "from=fromMK to=toMK"},
		{"loader ← loader_kind", "loader=loaderMK", "loader_kind=loaderMK"},
		{"entry ← entry_id", "entry=entryMK", "entry_id=entryMK"},
		{"plugin", "plugin=pluginMK", "plugin=pluginMK"},
		{"attrs 第 1 条（插入序）", "b.key=attrBMK", "b.key=attrBMK"},
		{"attrs 第 2 条（插入序）", "a.key=attrAMK", "a.key=attrAMK"},
		{"host ← host_id", "host=hostMK", "host_id=hostMK"},
		{"err ← error", "err=errMK", "error=errMK"},
		{"trace ← trace_id", "trace=traceMK", "trace_id=traceMK"},
	}

	lineAt, slogAt := -1, -1
	for _, f := range facts {
		li, si := strings.Index(line, f.line), strings.Index(slogLine, f.slog)
		if li < 0 {
			t.Fatalf("LineSink 缺事实 %s（%q）：%q", f.name, f.line, line)
		}
		if si < 0 {
			t.Fatalf("SlogSink 缺事实 %s（%q）：%q", f.name, f.slog, slogLine)
		}
		if li <= lineAt {
			t.Fatalf("LineSink 事实顺序错：%s 在第 %d 列，上一事实在第 %d 列\n%q", f.name, li, lineAt, line)
		}
		if si <= slogAt {
			t.Fatalf("SlogSink 事实顺序错：%s 在第 %d 列，上一事实在第 %d 列\n%q", f.name, si, slogAt, slogLine)
		}
		lineAt, slogAt = li, si
	}
}

// TestLineSinkAttrsInsertionOrderBeyondInlineCap 越过预留容量（attrInlineCap = 6）
// 的自然扩容路径同样按插入序输出：逆序插入 12 条，若还残留排序就会变成正序。
func TestLineSinkAttrsInsertionOrderBeyondInlineCap(t *testing.T) {
	var buf bytes.Buffer
	s := NewLineSink(&buf)
	rec := Record{Source: SourceAdapter, Event: "evt"}
	rec.Time = time.Unix(0, 0).UTC()

	const n = 12
	keys := make([]string, 0, n)
	for i := 0; i < n; i++ {
		keys = append(keys, "k."+string(rune('a'+i)))
	}
	inserted := make([]string, 0, n)
	for i := n - 1; i >= 0; i-- { // 逆序插入
		Set(&rec.Attrs, keys[i], "v")
		inserted = append(inserted, keys[i])
	}

	s.Write(rec)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSuffix(buf.String(), "\n")

	prev := -1
	for _, k := range inserted {
		at := strings.Index(line, k+"=")
		if at < 0 {
			t.Fatalf("属性 %q 丢失：%q", k, line)
		}
		if at <= prev {
			t.Fatalf("属性 %q 未按插入序（在第 %d 列，上一条在第 %d 列）：%q", k, at, prev, line)
		}
		prev = at
	}
}

// TestLineSinkQuotesKeyWhenNeeded 组内 k=v 的键与值同规则：含空格 / 等号的
// key 也加引号（与 slog.TextHandler 的 needsQuoting 口径一致）。
func TestLineSinkQuotesKeyWhenNeeded(t *testing.T) {
	var buf bytes.Buffer
	s := NewLineSink(&buf)
	rec := Record{Source: SourceAdapter, Event: "evt"}
	rec.Time = time.Unix(0, 0).UTC()
	Set(&rec.Attrs, "weird key", "v") // 含空格 → 加引号
	Set(&rec.Attrs, "plain.key", "v")
	Set(&rec.Attrs, "eq.key", "a=b") // 值含等号 → 加引号
	s.Write(rec)
	_ = s.Flush()

	out := buf.String()
	for _, want := range []string{`"weird key"=v`, "plain.key=v", `eq.key="a=b"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("%q 未按引号规则输出：%q", want, out)
		}
	}
}

// TestLineSinkQuotesValueWhenNeeded 尾段字段（host / err / trace）同一条
// 引号口径：TraceID 不只来自 NewTraceID——宿主可填外部 trace id，原样追加
// 会让含换行的值注入额外行，破坏「一行一记录」。
func TestLineSinkQuotesValueWhenNeeded(t *testing.T) {
	trace := "a b\"c\nd"
	var buf bytes.Buffer
	s := NewLineSink(&buf)
	rec := Record{Source: SourceAdapter, Event: "evt", HostID: "h 1", TraceID: trace}
	rec.Time = time.Unix(0, 0).UTC()
	s.Write(rec)
	_ = s.Flush()

	out := buf.String()
	if want := "trace=" + strconv.Quote(trace); !strings.Contains(out, want) {
		t.Fatalf("trace 未按引号口径输出：\n got %q\nwant 含 %q", out, want)
	}
	if want := `host="h 1"`; !strings.Contains(out, want) {
		t.Fatalf("host 口径回归：%q", out)
	}
	// 换行必须转义：一条记录只占一行。
	if got := strings.Count(out, "\n"); got != 1 {
		t.Fatalf("行数 = %d, want 1（换行必须转义）：%q", got, out)
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
