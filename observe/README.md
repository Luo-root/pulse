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
g, err := pulse.New(ctx, "demo", pulse.WithObserver(obs))
// …装图、Seed、Run
```

## 记录形状

**一次运行两条运行级记录**，加**每个节点两条分段计时记录**：

| 事件 | 何时产出 | `Duration` | `Status` |
|---|---|---|---|
| `pulse.graph_started` | 提交任何节点之前 | `0` | `running` |
| `pulse.graph_finished` | 全部节点终止之后（`Wait` 返回前） | 整轮耗时 | `completed` / `failed` / `canceled` |
| `pulse.node_wait_finished` | 等待段结束 | 等待段 | `running`，否则是对应终态 |
| `pulse.node_run_finished` | 执行段结束 | 执行段 | 终态 `completed` / `failed` / `canceled` |

图级两条把本轮的节点记录**夹在中间**（`started` 先于任何节点记录、`finished` 晚于全部）；只 `Start` 不 `Wait` 的宿主拿不到 `finished`。跳过节点**只有**一条 `skipped` 等待记录——它到达了，只是没执行。一轮里全部节点都跳过时运行级仍是 `completed`：跳过是节点级的事实。

归因维度走 `Attrs`（`pulse.AttrGraph` / `pulse.AttrNode`，key 契约由引擎定义，本包只消费）：节点记录两个都带，运行级两条只带 `pulse.AttrGraph`——节点维度对「一次运行」没有意义。

**嵌套层级**是第三个维度 `pulse.AttrPath`：`ObserveConfig.Path` 非空（建子图的出口填 `sc.Path()`）时**每一条**记录都带它，为空**不写**——空串会让「根」与「忘了传」长得一样，出口就没法用它判层级。它是**出口实例**上的一个值（不是每条记录各带一个），所以嵌套 = 按层各建一个出口，`Sub` 的 `build` 里正合适。分层怎么拆是出口的事（按 `/` 分即可），引擎与本包都不提供结构化树。

**耗时口径**：都是**墙钟**，且回调在调用方 goroutine 上同步执行——出口有多慢，段就有多长（`pulse.graph_finished` 的窗口覆盖整轮，其中包含出口写出的开销）。`AsyncSink` 只把**落盘**推后，不把 `Write` 调用从窗口里摘掉。段耗时被计时精度取整成 `0`（内置版式渲染 `-`）表示「量不出来」，不是「没有这一段」。

## 出口

`LineSink`（**默认**：一行一条的人读文本、自带缓冲、零分配、不经 slog）· `SlogSink`（接宿主既有 logger，要 JSON 喂采集器）· `MemorySink`（测试断言与演示）· `MultiSink`（扇出）· `AsyncSink`（**包装器**：把手慢的出口从回调路径上摘掉）。

`Sink` 是唯一需要你实现的接口——`Write(Record)` 一个方法。它必须并发安全且不得长时间阻塞：回调在**节点 goroutine** 上同步执行，出口有多慢，节点就有多慢。

## 宿主自带列

列式版式是默认的，不是唯一的。`WithRenderer(fn)` 换掉**行体**渲染器，用包内导出的六条编码原语（`AppendDuration` / `AppendTextValue` / `AppendAttrs` / `AppendAttrsExcept` / `AppendPadding` / `DisplayWidth`）拼自己的列——与内置版式**逐字节同形**，出口仍然不认识任何业务语义。可运行示例见 `Example_hostRenderer`。

## 文档

- 指南：[图观测](https://luo-root.github.io/pulse/guide/observability)
- 设计：[`docs/design/pulse.md`](../docs/design/pulse.md) §7–§13（观测部分）
