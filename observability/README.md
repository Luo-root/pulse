[English](README.md) | [中文](README_zh.md)

# observability

The official observability package of pulse v2: side-band subscription to kernel assembly events, writing out a unified `Record` envelope; runtime business events are folded into the same `Sink` by the companion bridge [`observability/bridge`](./bridge/).

After reading this you should be able to: `Use(Bootstrap)` first, pick a `Sink`, and know that runtime business metrics go through the bridge's `Attrs` open segment instead of named fields in this package.

## Layering discipline

| Layer | Knows about | Does not know about |
|---|---|---|
| This package | `kernel` typed events, `Sink`, `Record` | `llm` / `loop` / `flow` |
| The companion bridge `observability/bridge` | public llm/loop/flow events + this package's envelope | must not bypass the Sink to open another outlet posing as the official record |

The official package only produces `SourceKernel` records. Tokens, HITL, and node durations are folded into the same `Sink` by the bridge (`SourceBridge`) — **a shared outlet ≠ Record turning into a catch-all bag**; business dimensions (model name, token counts, tool name, node ID) go through the `Attrs` open segment, with key contracts defined by the package that owns the fact (`llm.AttrModel`, `loop.AttrTool`, `flow.AttrNode`).

## Wiring

```go
host := kernel.New()
defer host.Dispose()

sink := &observability.MemorySink{} // or SlogSink{Logger: slog.Default()}
// Must be Used first: kernel events are not replayed; a late Bootstrap
// can only recover the current view from the snapshot banner.
if _, err := kernel.Use(host, observability.Bootstrap("host-1", sink)); err != nil {
    panic(err)
}
// Then Use other plugins as usual; fiber_state / loader_action enter the Sink.

// Per request: the official bridge attaches listeners + the Collector
// service (see the bridge subpackage README).
reqScope, _ := host.Derive()
defer reqScope.Dispose()
b, err := bridge.Attach(reqScope, bridge.Config{
    Sink: sink, HostID: "host-1", TraceID: host.NewTraceID(),
})
```

`Bootstrap` subscribes to `fiber_state` / `loader_action` emitted tree-wide and writes a `host_ready` snapshot banner at the end of Apply. Tree teardown (Dispose) does **not** emit per-Fiber `fiber_state` (T7 ruling); the acceptance criterion is zero residue in the Sink after Dispose.

## Record / Sink

```text
General envelope: Time, HostID, TraceID, Source, Event, Duration, Status, Err
Assembly-only:    FiberName, From, To, LoaderKind, EntryID, PluginName
Attrs open seg:   scalar kv (~string/~int64/~float64/~bool)
```

- No `map[string]any` escape hatch; the only write path into `Attrs` is the generic `Set[T AttrValue]` — `[]byte`, structs, slices, and arbitrary objects cannot enter by type (the type part of the privacy boundary); self-describing keys plus a Sink-side redact hook cover deliberate scalar injection.
- `Sink.Write(Record)`: **no** `context.Context` (the kernel Emit path carries no ctx).
- When `Time` is zero, the builtin Sinks (`SlogSink` / `MemorySink`) fill in the wall clock; the `SlogSink` Attrs segment is emitted in key order (`Attrs.MarshalJSON` likewise).
- Builtins: `SlogSink`, `MemorySink`, `MultiSink`.

## Relationship to request-scoped events

| Fact | Dispatch | Who listens |
|---|---|---|
| fiber_state / loader_action | Tree-wide `Emit` | `Bootstrap` |
| tool / turn / llm generate | `EmitLocal` / `WaterfallLocal` | `bridge.Attach` (attached to `reqScope`) |
| Business custom facts | — | `bridge.CollectorKey` service direct write (`kernel.Get`) |

See [`docs/design/kernel-local-events.md`](../docs/design/kernel-local-events.md) and [`docs/design/observability-v1-design.md`](../docs/design/observability-v1-design.md) for details.

## Deliberately out of scope

- This package never imports / subscribes to llm, loop, or flow business events (only the bridge package knows them)
- No second string-event bus (no `Collector.Emit(string, map)`; business direct writes go through the typed `bridge.CollectorKey`)
- No stuffing token counts into official Record named fields (they go into Attrs, with keys owned by the fact's package)
