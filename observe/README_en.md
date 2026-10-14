[English](README_en.md) | [中文](README.md)

# observe

`github.com/Luo-root/pulse/observe` converts the Pulse engine's `Observer` callbacks into structured `Record`s and sends them to a host-selected `Sink`. This page is a package reference for configuration, events and sinks. For a step-by-step integration, see the [graph observation guide](https://luo-root.github.io/pulse/en/guide/observability).

The dependency direction is `pulse` ← `observe`. This package depends only on the root `pulse` package and the standard library; importing the root package does not require observation.

## Version and installation

Requires Go 1.25.0 or later. This page describes the current `main` API; some interfaces have not yet shipped in a release tag. The latest release tag is `v0.4.1`. To run these examples, pin a commit containing the documented interfaces:

```bash
go get github.com/Luo-root/pulse@1f67045d6c42
```

## Complete example

This program creates one node that needs no inputs and writes the run's records to standard output. Every error is propagated. The exit path flushes the buffer and checks the flush error even when the run fails.

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/observe"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() (err error) {
	sink := observe.NewLineSink(os.Stdout)
	defer func() {
		if flushErr := sink.Flush(); flushErr != nil {
			err = errors.Join(err, fmt.Errorf("flush records: %w", flushErr))
		}
	}()

	g, err := pulse.New("demo", "A single observed node")
	if err != nil {
		return err
	}
	if err := g.Add(pulse.NewNode("work", "Complete one step", nil, nil,
		func(*pulse.RunCtx) error { return nil },
	)); err != nil {
		return err
	}

	obs, err := observe.NewRecordObserver(observe.ObserveConfig{
		Sink:    sink,
		HostID:  "demo-host",
		TraceID: observe.NewTraceID(),
	})
	if err != nil {
		return err
	}
	if err := g.Run(context.Background(), pulse.Inputs{}, pulse.WithExecObserver(obs)); err != nil {
		return err
	}
	return nil
}
```

A successful run emits `pulse.graph_started`, `pulse.node_wait_finished`, `pulse.node_run_finished` and `pulse.graph_finished`. Timestamps, durations and execution identities come from the actual run and may change each time.

`g.Run` creates an Execution, calls Start and calls Wait, so the graph-finished callback has completed before it returns. LineSink buffers 32 KiB by default; the last batch may not reach that threshold. Call `Flush()` and check its result after producers stop writing, before closing the underlying writer or exiting the process. Add `observe.WithImmediate()` at construction for each line to be visible immediately; still check the final `Flush()`.

## Configuration and identity

`NewRecordObserver(cfg)` returns `(pulse.Observer, error)`. A nil `cfg.Sink` returns `ErrNilSink`.

| `ObserveConfig` field | Purpose | Usage |
|---|---|---|
| `Sink` | Record destination | Required; implementations must follow the concurrency and read-only contract below |
| `HostID` | Stable host identity | Host-provided service or instance identifier; an empty value is omitted |
| `TraceID` | Correlation identity for a host request or logical run | Supplied by one host-owned generator; adapters in one logical run share it, independent runs should receive a new value |
| `Path` | Fallback when a callback has no path | Usually empty; Site.Path takes priority, and a nonempty fallback also applies to root records |

These identities serve different purposes from the engine's execution identity:

| Identity | Source | Distinguishes |
|---|---|---|
| `pulse.graph` | `Site.Graph` | Graph definitions |
| `pulse.execution` | `Site.Execution` / `Execution.ID()` | Each independent Execution; running one definition twice produces different values |
| `pulse.path` | `Site.Path`, otherwise `ObserveConfig.Path` | Child mount positions such as `step` or `outer/inner` |
| `HostID` | Host configuration | The host producing a record |
| `TraceID` | Host configuration | Business correlation across graphs and components |

Node records also carry `pulse.node`; run-level records do not carry a node attribute. The root Site.Path is empty, so ordinary configuration omits `pulse.path` there. Every engine-run record carries `pulse.execution`; graph + path alone cannot distinguish concurrent runs.

The observer accounts by execution identity and can safely serve multiple graphs and concurrent executions. Reusing a configuration with one fixed TraceID, however, correlates independent requests. For a reusable graph, create configuration and an observer for each run and inject it with `pulse.WithExecObserver(obs)`, as in the example. The Sink may be shared. Use `pulse.WithObserver(obs)` for a definition-level default. Both options replace a single observer; use `pulse.MultiObserver` to compose it with a host observer.

A child without its own observer inherits the parent's effective observer. Each callback carries the mount position, so one Sink can receive records for the entire execution tree. Read the structural tree and live executions through Topology and execution snapshots; Record remains a flat envelope.

`NewTraceID()` returns an opaque correlation string. Calls are unique within a process, with a random segment distinguishing processes. The format is not a W3C trace-id. To integrate with an existing trace context, inject the host's own trace ID. The host owns spans, parent-id and trace-flags.

## Record fields

| Field | Type | Meaning |
|---|---|---|
| `Time` | `time.Time` | Record timestamp; LineSink / MemorySink fill a zero value with the current time, while SlogSink uses the logger handler's timestamp |
| `HostID` / `TraceID` | `string` | Host and correlation identities |
| `Source` | `Source` | This package emits `SourceObserver` (`"observe"`); a host writing records directly may use its own source |
| `Event` | `string` | Event name, listed below |
| `Duration` | `time.Duration` | Observed segment or whole-run wall-clock duration; zero when not timed |
| `Status` | `string` | `running` or a finish reason; direct host records may use a host-owned status vocabulary |
| `Err` | `error` | Associated failure or cancellation error; nil for normal skipping |
| `Attrs` | `Attrs` | Open scalar attributes, including business dimensions |

### Attrs

The zero value of Attrs is usable. Write with `observe.Set(&attrs, key, value)` and read with `observe.Get[T](attrs, key)`, checking the returned `ok`. Values are string, int64, float64, bool, or named types with one of those underlying types.

Attribute names follow `<component>.<field>`, such as `app.model`. The `pulse` package defines constants for engine attributes. First insertion determines order; replacing a value retains its position. Range, LineSink and SlogSink preserve insertion order; MarshalJSON sorts by key.

Record has no arbitrary-object or business-value container. Scalar constraints cannot detect sensitive content inside a string. Filtering, truncation and attribute-count limits belong in a host Sink when needed.

## Event reference

Observe owns these event constants. An Execution that successfully starts and is finalized by Wait has two run-level records. The node record count depends on the phases actually observed.

### Run-level events

| Constant / event name | Emitted when | `Duration` | `Status` |
|---|---|---|---|
| `EventGraphStarted` / `pulse.graph_started` | Startup validation passes, before any node goroutine is submitted | `0` | `running` |
| `EventGraphFinished` / `pulse.graph_finished` | All nodes have terminated, before Wait returns | Whole-run duration between the two graph callbacks | `completed` / `failed` / `canceled` |

The two records bracket this Execution's node records. Failed startup validation emits no started record. A host that calls Start without Wait receives no finished record. Repeated or concurrent Wait calls do not emit finished again.

### Node events

| Constant / event name | Emitted when | `Duration` | `Status` |
|---|---|---|---|
| `EventNodeWaitFinished` / `pulse.node_wait_finished` | Waiting ends by entering Run or terminating while waiting | Waiting segment | `running` when entering Run, otherwise the node's terminal status |
| `EventNodeRunFinished` / `pulse.node_run_finished` | A node that entered Run terminates | Execution segment | `completed` / `skipped` / `failed` / `canceled` |
| `EventNodeFinished` / `pulse.node_finished` | Only Finished was observed for a failed or canceled node, such as an Aspect rejecting it before next | `0`; no observed segment | `failed` / `canceled` |

The five Observer callback types do not map one-to-one onto these five Record events. Waiting and Running start or end timing; Finished finalizes it. Retry does not repeat Waiting / Running, so retries do not produce another set of node segment records. The execution segment can include multiple attempts and their delays.

### Skipping, short circuits and restoration

| Node path | Node records |
|---|---|
| All inputs skip; the body never runs | One waiting-finished record with status `skipped` |
| The body enters Run and returns `pulse.NoValue()` | Waiting-finished record + execution-finished record with status `skipped` |
| An Aspect fails or cancels before next | One untimed node_finished, with graph, execution, path and node attribution |
| An Aspect succeeds or normally skips before next | No invented waiting or execution records |
| A checkpoint-restored completed / skipped node | No invented business timing records; Durable events describe restoration |

Normal skipping has nil Record.Err. Even if all nodes skip, the run is `completed`. A reliable checkpoint failure retains its failure identity and error chain even if that chain contains `ErrSkipped`; filtering on that marker must not turn it into normal skipping.

### Duration accounting

Durations measure wall-clock time, not CPU time. Callbacks run synchronously on the caller's goroutine: node callbacks on node goroutines and graph callbacks on the Start / Wait call path. Sink work inside a timing window contributes to that segment or whole-run duration, and a slow sink directly delays its caller. AsyncSink defers inner writes, but enqueueing and any queue wait remain on the callback path.

A zero duration can mean no timing was observed or that the clock lacked enough precision. Use the event type to determine whether a phase occurred. The built-in LineSink layout displays zero duration as `-`. SlogSink emits `duration_ms` as numeric milliseconds with sub-millisecond precision: 585 microseconds becomes `0.585`. Zero duration omits the key.

## State views and event history

| Entry | Question answered | Boundary |
|---|---|---|
| `g.Topology()` | Which nodes, keys, ports and child trees are declared? | Static structure; creates no execution |
| `e.Snapshot()` | Where is this run now? | Independent of Observer; emits no events and contains no business values |
| `MemorySink.Snapshot()` | Which records has the sink collected? | A shallow copy of the record slice, not an execution-state snapshot |

Execution state changes before callbacks, so a callback can read `e.Snapshot()`. Its Graph.Execution / Graph.Path match the callback Site and Record attribution. A waiting node with pending inputs is waiting for those inputs. If all inputs have arrived, or there are no inputs, it awaits an opportunity to enter Run; this alone does not prove it is waiting for a concurrency quota.

Snapshot samples the definition, node state and slots separately; it is not an atomic view of one instant. Child is the live child Execution from the latest created attempt, retained after failure or cancellation. Calling Child.Snapshot samples another instant. The graph becomes terminal during Wait finalization; Start alone leaves it running.

Use `g.Topology().Mermaid(pulse.WithMermaidSnapshot(e.Snapshot()))` to overlay current state on static structure. Without a snapshot it draws structure only. Fixed values and error text do not enter the Mermaid diagram.

## Choosing and implementing a Sink

| Sink | Suitable for | Lifecycle and error checks |
|---|---|---|
| `NewLineSink(w, opts...)` | Human-readable columnar text | Buffered; check Flush at the end and Err during a run if needed |
| `SlogSink{Logger: logger}` | Existing host loggers, JSON and log collectors | A nil Logger uses slog.Default; the host manages the handler lifecycle |
| `&MemorySink{}` | Assertions, demonstrations and event collection | Read with Len / Snapshot; records remain in memory |
| `MultiSink{sink1, sink2}` | Sending one record to multiple sinks | Invokes members in order, skipping nil; the host flushes or closes leaf sinks separately |
| `NewAsyncSink(inner, opts...)` | Moving slower sink writes to one background worker | Check Flush(ctx) / Close(ctx), and inspect Dropped |

Sink requires only one method:

```go
type Sink interface {
	Write(Record)
}
```

An implementation must be concurrency-safe and should not block for long. The interface carries neither a context nor an error result. Implementations needing deadlines, buffering or error reporting must provide their own methods or internal queue. An ordinary Observer panic does not become a node failure, so the host must check sink errors independently.

Reference ownership follows these rules:

- A producer constructs independent Attrs for each record and does not mutate the record after handing it to Write.
- Sinks consume Record and Attrs read-only. MultiSink passes the same Attrs to its members.
- An asynchronous implementation copies the reference fields it needs before retaining a record; the built-in AsyncSink copies Attrs entries.
- MemorySink.Snapshot copies only the outer record slice. Continue treating the contained Attrs as read-only.

### LineSink options

| Option | Default / behavior |
|---|---|
| `WithBufSize(n)` | 32 KiB; n must be greater than 0 |
| `WithImmediate()` | Equivalent to a 1-byte threshold; each record immediately reaches the writer |
| `WithPrefix(text)` | Defaults to PULSE; an empty string disables the prefix |
| `WithColor(on)` | If unspecified, ANSI color is enabled only for an os.File character device |
| `WithRenderer(fn)` | Replaces the line-body renderer; fn must not be nil |

The default layout emits timestamp, status, duration with units, event, source, attributes, host, error and trace, in that order. Status width is a minimum, with no truncation. Attributes retain insertion order, and text values are quoted when needed. Color follows errors and duration, without inferring business meaning from free-form status text. Default rendering reuses its buffer and does not pass through slog. Allocation in a custom renderer depends on its implementation.

A nil writer, invalid buffer size or nil renderer is a construction-time programming error and panics. LineSink serializes writer calls, so the writer needs no additional concurrency support for this Sink. Err and Flush return the first write error. Failed batches are cleared to prevent unbounded accumulation. Flush writes the buffer; it does not close the underlying writer.

## AsyncSink reference

### Queue and backpressure

| Option / method | Meaning |
|---|---|
| `WithCapacity(n)` | Preallocated bounded queue; defaults to 1024 records, n must be greater than 0 |
| Default full-queue policy | Blocks the producer, retaining backpressure |
| `DropOnFull()` | Drops new records when full and counts them in Dropped |
| `Dropped()` | Combined count of undelivered records: queue overflow, writes after closure, blocked writers awakened by Close, remaining records after a close timeout, and inner.Write panics |

NewAsyncSink panics for a nil inner sink or invalid capacity. One worker invokes inner.Write in FIFO order and serializes inner flushing and error inspection. AsyncSink does not concurrently invoke Write / Flush / Err on its inner sink.

### Flush and Close

| Call | Waits for | Result of cancellation |
|---|---|---|
| `Flush(ctx)` | The enqueue snapshot at the call, including in-flight records, then an inner flush | Returns ctx.Err; records remain and the worker continues |
| `Close(ctx)` | Stops acceptance, drains, flushes and stops the worker | A timeout of the first call determines the final close result; queued leftovers count as Dropped and the worker exits once any inner call can return |

Later writes do not enlarge an earlier Flush boundary. Waiters with the same target share a flush task; distinct snapshots are registered in target order. Cancellation removes unstarted boundaries with no remaining waiters. There are at most capacity+1 boundaries, and Flush creates no additional goroutine per waiter.

At each boundary the worker pauses consumption to call the inner sink's optional Flush() error, or, if absent, its optional Err() error. It does not call both. Wrapping LineSink therefore writes its own buffer; handing records to inner.Write alone does not prove that an inner buffer has been written out. MultiSink has no flush method, so its leaf sinks still need separate host-managed flushing.

Close wakes producers waiting for queue space. Those records, and new Write calls after closure, count as Dropped without panicking. The first Close context controls draining and the final result. Once determined, repeated Close calls return that result. Until then, concurrent callers may cancel their own wait without changing the first close result. Flush after closure waits for the same close result.

The context also bounds waiting for inner Flush / Err, but cannot forcibly interrupt inner.Write / Flush / Err methods that have no context parameter. Cancellation can return immediately while the existing worker finishes an inner call. If that call never returns, the worker cannot exit.

### Inner panics

| Source | Handling |
|---|---|
| inner.Write panic | Recovered; the record counts as Dropped and the worker continues |
| inner.Flush / Err panic | Returns the fixed error `observe: AsyncSink inner Flush/Err panicked`, without formatting the panic payload |

A Flush / Err panic does not count records already handed to inner.Write as Dropped. If the panic payload is an error, Unwrap preserves it for errors.Is / errors.As. The fixed Error text does not call payload.Error, so reporting remains safe even if that method panics. The worker can still process later records.

Explicitly check Flush or Close on shutdown; the queue does not survive process exit. Use Close when the worker must stop.

## Custom line-body rendering

LineRenderer has the shape `func(dst []byte, r Record, color bool) []byte`. Install it with WithRenderer to append only the line body. LineSink continues to own the prefix, newline, color decision, buffering, write timing and error collection.

The six exported primitives use the same encoding rules as the built-in layout:

| Primitive | Purpose and boundary |
|---|---|
| `AppendDuration(dst, d)` | Appends duration with magnitude-selected ns, µs, ms or s; one decimal for µs, two for ms / s. Zero yields `0ns`; the built-in missing-duration placeholder is a separate layout decision |
| `AppendTextValue(dst, text)` | Quotes empty strings, spaces, equals signs, quotes and control characters as needed using Go string literals; an empty string is `""`, not a missing value |
| `AppendAttrs(dst, attrs)` | Appends k=v entries in insertion order with single spaces and no leading separator; an empty group emits zero bytes |
| `AppendAttrsExcept(dst, attrs, keys...)` | Same encoding while excluding attributes already placed in columns; nonexistent keys have no effect, and excluding everything emits zero bytes |
| `AppendPadding(dst, n)` | Appends n spaces; n <= 0 appends nothing |
| `DisplayWidth(text)` | Approximates terminal display columns for Padding; common East Asian characters use two columns, combining and zero-width characters use zero |

The corresponding fragments are byte-identical to the built-in layout. DisplayWidth uses an approximate table of common Unicode ranges; uncommon wide characters or emoji may differ from the actual terminal.

The renderer runs under the Sink's internal lock. Do not call that Sink's Write / Flush / Err from it or block for long. Check Get[T]'s ok to distinguish a missing attribute from a real zero value. The host layout supplies Attrs separators. With AppendAttrsExcept, use the actual appended length to detect an empty group; Attrs.Len alone is insufficient. Individual scalar columns can use the text primitive or strconv.AppendInt / AppendFloat(..., 'g', -1, 64) / AppendBool to match attribute encoding.

See the complete runnable [`Example_hostRenderer`](linelog_example_test.go):

```bash
go test -run '^Example_hostRenderer$' ./observe
```

## Common questions

| Question | Check or action |
|---|---|
| Run returned but the terminal shows no logs? | LineSink may still be below its buffer threshold; check Flush or use WithImmediate |
| Why does an ordinary node have only two Records? | The three node callbacks handle timing and finalization; records usually represent waiting-finished and execution-finished |
| Why are there no business timing records for a node? | Check for skipped inputs, an Aspect short circuit or checkpoint restoration; use the event table to identify the actual phases |
| Why does Run succeed when the sink fails? | Observer is a read-only observation boundary; check LineSink.Err / Flush, AsyncSink.Flush / Close, and Dropped |
| How can I inspect which inputs are still awaited? | Retain the Execution and read node Inputs and their arrival states from Snapshot |
| How do I correlate and distinguish repeated runs? | Execution distinguishes runs; TraceID supplies host business correlation and does not replace execution identity |

## Related documentation

- [Graph observation guide](https://luo-root.github.io/pulse/en/guide/observability)
- [`ObserveConfig`](config.go), [`Record / Attrs / Sink`](record.go), [event adapter](graph_record.go)
- [LineSink and encoding primitives](linelog.go), [SlogSink / MemorySink](sink.go), [AsyncSink](async.go)
- [Execution snapshots](../snapshot.go), [Observer and Site](../observer.go)
- The observation sections in the [design document](../docs/design/pulse.md)
