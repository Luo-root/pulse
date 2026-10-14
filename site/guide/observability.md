# 图观测

观测是**独立的一层**：引擎不认识任何观测包，只暴露一个 `Observer` seam；`pulse/observe` 实现这个 seam，把回调折成结构化记录写进出口。不需要观测的宿主只 import 根包。

## 随时看进度：执行快照

`e.Snapshot()` 是一次运行的当前读数；Observer 是事件历史，两者互补。快照不用订阅回调，读取不会推进运行或新增事件。每次执行有不同的 `Graph.Execution`，对应记录里的 `pulse.execution`；`Graph.Path`、`Site.Path` 和静态子树的 `Graph.Path` 使用同一套挂载路径。

```go
progress := e.Snapshot()
staticDiagram := g.Topology().Mermaid()
statusDiagram := g.Topology().Mermaid(pulse.WithMermaidSnapshot(progress))
_, _ = staticDiagram, statusDiagram
```

快照节点按定义声明序。输入 `ready` / `skipped` 都是到达：还有 `pending` 输入的 waiting 节点在等输入，输入全到达时在等进入 Run 的机会。切面也能扩展等待，不能单凭这条读数保证它正在排名额。状态先迁移再发回调，所以回调里能读到这次迁移；Retry 不倒退也不重复事件。

快照不携带业务值；节点与槽位分别采样，running 节点已有 ready / skipped 输出是合法的。图创建后 pending，成功 Start 后 running，Wait 收尾时才写终态。Child 是最新已创建尝试的活 `*Execution`，失败与取消仍保留，未建出才为 nil；再调用 `progress.Nodes[i].Child.Snapshot()` 取得的是另一时刻的子读数。

Topology 包含完整声明端口、绑定与静态子树；Mermaid 只从它派生结构，没有快照就不着色。给快照后按图 ID + Path + Node ID 叠状态，子执行逐层采样，固定值和错误正文不进入图。只查看 YAML 可用无需 Registry 的 Analyze，详见 [YAML 装配](/packages/yaml/)。

## 业务结果与进度分开

快照始终不带业务值。宿主需要结果时，用 `pulse.ReadOutput(e, result)` 读取已声明的同名同类型出口；主路径先成功 Start / Wait，再处理返回的 `value, state, err`。有值（含零值）是 ready，正常缺值是 skipped，可选透传未提供仍为 pending；后两者不报执行失败，也不等待或修改槽位。

尚未收尾或真实失败/取消不返回部分业务值；错误时 value/state 无效。Wait 的正常 ctx 释放不污染成功结果，GraphFinished 回调也能读已确定结果。返回的可变业务对象不深拷贝，宿主仍负责同步。完整调用示例见[根 README](https://github.com/Luo-root/pulse/blob/main/README.md#取得本轮结果)。

## 恢复成果的观测

检查点恢复创建新的 Execution。已确认节点不重跑切面或业务体，只在 GraphStarted 之后迁移终态并发一次 Finished；不伪造 Waiting/Running、未创建的 Child 或子图事件。Snapshot 展示本轮实际创建的对象，仍不包含业务值。

RecordObserver 不把已恢复的 completed/skipped 的单独 Finished 伪装成业务计时；恢复事实由独立 Durable 的 `EventRestored` 与持久记录提供。未进入等待段就失败或取消的单独 Finished 则保留为 `pulse.node_finished`，带节点归因，耗时留零。可靠的检查点钩子属于执行控制，错误会阻止发布，与 Observer 的只读异常隔离分开。方案 A 的子图作用域、重试预算和提交边界见[桥接契约](https://github.com/Luo-root/pulse/blob/main/docs/design/checkpoint.md)。

## 引擎的 seam

```go
type Observer interface {
	OnGraphStarted(site Site)
	OnGraphFinished(site Site, reason NodeFinishReason, err error)
	OnNodeWaiting(site Site, nodeID string)
	OnNodeRunning(site Site, nodeID string)
	OnNodeFinished(site Site, nodeID string, reason NodeFinishReason, err error)
}
```

契约五条，都是冻结面：

1. **回调次数**：图级 `Started ≤ 1`、`Finished ≤ 1`；每个节点 `Waiting ≤ 1`、`Running ≤ 1`、`Finished = 1`。`Retry` 的多次 attempt **不重复打点**；
2. **只读**：观察者的 panic 与 error **不得**升格为节点失败（引擎侧已吞掉）；
3. **并发安全**：节点三条在**节点自己的 goroutine** 上同步执行，图级两条在 **`Start` / `Wait` 的调用方 goroutine** 上——实现必须并发安全，且不得长时间阻塞。**别在回调里调本图的 `Start` / `Wait`**：回调就在它们的调用路径上，那是同一个 goroutine 等自己，会死等；
4. **时序是夹住**：`GraphStarted` 先于本轮的**任何**节点回调（启动校验失败的图没有启动，不发），`GraphFinished` 晚于**全部**节点回调；只 `Start` 不 `Wait` 的宿主拿不到 `finished`，而并发 / 重复 `Wait` 都在这一发返回之后才返回（任何一个 `Wait` 返回时，本轮的收尾都已在出口落地）；
5. **位置由引擎给出**：每条回调都带一个 `Site`——定义身份（`Site.Graph`）+ 本次运行的身份（`Site.Execution`）+ 挂载位置（`Site.Path`），实现侧不必从构造参数自行携带；同一张图跑两次的记录靠 `Site.Execution` 分得开。

观察者默认 no-op（不挂就不执行观察回调；快照的状态记账仍然存在），`pulse.WithObserver(...)` 挂载（它有 `WithExecObserver(...)` 的**本次运行覆盖**：本次运行 > 定义声明 > 父执行环境）；要多个观察者用 `pulse.MultiObserver` 组合（后写覆盖前写，所以组合要在传参前做完）。

## observe 的运行记录与节点记录

| 事件 | 何时产出 | `Duration` | `Status` |
|---|---|---|---|
| `pulse.graph_started` | 提交任何节点之前 | `0` | `running` |
| `pulse.graph_finished` | 全部节点终止之后（`Wait` 返回前） | 整轮耗时 | 运行终态（`completed` / `failed` / `canceled`） |
| `pulse.node_wait_finished` | 等待结束（进入执行，或以 skip / 失败终结） | 等待段 | `running`，否则是对应的终态 |
| `pulse.node_run_finished` | 执行结束 | 执行段 | 终态（`completed` / `skipped` / `failed` / `canceled`） |
| `pulse.node_finished` | 切面在调用 next 前失败或取消，未进入等待段 | `0`（未测量） | `failed` / `canceled` |

运行级两条把本轮的节点记录**夹在中间**。一轮里全部节点都跳过，整轮仍是 `completed`。输入全跳过、未进业务体的节点只有一条 `skipped` 等待记录；业务体返回 `NoValue()` 则保留等待与 `skipped` 执行记录，因为这轮确实进入过业务体。正常跳过的 `Record.Err` 为 nil，结构化日志不写 error 字段；包装 `ErrSkipped` 的可靠检查点故障仍是失败。

切面在 next 前拒绝执行时，`pulse.node_finished` 保留图、执行、挂载路径与失败节点，耗时为未测量的零值，不伪造等待或执行段。

归因维度走 `Attrs`：`pulse.AttrGraph` + `pulse.AttrNode` + `pulse.AttrExecution`（key 契约由**引擎**定义，`observe` 只消费不定义）。节点记录带图 + 节点两个，运行级两条只带 `pulse.AttrGraph`——节点维度对「一次运行」没有意义；`pulse.AttrExecution` 每条都带，同一张图跑两次的记录靠它分得开。

**嵌套层级**是第三个维度 `pulse.AttrPath`（不透明字符串、`/` 连接各层）：**挂载位置随每条回调由引擎发出来**（`Site.Path`），那一层的**每一条**记录就都带它；为空**不写**这个 key——空串会让「根」与「忘了传」长得一样。分工是清楚的：`pulse.graph` 是**你起的图 id**，`pulse.path` 是**引擎记的层级**——同一棵子树装在两处，两处各有各的 `path`，靠它分得开（见[编排](/guide/orchestration)的「图即节点」）。**一个出口从根挂下去就够**：子图的记录自带层级，继承的出口也带 `pulse.path`；`ObserveConfig.Path` 只在 `Site` 没给位置时兜底（自己直写记录之类不经过引擎的场景）。记录中的层级怎么拆是出口的事（按 `/` 分即可）；静态树与活子执行分别从 Topology / Execution 快照查看。

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
g, err := pulse.New("docs-pipeline", "文档摘要流水线", pulse.WithObserver(obs))
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

`SlogSink` 的 `duration_ms` 是数值毫秒，保留亚毫秒精度，零值省略。`AsyncSink.Flush(ctx)` 以本次入队序号为界，后续 Write 不延长排空范围；等待内层 Flush/Err 时也能按 ctx 返回，取消后记录仍由唯一 worker 处理。内层 Write/Flush/Err 串行，不同快照边界按序登记、同目标共享，较早目标不扩充；取消无人等待的未开始边界，边界数至多容量加一。Close 先停收，首次调用决定最终结果；并发 Close 可取消自己的等待。内层无 ctx 的调用不能强杀，取消后 worker 可能留在该调用里，直到它返回。Flush/Err panic 会在 worker 边界转成固定错误反馈，error payload 保留 Unwrap，不打印正文、不误计已交付记录为 Dropped；worker 可继续处理后续记录。

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
