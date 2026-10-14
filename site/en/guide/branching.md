# Branches and joins

This page is for developers who need to select processing routes and combine several results. It requires Go 1.25+ and familiarity with [inputs and outputs](./inputs-outputs.md). The example uses unreleased development APIs.

Branches are expressed through output values or skipped states. A join first waits for every input to arrive, then applies its policy to values and absent inputs. Normal skipping does not fail the run.

## Complete example: select and collect a route

Save this program as `main.go` in an empty directory. It chooses `left` or `right` from the input and collects the route with a value into a report.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/Luo-root/pulse"
)

func run() error {
	request := pulse.NewKey[string]("request")
	left := pulse.NewKey[string]("left")
	right := pulse.NewKey[string]("right")
	report := pulse.NewKey[string]("report")
	g, err := pulse.New("branch-demo", "Select and collect a route")
	if err != nil {
		return err
	}
	if err := g.DeclareInput(pulse.NewPort(request)); err != nil {
		return err
	}
	if err := g.DeclareOutput(pulse.NewPort(report)); err != nil {
		return err
	}
	if err := g.Add(pulse.NewNode("route", "Select a processing route",
		pulse.Requires(request), pulse.Provides(left, right),
		func(rc *pulse.RunCtx) error {
			value, err := pulse.Get(rc, request)
			if err != nil {
				return err
			}
			if strings.HasPrefix(value, "urgent:") {
				return pulse.Only(rc, left, strings.TrimPrefix(value, "urgent:"))
			}
			return pulse.Only(rc, right, value)
		})); err != nil {
		return err
	}
	if err := pulse.Join(g, "collect", pulse.Keys(left, right), report,
		func(rc *pulse.RunCtx, batch pulse.Batch[string]) (string, error) {
			return strings.Join(batch.Values(), ", "), nil
		}); err != nil {
		return err
	}
	if err := g.Validate(); err != nil {
		return err
	}
	var in pulse.Inputs
	if err := pulse.Seed(&in, request, "urgent:review"); err != nil {
		return err
	}
	e, err := g.NewExecution(context.Background(), in)
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
	value, state, err := pulse.ReadOutput(e, report)
	if err != nil {
		return err
	}
	fmt.Printf("report=%s state=%s\n", value, state)
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
```

Run these commands in the directory containing the file:

```sh
go mod init example.com/pulse-branches
go get github.com/Luo-root/pulse@1f67045d6c42
go run .
```

Expected output:

```text
report=review state=ready
```

`Only` writes the selected key and skips every other output of this node. `collect` sees `left=ready` and `right=skipped`, entering its callback after both arrive. `Values` returns only present values, so the report contains `review`.

Replace the input with `pulse.SkipSeed(&in, request)` and neither `route` nor `collect` enters its body. The report is `skipped` and the run still succeeds. An input of `""` remains a present value; zero values do not automatically skip.

## The two join decisions

“Collect what is present” is a processing policy, not permission to run on the first arriving value. Suppose a node requires `a`, `b`, and `c`:

| Input states | Body entered? | Explanation |
|---|---|---|
| `ready`, `pending`, `skipped` | No | `b` has not arrived |
| `ready`, `ready`, `skipped` | Yes | All arrived and at least one carries a value |
| `skipped`, `skipped`, `skipped` | No | The node itself skips normally |

For a strict join, explicitly require every input to have a value before processing. This callback can replace the `Join` callback in the complete example:

```go
func(rc *pulse.RunCtx, batch pulse.Batch[string]) (string, error) {
	if err := batch.WaitAll(); err != nil {
		return "", err
	}
	return strings.Join(batch.Values(), ", "), nil
}
```

The example uses an exclusive branch, so one input is always absent. With this strict callback, the report skips normally. That is the policy the caller declared. A plain `NewNode` can use `pulse.WaitAll(rc, pulse.Deps(pulse.Requires(a), pulse.Requires(b))...)` for the same check; `a` and `b` must be declared inputs of that node.

## Read a Batch and choose a branch operation

`Batch.Items` retains every input in declaration order, including absent items with `Present=false`. `Len` counts present values; `Values` returns those values; `Missing` returns skipped key names. Use `batch.Get(key)` for a particular source instead of guessing the origin of same-typed results by slice position.

`Batch.Get` returns `*SkipError` for a skipped item. Returning it skips the current node. Passing a key outside the batch returns `ErrUndeclared`, a calling error. For inputs of different types, use `NewNode` with `Deps` rather than forcing them into one `Batch[T]`.

| Operation | Use | Node outcome |
|---|---|---|
| `Only(rc, key, value)` | Select exactly one output of this node | `completed` when returning nil |
| `Set` and `Skip` | Write several outputs and mark absent ones explicitly | `completed` when returning nil |
| `NoValue()` | The entire node has no result this run | `skipped` |

Use `Only` in place of `Set` for that branch; do not write other outputs first. `Skip` affects one output only, and a node returning nil still completes. Unwritten outputs automatically skip, but explicit selection prevents accidentally omitting the necessary `Set`.

## Parallel instances: FanOut

At assembly time, `FanOut` creates `id-1` through `id-N` nodes from the output count. Shard numbers start at 1. Every instance reads the complete ordinary input value and chooses its work by shard number. It does not automatically divide a slice into pieces.

Place this fragment in an assembly function returning `error`, with existing `g`, `docs` (`Key[[]string]`), and `r1`, `r2` (`Key[string]`):

```go
if err := pulse.FanOut(g, "worker", docs, pulse.Keys(r1, r2),
	func(rc *pulse.RunCtx, shard int, values []string) (string, error) {
		if shard > len(values) {
			return "", pulse.NoValue()
		}
		return strings.ToUpper(values[shard-1]), nil
	}); err != nil {
	return err
}
```

An instance error can fail the execution; normal absence can be collected by a downstream `Join`. The instance count is fixed, and failed assembly leaves no partial batch of nodes. A channel input uses competing consumption; see [streams](./streams.md).

## Common errors and next steps

Do not treat `SkipError` as a retryable fault or turn a real failure into `NoValue`. Check a strict join before writing outputs. Ordinary outputs already published are not withdrawn when the node later returns a skip.

Read [retry, timeout, and cancellation](./retry-cancellation.md) to control attempts and time limits, or [static subgraphs](./subgraphs.md) to reuse a branching flow.
