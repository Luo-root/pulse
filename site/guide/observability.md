# 图观测

观测是**独立的一层**：引擎不认识任何观测包，只暴露一个 `Observer` seam；`pulse/observe` 实现这个 seam，把回调折成结构化记录写进出口。不需要观测的宿主只 import 根包。

## 引擎的 seam

```go
type Observer interface {
	OnNodeWaiting(graphID, nodeID string)
	OnNodeRunning(graphID, nodeID string)
	OnNodeFinished(graphID, nodeID string, reason NodeFinishReason, err error)
}
```

契约四条，都是冻结面：

1. **回调次数**：每个节点 `Waiting ≤ 1`、`Running ≤ 1`、`Finished = 1`。`Retry` 的多次 attempt **不重复打点**；
2. **只读**：观察者的 panic 与 error **不得**升格为节点失败（引擎侧已吞掉）；
3. **并发安全**：回调在**节点自己的 goroutine** 上同步执行——所以实现必须并发安全，且不得长时间阻塞；
4. **归因键由引擎给出**：`graphID` 随每次回调发出，实现侧不必从构造参数自行携带。

图默认 no-op（不挂就没有任何开销），`pulse.WithObserver(...)` 挂载；要多个观察者用 `pulse.MultiObserver` 组合（后写覆盖前写，所以组合要在传参前做完）。

## observe 把它折成两条分段计时

| 事件 | 何时产出 | `Duration` | `Status` |
|---|---|---|---|
| `pulse.node_wait_finished` | 等待结束（进入执行，或以 skip / 失败终结） | 等待段 | `running`，否则是对应的终态 |
| `pulse.node_run_finished` | 执行结束 | 执行段 | 终态（`completed` / `failed` / `canceled`） |

跳过节点**只有一条 `skipped` 等待记录**——它确实到达了，只是没执行。归因维度走 `Attrs`：`pulse.AttrGraph` + `pulse.AttrNode`（key 契约由**引擎**定义，`observe` 只消费不定义）。

## 最短接入

```go
sink := observe.NewLineSink(os.Stdout, observe.WithImmediate())
defer sink.Flush() // 关闭前必须 Flush：最后一批还在缓冲里

obs, err := observe.NewRecordObserver(observe.ObserveConfig{
	Sink:    sink,
	HostID:  "quickstart",              // 宿主身份（跨运行稳定），与 TraceID 组成「谁 + 哪一次」
	TraceID: observe.NewTraceID(),      // 本次运行的关联 id（运行期事实）
})
if err != nil {
	return err
}
g, err := pulse.New(ctx, "docs-pipeline", pulse.WithObserver(obs))
```

同一个图（单节点）的真实输出：

```text
PULSE | 2026/10/08 - 15:32:14.066 | running    |         - | pulse.node_wait_finished | source=observe | pulse.graph=docs-pipeline pulse.node=summarize | host=quickstart | trace=1791444734065505900-3ca14140-1
PULSE | 2026/10/08 - 15:32:14.082 | completed  |   15.98ms | pulse.node_run_finished | source=observe | pulse.graph=docs-pipeline pulse.node=summarize | host=quickstart | trace=1791444734065505900-3ca14140-1
```

行体列序：`时间 | 状态 | 耗时 | 事件 | source | attrs | host | err | trace`。缺的状态或耗时列渲染 `-`，于是事件列起点恒定（`-` 是**内置版式的空列占位**，不是原语行为）。时间戳、trace 与耗时随运行变化。

## 出口

`Sink` 是唯一需要你实现的接口：

```go
type Sink interface{ Write(r Record) }
```

契约三条：

1. **并发安全**，且不得长时间阻塞调用方（`Write` 在节点 goroutine 的回调路径上）；
2. **没有 `context.Context`**——回调不带 ctx；需要截止时间的导出器自行持有内部队列；
3. **引用语义**：产出方构造独立 `Attrs`，`Write` 返回后不再修改；Sink 只读消费。异步导出器必须自己拷贝。

内置出口：

| 出口 | 形态 | 适用 |
|---|---|---|
| `LineSink` | **默认**。一行一条的人读文本，自带 32 KiB 缓冲、零分配、不经 slog | 终端、日志文件 |
| `SlogSink` | `log/slog`（Text / JSON） | 接宿主既有 logger、要 JSON 喂采集器 |
| `MemorySink` | 内存收集 | 测试断言、演示 |
| `MultiSink` | 扇出到多个出口 | 同时落盘 + 收集 |
| `AsyncSink` | **包装器**：有界队列 + 单后台协程 | 手慢的出口（文件 / 网络） |

### 耗时口径（实测）

「执行段」是从进入 `Run` 到节点结束的**墙钟时间**——而等待段那条记录是在 `Running` 回调里写进出口的，所以**出口有多慢，执行段就有多长**。同一张图、同一个空节点，两种出口：

```text
# MemorySink
pulse.node_wait_finished       running    0s
pulse.node_run_finished        completed  0s

# LineSink(WithImmediate) → 控制台
PULSE | ... | running    |         - | pulse.node_wait_finished | pulse.node=noop | ...
PULSE | ... | completed  |   16.60ms | pulse.node_run_finished  | pulse.node=noop | ...
```

节点体是空的，16.60ms 全是控制台写出的开销。所以：

- 耗时列偏大先怀疑出口，而不是节点；
- 慢出口套 `AsyncSink`（`Write` 只做 `Attrs` 深拷 + 入队）。注意异步**不提高吞吐上限**：持续速率超过出口能力时，有界队列回压到出口速率——这正是「不丢记录」的代价，要丢不堵用 `DropOnFull()`。对已经很快的出口（如 `MemorySink`）套异步是负优化；
- 等待段为 `0`（渲染成 `-`）表示该段耗时被计时精度取整为 0，而不是「没有这段」。

## 宿主自带列（WithRenderer）

列式版式是**默认**的，不是唯一的。宿主想让域事实进列时，用 `WithRenderer(fn)` 换掉**行体**渲染器，用包内导出的编码原语拼自己的列——不必自带一个 Sink：

```go
render := func(dst []byte, r observe.Record, color bool) []byte {
	dst = r.Time.AppendFormat(dst, "15:04:05.000")
	dst = append(dst, " | "...)
	node, _ := observe.Get[string](r.Attrs, pulse.AttrNode)
	dst = observe.AppendTextValue(dst, node)
	dst = append(dst, " | "...)
	dst = observe.AppendTextValue(dst, r.Status)
	dst = append(dst, " | "...)
	return observe.AppendDuration(dst, r.Duration)
}

sink := observe.NewLineSink(os.Stdout,
	observe.WithImmediate(),
	observe.WithRenderer(render))
```

真实输出（同一张图）：

```text
PULSE | 15:35:50.150 | step | running | 0ns
PULSE | 15:35:50.164 | step | completed | 14.11ms
```

渲染器的契约只有四条：**只产行体**（行首标识与结尾换行由 sink 加）；拿到的 `color` 是 sink 解析好的结论（所以不必自己探测终端，也不会把 ANSI 写进重定向到文件的日志）；不缓冲、不写 writer（写出的时机归 sink，`WithImmediate` 控制即时性）；**在 sink 的内部锁内被调用**——别在渲染器里回调同一 sink 的 `Write` / `Flush` / `Err`（`sync.Mutex` 不可重入，会安静挂住）。

六条编码原语（`AppendDuration` / `AppendTextValue` / `AppendAttrs` / `AppendAttrsExcept` / `AppendPadding` / `DisplayWidth`）与内置版式**同形**，属冻结面：同一条记录在两种版式下，耗时、属性组、列补齐**逐字节一致**。其中 `AppendAttrsExcept` 用来把「固定列盖不住的属性」补到行尾，不必自己重写标量与引号口径。

## Record 与 Attrs

```go
type Record struct {
	Time     time.Time
	HostID   string
	TraceID  string
	Source   Source
	Event    string
	Duration time.Duration
	Status   string
	Err      error
	Attrs    Attrs // 开放段
}
```

设计约束：**业务维度一律经 `Attrs` 进入，不扩具名字段**。具名字段只服务于所有记录共有的事实——一旦为某个域加具名字段，`Record` 就会长成所有域观测字段的汇聚点。

`Attrs` 是**插入序小切片**（不是 map）：顺序 = 产生方的语义序，出口无需排序即得稳定输出；读取为线性扫描，条数少时快于哈希。key 用 `<组件>.<字段>` 点分约定，各组件独立 key 空间（`pulse.graph` / `pulse.node`，宿主自成一段）。

宿主直写自己的记录（例如业务事实）时，用同一套 API：

```go
rec := observe.Record{
	HostID:  "host-1",
	TraceID: "t-1",
	Source:  "app", // 自报家门：本包只自产 source=observe
	Event:   "app.turn_finished",
	Status:  "ok",
}
observe.Set(&rec.Attrs, "app.turns", int64(3))
sink.Write(rec)
```

```text
PULSE | 2026/10/08 - 15:36:12.015 | ok         |         - | app.turn_finished | source=app | app.turns=3 | host=host-1 | trace=t-1
```

## TraceID

由宿主**单一生成源**注入：每次运行调用一次 `NewTraceID()` 即构成单一生成源，也可以完全自带方案（如 hostID 前缀 + 自增序号）。返回值**无契约语义**，消费方不要解析其结构。同一次运行的全部记录共享同一个 TraceID——这是运行级关联的全部机制。

接 OTel / W3C trace context 的宿主**注入自己的 trace id**，而不是套用 `NewTraceID()` 的形状：

```go
TraceID: span.SpanContext().TraceID().String(), // 32 位小写 hex（W3C trace-id）
```

`NewTraceID()` 返回的是「时间戳-随机段-序号」：人读友好、天然可排序，但**不是** W3C 的 32 位 hex。两条边界值得记住：

- 只有 trace-id 也发不出一条合法的 `traceparent`——`parent-id`（16 位）与 `trace-flags` 是 span 语义，`observe` 只有「一次运行」的概念、没有 span，那两段归宿主；
- **入站续接**就是把收到的 trace-id 填进 `ObserveConfig.TraceID`；非法 `traceparent` 整条忽略（W3C 的规定）同样是宿主的责任。

`ObserveConfig` 的生命周期**等于一次运行**（图的一次 `Run`）：跨运行必须新建（TraceID 每次唯一，复用旧值会造成假关联）。

## 隐私边界

`Record` 没有 `map[string]any` 逃生舱：`Attrs` 的写入面只有泛型 `Set`（约束 `~string | ~int64 | ~float64 | ~bool`），**prompt、附件字节、密钥、思维链在类型上就无法进入**。

边界的残余部分要说清：`Err` 是 `error`，来源是调用方传入的——所以**适配层不得把上游原始错误体直接塞进 `Err`**，应传已分类的摘要。「把 payload 塞进一个标量」属于蓄意行为，防线在 key 自述意图 + Sink 侧 redact 钩子（实现可拒绝敏感 key / 截断超长 / 限条数）。

包级 API 与基准口径见 [observe 包文档](/packages/observe/)；完整设计见[设计文档](https://github.com/Luo-root/pulse/blob/main/docs/design/pulse.md)。
