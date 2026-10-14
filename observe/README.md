[English](README_en.md) | [中文](README.md)

# observe

`pulse/observe` 是**图观测**：把引擎 `Observer` 的回调（图级两条 + 每节点三条）折成结构化 `Record`，写进宿主选的 `Sink`。

依赖是单向的（`pulse` ← `observe`）：引擎不认识本包，只暴露 seam；不需要观测的宿主不 import 它。

## 最短用法

```go
sink := observe.NewLineSink(os.Stdout)
defer sink.Flush() // 关闭前必须 Flush：最后一批还在缓冲里

obs, err := observe.NewRecordObserver(observe.ObserveConfig{
	Sink:    sink,
	HostID:  "host-1",             // 宿主身份：跨运行稳定，与 TraceID 组成「谁 + 哪一次」
	TraceID: observe.NewTraceID(), // 运行期身份，单一生成源
})
g, err := pulse.New("demo", "演示图", pulse.WithObserver(obs))
// …装图、Seed、Run
```

## 当前进度与事件历史

`observe` 保存引擎已经发出的事实；需要随时问「本轮卡在哪」时，用根包的 `e.Snapshot()`。它不依赖是否订阅 Observer，不发新事件，状态先迁移再进入回调，所以回调里也能拍快照。快照的 `Graph.Execution` / `Graph.Path` 与 `Site` 一致，能和记录中的 `pulse.execution` / `pulse.path` 对上。

快照只有节点、键类型、来源与到达状态，不带业务值。`waiting` 节点若还有 `pending` 输入，就是在等这些输入；输入全到达则在等进入 Run 的机会。`Child` 是最后一次已创建尝试的活子执行，失败与取消也保留。读取节点与槽位、继续读子执行都不是同一时刻的原子切面；图在 Wait 收尾时才转终态，只 Start 不 Wait 仍为 running。

静态结构用 `g.Topology()`，包括完整声明端口与子树；`g.Topology().Mermaid(pulse.WithMermaidSnapshot(e.Snapshot()))` 可把本轮状态叠上去。无快照时只画结构，固定值与错误正文不进入图。`MemorySink.Snapshot()` 则是已收记录的副本，和执行快照各有用途。

## 记录形状

**一次运行两条运行级记录**；节点按实际经历的等待段、执行段产出记录，未进入等待段的失败另发终态记录：

| 事件 | 何时产出 | `Duration` | `Status` |
|---|---|---|---|
| `pulse.graph_started` | 提交任何节点之前 | `0` | `running` |
| `pulse.graph_finished` | 全部节点终止之后（`Wait` 返回前） | 整轮耗时 | `completed` / `failed` / `canceled` |
| `pulse.node_wait_finished` | 等待段结束 | 等待段 | `running`，否则是对应终态 |
| `pulse.node_run_finished` | 执行段结束 | 执行段 | 终态 `completed` / `skipped` / `failed` / `canceled` |
| `pulse.node_finished` | 切面在调用 next 前失败或取消，没有等待/执行计时 | `0`（未测量） | `failed` / `canceled` |

图级两条把本轮的节点记录**夹在中间**（`started` 先于任何节点记录、`finished` 晚于全部）；只 `Start` 不 `Wait` 的宿主拿不到 `finished`。输入全跳过、未进入业务体的节点只有一条 `skipped` 等待记录；业务体返回 `NoValue()` 时已经有执行段，因此保留等待与 `skipped` 执行记录。正常跳过的 `Record.Err` 为 nil，一轮里全部节点都跳过时运行级仍是 `completed`。可靠检查点故障即使包装了 `ErrSkipped` 仍保留失败身份和错误链。

切面提前拒绝执行的 `pulse.node_finished` 带完整图、执行、路径与节点归因，未测量耗时留零。恢复的 completed/skipped 节点不伪造业务计时记录，恢复事实由 Durable 事件提供。

归因维度走 `Attrs`（`pulse.AttrGraph` / `pulse.AttrNode` / `pulse.AttrExecution`，key 契约由引擎定义，本包只消费）：节点记录两个都带，运行级两条只带 `pulse.AttrGraph`——节点维度对「一次运行」没有意义；`pulse.AttrExecution` 每条都带（同一张图跑两次的记录靠它分得开）。

**嵌套层级**是第三个维度 `pulse.AttrPath`：引擎把**挂载位置**随每条回调发出来（`Site.Path`），本包由此给那一层的**每一条**记录都写上它——根图不写，空串会让「根」与「忘了传」长得一样，出口就没法用它判层级。一个出口从根挂下去就够：子图的记录自带层级，不必按层各建一个出口；`ObserveConfig.Path` 只在 `Site` 没给位置时兜底（自己直写记录之类不经过引擎的场景）。记录里的分层怎么拆是出口的事（按 `/` 分即可）；本包保留平面 Record 信封，结构树与活子执行分别从 Topology / Execution 快照查看。

**耗时口径**：都是**墙钟**，且回调在调用方 goroutine 上同步执行——出口有多慢，段就有多长（`pulse.graph_finished` 的窗口覆盖整轮，其中包含出口写出的开销）。`AsyncSink` 只把**落盘**推后，不把 `Write` 调用从窗口里摘掉。段耗时被计时精度取整成 `0`（内置版式渲染 `-`）表示「量不出来」，不是「没有这一段」。

## 出口

`LineSink`（**默认**：一行一条的人读文本、自带缓冲、零分配、不经 slog）· `SlogSink`（接宿主既有 logger，要 JSON 喂采集器）· `MemorySink`（测试断言与演示）· `MultiSink`（扇出）· `AsyncSink`（**包装器**：把手慢的出口从回调路径上摘掉）。

`Sink` 是唯一需要你实现的接口——`Write(Record)` 一个方法。它必须并发安全且不得长时间阻塞：回调在**节点 goroutine** 上同步执行，出口有多慢，节点就有多慢。

`AsyncSink.Flush(ctx)` 固定本次入队快照，等这些记录送达并刷新内层；后续写入不延长它的排空范围。取消在内层 Flush/Err 阻塞时也能返回，记录保留、worker 继续处理。Write/Flush/Err 都由同一个 worker 串行调用。不同快照边界按序登记，同目标共享，较早目标不扩充；取消后撤销无人等待且未开始的边界，边界数至多容量加一。`Close(ctx)` 先停收，再排空刷新；首次调用决定最终结果，并发调用可用自己的 ctx 停止等待。内层无 context 的调用无法强杀，取消后最多保留这个 worker，等内层返回才能退出。内层 Flush/Err 的 panic 在 worker 边界捕获并返回固定错误；error payload 经 Unwrap 保留，不打印正文，不把已经交给 inner.Write 的记录计为 Dropped，worker 仍能处理后续记录。

`SlogSink` 的 `duration_ms` 是数值毫秒，保留亚毫秒精度；零耗时省略该键。

## 宿主自带列

列式版式是默认的，不是唯一的。`WithRenderer(fn)` 换掉**行体**渲染器，用包内导出的六条编码原语（`AppendDuration` / `AppendTextValue` / `AppendAttrs` / `AppendAttrsExcept` / `AppendPadding` / `DisplayWidth`）拼自己的列——与内置版式**逐字节同形**，出口仍然不认识任何业务语义。可运行示例见 `Example_hostRenderer`。

## 文档

- 指南：[图观测](https://luo-root.github.io/pulse/guide/observability)
- 设计：[`docs/design/pulse.md`](../docs/design/pulse.md) §7–§13（观测部分）
