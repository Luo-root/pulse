# observability

pulse v2 正式观测包：旁路订阅 kernel 装配事件，写出统一 `Record` 信封。

读完这篇应能：最先 `Use(Bootstrap)`、选一个 `Sink`、知道运行期业务指标为什么不进本包。

## 分层纪律

| 层 | 认识什么 | 不认识什么 |
|---|---|---|
| 本包 | `kernel` typed 事件、`Sink`、`Record` | `llm` / `loop` / `flow` |
| 装配层桥（如 `examples/internal/demoapp`） | llm/loop/flow 公开事件 | 不得绕过 Sink 另开出口冒充官方记录 |

正式包只产生 `SourceKernel` 记录。token、HITL、节点耗时由桥折进同一 `Sink`（`SourceBridge`），装不进信封的指标走 slog 附加键——**同一出口 ≠ Record 变万能袋**。

## 接入

```go
host := kernel.New()
defer host.Dispose()

sink := &observability.MemorySink{} // 或 SlogSink{Logger: slog.Default()}
// 必须最先 Use：kernel 事件不回放；后装只能靠快照横幅兜底当前视图。
if _, err := kernel.Use(host, observability.Bootstrap("host-1", sink)); err != nil {
    panic(err)
}
// 此后其它插件正常 Use；fiber_state / loader_action 进 Sink。
```

`Bootstrap` 订阅全树 `Emit` 的 `fiber_state` / `loader_action`，并在 Apply 末尾写 `host_ready` 快照横幅。树销毁（Dispose）**不发**逐 Fiber `fiber_state`（T7 裁决）；验收是 Dispose 后 Sink 零残留。

## Record / Sink

```text
通用信封：Time, HostID, TraceID, Source, Event, Duration, Status, Err
装配专用：FiberName, From, To, LoaderKind, EntryID, PluginName
```

- 无 `Attributes map`：隐私边界由类型保证。
- `Sink.Write(Record)`：**无** `context.Context`（kernel Emit 路径不带 ctx）。
- `Time` 为零时由内置 Sink（`SlogSink` / `MemorySink`）补 wall clock。
- 内置：`SlogSink`、`MemorySink`、`MultiSink`。

## 与请求级事件的关系

| 事实 | 派发 | 谁听 |
|---|---|---|
| fiber_state / loader_action | 全树 `Emit` | `Bootstrap` |
| tool / turn / HITL / llm generate | `EmitLocal` / `WaterfallLocal` | 挂在 `reqScope` 的桥 |

详见 [`docs/design/kernel-local-events.md`](../docs/design/kernel-local-events.md) 与 [`docs/design/observability-v1-design.md`](../docs/design/observability-v1-design.md)。

## 刻意不做

- 不 import / 不订阅 llm、loop、flow 业务事件
- 不做第二套字符串事件总线（无 `Collector.Emit(string, map)`）
- 不把 token 计数塞进官方 Record 字段
