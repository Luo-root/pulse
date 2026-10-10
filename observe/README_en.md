[English](README_en.md) | [中文](README.md)

# observe

`pulse/observe` is **graph observation**: it folds the engine's `Observer` callbacks (two run-level plus three per node) into structured `Record`s and writes them to a host-chosen `Sink`.

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

**Two run-level records per run**, plus **two segmented-timing records per node**:

| Event | Produced when | `Duration` | `Status` |
|---|---|---|---|
| `pulse.graph_started` | before any node is committed | `0` | `running` |
| `pulse.graph_finished` | after every node terminated (before `Wait` returns) | the whole run | `completed` / `failed` / `canceled` |
| `pulse.node_wait_finished` | the waiting segment ends | the waiting segment | `running`, otherwise the finish reason |
| `pulse.node_run_finished` | the execution segment ends | the execution segment | `completed` / `failed` / `canceled` |

The two run-level records **bracket** the run's node records (`started` precedes every node record, `finished` follows all of them); a host that only calls `Start` without `Wait` never sees `finished`. A skipped node gets **only** a `skipped` waiting record — it did arrive, it just never executed. A run in which every node skipped is still `completed` at run level: skipping is a node-level fact.

Attribution rides `Attrs` (`pulse.AttrGraph` / `pulse.AttrNode`; the key contract is defined by the engine, this package only consumes it): node records carry both, the run-level two carry only `pulse.AttrGraph` — the node dimension means nothing for "one run".

**Nesting depth** is the third dimension, `pulse.AttrPath`: when `ObserveConfig.Path` is non-empty (an egress that builds a child graph passes `sc.Path()`) **every** record carries it, and when it is empty the key is **not written** — an empty string would make "root" and "forgot to pass it" look alike, leaving an egress unable to use it for layering. It is a value on the **egress instance** (not something each record brings along), so nesting means one egress per level — exactly where `Sub`'s `build` runs. How to split the layers is the egress's business (`/` works); neither the engine nor this package offers a structured tree.

**Duration accounting**: everything is **wall clock**, and callbacks run synchronously on the caller's goroutine — however slow the egress is, that is how long the segment is (the `pulse.graph_finished` window covers the whole run, egress writes included). `AsyncSink` only defers **landing**, it does not take the `Write` call out of the window. A segment rounded to `0` (rendered `-` by the built-in layout) means "too small to measure", not "there was no such segment".

## Egress

`LineSink` (**default**: one human-readable line per record, buffered, zero-alloc, no slog) · `SlogSink` (attach to an existing logger, or JSON for a collector) · `MemorySink` (test assertions and demos) · `MultiSink` (fan out) · `AsyncSink` (a **wrapper** that takes a slow egress off the callback path).

`Sink` is the only interface you have to implement — a single `Write(Record)` method. It must be concurrency-safe and must not block for long: callbacks run synchronously **on the node's goroutine**, so a slow egress is a slow node.

## Host-supplied columns

The columnar layout is the default, not the only one. `WithRenderer(fn)` replaces the **line body** renderer so you can build your own columns from the six exported encoding primitives (`AppendDuration` / `AppendTextValue` / `AppendAttrs` / `AppendAttrsExcept` / `AppendPadding` / `DisplayWidth`) — **byte-identical** to the built-in layout, and the egress still knows nothing about your domain. See `Example_hostRenderer` for a runnable example.

## Docs

- Guide: [Graph observation](https://luo-root.github.io/pulse/en/guide/observability)
- Design: [`docs/design/pulse.md`](../docs/design/pulse.md) §7–§13 (the observation part)
