# 图观测

观测是**独立的一层**：引擎不认识任何观测包，只暴露一个 `Observer` seam；`pulse/observe` 实现这个 seam，把回调折成结构化记录写进出口。不需要观测的宿主只 import 根包。

## 引擎的 seam

```go
type Observer interface {
	OnGraphStarted(graphID string)
	OnGraphFinished(graphID string, reason NodeFinishReason, err error)
	OnNodeWaiting(graphID, nodeID string)
	OnNodeRunning(graphID, nodeID string)
	OnNodeFinished(graphID, nodeID string, reason NodeFinishReason, err error)
}
```

契约五条，都是冻结面：

1. **回调次数**：图级 `Started ≤ 1`、`Finished ≤ 1`；每个节点 `Waiting ≤ 1`、`Running ≤ 1`、`Finished = 1`。`Retry` 的多次 attempt **不重复打点**；
2. **只读**：观察者的 panic 与 error **不得**升格为节点失败（引擎侧已吞掉）；
3. **并发安全**：节点三条在**节点自己的 goroutine** 上同步执行，图级两条在 **`Start` / `Wait` 的调用方 goroutine** 上——实现必须并发安全，且不得长时间阻塞。**别在回调里调本图的 `Start` / `Wait`**：回调就在它们的调用路径上，那是同一个 goroutine 等自己，会死等；
4. **时序是夹住**：`GraphStarted` 先于本轮的**任何**节点回调（启动校验失败的图没有启动，不发），`GraphFinished` 晚于**全部**节点回调；只 `Start` 不 `Wait` 的宿主拿不到 `finished`，而并发 / 重复 `Wait` 都在这一发返回之后才返回（任何一个 `Wait` 返回时，本轮的收尾都已在出口落地）；
5. **归因键由引擎给出**：`graphID` 随每次回调发出，实现侧不必从构造参数自行携带。

图默认 no-op（不挂就没有任何开销），`pulse.WithObserver(...)` 挂载；要多个观察者用 `pulse.MultiObserver` 组合（后写覆盖前写，所以组合要在传参前做完）。

## observe 折成运行级两条 + 节点级两条

| 事件 | 何时产出 | `Duration` | `Status` |
|---|---|---|---|
| `pulse.graph_started` | 提交任何节点之前 | `0` | `running` |
| `pulse.graph_finished` | 全部节点终止之后（`Wait` 返回前） | 整轮耗时 | 运行终态（`completed` / `failed` / `canceled`） |
| `pulse.node_wait_finished` | 等待结束（进入执行，或以 skip / 失败终结） | 等待段 | `running`，否则是对应的终态 |
| `pulse.node_run_finished` | 执行结束 | 执行段 | 终态（`completed` / `failed` / `canceled`） |

运行级两条把本轮的节点记录**夹在中间**：宿主不必再靠 `pulse.graph` 这个 Attr 把节点记录自行拼回一轮——一轮在观测里是一个有头有尾的实体。运行级终态只会是 `completed` / `failed` / `canceled`：**一轮里全部节点都跳过，整轮仍是 `completed`**（跳过是节点级的事实，不升格为失败）。跳过节点**只有一条 `skipped` 等待记录**——它确实到达了，只是没执行。

归因维度走 `Attrs`：`pulse.AttrGraph` + `pulse.AttrNode`（key 契约由**引擎**定义，`observe` 只消费不定义）。节点记录两个都带，运行级两条只带 `pulse.AttrGraph`——节点维度对「一次运行」没有意义。

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

同一个图（单节点）的真实输出（时间戳 / trace / 耗时随运行变化）：

```text
PULSE | 2026/10/09 - 10:42:50.472 | running    |         - | pulse.graph_started | source=observe | pulse.graph=docs-pipeline | host=quickstart | trace=1791513770472465000-1667c395-2
PULSE | 2026/10/09 - 10:42:50.472 | running    |         - | pulse.node_wait_finished | source=observe | pulse.graph=docs-pipeline pulse.node=summarize | host=quickstart | trace=1791513770472465000-1667c395-2
PULSE | 2026/10/09 - 10:42:50.487 | completed  |   15.50ms | pulse.node_run_finished | source=observe | pulse.graph=docs-pipeline pulse.node=summarize | host=quickstart | trace=1791513770472465000-1667c395-2
PULSE | 2026/10/09 - 10:42:50.487 | completed  |   15.50ms | pulse.graph_finished | source=observe | pulse.graph=docs-pipeline | host=quickstart | trace=1791513770472465000-1667c395-2
```

四条：运行级两条夹住节点两条。运行级那条的 `Duration` 是**整轮**（两回调之间的墙钟）——这里与节点执行段同量级，是因为这一轮几乎全花在那个节点上；装图、`Seed` 与 `Flush` 不在它的窗口里（口径见下）。`pulse.graph_finished` 的 `Status` 是运行终态，`Err` 与 `Run()` 的返回值同源。

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

四段都是**墙钟**，而回调是同步执行的——所以**出口有多慢，段就有多长**。同一张图、同一个空节点（`noop`），两种出口（`MemorySink`，与一个 `Write` 每条睡 20ms 的假出口）：

```text
# MemorySink（写出一条的代价可以忽略）
pulse.graph_started            node=-      running    0s
pulse.node_wait_finished       node=noop   running    0s
pulse.node_run_finished        node=noop   completed  0s
pulse.graph_finished           node=-      completed  0s

# 每条 Write 睡 20ms 的假出口
pulse.graph_started            node=-      running    0s
pulse.node_wait_finished       node=noop   running    0s
pulse.node_run_finished        node=noop   completed  20.5701ms
pulse.graph_finished           node=-      completed  62.2437ms
```

节点体是空的：`20.5701ms` 全是那一次写出的开销（等待段那条记录在 `Running` 回调里写，而执行段的计时点在它之前）。**整轮那条更长**（`62.2437ms`）——运行级的窗口覆盖「提交节点 → 全部终止」，这一轮里三次写出都在窗口内（运行级 started、节点等待段、节点执行段）。由此：

- 耗时列偏大先怀疑出口，而不是节点；真终端上的 `LineSink` 本身就是慢出口（每条一次写系统调用），十几毫秒起的开销同样落在段里，且随终端与重定向而变；
- 慢出口套 `AsyncSink`（`Write` 只做 `Attrs` 深拷 + 入队）。注意异步**不提高吞吐上限**：持续速率超过出口能力时，有界队列回压到出口速率——这正是「不丢记录」的代价，要丢不堵用 `DropOnFull()`。对已经很快的出口（如 `MemorySink`）套异步是负优化；它也**不把 `Write` 调用从窗口里摘掉**，只是把落盘推后；
- 段耗时为 `0`（渲染成 `-`）表示该段耗时被计时精度取整为 0，而不是「没有这段」。

**运行级那条与你自己在 `Run()` 外掐表的边界差别**：它的窗口起点在 `Start()` 内部（提交节点之前）、终点在 `Wait()` 返回之前——所以**不含**装图、`Seed` 与 `Wait()` 返回后的出口 `Flush`，你从外面掐一般更长；反过来它含了「运行级 started 那条记录写出」的耗时，而那部分不在任何节点段里。要「一轮的边界」用它，要「我这个函数在 `Run` 上花了多久」就自己掐表。

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
