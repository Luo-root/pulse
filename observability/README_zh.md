# observability

pulse v2 正式观测包（双基座之一）：旁路订阅 kernel 装配事件，写出统一 `Record` 信封；运行期业务事实由各业务包的观测适配（`llm.Observe` / `loop.Observe` / `flow.NewRecordObserver`）折进同一 `Sink`。

读完这篇应能：最先 `Use(Bootstrap)`、选一个 `Sink`、知道运行期业务指标经 `ObserveConfig` 复用同一出口与 TraceID。

## 分层纪律（双基座模型）

| 层 | 认识什么 | 不认识什么 |
|---|---|---|
| 本包（基座） | `kernel` typed 事件、`Sink`、`Record` | `llm` / `loop` / `flow` |
| 业务包（租户） | kernel + observability + 自身事实 | —— |
| 宿主（装配层） | 全部 | —— |

依赖箭头统一朝基座：本包只 import kernel 且无任何例外（原伴生 bridge 包已废除，折叠适配下沉至各事实归属包）。业务维度（模型名、token 数、工具名、节点 ID）走 `Attrs` 开放段，key 契约由事实归属包定义（`llm.AttrModel`、`loop.AttrTool`、`flow.AttrNode`）——**同一出口 ≠ Record 变万能袋**。

## 接入

```go
host := kernel.New()
defer host.Dispose()

sink := &observability.MemorySink{} // 或 SlogSink{Logger: slog.Default()}
// 必须最先 Use：kernel 事件不回放；后装只能靠快照横幅兜底当前视图。
if _, err := kernel.Use(host, observability.Bootstrap("host-1", sink)); err != nil {
    panic(err)
}

// 每请求：派生请求 scope，观测监听随 scope Dispose 自动摘除。
reqScope, err := host.Derive()
if err != nil {
    panic(err)
}
defer reqScope.Dispose()

// 每请求：cfg 复用即 D3 请求级关联；跨请求必须新建。
// TraceID 由宿主每请求调用一次 NewTraceID 生成（单一生成源）；
// 也可以完全自带方案（宿主自有格式，如 hostID 前缀 + 自增序号）。
cfg := observability.ObserveConfig{Sink: sink, HostID: "host-1", TraceID: observability.NewTraceID()}
c, err := observability.AttachCollector(reqScope, cfg) // 业务插件直写服务（作用域局部：仅本请求 scope 及后代可读）
err = llm.Observe(reqScope, cfg)                       // llm 包适配
err = loop.Observe(reqScope, cfg)                      // loop 包适配
// flow 图：flow.WithObserver(must(flow.NewRecordObserver(cfg)))
```

`Bootstrap` 订阅全树 `Emit` 的 `fiber_state` / `loader_action`，并在 Apply 末尾写 `host_ready` 快照横幅。树销毁（Dispose）**不发**逐 Fiber `fiber_state`（T7 裁决）；验收是 Dispose 后 Sink 零残留。

## Record / Sink

```text
通用信封：Time, HostID, TraceID, Source, Event, Duration, Status, Err
装配专用：FiberName, From, To, LoaderKind, EntryID, PluginName
Attrs 开放段：标量 kv（~string/~int64/~float64/~bool）
```

- 无 `map[string]any` 逃生舱；`Attrs` 的写入面只有泛型 `Set[T AttrValue]`——`[]byte`、struct、slice、任意对象在类型上进不来（隐私边界的类型部分），key 自述意图 + Sink 侧 redact 钩子兜住蓄意标量注入。
- `Sink.Write(Record)`：**无** `context.Context`（kernel Emit 路径不带 ctx）。
- `Time` 为零时由内置 Sink（`SlogSink` / `MemorySink`）补 wall clock；`SlogSink` 的 Attrs 段按 key 字典序输出（`Attrs.MarshalJSON` 同序）。
- 内置：`SlogSink`、`MemorySink`、`MultiSink`。

## 异步出口（AsyncSink）

慢出口（文件 / 网络导出器）用 `AsyncSink` 包一层：`Write` 只做 `Attrs` 深拷 +
入队即返回，单后台协程按 FIFO 写出。kernel 的事件派发全同步（`Emit` /
`EmitLocal` / `Waterfall`；`Parallel` 也等完成），不包这一层，出口有多慢，请求
与 agent 步进就有多慢。

```go
sink := observability.NewAsyncSink(observability.SlogSink{Logger: lg},
    observability.WithCapacity(1024)) // 缺省 1024；满时阻塞（不丢）
defer sink.Close(ctx)                 // 宿主关闭路径：排空 + 停协程
```

- **满时策略**：缺省 block（回压给生产者，不丢）；`DropOnFull()` 丢新并计入
  `Dropped()`；
- **`Flush(ctx)`** 等「调用时刻已入队（含在途）」全部送达；ctx 过期返回错误、
  **队列保留**（后台继续处理，记录不丢）；
- **`Close(ctx)`** 停收 + 排空 + 停协程（幂等）；ctx 过期立即返回，队列剩余计丢；
- **`Dropped()`** 是合一计数：满丢弃 / Close 后写入 / Close 唤醒的阻塞写入 /
  Close 超时残留 / inner panic 跳过，全部计入；
- **inner panic** 被 recover 并计数，worker 继续（它是唯一消费者，静默停摆比
  崩溃更隐蔽）；
- **显式 Close/Flush 是前置**：进程退出不做这件事，队列内记录就随进程消失。
  宿主关闭路径的可抄接线：

```go
func main() {
    sink := observability.NewAsyncSink(realSink, observability.WithCapacity(4096))
    defer func() {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        _ = sink.Close(ctx) // 先排空异步出口，再让进程退出
    }()
    // ... 业务：sink.Write(...) 走请求路径，不阻塞
}
```

实测（i9-14900HX，Windows；对照 = `SlogSink` 直写无缓冲文件）：

| 口径 | 直接写 | AsyncSink |
|---|---|---|
| 突发 1000 条（生产者侧） | 95.3 ms（95 µs/条） | **1.5 ms（1.5 µs/条）≈ 63×** |
| 后台排空（同批） | —（含在上面） | 52 ms（成本仍在，只是离开生产者路径） |
| 入队（空 Attrs） | — | 211–242 ns，0 alloc |
| 入队（含 3 Attrs） | — | ≈ 1.0 µs，2 allocs（深拷 O(N)） |

**异步不提高吞吐上限**：持续速率超过出口能力时，有界队列会被填满并把生产者回压
到出口速率——这正是「不丢记录」的代价；要丢不堵就用 `DropOnFull()`。它对已经很快
的出口（如 `MemorySink`）是负优化，别默认套。

## 与请求级事件的关系

| 事实 | 派发 | 谁听 |
|---|---|---|
| fiber_state / loader_action | 全树 `Emit` | `Bootstrap` |
| tool / turn / llm generate | `EmitLocal` / `WaterfallLocal` | 各包 `Observe`（挂 reqScope） |
| flow 节点分段 | Observer 回调 | `flow.NewRecordObserver` |
| 业务自定义事实 | —— | `observability.CollectorKey` 直写（**作用域局部绑定**：持请求 scope 或其子孙 `kernel.Get`；父 / 兄弟 / 其他并发请求读不到，不串台） |

详见 [`docs/design/kernel-local-events.md`](../docs/design/kernel-local-events.md) 与 [`docs/design/observability-v1-design.md`](../docs/design/observability-v1-design.md)。

## 刻意不做

- 本包不 import / 不订阅 llm、loop、flow 业务事件（各包自适配，依赖箭头朝基座）
- 不做第二套字符串事件总线（无 `Collector.Emit(string, map)`；业务直写走类型化的 `CollectorKey`）
- 不把 token 计数塞进官方 Record 具名字段（走 Attrs，key 归属包定义）
