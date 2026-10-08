package observe_test

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/observe"
)

// 本文件是「宿主自带出口」的**可运行**示例：想让域事实进列时，换掉
// 行体渲染器，用包内导出的编码原语拼自己的列——出口仍然不认识任何
// 业务语义。
//
// 放在外部测试包：示例要同时用 pulse 引擎的 attrs key 常量与本包出口。

// Example_hostRenderer 宿主自带出口：时间 | 节点 | 步数 | 耗时。
//
// 行体之外的语义仍归 LineSink：行首标识（WithPrefix）、结尾换行、缓冲与
// Flush、写错误收集、颜色判定。渲染器只负责「这条记录长什么样」。
func Example_hostRenderer() {
	// 宿主自己的渲染器：color 是 sink 解析好的结论（TTY 判定 + WithColor
	// 覆盖），所以宿主不必自己判断目的地，也不会把 ANSI 写进重定向的日志。
	render := func(dst []byte, r observe.Record, color bool) []byte {
		dst = r.Time.AppendFormat(dst, "2006/01/02 - 15:04:05.000")

		// 域事实取自 attrs：Get[T] 拿类型化值（不经 any），**看 ok 位**——
		// 缺属性很正常（不是每条记录都带节点），缺了按自己的版式渲染
		// 占位符，别把零值当有值
		node, ok := observe.Get[string](r.Attrs, pulse.AttrNode)
		if !ok {
			node = "-" // 占位是版式选择；内置版式缺列同样用 `-`
		}
		dst = append(dst, " | "...)
		dst = observe.AppendTextValue(dst, node)
		dst = observe.AppendPadding(dst, 12-observe.DisplayWidth(node))

		// 宿主自定义维度用自述 key（<组件>.<字段> 点分约定）
		steps, okSteps := observe.Get[int64](r.Attrs, "host.steps")
		dst = append(dst, " | "...)
		if !okSteps {
			dst = append(dst, "-"...)
		} else {
			dst = strconv.AppendInt(dst, steps, 10)
		}

		// 耗时复用同一口径（带单位、不取整）；右对齐用栈上小缓冲算长度，
		// 不为了量宽度分配字符串
		var scratch [16]byte
		text := observe.AppendDuration(scratch[:0], r.Duration)
		dst = append(dst, " | "...)
		dst = observe.AppendPadding(dst, 9-utf8.RuneCount(text))
		return append(dst, text...)
	}

	var buf bytes.Buffer
	sink := observe.NewLineSink(&buf,
		observe.WithImmediate(), // 盯终端：每条写完即见
		observe.WithRenderer(render),
	)

	rec := observe.Record{Event: observe.EventNodeRunFinished, Duration: 7620 * time.Microsecond}
	rec.Time = time.Date(2026, 9, 14, 12, 42, 3, 0, time.UTC)
	observe.Set(&rec.Attrs, pulse.AttrNode, "summarize")
	observe.Set(&rec.Attrs, "host.steps", int64(3))
	sink.Write(rec)

	fmt.Print(buf.String())
	// Output:
	// PULSE | 2026/09/14 - 12:42:03.000 | summarize    | 3 |    7.62ms
}

// TestHostRendererNoAlloc 宿主渲染器同样可以零分配——本包对默认出口的承诺
// 不因换渲染器而失效，但前提是渲染器自己别分配（原语都是追加式、不返回新
// 分配的东西）。这条钉住「导出原语不引入隐藏分配」。
func TestHostRendererNoAlloc(t *testing.T) {
	rec := observe.Record{
		HostID:   "h1",
		TraceID:  "tr-1",
		Source:   observe.SourceObserver,
		Event:    observe.EventNodeRunFinished,
		Status:   "completed",
		Duration: 7620 * time.Microsecond,
	}
	rec.Time = time.Date(2026, 9, 14, 12, 42, 3, 0, time.UTC)
	observe.Set(&rec.Attrs, pulse.AttrNode, "summarize")
	observe.Set(&rec.Attrs, "host.steps", int64(3))

	s := observe.NewLineSink(io.Discard, observe.WithRenderer(
		func(dst []byte, r observe.Record, color bool) []byte {
			var scratch [16]byte
			dst = r.Time.AppendFormat(dst, "2006/01/02 - 15:04:05.000")
			// AppendAttrs 对空组产 0 字节，分隔符得自己按 Len() 判断——
			// 直接「先补 ` | ` 再调它」会在无属性记录上多出一段空列
			if r.Attrs.Len() > 0 {
				dst = append(dst, " | "...)
				dst = observe.AppendAttrs(dst, r.Attrs)
			}
			dst = append(dst, " | "...)
			text := observe.AppendDuration(scratch[:0], r.Duration)
			dst = observe.AppendPadding(dst, 9-utf8.RuneCount(text))
			return append(dst, text...)
		}))

	if got := testing.AllocsPerRun(200, func() { s.Write(rec) }); got != 0 {
		t.Fatalf("宿主渲染器热路径分配 = %v, want 0", got)
	}
}
