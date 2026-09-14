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

// Default egress: LineSink. Plug into an existing logger with SlogSink{Logger: …},
// or use &MemorySink{} for tests.
sink := observability.NewLineSink(os.Stdout)
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

- `Attrs` is an **insertion-ordered slice** internally (#179): the first write reserves room for 6 entries, so a typical record costs one allocation; `Range` walks in insertion order (deterministic, and it is the order the producer wrote them in — its semantic order). The builtin egresses (`SlogSink` / `LineSink`) emit that same order and never sort; only `Attrs.MarshalJSON` sorts by key, for tooling that expects stable object keys. Overwriting a key keeps its original position.
- No `map[string]any` escape hatch; the only write path into `Attrs` is the generic `Set[T AttrValue]` — `[]byte`, structs, slices, and arbitrary objects cannot enter by type (the type part of the privacy boundary); self-describing keys plus a Sink-side redact hook cover deliberate scalar injection.
- `Sink.Write(Record)`: **no** `context.Context` (the kernel Emit path carries no ctx).
- When `Time` is zero, the builtin egresses that print it (`LineSink` / `MemorySink`) fill in the wall clock; `SlogSink` never touches `Time` — the time field comes from your handler, so a record can never end up with two `time=` keys.
- Builtins: `SlogSink`, `LineSink`, `MemorySink`, `MultiSink`; `AsyncSink` is a **wrapper** (makes any downstream egress asynchronous — see “Choosing an egress”).

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

Measured (i9-14900HX, Windows, AC power and idle; **absolute ns varies 2–4x with
power/load state — trust ratios and alloc counts**; baseline = `SlogSink` writing
straight to an unbuffered file):

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
| Business custom facts | — | `observability.CollectorKey` direct write (**scope-local binding**: read via `kernel.Get` with the request scope or a descendant; parents / siblings / other concurrent requests cannot see it); **it does not satisfy `kernel.Require`** — local bindings stay out of dependency resolution, so a plugin declaring it as a dependency sits silently in `inactive`; diagnose with `FiberSnapshots().WaitingFor` |

See [`docs/design/kernel-local-events.md`](../docs/design/kernel-local-events.md) and [`docs/design/observability-v1-design.md`](../docs/design/observability-v1-design.md) for details.

## Deliberately out of scope

- This package never imports / subscribes to llm, loop, or flow business events (each package adapts itself; arrows point to the foundations)
- No second string-event bus (no `Collector.Emit(string, map)`; business direct writes go through the typed `CollectorKey`)
- No stuffing token counts into official Record named fields (they go into Attrs, with keys owned by the fact's package)

## Choosing an egress (LineSink / SlogSink / MemorySink)

| Egress | Form | Use when |
|---|---|---|
| `LineSink` | **Default.** One human-readable line per record, self-buffered, never through `log/slog` | Anything a person reads: terminal, log file, startup banner. ~5x cheaper than `SlogSink`, zero-alloc |
| `SlogSink` | `log/slog` with a Text / JSON handler | You must plug into an existing logger, or need JSON for a collector |
| `MemorySink` | In-memory collection | Test assertions and demos |

```text
PULSE | 2026/09/14 - 12:42:03.531 | completed  |   585.0µs | llm.generate_finished | source=bridge | llm.model=gpt-4o-mini llm.tokens_in=42 | host=pulse-web | trace=6504f73f
PULSE | 2026/09/14 - 12:42:03.100 | -          |         - | pulse.kernel.fiber_state | source=kernel | fiber=llmAdapter#3 | state=loading→active
```

`LineSink` carries the **same facts in the same order** as `SlogSink`; only the presentation differs, and every difference is deliberate:

- **Columns** `| <status> | <duration> | <event> |` replace three `k=v` fields. A missing column renders `-`, so the event column starts at the same offset on every line — alignment is the whole point of a format you scan rather than parse.
- **Duration keeps its unit** (`820ns` / `585.1µs` / `7.62ms` / `1.23s`) instead of an integer `duration_ms`, which truncated every sub-millisecond record to `0`. Integer math, no float formatting.
- **Shorter keys** where it reads better: `host_id`→`host`, `trace_id`→`trace`, `error`→`err`, `loader_kind`→`loader`, `entry_id`→`entry`, and `from`/`to` collapse into `state=loading→active`. Values are unchanged — a fact is never dropped, only renamed.
- **`Attrs` in insertion order** (the producer's semantic order), as in `SlogSink`; nothing is sorted.
- **ANSI colour only when the destination is a terminal** (`*os.File` + char device), and only on structural signals: dim prefix / time / trace, red duration on error, yellow duration ≥ 1s. Never on the `Status` string — that vocabulary belongs to the package that owns the fact, not to the base layer.

```go
sink := observability.NewLineSink(file)   // 32 KiB line buffer by default
defer sink.Flush()                        // mandatory before shutdown (last batch is buffered)
_ = sink.Err()                            // first write error is recorded, never panics
```

Measured (i9-14900HX / Windows, AC power and idle; **absolute ns varies 2–4x with power/load state — trust ratios and alloc counts, and only compare numbers from the same run**; envelope + 3 Attrs; whole table re-measured in one session for #194):

| Case | `SlogSink` | `LineSink` |
|---|---|---|
| Formatting (ns/op) | 1135–1155 | **213** |
| Allocations (allocs/op) | 14 | **0** |
| Including disk (slog+bufio ↔ LineSink's own buffer) | 1258 | **347** |
| Unbuffered disk (control) | 12267 | — |

Field-count sensitivity (discarded output): `SlogSink` ≈ 0.87 / 1.15 / 1.85 µs at 0 / 3 / 10 attrs (8–29 allocs) versus `LineSink` ≈ 0.18 / 0.21 / 0.29 µs (**0 allocs at every width**) — per-field cost drops from ~0.5 µs to ~0.06 µs, and the widest records gain most (the old egress sorted keys once past 8 attrs).

Standing benchmarks: `go test -bench . ./observability/` (`sink_bench_test.go` splits construction → formatting → disk so any egress change can be compared layer by layer). Conclusion: **bottleneck order = unbuffered write syscall ≫ slog formatting > fold/construction > kernel dispatch**.

## Host-provided egress (WithRenderer)

The columnar layout above is the **default**, not the only one. When a host wants **domain facts in columns** (HTTP method / path / client, LLM model / usage, …) it replaces the line-body renderer and builds its own columns from the exported encoding primitives — the egress still knows nothing about any business vocabulary; the domain stays on the host side.

The renderer contract is three lines long:

- **Body only**: the line prefix (`WithPrefix`, including its dim colouring) and the trailing newline are added by the sink — they are sink semantics, not layout.
- **`color` is the sink's resolved decision** (TTY detection + `WithColor` override): the host never probes the destination itself, and never writes ANSI into a log file that was redirected to disk.
- **No buffering, no writes to the writer**: the write timing belongs to the sink (batched by default, per-record with `WithImmediate()`).

```go
render := func(dst []byte, r observability.Record, color bool) []byte {
	dst = r.Time.AppendFormat(dst, "2006/01/02 - 15:04:05.000")
	dst = append(dst, " | "...)
	model, _ := observability.Get[string](r.Attrs, llm.AttrModel) // typed read, no `any`
	dst = observability.AppendTextValue(dst, model)
	dst = append(dst, " | "...)
	return observability.AppendDuration(dst, r.Duration)
}
sink := observability.NewLineSink(os.Stdout,
	observability.WithImmediate(), // watching a terminal: every line lands at once
	observability.WithRenderer(render))
```

The exported primitives are **byte-identical** to the built-in layout — `linelog_renderer_test.go` slices the built-in line and compares, rather than relying on convention:

| Primitive | Rule it shares with the built-in layout |
|---|---|
| `AppendDuration` | Unit-carrying, never rounded: `820ns` / `585.1µs` / `7.62ms` / `1.23s` |
| `AppendTextValue` | The `k=v` quoting rule (spaces / equals / quotes / control characters) |
| `AppendAttrs` | A whole attrs group: insertion order + four scalar kinds + the same quoting rule |
| `DisplayWidth` + `AppendPadding` | Padding by **display column** (CJK / fullwidth 2 columns, combining marks 0) |

**When to bring your own egress**: when you need to change the **layout or the colouring** by your own domain semantics. If the default layout is fine and you only need your existing logger or JSON, keep `SlogSink`; if you only need a different destination, change nothing — `NewLineSink(w)` is enough.

These five primitives and `LineRenderer` are part of the **frozen surface** (see Release & Compatibility in the root README): their rules may only change in a minor release. A runnable full example (four columns plus the zero-allocation shape) is `Example_hostRenderer`.

