# Quick start

Pulse is a **data-arrival-driven graph engine** plus a **graph-observation layer**. This page walks the shortest path end to end: **declare Keys and ports → add nodes → create an Execution → `Start` / `Wait` → `ReadOutput` → attach observation**.

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
	g, err := pulse.New("docs-pipeline", "docs summary pipeline")
	if err != nil {
		panic(err)
	}

	// DeclareInput: declare docs as an inlet of this graph — declare it before feeding it.
	must(g.DeclareInput(pulse.NewPort(Docs)))

	// Seed: write this run's value into Inputs — a key must be declared an inlet before it is fed.
	var in pulse.Inputs
	must(pulse.Seed(&in, Docs, []string{"pulse only orchestrates and observes", "slot tri-state: skip is arrival"}))

	// summarize enters Run only after docs has arrived.
	must(g.Add(pulse.NewNode("summarize", "fold docs into a summary",
		pulse.Requires(Docs),
		pulse.Provides(Summary),
		func(rc *pulse.RunCtx) error {
			docs, err := pulse.Get(rc, Docs)
			if err != nil {
				return err
			}
			return pulse.Set(rc, Summary, fmt.Sprintf("%d segments / %d chars", len(docs), len([]rune(strings.Join(docs, "")))))
		})))

	must(g.Add(pulse.NewNode("report", "wrap the summary into a report",
		pulse.Requires(Summary),
		pulse.Provides(Report),
		func(rc *pulse.RunCtx) error {
			summary, err := pulse.Get(rc, Summary)
			if err != nil {
				return err
			}
			return pulse.Set(rc, Report, "Report: "+summary)
		})))
	// Declare a public outlet; read from this Execution without a shared result closure.
	must(g.DeclareOutput(pulse.NewPort(Report)))

	e, err := g.NewExecution(context.Background(), in)
	must(err)
	must(e.Start())
	must(e.Wait())
	report, state, err := pulse.ReadOutput(e, Report)
	must(err) // With a non-nil error, both report and state are invalid.
	switch state {
	case pulse.SlotStateReady:
		fmt.Println(report)
	case pulse.SlotStateSkipped:
		fmt.Println("no report this run")
	case pulse.SlotStatePending:
		fmt.Println("optional pass-through output was not supplied")
	}
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

This code separates the definition, one run and result reading:

| What you write | What it is |
|---|---|
| `pulse.NewKey[T]("docs")` | **Key**: a typed data slot. The name is for diagnostics and YAML reconciliation; the type gives compile-time safety. The same name must always be registered with the same `T` |
| `g.DeclareInput(...)` + `pulse.Seed(&in, Docs, …)` | **Inlet + Seed**: `DeclareInput` says "this key comes from outside", `Seed` writes **this run's** value into `Inputs` — each Key allows only one source (a declared inlet, or exactly one node's `Provides`; both producing it → `ErrDuplicateSource`) |
| `pulse.NewNode(id, description, Requires, Provides, run)` | **Node**: declares only which slots it reads and which it writes. It does **not** declare who the next node is |
| `g.NewExecution(ctx, in)` + `e.Start()` / `e.Wait()` | **Graph + Execution**: the graph is the definition, the execution is this run; `Wait` waits for all nodes to terminate and returns the first error or real cancellation before finalization, **excluding normal skips**; bodies must still exit cooperatively |
| `g.DeclareOutput(...)` + `pulse.ReadOutput(e, Report)` | **Outlet + result**: read only this run's declared public outputs; ready, normal skipped and an unfed optional pass-through pending are normal states, while a non-nil error invalidates both value and state |

## Data arrival is scheduling

- A node blocks on its input slots in **its own goroutine** and enters `Run` the moment the input arrives — you do not order nodes, and there is no topological-sort step;
- The topology is **implicit**: whoever writes `summary` and whoever reads `summary` already form the dependency. There are no edge objects and no scheduler loop;
- `Requires` is an **AND** precondition: the gate is judged once all inputs have arrived (ready or skipped) — **collect whatever arrives**, so one input with a value is enough to execute (only when no input brought a value does it not execute — see [Core concepts](/en/guide/concepts)).

## A reusable definition, one run, one world

A `Graph` is a **reusable static definition** (nodes, dependencies, ports, mounted child graphs), not a run itself. This is an external contract, not an implementation detail:

- one definition can run many times sequentially, many times concurrently, and be mounted as a child graph in several places; one run's cancellation / failure / completion pollutes neither the definition nor any other run;
- every call to `g.Run(ctx, in)` is **an independent run** (inside: `NewExecution` + `Start` + `Wait`); for step-by-step control build an `Execution` yourself — its `Start()` succeeds only once, and once a slot has arrived it is closed and never reopens;
- cross-run state (history, caches, sessions) **belongs to the caller**; the engine holds none of it. The reasoning is in [Core concepts](/en/guide/concepts).

## Add observation: three lines

```go
sink := observe.NewLineSink(os.Stdout, observe.WithImmediate())
defer sink.Flush()

obs, err := observe.NewRecordObserver(observe.ObserveConfig{
	Sink:    sink,
	HostID:  "quickstart",
	TraceID: observe.NewTraceID(),
})
g, err := pulse.New("docs-pipeline", "docs summary pipeline", pulse.WithObserver(obs))
```

With those three lines added, the real output of the same graph (two run-level records bracketing four node records; timestamps / trace / durations vary per run):

```text
PULSE | 2026/10/09 - 10:42:50.372 | running    |         - | pulse.graph_started | source=observe | pulse.graph=docs-pipeline | host=quickstart | trace=1791513770372309700-d47dc250-1
PULSE | 2026/10/09 - 10:42:50.388 | running    |         - | pulse.node_wait_finished | source=observe | pulse.graph=docs-pipeline pulse.node=summarize | host=quickstart | trace=1791513770372309700-d47dc250-1
PULSE | 2026/10/09 - 10:42:50.388 | completed  |         - | pulse.node_run_finished | source=observe | pulse.graph=docs-pipeline pulse.node=summarize | host=quickstart | trace=1791513770372309700-d47dc250-1
PULSE | 2026/10/09 - 10:42:50.388 | running    |         - | pulse.node_wait_finished | source=observe | pulse.graph=docs-pipeline pulse.node=report | host=quickstart | trace=1791513770372309700-d47dc250-1
PULSE | 2026/10/09 - 10:42:50.388 | completed  |         - | pulse.node_run_finished | source=observe | pulse.graph=docs-pipeline pulse.node=report | host=quickstart | trace=1791513770372309700-d47dc250-1
PULSE | 2026/10/09 - 10:42:50.388 | completed  |   15.80ms | pulse.graph_finished | source=observe | pulse.graph=docs-pipeline | host=quickstart | trace=1791513770372309700-d47dc250-1
```

The engine also emits two run-level callbacks per run (start / finish), and `observe` folds them into **two run-level records**: `pulse.graph_started` precedes every node record and `pulse.graph_finished` follows all of them, its `Status` being the run's terminal state (skipping is a node-level fact, so a skipped node does **not** make the run fail). The two node records are **segmented timing** — the wait segment (how long the node spent waiting for input) and the run segment (how long `Run` took): a wait record's `Status` is always `running` and its `Duration` is the wait itself (`summarize`'s input is written by `Seed` up front, hence `-`), while a run record's `Status` is the finish reason and its `Duration` is `Run` itself.

Neither node spent any time of its own here (their segments were all rounded to `-`), yet the run-level record reports `15.80ms`: its window covers "commit the nodes → everything terminated", which includes runtime and egress-write costs — for its accounting and how it differs from your own stopwatch see [Graph observation](/en/guide/observability).

## Next steps

- **Core concepts**: Key / Node / Graph / slot tri-state / aspects → [Core concepts](/en/guide/concepts)
- **Orchestration**: branching, fan-in, timeouts, retries, rate limiting → [Orchestration](/en/guide/orchestration)
- **Declarative assembly**: topology belongs to YAML → [Declarative assembly](/en/guide/assembly)
- **Graph observation**: egress choice, host-provided columns, privacy boundary → [Graph observation](/en/guide/observability)
- **Per-package API**: full documentation of all three packages (same source as the repository README) → [Packages](/en/packages/)
