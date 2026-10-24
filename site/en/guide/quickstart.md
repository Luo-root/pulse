# Quick start

Build a workflow that turns a document list into a summary and then a report. By the end, you will know how to declare data, add nodes, supply input, run the workflow, and read its result.

## Before you begin

The example uses Pulse v0.5.1 and requires Go 1.26.0+. See [installation and versions](installation.md).

Initialize a module in a new directory:

```sh
mkdir pulse-quickstart
cd pulse-quickstart
go mod init example.com/pulse-quickstart
go get github.com/Luo-root/pulse@v0.5.1
```

Run all subsequent commands in that directory.

## 1. Create the program

Create `main.go` with this complete source:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/Luo-root/pulse"
)

func main() {
	documents := pulse.NewKey[[]string]("documents")
	summary := pulse.NewKey[string]("summary")
	report := pulse.NewKey[string]("report")

	g, err := pulse.New("documents", "Build a document report")
	check(err)
	check(g.DeclareInput(pulse.NewPort(documents)))
	check(g.DeclareOutput(pulse.NewPort(report)))

	check(g.Add(pulse.NewNode("summarize", "Summarize the document list",
		pulse.Requires(documents), pulse.Provides(summary),
		func(rc *pulse.RunCtx) error {
			docs, err := pulse.Get(rc, documents)
			if err != nil {
				return err
			}
			text := fmt.Sprintf("%s (%d documents)", strings.Join(docs, ", "), len(docs))
			return pulse.Set(rc, summary, text)
		})))

	check(g.Add(pulse.NewNode("format", "Build the final report",
		pulse.Requires(summary), pulse.Provides(report),
		func(rc *pulse.RunCtx) error {
			text, err := pulse.Get(rc, summary)
			if err != nil {
				return err
			}
			return pulse.Set(rc, report, "Report: "+text)
		})))

	var in pulse.Inputs
	check(pulse.Seed(&in, documents, []string{"alpha", "beta"}))
	e, err := g.NewExecution(context.Background(), in)
	check(err)
	check(e.Start())
	check(e.Wait())

	value, state, err := pulse.ReadOutput(e, report)
	check(err)
	if state != pulse.SlotStateReady {
		log.Fatalf("expected a report, got %s", state)
	}
	fmt.Println(value)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
```

The `check` helper reports an error and exits this standalone program. Node functions return errors to Pulse, which handles execution failure. A service should return assembly and execution errors to its caller.

## 2. Run and inspect the result

```sh
go run .
```

Expected output:

```text
Report: alpha, beta (2 documents)
```

The dependency chain is `documents → summarize → summary → format → report`. The `format` node can process data only after `summary` arrives. `Requires` / `Provides` determine that order, independently of the order of `Add` calls.

## 3. Understand the steps

### Declare data and ports

`NewKey[[]string]("documents")` creates a typed key. Its name identifies the data; its type determines the arguments to `Get` and `Set`. Keys with the same name and type identify the same data item.

`DeclareInput` allows the host to supply `documents`. `DeclareOutput` allows the host to read `report`. The intermediate `summary` stays internal and needs no port declaration.

### Add processing nodes

`NewNode` accepts an ID, description, input declarations, output declarations, and function. The function uses `RunCtx` to read declared inputs and write declared outputs. Each key has one source; a second producer is rejected during assembly.

### Supply this run's input

`Seed` writes a value into `Inputs`, without changing the definition. Create independent inputs and a new `Execution` for the next request.

Use `SkipSeed` when an input is explicitly absent in this run. A missing required inlet is rejected at execution creation or startup, rather than leaving a node waiting indefinitely. [Inputs and outputs](inputs-outputs.md) explains optional inputs and result states.

### Create and finish the execution

`NewExecution` creates the handle. `Start` validates and begins work. `Wait` waits for every node and finalizes the result. A successful start seals the entire definition tree. You can run a sealed definition again, but cannot add nodes or change ports.

`ReadOutput` reads a public outlet after `Wait` finalizes. Check its error before its state: `ready` carries a value, `skipped` is normal absence, and an optional pass-through outlet can remain `pending`. This example requires a report, so it checks for `ready` explicitly.

## 4. Run with different inputs

Keep the definition and create new inputs and an execution. Append this fragment to the end of `main` above:

```go
var another pulse.Inputs
check(pulse.Seed(&another, documents, []string{"gamma"}))
ex, err := g.NewExecution(context.Background(), another)
check(err)
check(ex.Start())
check(ex.Wait())
nextReport, nextState, err := pulse.ReadOutput(ex, report)
check(err)
fmt.Println(nextReport, nextState)
```

Additional output:

```text
Report: gamma (1 documents) ready
```

This second execution has its own slots, state, and cancellation. Do not start the same Execution again; the Graph needs no reset.

## Troubleshooting

**Startup reports an input or source error.** Check that `DeclareInput`, `Requires`, and `Seed` use a key with the same name and type, and that no internal node also produces that key.

**`ReadOutput` reports an undeclared key.** The host can read only declared outlets. Call `DeclareOutput` for a host-facing result before starting.

**You want logs or progress.** Continue to [observation](observability.md). Use `Snapshot` for state and declared outlets for business results.

## Next steps

- [Core concepts](concepts.md): definitions, executions, and the three data states.
- [Branches and joins](branching.md): conditional outputs and aggregation policies.
- [Subgraphs](subgraphs.md): reuse a complete workflow as one step.
