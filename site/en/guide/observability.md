# Graph observation

Observation is an **independent layer**: the engine knows no observation package and only exposes one `Observer` seam; `pulse/observe` implements that seam, folding callbacks into structured records and writing them to an egress. A host that does not need observation imports the root package alone.

## The engine's seam

```go
type Observer interface {
	OnGraphStarted(graphID string)
	OnGraphFinished(graphID string, reason NodeFinishReason, err error)
	OnNodeWaiting(graphID, nodeID string)
	OnNodeRunning(graphID, nodeID string)
	OnNodeFinished(graphID, nodeID string, reason NodeFinishReason, err error)
}
```

The contract has five clauses, all of them frozen surface:

1. **Callback counts**: run level `Started ≤ 1`, `Finished ≤ 1`; per node `Waiting ≤ 1`, `Running ≤ 1`, `Finished = 1`. `Retry`'s several attempts **do not re-emit**;
2. **Read-only**: an observer's panic or error **must not** be promoted into a node failure (the engine side already swallows it);
3. **Concurrency-safe**: the three node callbacks run synchronously **on the node's own goroutine**, the two run-level ones on the **`Start` / `Wait` caller's goroutine** — so an implementation must be concurrency-safe, and must not block for long. **Do not call this graph's `Start` / `Wait` from inside a callback**: the callback runs on their calling path, so it is a goroutine waiting on itself and it deadlocks;
4. **The timing is a bracket**: `GraphStarted` precedes **every** node callback of the run (a graph that fails start validation never started, so nothing is emitted), and `GraphFinished` follows **all** of them; a host that calls `Start` without `Wait` never sees `finished`, while concurrent or repeated `Wait` calls all return only after that emission returned (by the time any `Wait` returns, the run's finish record has landed in the egress);
5. **The attribution key comes from the engine**: `graphID` is emitted with every callback, so an implementation never has to carry it in from constructor arguments of its own.

A graph defaults to no-op (attach nothing, pay nothing); `pulse.WithObserver(...)` attaches one; for several observers, combine them with `pulse.MultiObserver` (a later write overwrites an earlier one, so do the combining before passing it in).

## observe folds it into two run-level + two per-node records

| Event | Produced when | `Duration` | `Status` |
|---|---|---|---|
| `pulse.graph_started` | before any node is committed | `0` | `running` |
| `pulse.graph_finished` | after every node terminated (before `Wait` returns) | the whole run | the run's terminal state (`completed` / `failed` / `canceled`) |
| `pulse.node_wait_finished` | waiting ends (execution is entered, or the node terminates with skip / failure) | the waiting segment | `running`, otherwise the matching terminal state |
| `pulse.node_run_finished` | execution ends | the execution segment | the terminal state (`completed` / `failed` / `canceled`) |

The two run-level records **bracket** the run's node records: a host no longer has to stitch node records back into a run by way of the `pulse.graph` attribute — a run is an entity with a head and a tail in the observation. The run-level terminal state is only ever `completed` / `failed` / `canceled`: **a run in which every node skipped is still `completed`** (skipping is a node-level fact and is not promoted to failure). A skipped node gets **only one `skipped` waiting record** — it did arrive, it just never executed.

The attribution dimensions ride `Attrs`: `pulse.AttrGraph` + `pulse.AttrNode` (the key contract is defined by the **engine**; `observe` only consumes it, never defines it). Node records carry both; the run-level two carry only `pulse.AttrGraph` — the node dimension means nothing for "one run".

**Nesting depth** is the third dimension, `pulse.AttrPath` (an opaque string, layers joined by `/`): an egress that builds a child graph sets `ObserveConfig.Path` to `sc.Path()`, and then **every** record of that layer carries it; when it is empty the key is **not written** — an empty string would make "root" and "forgot to pass it" look alike. The split of duties is clear: `pulse.graph` is the **graph id you chose**, `pulse.path` is the **layering the engine recorded** — running one child template twice means two graph instances and two paths, which is what makes them separable (see "A graph as a node" in [Orchestration](/en/guide/orchestration)). How to split the layers is the egress's business (`/` works); the engine offers no structured tree.

## Shortest wiring

```go
sink := observe.NewLineSink(os.Stdout, observe.WithImmediate())
defer sink.Flush() // Flush before shutdown: the last batch is still buffered

obs, err := observe.NewRecordObserver(observe.ObserveConfig{
	Sink:    sink,
	HostID:  "quickstart",              // host identity (stable across runs); with TraceID it means "who + which run"
	TraceID: observe.NewTraceID(),      // this run's correlation id (a run-time fact)
})
if err != nil {
	return err
}
g, err := pulse.New(ctx, "docs-pipeline", pulse.WithObserver(obs))
```

Real output for that same graph (a single node; timestamps, trace ids and durations vary per run):

```text
PULSE | 2026/10/09 - 10:42:50.472 | running    |         - | pulse.graph_started | source=observe | pulse.graph=docs-pipeline | host=quickstart | trace=1791513770472465000-1667c395-2
PULSE | 2026/10/09 - 10:42:50.472 | running    |         - | pulse.node_wait_finished | source=observe | pulse.graph=docs-pipeline pulse.node=summarize | host=quickstart | trace=1791513770472465000-1667c395-2
PULSE | 2026/10/09 - 10:42:50.487 | completed  |   15.50ms | pulse.node_run_finished | source=observe | pulse.graph=docs-pipeline pulse.node=summarize | host=quickstart | trace=1791513770472465000-1667c395-2
PULSE | 2026/10/09 - 10:42:50.487 | completed  |   15.50ms | pulse.graph_finished | source=observe | pulse.graph=docs-pipeline | host=quickstart | trace=1791513770472465000-1667c395-2
```

Four records: the two run-level ones bracket the two node ones. The run-level `Duration` is the **whole run** (wall clock between the two callbacks) — here it is the same order as the node's execution segment because this run was spent almost entirely in that node; graph assembly, `Seed` and `Flush` are outside its window (accounting below). The `Status` of `pulse.graph_finished` is the run's terminal state and its `Err` is the very value `Run()` returns.

Line-body column order: `time | status | duration | event | source | attrs | host | err | trace`. A missing status or duration column renders `-`, which is what keeps the event column's start fixed (`-` is the **built-in layout's empty-column placeholder**, not primitive behaviour). Timestamps, trace ids and durations vary per run.

## Egress

`Sink` is the only interface you have to implement:

```go
type Sink interface{ Write(r Record) }
```

Three contract clauses:

1. **Concurrency-safe**, and must not block the caller for long (`Write` sits on the callback path, on the node's goroutine);
2. **No `context.Context`** — callbacks carry no ctx; an exporter that needs a deadline keeps an internal queue of its own;
3. **Reference semantics**: the producer builds a fresh `Attrs` and does not modify it once `Write` returns; the Sink consumes read-only. An asynchronous exporter must copy for itself.

Built-in egresses:

| Egress | Shape | Fits |
|---|---|---|
| `LineSink` | **The default**. One human-readable line per record, with its own 32 KiB buffer, zero-alloc, no slog | Terminals, log files |
| `SlogSink` | `log/slog` (Text / JSON) | Attaching to an existing host logger; JSON fed to a collector |
| `MemorySink` | In-memory collection | Test assertions, demos |
| `MultiSink` | Fan out to several egresses | Landing on disk and collecting at the same time |
| `AsyncSink` | **A wrapper**: bounded queue + a single background goroutine | Slow egresses (files / network) |

### Duration accounting (measured)

All four segments are **wall clock**, and callbacks run synchronously — so **however slow the egress is, that is how long the segment is**. The same graph, the same empty node (`noop`), two egresses (`MemorySink`, and a fake egress whose `Write` sleeps 20ms per record):

```text
# MemorySink (writing a record costs about nothing)
pulse.graph_started            node=-      running    0s
pulse.node_wait_finished       node=noop   running    0s
pulse.node_run_finished        node=noop   completed  0s
pulse.graph_finished           node=-      completed  0s

# a fake egress whose Write sleeps 20ms
pulse.graph_started            node=-      running    0s
pulse.node_wait_finished       node=noop   running    0s
pulse.node_run_finished        node=noop   completed  20.5701ms
pulse.graph_finished           node=-      completed  62.2437ms
```

The node body is empty: the `20.5701ms` is entirely that one write (the waiting-segment record is written inside the `Running` callback, and the execution segment's timer starts before it). **The run-level record is longer** (`62.2437ms`) — its window covers "commit the nodes → everything terminated", and this run performed three writes inside that window (run-level started, node waiting segment, node execution segment). Hence:

- When the duration column looks too large, suspect the egress before the node; `LineSink` on a real terminal is itself a slow egress (one write syscall per record), its double-digit-millisecond costs land in the segments the same way, and they vary with the terminal and with redirection;
- Wrap a slow egress in `AsyncSink` (`Write` only deep-copies `Attrs` and enqueues). Note that async **does not raise the throughput ceiling**: when the sustained rate exceeds what the egress can take, the bounded queue back-pressures to the egress rate — that is precisely the price of "never drop a record"; to drop instead of blocking, use `DropOnFull()`. Wrapping an already fast egress (such as `MemorySink`) in async is a pessimisation; it also **does not take the `Write` call out of the window**, it only defers landing;
- A segment of `0` (rendered `-`) means the segment was rounded to 0 by the timer's precision, not that "there was no such segment".

**How the run-level record differs from your own stopwatch around `Run()`**: its window starts inside `Start()` (before any node is committed) and ends before `Wait()` returns — so it **excludes** graph assembly, `Seed`, and the egress `Flush` after `Wait()` returns, which makes your outside stopwatch generally longer; conversely it includes the write of the run-level started record, which is in no node segment at all. Use it for "the boundary of one run"; keep your own stopwatch for "how long my function spent in `Run`".

## Host-supplied columns (WithRenderer)

The columnar layout is the **default**, not the only one. When a host wants domain facts in columns, it swaps the **line body** renderer with `WithRenderer(fn)` and builds its own columns from the package's exported encoding primitives — no need to bring a Sink of its own:

```go
render := func(dst []byte, r observe.Record, color bool) []byte {
	dst = r.Time.AppendFormat(dst, "15:04:05.000")
	dst = append(dst, " | "...)
	node, _ := observe.Get[string](r.Attrs, pulse.AttrNode)
	dst = observe.AppendTextValue(dst, node)
	dst = append(dst, " | "...)
	dst = observe.AppendTextValue(dst, r.Status)
	dst = append(dst, " | "...)
	return observe.AppendDuration(dst, r.Duration)
}

sink := observe.NewLineSink(os.Stdout,
	observe.WithImmediate(),
	observe.WithRenderer(render))
```

Real output (the same graph):

```text
PULSE | 15:40:40.526 | step | running | 0ns
PULSE | 15:40:40.540 | step | completed | 13.65ms
```

The renderer's contract is only four clauses: it **produces the line body only** (the line prefix and the trailing newline are added by the sink); the `color` it receives is the sink's already-resolved verdict (so it need not probe the terminal itself, and will not write ANSI into logs redirected to a file); it does not buffer and does not write to the writer (when to write belongs to the sink, and `WithImmediate` controls immediacy); and **it is called inside the sink's internal lock** — do not call back into the same sink's `Write` / `Flush` / `Err` from inside a renderer (`sync.Mutex` is not reentrant, so it hangs silently).

The six encoding primitives (`AppendDuration` / `AppendTextValue` / `AppendAttrs` / `AppendAttrsExcept` / `AppendPadding` / `DisplayWidth`) are **isomorphic** to the built-in layout and belong to the frozen surface: for one record under the two layouts, durations, attribute groups and column padding are **byte-identical**. `AppendAttrsExcept` is there to add "attributes the fixed columns cannot hold" at the end of the line, without rewriting the scalar and quoting rules yourself.

## Record and Attrs

```go
type Record struct {
	Time     time.Time
	HostID   string
	TraceID  string
	Source   Source
	Event    string
	Duration time.Duration
	Status   string
	Err      error
	Attrs    Attrs // the open segment
}
```

Design constraint: **business dimensions always enter through `Attrs`; named fields are never extended**. Named fields serve only the facts every record shares — once a named field is added for one domain, `Record` grows into a convergence point for every domain's observation fields.

`Attrs` is an **insertion-ordered small slice** (not a map): the order is the producer's semantic order, so an egress gets stable output without sorting; reads are a linear scan, which beats hashing when the entry count is small. Keys use the `<component>.<field>` dot convention, each component with its own key space (`pulse.graph` / `pulse.node`; a host forms a segment of its own).

When a host writes its own records (business facts, say), it uses the same API:

```go
rec := observe.Record{
	HostID:  "host-1",
	TraceID: "t-1",
	Source:  "app", // declare your own home: this package only produces source=observe
	Event:   "app.turn_finished",
	Status:  "ok",
}
observe.Set(&rec.Attrs, "app.turns", int64(3))
sink.Write(rec)
```

```text
PULSE | 2026/10/08 - 15:40:11.308 | ok         |         - | app.turn_finished | source=app | app.turns=3 | host=host-1 | trace=t-1
```

## TraceID

It is injected by the host from a **single generation source**: calling `NewTraceID()` once per run is a single generation source, and a scheme you bring entirely yourself (such as a hostID prefix plus a monotonic sequence number) works just as well. The return value has **no contractual semantics**; consumers must not parse its structure. Every record of one run shares the same TraceID — that is the entire mechanism for run-level correlation.

A host that speaks OTel / W3C trace context **injects its own trace id** rather than adopting the shape of `NewTraceID()`:

```go
TraceID: span.SpanContext().TraceID().String(), // 32 lowercase hex chars (a W3C trace-id)
```

`NewTraceID()` returns a "timestamp-random-seq" string — friendly to humans and naturally sortable, but **not** the 32-hex W3C form. Two boundaries are worth remembering:

- a trace-id alone cannot produce a valid `traceparent`: `parent-id` (16 chars) and `trace-flags` are span semantics, and `observe` only knows about "one run" — it has no spans, so those two fields belong to the host;
- **continuing an inbound trace** is simply putting the received trace-id into `ObserveConfig.TraceID`; ignoring an invalid `traceparent` wholesale (as W3C requires) is the host's responsibility too.

An `ObserveConfig`'s lifetime **equals one run** (one `Run` of the graph): across runs a new one is required (the TraceID is unique per run, and reusing an old value manufactures false correlation).

## Privacy boundary

`Record` has no `map[string]any` escape hatch: the only write surface for `Attrs` is the generic `Set` (constrained to `~string | ~int64 | ~float64 | ~bool`), so **prompts, attachment bytes, secrets and chain-of-thought cannot enter by type**.

The residual part of the boundary should be stated plainly: `Err` is an `error`, and where it comes from is whatever the caller passed — so **an adapter layer must not stuff an upstream's raw error body into `Err`**; it should pass an already-classified summary. "Cramming a payload into a scalar" is deliberate behaviour; the defence is the key declaring its own intent, plus a redact hook on the Sink side (an implementation can reject sensitive keys / truncate over-long values / cap the count).

Package-level API and benchmark accounting: [observe package docs](/en/packages/observe/); the full design: [design doc](https://github.com/Luo-root/pulse/blob/main/docs/design/pulse.md).
