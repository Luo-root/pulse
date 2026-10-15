# Observation and diagnostics

This page is for developers inspecting progress, recording outcomes, or diagnosing flow problems. It requires Go 1.25+ and familiarity with [inputs and outputs](./inputs-outputs.md). Examples target the development version.

Pulse offers three complementary views: `Topology` inspects the definition, `Snapshot` reads one execution's current state, and `Observer` receives lifecycle callbacks. `ReadOutput` separately provides business results. Structure and progress views contain no business values.

## Complete example: record two independent runs

The program reuses one graph twice. Each run creates a fresh observer configuration and TraceID, keeping request correlation separate from execution identity. Save it as `main.go` in an empty directory:

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/observe"
)

func run() error {
	result := pulse.NewKey[int]("result")
	g, err := pulse.New("observe-demo", "Record independent executions")
	if err != nil {
		return err
	}
	if err := g.DeclareOutput(pulse.NewPort(result)); err != nil {
		return err
	}
	if err := g.Add(pulse.NewNode("answer", "Produce the answer", nil,
		pulse.Provides(result), func(rc *pulse.RunCtx) error {
			return pulse.Set(rc, result, 42)
		})); err != nil {
		return err
	}
	for i := 1; i <= 2; i++ {
		sink := &observe.MemorySink{}
		obs, err := observe.NewRecordObserver(observe.ObserveConfig{
			Sink: sink, HostID: "docs", TraceID: observe.NewTraceID(),
		})
		if err != nil {
			return err
		}
		e, err := g.NewExecution(context.Background(), pulse.Inputs{},
			pulse.WithExecObserver(obs))
		if err != nil {
			return err
		}
		defer e.Cancel()
		if err := e.Start(); err != nil {
			return err
		}
		if err := e.Wait(); err != nil {
			return err
		}
		value, state, err := pulse.ReadOutput(e, result)
		if err != nil {
			return err
		}
		progress := e.Snapshot()
		fmt.Printf("run=%d status=%s value=%d state=%s records=%d\n",
			i, progress.Graph.Status, value, state, sink.Len())
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
```

Run in that directory:

```sh
go mod init example.com/pulse-observation
go get github.com/Luo-root/pulse@main
go run .
```

Expected output:

```text
run=1 status=completed value=42 state=ready records=4
run=2 status=completed value=42 state=ready records=4
```

Each run records graph start, node wait completion, node run completion, and graph finish. Graph IDs match across runs; Execution IDs and TraceIDs differ. When Wait returns, the graph finish callback has also returned. An asynchronous sink still needs host Flush or Close to drain actual delivery.

## Identify the definition, execution, and mount

Every engine callback carries `pulse.Site`:

| Field | Meaning | Record attribute |
|---|---|---|
| `Graph` | Definition identity supplied to New | `pulse.graph` |
| `Execution` | This execution's identity, unique per instance within the process | `pulse.execution` |
| `Path` | Mount position; empty at root, nested as `outer/inner` | `pulse.path` when nonempty |

Two mounts of one child may share Graph but have different Path and Execution values. Retry at one mount creates another child Execution ID. An Execution ID is not a durable RunID across processes.

Children inherit observers by default. One sink can serve the complete execution tree without an observer per layer. `ObserveConfig.Path` is only a fallback when Site has no path and is normally empty for engine runs. A nonempty fallback also applies to root records.

## Inspect progress and Mermaid

This fragment belongs in a host program with existing `g`, `e`, and the `fmt` import:

```go
progress := e.Snapshot()
fmt.Println(progress.Graph.Status)
fmt.Println(g.Topology().Mermaid())
fmt.Println(g.Topology().Mermaid(pulse.WithMermaidSnapshot(progress)))
```

Topology includes all declared ports, nodes, binds, the static child tree, and `Problems`. Reading does not seal definitions and can report problems during incomplete assembly. Mermaid derives its structure from Topology and adds no execution state without a snapshot.

Snapshot nodes follow declaration order. A waiting node with pending inputs is waiting for data. If all inputs arrived, it is waiting for a chance to enter its body, possibly due to quota or aspects. This does not identify a specific queue.

A snapshot is not an atomic sample of every layer and slot, and it contains no business values. A running node may have ready outputs while others remain pending. Unpublished Retry staging does not appear in snapshots.

`NodeState.Child` is the latest child execution already created. It remains available after failure or cancellation and is nil when no child was created. Calling its Snapshot samples another time. Mermaid with a snapshot matches states by graph ID, Path, and node ID and samples children separately. Fixed values and error text are excluded from the diagram.

## Choose a record sink

`observe.NewRecordObserver` converts engine callbacks into `Record` values delivered through `Sink.Write(Record)`.

| Sink | Use | Finalization |
|---|---|---|
| `MemorySink` | Tests, demonstrations, assertions | Snapshot reads collected records |
| `LineSink` | Terminal or text logs | Check Flush errors |
| `SlogSink` | Existing slog logger, text or JSON | Follow the host handler's policy |
| `MultiSink` | Several destinations | Finalize each destination |
| `AsyncSink` | Bounded queue around a slow sink | Context-aware Flush/Close |

This helper demonstrates LineSink finalization. It needs `context`, `errors`, `os`, `pulse`, and `observe`; `g` is an assembled graph:

```go
func runLogged(ctx context.Context, g *pulse.Graph, in pulse.Inputs) error {
	sink := observe.NewLineSink(os.Stdout)
	obs, err := observe.NewRecordObserver(observe.ObserveConfig{
		Sink: sink, HostID: "app", TraceID: observe.NewTraceID(),
	})
	if err != nil {
		return err
	}
	runErr := g.Run(ctx, in, pulse.WithExecObserver(obs))
	return errors.Join(runErr, sink.Flush())
}
```

Observer callbacks are synchronous on the execution path, so slow sinks extend wall-clock durations. AsyncSink moves delivery to a worker and backpressures when its queue is full by default. `DropOnFull` permits dropping; inspect Dropped. Flush waits for the enqueue boundary captured at the call, so later writes do not extend it. Close stops acceptance and performs final draining. Canceling the wait cannot forcibly stop a blocking inner operation that accepts no context.

## Records, callbacks, and recovery

A normally completed node usually produces a wait segment and a run segment. All-skipped inputs produce no run segment. A body returning NoValue still has a run segment, with skipped status. An aspect failing before waiting produces `pulse.node_finished`, with zero duration because timing was not measured. Exact callback counts, order, and the event table are in the [lifecycle reference](../reference/lifecycle.md).

Observer has no error return. Its panics are isolated and do not change node results. Check delivery failures through the sink's own error interface. Do not call Start or Wait on the same execution inside a callback; it runs on that execution's call path and may wait for itself.

A confirmed restored node emits Finished once without repeating its body, aspects, or Commit. It invents no Waiting, Running, or lifecycle events for children that were never created. RecordObserver does not generate business timing records from this lone completed/skipped Finished. Durable reports restoration through its own recovery events and persistent records.

## Attributes and data boundaries

Business dimensions belong in `Attrs`, with names such as `<component>.<field>`. Values support string, int64, float64, bool, and named versions of these types. Record fields do not store arbitrary business objects. Strings and errors can still contain sensitive data; hosts should supply processed attributes and error summaries.

The host generates a TraceID for one run or extracts it from incoming tracing data. Do not reuse it across unrelated requests. `observe.NewTraceID` does not produce a W3C trace-id. Supply host trace IDs and span semantics when integrating with W3C or OTel.

## Common errors and next steps

When logs mix runs, group by Execution and TraceID. For missing child records, check observer overrides and paths. If the graph stays running after nodes finish, confirm the host called Wait. Do not use snapshots as recovery plans or result containers.

The [lifecycle reference](../reference/lifecycle.md) gives exact facts, and the [error reference](../reference/errors.md) gives follow-up actions. [YAML assembly](./assembly.md) also provides Analyze without business execution.
