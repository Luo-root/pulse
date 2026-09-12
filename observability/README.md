[English](README.md) | [中文](README_zh.md)

# observability

The official observability package of pulse v2 (one of the two foundations): side-band subscription to kernel assembly events, writing out a unified `Record` envelope; runtime business facts are folded into the same `Sink` by each business package's observation adapter (`llm.Observe` / `loop.Observe` / `flow.NewRecordObserver`).

After reading this you should be able to: `Use(Bootstrap)` first, pick a `Sink`, and know that runtime business metrics share the same outlet and TraceID via `ObserveConfig`.

## Layering discipline (the dual-foundation model)

| Layer | Knows about | Does not know about |
|---|---|---|
| This package (foundation) | `kernel` typed events, `Sink`, `Record` | `llm` / `loop` / `flow` |
| Business packages (tenants) | kernel + observability + their own facts | —— |
| Host (assembly layer) | everything | —— |

Dependency arrows all point to the foundations: this package imports only kernel with no exceptions (the former companion bridge package was removed; folding adapters moved down into the packages that own the facts). Business dimensions (model name, token counts, tool name, node ID) go through the `Attrs` open segment, with key contracts defined by the package that owns the fact (`llm.AttrModel`, `loop.AttrTool`, `flow.AttrNode`) — **a shared outlet ≠ Record turning into a catch-all bag**.

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

// Per request: derive a request scope; observers are removed
// automatically when the scope is disposed.
reqScope, err := host.Derive()
if err != nil {
    panic(err)
}
defer reqScope.Dispose()

// Per request: reusing cfg IS the request-level correlation (D3);
// a new cfg is mandatory across requests.
// The host calls NewTraceID once per request (the single generation
// source); a fully custom scheme works too (host-owned formats, e.g.
// a hostID prefix plus a per-request sequence).
cfg := observability.ObserveConfig{Sink: sink, HostID: "host-1", TraceID: observability.NewTraceID()}
c, err := observability.AttachCollector(reqScope, cfg) // direct-write service (scope-local: readable only inside this request subtree)
err = llm.Observe(reqScope, cfg)                       // llm package adapter
err = loop.Observe(reqScope, cfg)                      // loop package adapter
// flow graph: flow.WithObserver(must(flow.NewRecordObserver(cfg)))
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

## Async egress (AsyncSink)

Wrap a slow egress (file / network exporter) in `AsyncSink`: `Write` only deep-copies
`Attrs` and enqueues, then returns; a single background goroutine writes in FIFO order.
Kernel event dispatch is fully synchronous (`Emit` / `EmitLocal` / `Waterfall`;
`Parallel` waits too), so without this layer the egress's latency lands directly on
requests and agent steps.

```go
sink := observability.NewAsyncSink(observability.SlogSink{Logger: lg},
    observability.WithCapacity(1024)) // default 1024; blocks when full (no loss)
defer sink.Close(ctx)                 // host shutdown path: drain + stop the worker
```

- **Full-queue policy**: blocks by default (backpressure, no loss); `DropOnFull()`
  drops the newest record and counts it in `Dropped()`;
- **`Flush(ctx)`** waits for everything enqueued up to the call (including in-flight);
  an expired ctx returns an error and **keeps the queue** (the worker keeps draining);
- **`Close(ctx)`** stops intake, drains, and stops the worker (idempotent); an expired
  ctx returns immediately with the remaining queue counted as dropped;
- **`Dropped()`** is a single combined counter: full-queue drops / writes after Close /
  blocked writes woken by Close / leftovers on Close timeout / records skipped by an
  inner panic;
- **inner panics** are recovered and counted; the worker keeps consuming (it is the
  only consumer — a silently stalled worker is worse than a crash);
- **Explicit Close/Flush is a prerequisite**: skip it and queued records die with the
  process. Copyable host shutdown wiring:

```go
func main() {
    sink := observability.NewAsyncSink(realSink, observability.WithCapacity(4096))
    defer func() {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        _ = sink.Close(ctx) // drain the async egress before the process exits
    }()
    // ... business code: sink.Write(...) stays off the request path
}
```

Measured (i9-14900HX, Windows; baseline = `SlogSink` writing straight to an
unbuffered file):

| Case | Direct | AsyncSink |
|---|---|---|
| Burst of 1000 (producer side) | 95.3 ms (95 µs/rec) | **1.5 ms (1.5 µs/rec) ≈ 63x** |
| Background drain (same batch) | — (included above) | 52 ms (the work still exists, just off the producer path) |
| Enqueue (empty Attrs) | — | 211–242 ns, 0 alloc |
| Enqueue (3 Attrs) | — | ≈ 1.0 µs, 2 allocs (deep copy is O(N)) |

**Async does not raise the throughput ceiling**: once the sustained rate exceeds the
egress's capacity the bounded queue fills and backpressures the producer to the
egress's rate — that is the price of "no loss"; use `DropOnFull()` to drop instead of
block. It is a pessimization for already-fast sinks (e.g. `MemorySink`) — do not wrap
those by default.

## Relationship to request-scoped events

| Fact | Dispatch | Who listens |
|---|---|---|
| fiber_state / loader_action | Tree-wide `Emit` | `Bootstrap` |
| tool / turn / llm generate | `EmitLocal` / `WaterfallLocal` | Per-package `Observe` (attached to reqScope) |
| flow node segments | Observer callbacks | `flow.NewRecordObserver` |
| Business custom facts | — | `observability.CollectorKey` direct write (**scope-local binding**: read via `kernel.Get` with the request scope or a descendant; parents / siblings / other concurrent requests cannot see it) |

See [`docs/design/kernel-local-events.md`](../docs/design/kernel-local-events.md) and [`docs/design/observability-v1-design.md`](../docs/design/observability-v1-design.md) for details.

## Deliberately out of scope

- This package never imports / subscribes to llm, loop, or flow business events (each package adapts itself; arrows point to the foundations)
- No second string-event bus (no `Collector.Emit(string, map)`; business direct writes go through the typed `CollectorKey`)
- No stuffing token counts into official Record named fields (they go into Attrs, with keys owned by the fact's package)
