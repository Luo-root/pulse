[English](README_en.md) | [中文](README.md)

# observe

`pulse/observe` is **graph observation**: it folds the engine's three `Observer` callbacks into structured `Record`s and writes them to a host-chosen `Sink`.

The dependency is one-way (`pulse` ← `observe`): the engine does not know this package exists and only exposes a seam; a host that needs no observation never imports it.

## Shortest usage

```go
sink := observe.NewLineSink(os.Stdout)
defer sink.Flush() // Flush before shutdown: the last batch is still buffered

obs, err := observe.NewRecordObserver(observe.ObserveConfig{
	Sink:    sink,
	HostID:  "host-1",             // host identity: stable across runs; with TraceID it means "who + which run"
	TraceID: observe.NewTraceID(), // run-time identity, single generation source
})
g, err := pulse.New(ctx, "demo", pulse.WithObserver(obs))
// … add nodes, seed, run
```

## Record shape

Two **segmented-timing** records per node:

| Event | `Duration` | `Status` |
|---|---|---|
| `pulse.node_wait_finished` | the waiting segment | `running`, otherwise the finish reason |
| `pulse.node_run_finished` | the execution segment | `completed` / `failed` / `canceled` |

A skipped node gets **only** a `skipped` waiting record — it did arrive, it just never executed. Attribution rides `Attrs` (`pulse.AttrGraph` / `pulse.AttrNode`; the key contract is defined by the engine, this package only consumes it).

## Egress

`LineSink` (**default**: one human-readable line per record, buffered, zero-alloc, no slog) · `SlogSink` (attach to an existing logger, or JSON for a collector) · `MemorySink` (test assertions and demos) · `MultiSink` (fan out) · `AsyncSink` (a **wrapper** that takes a slow egress off the callback path).

`Sink` is the only interface you have to implement — a single `Write(Record)` method. It must be concurrency-safe and must not block for long: callbacks run synchronously **on the node's goroutine**, so a slow egress is a slow node.

## Host-supplied columns

The columnar layout is the default, not the only one. `WithRenderer(fn)` replaces the **line body** renderer so you can build your own columns from the six exported encoding primitives (`AppendDuration` / `AppendTextValue` / `AppendAttrs` / `AppendAttrsExcept` / `AppendPadding` / `DisplayWidth`) — **byte-identical** to the built-in layout, and the egress still knows nothing about your domain. See `Example_hostRenderer` for a runnable example.

## Docs

- Guide: [Graph observation](https://luo-root.github.io/pulse/en/guide/observability)
- Design: [`docs/design/pulse.md`](../docs/design/pulse.md) §7–§13 (the observation part)
