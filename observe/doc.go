// Package observe 是 pulse 的图观测组件：把图引擎的 Observer 回调折成
// 结构化 Record，写进宿主选的 Sink。
//
// # pulse 的两件东西之一
//
// pulse 只有两样：**图引擎**（根包 pulse）与**图观测**（本包）。
// 引擎的依赖闭包为零——它不依赖任何观测包；本包依赖引擎，并实现它的
// Observer seam。图节点分段计时（等待段 / 执行段）由本包的
// NewRecordObserver 折叠，事件名 `pulse.node_wait_finished` /
// `pulse.node_run_finished`，归因维度走 Attrs（`pulse.graph` /
// `pulse.node`，key 契约由引擎定义）。
//
// # 三个概念
//
//   - **Record** 是观测信封：通用字段 + Attrs 开放段。业务维度一律经
//     Attrs 进入，**不扩具名字段**——具名字段只服务于所有记录共有的事实。
//
//   - **Sink** 是出口。默认 **LineSink**——一行一条的人读文本（列式版式、
//     属性插入序、只在终端上色），自带 32 KiB 缓冲、零分配、不经 slog。
//
//   - **Observer 适配**把引擎的三条回调折成记录；引擎不认识 Sink。
//
//     sink := observe.NewLineSink(os.Stdout) // 关闭前必须 Flush()
//     obs, err := observe.NewRecordObserver(observe.ObserveConfig{
//     Sink: sink, HostID: "host-1", TraceID: observe.NewTraceID(),
//     })
//     g, _ := pulse.New(ctx, "demo", pulse.WithObserver(obs))
//     // ... 装图、Seed、Run
//
// 需要接宿主既有 logger、或要 JSON 喂采集器时换 SlogSink（同一批字段、
// 同一顺序，只是给机器读）。
//
// # 宿主自带出口
//
// 上面的列式版式是**默认**的，不是唯一的。宿主想让域事实进列时用
// WithRenderer 换掉行体渲染器，用本包导出的编码原语拼自己的列——出口
// 仍然不认识任何业务语义，域语义留在宿主手里。
//
// 渲染器的契约只有四条：**只产行体**（行首标识与结尾换行由 sink 加）；
// 拿到的 color 是 sink 解析好的结论（不必自己判终端，也不会把 ANSI 写进
// 重定向到文件的日志）；不缓冲、不写 w（写出时机归 sink，WithImmediate
// 控制即时性）；**在 sink 的内部锁内被调用**——别在渲染器里回调 sink 的
// Write / Flush / Err（sync.Mutex 不可重入，会安静挂住），也别长时间阻塞。
//
//	render := func(dst []byte, r observe.Record, color bool) []byte {
//		dst = r.Time.AppendFormat(dst, "2006/01/02 - 15:04:05.000")
//		dst = append(dst, " | "...)
//		node, _ := observe.Get[string](r.Attrs, pulse.AttrNode)
//		dst = observe.AppendTextValue(dst, node)
//		dst = append(dst, " | "...)
//		return observe.AppendDuration(dst, r.Duration)
//	}
//	sink := observe.NewLineSink(os.Stdout,
//		observe.WithImmediate(),
//		observe.WithRenderer(render))
//
// 导出原语（AppendDuration / AppendTextValue / AppendAttrs /
// AppendAttrsExcept / AppendPadding / DisplayWidth）与内置版式**同形**：
// 同一条记录在两种版式下，耗时、属性组、列补齐逐字节一致——一致性由基座
// 保证，宿主只决定「我这个域有哪些列」。可运行的完整示例（含四列与零分配
// 写法）见 Example_hostRenderer。
//
// # TraceID 生成
//
// TraceID 由宿主单一生成源注入：宿主每次运行调用 NewTraceID 一次即构成
// 单一生成源，也可以完全自带方案（宿主自有格式，如 hostID 前缀 + 自增
// 序号）。返回值无契约语义，消费方不要解析其结构。
//
// 需要 W3C trace context 的宿主注入自己的 trace id（OTel SDK 的
// span.SpanContext().TraceID().String()，或按 W3C 规范生成的 32 位小写
// hex）：NewTraceID 的形状是「时间戳-随机段-序号」，不是 W3C 格式；而且
// 只有 trace-id 也发不出一条合法 traceparent——parent-id 与 trace-flags
// 是 span 语义，归宿主（本包只有「一次运行」，没有 span）。
//
// # 隐私边界
//
// Record 无 map[string]any 逃生舱：Attrs 的写入面只有泛型 Set（标量约束
// ~string|~int64|~float64|~bool），prompt、附件字节、密钥、思维链无法通过
// 字段进入。注意边界：Err 是调用方传入的 error——适配层不得把上游原始错误体
// 直接塞入 Err，应传已分类的摘要。
//
// 设计全貌见 docs/design/pulse.md。
package observe
