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

- **Data arrival is scheduling.** `Requires` is an AND precondition: a node enters `Run` only when all of its inputs have arrived.
- **Slots have three states**: `pending` | `ready(value)` | `skipped`. Ready and skipped are *both* arrival — skipping is not failure. Branching is written by calling `Skip` on the `Provide` you did not choose.
- **Failure is explicit.** A node error records the first error and cancels the whole run; it is never rewritten as `ErrSkipped`.
- **One run, one world.** A `Graph` is one instantiation of a template, not a re-runnable container. Reusing the template means calling `New` again — like a CI/CD workflow definition being run many times, one run instance each. Cross-run state (history, caches, sessions) belongs to the caller, not to the engine.
- **Aspects wrap the whole "wait for input + execute" span**, so `Timeout` can interrupt a node that is still waiting for data.
- **Observation rides a single seam.** The engine emits at most three callbacks per node; folding them into records is `observe`'s job.

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
