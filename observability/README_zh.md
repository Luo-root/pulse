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
// TraceID 由宿主自行生成注入（每请求唯一即可，方案自选：自增序号 / UUID 等）。
// 参考实现：examples/internal/demoapp/host.go 的 Host.NewTraceID。
traceID := "host-1-req-1" // 占位：宿主自有生成源
cfg := observability.ObserveConfig{Sink: sink, HostID: "host-1", TraceID: traceID}
c, err := observability.AttachCollector(reqScope, cfg) // 业务插件直写服务
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

## 与请求级事件的关系

| 事实 | 派发 | 谁听 |
|---|---|---|
| fiber_state / loader_action | 全树 `Emit` | `Bootstrap` |
| tool / turn / llm generate | `EmitLocal` / `WaterfallLocal` | 各包 `Observe`（挂 reqScope） |
| flow 节点分段 | Observer 回调 | `flow.NewRecordObserver` |
| 业务自定义事实 | —— | `observability.CollectorKey` 服务直写（`kernel.Get`） |

详见 [`docs/design/kernel-local-events.md`](../docs/design/kernel-local-events.md) 与 [`docs/design/observability-v1-design.md`](../docs/design/observability-v1-design.md)。

## 刻意不做

- 本包不 import / 不订阅 llm、loop、flow 业务事件（各包自适配，依赖箭头朝基座）
- 不做第二套字符串事件总线（无 `Collector.Emit(string, map)`；业务直写走类型化的 `CollectorKey`）
- 不把 token 计数塞进官方 Record 具名字段（走 Attrs，key 归属包定义）
