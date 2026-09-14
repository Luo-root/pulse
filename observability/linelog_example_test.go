package observability_test

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/observability"
)

// 本文件是「宿主自带出口」的**可运行**示例（#198）：想让域事实进列时，换掉
// 行体渲染器，用包内导出的编码原语拼自己的列——出口仍然不认识任何业务语义。
//
// 放在外部测试包：示例要用 llm 的属性 key 常量（框架自己的域事实），而 llm
// 反向依赖 observability（折叠适配器），同包测试里 import 会成环。

// Example_hostRenderer 宿主自带出口：时间 | 模型 | 用量 | 耗时。
//
// 行体之外的语义仍归 LineSink：行首标识（WithPrefix）、结尾换行、缓冲与
// Flush、写错误收集、颜色判定。渲染器只负责「这条记录长什么样」。
func Example_hostRenderer() {
	// 宿主自己的渲染器：color 是 sink 解析好的结论（TTY 判定 + WithColor
	// 覆盖），所以宿主不必自己判断目的地，也不会把 ANSI 写进重定向的日志。
	render := func(dst []byte, r observability.Record, color bool) []byte {
		dst = r.Time.AppendFormat(dst, "2006/01/02 - 15:04:05.000")

		// 域事实取自 attrs：Get[T] 拿类型化值，不用经过 any
		model, _ := observability.Get[string](r.Attrs, llm.AttrModel)
		dst = append(dst, " | "...)
		dst = observability.AppendTextValue(dst, model)
		dst = observability.AppendPadding(dst, 12-observability.DisplayWidth(model))

		in, _ := observability.Get[int64](r.Attrs, llm.AttrTokensIn)
		out, _ := observability.Get[int64](r.Attrs, llm.AttrTokensOut)
		dst = append(dst, " | "...)
		dst = strconv.AppendInt(dst, in, 10)
		dst = append(dst, '/')
		dst = strconv.AppendInt(dst, out, 10)

		// 耗时复用同一口径（带单位、不取整）；右对齐用栈上小缓冲算长度，
		// 不为了量宽度分配字符串
		var scratch [16]byte
		text := observability.AppendDuration(scratch[:0], r.Duration)
		dst = append(dst, " | "...)
		dst = observability.AppendPadding(dst, 9-utf8.RuneCount(text))
		return append(dst, text...)
	}

	var buf bytes.Buffer
	sink := observability.NewLineSink(&buf,
		observability.WithImmediate(), // 盯终端：每条写完即见
		observability.WithRenderer(render),
	)

	rec := observability.Record{Event: "llm.generate_finished", Duration: 7620 * time.Microsecond}
	rec.Time = time.Date(2026, 9, 14, 12, 42, 3, 0, time.UTC)
	observability.Set(&rec.Attrs, llm.AttrModel, "gpt-4o-mini")
	observability.Set(&rec.Attrs, llm.AttrTokensIn, int64(1250))
	observability.Set(&rec.Attrs, llm.AttrTokensOut, int64(180))
	sink.Write(rec)

	fmt.Print(buf.String())
	// Output:
	// PULSE | 2026/09/14 - 12:42:03.000 | gpt-4o-mini  | 1250/180 |    7.62ms
}

// TestHostRendererNoAlloc 宿主渲染器同样可以零分配——本包对默认出口的承诺
// 不因换渲染器而失效，但前提是渲染器自己别分配（原语都是追加式、不返回新
// 分配的东西）。这条钉住「导出原语不引入隐藏分配」。
func TestHostRendererNoAlloc(t *testing.T) {
	rec := observability.Record{
		HostID:   "h1",
		TraceID:  "tr-1",
		Source:   observability.SourceAdapter,
		Event:    "llm.generate_finished",
		Status:   "stop",
		Duration: 7620 * time.Microsecond,
	}
	rec.Time = time.Date(2026, 9, 14, 12, 42, 3, 0, time.UTC)
	observability.Set(&rec.Attrs, llm.AttrModel, "gpt-4o-mini")
	observability.Set(&rec.Attrs, llm.AttrTokensIn, int64(1250))
	observability.Set(&rec.Attrs, llm.AttrTokensOut, int64(180))

	s := observability.NewLineSink(io.Discard, observability.WithRenderer(
		func(dst []byte, r observability.Record, color bool) []byte {
			var scratch [16]byte
			dst = r.Time.AppendFormat(dst, "2006/01/02 - 15:04:05.000")
			dst = append(dst, " | "...)
			dst = observability.AppendAttrs(dst, r.Attrs)
			dst = append(dst, " | "...)
			text := observability.AppendDuration(scratch[:0], r.Duration)
			dst = observability.AppendPadding(dst, 9-utf8.RuneCount(text))
			return append(dst, text...)
		}))

	if got := testing.AllocsPerRun(200, func() { s.Write(rec) }); got != 0 {
		t.Fatalf("宿主渲染器热路径分配 = %v, want 0", got)
	}
}
