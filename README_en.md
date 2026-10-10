[English](README_en.md) | [中文](README.md)

<div align="center">
  <a href="https://luo-root.github.io/pulse/">
    <img alt="Pulse" src=".github/assets/logo.svg" width="260" />
  </a>
</div>

<div align="center">
  <h3>A one-shot graph engine for Go — data-arrival scheduling, explicit failure.</h3>
</div>

<div align="center">
  <a href="https://go.dev/"><img alt="Go 1.25.0" src="https://img.shields.io/badge/Go-1.25.0-blue.svg" /></a>
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/License-MIT-green.svg" /></a>
  <a href="https://github.com/Luo-root/pulse/releases/tag/v0.4.0"><img alt="Release v0.4.0" src="https://img.shields.io/badge/release-v0.4.0-2563eb.svg" /></a>
  <a href="https://luo-root.github.io/pulse/en/"><img alt="Docs" src="https://img.shields.io/badge/docs-%E4%B8%AD%E6%96%87%20%7C%20English-2563eb.svg" /></a>
  <a href="docs/design/pulse.md"><img alt="Design doc" src="https://img.shields.io/badge/design-pulse.md-2563eb.svg" /></a>
</div>

<br />

**Pulse** is a one-shot graph engine plus a graph-observation layer. Nodes declare which keys they read and write; the topology is implied by data production and consumption. There are no edge objects, no topological sort, no scheduler loop.

Pulse is exactly two things:

| Package | What it is | Depends on |
|---|---|---|
| `pulse` (root) | the graph engine | **nothing** (standard library only) |
| `pulse/observe` | graph observation: folds the engine's `Observer` callbacks into structured records | `pulse` |
| `pulse/yaml` | declarative graph assembly: YAML → graph | `pulse` + `yaml.v3` |

The engine does not import any observation package — it only exposes an `Observer` seam. A host that does not need observation imports the root package alone.

## Install

```bash
go get github.com/Luo-root/pulse
```

## Quick start

```go
package main

import (
	"context"
	"fmt"

	"github.com/Luo-root/pulse"
)

var Docs = pulse.NewKey[[]string]("docs")
var Summary = pulse.NewKey[string]("summary")

func main() {
	g, err := pulse.New(context.Background(), "demo")
	if err != nil {
		panic(err)
	}
	_ = pulse.Seed(g, Docs, []string{"a", "b"})

	_ = g.Add(pulse.NewNode("summarize",
		pulse.Requires(Docs),
		pulse.Provides(Summary),
		func(rc *pulse.RunCtx) error {
			docs, err := pulse.Get(rc, Docs)
			if err != nil {
				return err
			}
			return pulse.Set(rc, Summary, fmt.Sprintf("%d docs", len(docs)))
		}))

	if err := g.Run(); err != nil {
		panic(err)
	}
	// Summary has arrived.
}
```

## Design in one screen

- **Data arrival is scheduling.** `Requires` is an AND precondition: the gate is judged once all inputs have arrived (ready or skipped); **collect whatever arrives** — as soon as one input arrived with a value the node enters `Run`, and only when no input brought a value does it skip itself.
- **Slots have three states**: `pending` | `ready(value)` | `skipped`. Ready and skipped are *both* arrival — skipping is not failure. Branching is written by calling `Skip` on the `Provide` you did not choose (and `Set` on the one you did); **both sides must speak**, since skipping one and forgetting to `Set` the chosen one auto-skips that output too, leaving both downstreams unexecuted.
- **Failure is explicit.** A node error records the first error and cancels the whole run; it is never rewritten as `ErrSkipped`.
- **One run, one world.** A `Graph` is one instantiation of a template, not a re-runnable container. Reusing the template means calling `New` again — like a CI/CD workflow definition being run many times, one run instance each. Cross-run state (history, caches, sessions) belongs to the caller, not to the engine.
- **Aspects wrap the whole "wait for input + execute" span**, so `Timeout` can interrupt a node that is still waiting for data.
- **The assembly sugar only collects names; it never changes semantics.** `pulse.FanOut` / `pulse.Join` pull "one input → N parallel instances" and "N routes of one type → one batch" into the function signature (`pulse.Keys(...)` + `pulse.Batch[T]`): every route carries its **source name** (`Batch.Items`, missing routes included), the whole batch can be made strict with `m.WaitAll()`, a single route with `m.Get(k)` (a skipped route yields `*SkipError`, an undeclared one `ErrUndeclared`), and both callbacks receive **this node's own `*RunCtx`** (long jobs watch `rc.Context()` for cancellation). `pulse.NoValue()` is a **node-level** skip — not the same thing as `Skip(rc, key)`, which skips one output slot; and `FanOut`'s N workers are validated and committed as **one atomic batch**, so an incomplete batch fails entirely and leaves no half fan-out on the graph. The graph the sugar builds and the graph you wire by hand yield **field-for-field identical observation records** (pinned by an equivalence-anchor test). **The compiler locks element types and arity, not the order of same-typed slots** (`Keys(a,b)` and `Keys(b,a)` both compile — measured) — taking a route by its `Key` is unaffected; that boundary is written down in the design doc. **Beyond wiring there are three more**: `pulse.Only(rc, k, v)` states an exclusive branch in one call (write this one, void every other output of the node — hand-written omissions leave *both* downstreams unrun while the run still reports success); `pulse.Produce` / `pulse.Consume` fold a stream's channel creation, close responsibility, cancellation response and loop into one call (the producer **stays alive until it is done**; the consumer uses `select`, not `for range` — the latter drains the buffer after cancellation and still reports success); and `pulse.Tee` **copies** one stream to N downstreams (plain values do not need it — one `Provides` with N `Requires` is already a broadcast; only channels get split). The streaming trio does a quick assembly-time check and `Start` re-validates the **whole stream graph**: every stream node and every reader of a stream output (a consumer may be hand-written) must fit in the slot budget at once, and every stream output needs exactly one consumer (a dangling downstream, or two readers silently splitting the values, is rejected at `Start` instead of hanging at runtime).
- **Observation rides a single seam.** The engine emits two run-level callbacks (start / finish, bracketing the run) plus at most three per node; folding them into records is `observe`'s job.

Full design: [`docs/design/pulse.md`](docs/design/pulse.md) (Chinese; the single design doc for both orchestration and observation).

## Build & test

```bash
go build ./...          # verify compilation
go vet ./...
go test -race -count=1 ./...
```

Requires **Go 1.25.0+**. No Makefile, no linter config; CI runs build, vet, a gofmt check and the race-enabled test suite on every PR and on pushes to `main`.

## Documentation

- Guides (中文 / English): <https://luo-root.github.io/pulse/en/>
- Design doc (orchestration + observation): [`docs/design/pulse.md`](docs/design/pulse.md)
- API contracts live in godoc; every exported symbol carries one.

## License

MIT — see [LICENSE](LICENSE).
