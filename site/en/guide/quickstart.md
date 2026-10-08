# Quick start

Pulse is a **one-shot graph engine** plus a **graph-observation layer**. This page walks the shortest path end to end: **declare Keys → add nodes → `Run` → attach observation**.

## Requirements

- **Go 1.25.0+** (toolchain auto-downloads if missing)
- No other dependencies: the engine uses only the standard library; the examples on this page need only the root package

## Install

```bash
go get github.com/Luo-root/pulse
```

## A minimal graph

```go
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/Luo-root/pulse"
)

var (
	Docs    = pulse.NewKey[[]string]("docs")
	Summary = pulse.NewKey[string]("summary")
	Report  = pulse.NewKey[string]("report")
)

func main() {
	g, err := pulse.New(context.Background(), "docs-pipeline")
	if err != nil {
		panic(err)
	}

	// Seed writes external input before the run: it declares the host as the source of docs.
	_ = pulse.Seed(g, Docs, []string{"pulse only orchestrates and observes", "slot tri-state: skip is arrival"})

	// summarize enters Run only after docs has arrived.
	must(g.Add(pulse.NewNode("summarize",
		pulse.Requires(Docs),
		pulse.Provides(Summary),
		func(rc *pulse.RunCtx) error {
			docs, err := pulse.Get(rc, Docs)
			if err != nil {
				return err
			}
			return pulse.Set(rc, Summary, fmt.Sprintf("%d segments / %d chars", len(docs), len([]rune(strings.Join(docs, "")))))
		})))

	// report takes the result itself: the Graph has no public slot read after Run, so outputs belong to the caller.
	var report string
	must(g.Add(pulse.NewNode("report",
		pulse.Requires(Summary),
		pulse.Provides(Report),
		func(rc *pulse.RunCtx) error {
			summary, err := pulse.Get(rc, Summary)
			if err != nil {
				return err
			}
			report = "Report: " + summary
			return pulse.Set(rc, Report, report)
		})))

	if err := g.Run(); err != nil {
		panic(err)
	}
	fmt.Println(report)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
```

Save it as `main.go`, then `go run ./main.go`:

```text
Report: 2 segments / 67 chars
```

All four basic concepts appear in this code:

| What you write | What it is |
|---|---|
| `pulse.NewKey[T]("docs")` | **Key**: a typed data slot. The name is for diagnostics and YAML reconciliation; the type gives compile-time safety. The same name must always be registered with the same `T` |
| `pulse.Seed(g, Docs, …)` | **Seed**: external input written before the run — a seed and a node are both sources for a Key, but each Key allows only one source (both producing it → `ErrDuplicateSource`) |
| `pulse.NewNode(id, Requires, Provides, run)` | **Node**: declares only which slots it reads and which it writes. It does **not** declare who the next node is |
| `g.Run()` | **Graph**: submits all nodes and blocks until all have terminated; returns the first error, **excluding skips** |

## Data arrival is scheduling

- A node blocks on its input slots in **its own goroutine** and enters `Run` the moment the input arrives — you do not order nodes, and there is no topological-sort step;
- The topology is **implicit**: whoever writes `summary` and whoever reads `summary` already form the dependency. There are no edge objects and no scheduler loop;
- `Requires` is an **AND** precondition: all inputs must arrive before the node executes (if any one is skipped the whole node does not execute — see [Core concepts](/en/guide/concepts)).

## One run, one world

A `Graph` is **one instantiation of a template**, not a re-runnable container. This is an external contract, not an implementation detail:

- A second call to `Start()` returns `ErrGraphStarted`;
- `Seed` after the graph has started is rejected the same way; once a slot has arrived it is closed and never reopens;
- To run the same graph a second time, the correct move is to call `New` again — just as a CI/CD workflow definition is run countless times, each one an independent run.

Cross-run state (history, caches, sessions) **belongs to the caller**; the engine holds none of it. The reasoning is in [Core concepts](/en/guide/concepts).

## Add observation: three lines

```go
sink := observe.NewLineSink(os.Stdout, observe.WithImmediate())
defer sink.Flush()

obs, err := observe.NewRecordObserver(observe.ObserveConfig{
	Sink:    sink,
	HostID:  "quickstart",
	TraceID: observe.NewTraceID(),
})
g, err := pulse.New(ctx, "docs-pipeline", pulse.WithObserver(obs))
```

With those three lines added, the real output of the same graph (two records per node; timestamps / trace / durations vary per run):

```text
PULSE | 2026/10/08 - 16:43:59.485 | running    |         - | pulse.node_wait_finished | source=observe | pulse.graph=docs-pipeline pulse.node=summarize | host=quickstart | trace=1791449039485090000-7a4ee798-1
PULSE | 2026/10/08 - 16:43:59.501 | completed  |   16.53ms | pulse.node_run_finished | source=observe | pulse.graph=docs-pipeline pulse.node=summarize | host=quickstart | trace=1791449039485090000-7a4ee798-1
PULSE | 2026/10/08 - 16:43:59.501 | running    |   16.53ms | pulse.node_wait_finished | source=observe | pulse.graph=docs-pipeline pulse.node=report | host=quickstart | trace=1791449039485090000-7a4ee798-1
PULSE | 2026/10/08 - 16:43:59.501 | completed  |         - | pulse.node_run_finished | source=observe | pulse.graph=docs-pipeline pulse.node=report | host=quickstart | trace=1791449039485090000-7a4ee798-1
```

The engine emits at most three callbacks per node, and `observe` folds them into **two segmented timing records** — the wait segment (how long the node spent waiting for input) and the run segment (how long `Run` took): a wait record's `Status` is always `running` and its `Duration` is the wait itself (`summarize`'s input is written by `Seed` up front, hence `-`; `report` waited 16.53ms for `summary`), while a run record's `Status` is the finish reason and its `Duration` is `Run` itself. For the semantics of the duration column see [Graph observation](/en/guide/observability).

## Next steps

- **Core concepts**: Key / Node / Graph / slot tri-state / aspects → [Core concepts](/en/guide/concepts)
- **Orchestration**: branching, fan-in, timeouts, retries, rate limiting → [Orchestration](/en/guide/orchestration)
- **Declarative assembly**: topology belongs to YAML → [Declarative assembly](/en/guide/assembly)
- **Graph observation**: egress choice, host-provided columns, privacy boundary → [Graph observation](/en/guide/observability)
- **Per-package API**: full documentation of all three packages (same source as the repository README) → [Packages](/en/packages/)
