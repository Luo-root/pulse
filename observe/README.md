[English](README_en.md) | [中文](README.md)

# observe

`pulse/observe` 是**图观测**：把引擎 `Observer` 的三条回调折成结构化 `Record`，写进宿主选的 `Sink`。

依赖是单向的（`pulse` ← `observe`）：引擎不认识本包，只暴露 seam；不需要观测的宿主不 import 它。

## 最短用法

```go
sink := observe.NewLineSink(os.Stdout)
defer sink.Flush() // 关闭前必须 Flush：最后一批还在缓冲里

obs, err := observe.NewRecordObserver(observe.ObserveConfig{
	Sink:    sink,
	HostID:  "host-1",             // 装配期身份
	TraceID: observe.NewTraceID(), // 运行期身份，单一生成源
})
g, err := pulse.New(ctx, "demo", pulse.WithObserver(obs))
// …装图、Seed、Run
```

## 记录形状

每个节点两条**分段计时**记录：

| 事件 | `Duration` | `Status` |
|---|---|---|
| `pulse.node_wait_finished` | 等待段 | `running`，否则是对应终态 |
| `pulse.node_run_finished` | 执行段 | 终态 `completed` / `failed` / `canceled` |

跳过节点**只有**一条 `skipped` 等待记录——它到达了，只是没执行。归因维度走 `Attrs`（`pulse.AttrGraph` / `pulse.AttrNode`，key 契约由引擎定义，本包只消费）。

## 出口

`LineSink`（**默认**：一行一条的人读文本、自带缓冲、零分配、不经 slog）· `SlogSink`（接宿主既有 logger，要 JSON 喂采集器）· `MemorySink`（测试断言与演示）· `MultiSink`（扇出）· `AsyncSink`（**包装器**：把手慢的出口从回调路径上摘掉）。

`Sink` 是唯一需要你实现的接口——`Write(Record)` 一个方法。它必须并发安全且不得长时间阻塞：回调在**节点 goroutine** 上同步执行，出口有多慢，节点就有多慢。

## 宿主自带列

列式版式是默认的，不是唯一的。`WithRenderer(fn)` 换掉**行体**渲染器，用包内导出的六条编码原语（`AppendDuration` / `AppendTextValue` / `AppendAttrs` / `AppendAttrsExcept` / `AppendPadding` / `DisplayWidth`）拼自己的列——与内置版式**逐字节同形**，出口仍然不认识任何业务语义。可运行示例见 `Example_hostRenderer`。

## 文档

- 指南：[图观测](https://luo-root.github.io/pulse/guide/observability)
- 设计：[`docs/design/pulse.md`](../docs/design/pulse.md) §7–§13（观测部分）
