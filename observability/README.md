[English](README.md) | [中文](README_zh.md)

# observability

The official observability package of pulse v2: side-band subscription to kernel assembly events, writing out a unified `Record` envelope.

After reading this you should be able to: `Use(Bootstrap)` first, pick a `Sink`, and know why runtime business metrics do not enter this package.

## Layering discipline

| Layer | Knows about | Does not know about |
|---|---|---|
| This package | `kernel` typed events, `Sink`, `Record` | `llm` / `loop` / `flow` |
| The assembly-layer bridge (e.g. `examples/internal/demoapp`) | public llm/loop/flow events | must not bypass the Sink to open another outlet posing as the official record |

The official package only produces `SourceKernel` records. Tokens, HITL, and node durations are folded into the same `Sink` by the bridge (`SourceBridge`); metrics that do not fit the envelope go into slog extra keys — **a shared outlet ≠ Record turning into a catch-all bag**.

## Wiring

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

`Bootstrap` subscribes to `fiber_state` / `loader_action` emitted tree-wide and writes a `host_ready` snapshot banner at the end of Apply. Tree teardown (Dispose) does **not** emit per-Fiber `fiber_state` (T7 ruling); the acceptance criterion is zero residue in the Sink after Dispose.

## Record / Sink

```text
通用信封：Time, HostID, TraceID, Source, Event, Duration, Status, Err
装配专用：FiberName, From, To, LoaderKind, EntryID, PluginName
```

- No `Attributes map`: the privacy boundary is guaranteed by types.
- `Sink.Write(Record)`: **no** `context.Context` (the kernel Emit path carries no ctx).
- When `Time` is zero, the builtin Sinks (`SlogSink` / `MemorySink`) fill in the wall clock.
- Builtins: `SlogSink`, `MemorySink`, `MultiSink`.

## Relationship to request-scoped events

| Fact | Dispatch | Who listens |
|---|---|---|
| fiber_state / loader_action | Tree-wide `Emit` | `Bootstrap` |
| tool / turn / HITL / llm generate | `EmitLocal` / `WaterfallLocal` | The bridge attached to `reqScope` |

See [`docs/design/kernel-local-events.md`](../docs/design/kernel-local-events.md) and [`docs/design/observability-v1-design.md`](../docs/design/observability-v1-design.md) for details.

## Deliberately out of scope

- No importing / subscribing to llm, loop, or flow business events
- No second string-event bus (no `Collector.Emit(string, map)`)
- No stuffing token counts into official Record fields
