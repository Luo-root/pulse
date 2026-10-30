[English](README_en.md) | [中文](README.md)

<p align="center">
  <img src=".github/assets/logo.svg" alt="Pulse" width="260">
</p>

<p align="center">Data-driven workflow execution for Go</p>

<p align="center">
  <a href="https://go.dev/"><img src="https://img.shields.io/badge/Go-1.26%2B-2563eb" alt="Go 1.26+"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-2563eb" alt="MIT license"></a>
  <a href="https://luo-root.github.io/pulse/en/"><img src="https://img.shields.io/badge/docs-中文%20%7C%20English-2563eb" alt="Bilingual documentation"></a>
</p>

<p align="center">
  <a href="https://github.com/Luo-root/pulse/stargazers"><img src="https://img.shields.io/github/stars/Luo-root/pulse?style=social" alt="Stars"></a>
  <a href="https://github.com/Luo-root/pulse/graphs/contributors"><img src="https://img.shields.io/github/contributors/Luo-root/pulse?style=social" alt="Contributors"></a>
</p>

Pulse composes Go functions into reusable workflows. Each node declares the data it reads and produces. Those declarations determine the dependencies: when inputs arrive, eligible nodes begin work. Embed it in a service, command-line program, or Agent application without running a separate server.

A `Graph` defines the workflow; an `Execution` holds one run. Reuse the same definition for different requests, run it concurrently, or mount it as a subgraph in several places. Each execution has independent inputs, state, and cancellation. The core package uses only the Go standard library.

## When to use Pulse

- **Multi-step processing**: compose downloading, parsing, validation, and generation by data dependency, allowing independent steps to execute concurrently.
- **Branches and aggregation**: distinguish a result from a normal skip, then collect available results or explicitly require all of them.
- **Reusable workflow modules**: compose subgraphs through declared ports while keeping internal data separate from external wiring.
- **Continuous data processing**: send, acknowledge, and broadcast managed messages; pass live streams across subgraphs and deliver them to the host during execution.
- **Observable execution**: inspect structure, execution snapshots, and node events to locate waiting, running, failed, and canceled steps.

To continue after a process exits, use the separate [pulse-durable](https://github.com/Luo-root/pulse-durable) library. It provides storage, encoding, local `Run/Resume`, and managed-stream process recovery through Pulse's checkpoint bridge. For multi-machine coordination, the experimental [distributed module](https://github.com/Luo-root/pulse-durable/blob/main/distributed/README.md) on main provides `Submit/Worker`, using PostgreSQL for whole-workflow claiming, leases, fencing, and failover through the same checkpoint bridge. It does not dispatch individual workflow nodes across machines. The module has not been released. Use the tested dependency combination in the [Durable installation guide](https://github.com/Luo-root/pulse-durable/blob/main/site/en/guide/installation.md); actual tags/releases determine release status. Unconfirmed steps may execute again; application code still needs idempotency or transactions for external side effects.

## Installation

Pulse v0.5.2 requires **Go 1.26.0 or later**. See [installation and versions](site/en/guide/installation.md).

> This README and the documentation site use **v0.5.2** as their API baseline, including managed Stream, live Sub, and incremental checkpoints. Read [migration](site/en/reference/migration.md) before upgrading from v0.4.1. The command below pins v0.5.2. See [installation and versions](site/en/guide/installation.md) for development or unmerged changes; the tag and Release establish publication status.

Run this in a Go module:

```sh
go get github.com/Luo-root/pulse@v0.5.2
```

## Your first workflow

This complete program accepts an integer, doubles it, and reads the result from this execution. Save it as `main.go` and run `go run .`.

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/Luo-root/pulse"
)

func main() {
	input := pulse.NewKey[int]("input")
	output := pulse.NewKey[int]("output")
	g, err := pulse.New("double", "Double the input")
	if err != nil {
		log.Fatal(err)
	}
	if err := g.DeclareInput(pulse.NewPort(input)); err != nil {
		log.Fatal(err)
	}
	if err := g.DeclareOutput(pulse.NewPort(output)); err != nil {
		log.Fatal(err)
	}
	if err := g.Add(pulse.NewNode("double", "Compute twice the value",
		pulse.Requires(input), pulse.Provides(output),
		func(rc *pulse.RunCtx) error {
			value, err := pulse.Get(rc, input)
			if err != nil {
				return err
			}
			return pulse.Set(rc, output, value*2)
		})); err != nil {
		log.Fatal(err)
	}

	var in pulse.Inputs
	if err := pulse.Seed(&in, input, 21); err != nil {
		log.Fatal(err)
	}
	e, err := g.NewExecution(context.Background(), in)
	if err != nil {
		log.Fatal(err)
	}
	if err := e.Start(); err != nil {
		log.Fatal(err)
	}
	if err := e.Wait(); err != nil {
		log.Fatal(err)
	}
	value, state, err := pulse.ReadOutput(e, output)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(value, state)
}
```

Expected output:

```text
42 ready
```

A `Key[T]` identifies typed data. A `Node` declares read/write permissions and a function. The `Graph` holds the definition; the `Execution` holds this run's state. `DeclareInput` / `DeclareOutput` define the host-facing interface, `Seed` supplies this run's input, and `ReadOutput` reads a declared outlet after `Wait` finalizes the execution.

Use `g.Run(ctx, in)` when you only need to wait for completion. Use `NewExecution`, `Start`, and `Wait` as above when you need the execution handle, output values, or lifecycle control. See the [quick start](site/en/guide/quickstart.md) for the full walkthrough.

To reuse default policies without structure, pass the Option returned by `g.Defaults()` to another graph's New. It snapshots the applied graph aspects, observer, and quota declarations at the call, without replaying original Options, copying nodes/ports/identity/sealing, or sealing the source. This is not an execution's effective environment after parent inheritance or ExecOption overrides. It replaces the target's complete default declarations; later WithMaxRunning / WithObserver still override and WithAspects still appends. Slices are independent; objects referenced by closures and observers remain host-managed.

For continuous data, use `Key[pulse.Stream[T]]` and `ProduceStream` / `ConsumeStream` / `TeeStream`. Managed waits temporarily release business quota; explicit Ack releases in-flight capacity. The [complete stream example](site/en/guide/streams.md) broadcasts to two consumers with one quota slot, each summing to 6. For live host consumption, call `SubscribeStream` after `NewExecution` and before `Start`, then consume and acknowledge. `Wait` also waits for registered reading groups to complete or Stop. Sub connects live stream outputs early; ordinary values still bridge after successful child completion. Existing `Key[<-chan T]` and Produce/Consume/Tee keep native compatibility semantics without message acknowledgement, automatic rewind, or persistent-stream guarantees.

## Documentation

| What you need to do | Start here |
| --- | --- |
| Evaluate the library, install it, and complete a first run | [Introduction](site/en/guide/index.md) · [Installation](site/en/guide/installation.md) · [Quick start](site/en/guide/quickstart.md) |
| Understand definitions, executions, data, and normal skips | [Core concepts](site/en/guide/concepts.md) |
| Supply inputs, read results, and build branches and joins | [Inputs and outputs](site/en/guide/inputs-outputs.md) · [Branches and joins](site/en/guide/branching.md) |
| Configure retries, timeouts, and concurrency; compose subgraphs or streams | [Retries and cancellation](site/en/guide/retry-cancellation.md) · [Subgraphs](site/en/guide/subgraphs.md) · [Streams](site/en/guide/streams.md) |
| Compose approvals, input-dependent batches, or Graph tool loops | [Business composition](site/en/guide/composition.md) |
| Define workflows in YAML and integrate events or logs | [YAML assembly](site/en/guide/assembly.md) · [Observation](site/en/guide/observability.md) |
| Look up lifecycle rules, errors, the recovery bridge, or version differences | [Reference index](site/en/reference/index.md) |
| Evaluate execution, resource, SQLite and recovery costs | [Unified production comparison](site/en/reference/production-comparison.md) · [Benchmark source and method](benchmarks/README_en.md) |

Read online: [English documentation](https://luo-root.github.io/pulse/en/) · [中文文档](https://luo-root.github.io/pulse/).

## Packages and scope

| Package | Responsibility | Dependencies |
| --- | --- | --- |
| `github.com/Luo-root/pulse` | Definitions, execution, data flow, subgraphs, the checkpoint bridge, and state views | Go standard library |
| [`pulse/observe`](observe/README_en.md) | Convert engine events into structured records for logs and custom sinks | `pulse` |
| [`pulse/yaml`](yaml/README_en.md) | Assemble workflows from YAML or analyze their structure without running code | `pulse`, `yaml.v3` |

A normal skip means that a result is absent; the execution can still succeed. A real node error cancels this execution and retains its cause. `Retry` isolates failed attempts' writes to Pulse outputs. Application code owns database, HTTP, file, and other external side effects.

Aspects can read this execution's location with `rc.Site()`. With `observe.NewRecordObserver` installed, existing `Retry` / `Timeout` usage automatically records attempts, actual retries, backoff and the current timeout's expiry, while the five Observer lifecycle callback counts stay unchanged. Register custom entry/exit observation explicitly with `pulse.NamedAspect("approval", aspect)`. Without an observer, no logs are emitted. See the [observation guide](site/en/guide/observability.md) for identities, phases and recovery boundaries.

Pulse provides single-process execution without database or scheduling-service dependencies. Durable depends on Pulse in one direction and provides checkpoint storage, recovery, and experimental whole-workflow coordination. Hosts can compose approval waits with aspects, business wait records, and Cancel/Resume, or call Graph tools from a loop node; see [business composition](site/en/guide/composition.md) for wiring and guarantee boundaries. Durable waits, a built-in wake-up service, and dispatching individual workflow nodes across machines remain future directions. Design rationale and plans remain in the [design document](docs/design/pulse.md), [design philosophy](docs/design/philosophy.md), and [cross-repository responsibilities](docs/design/workflow-foundation.md).

## Development and contributions

From the repository root:

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...
```

Run the repository's [Go examples](examples_test.go) with `go test -run Example -v .`. Bilingual chapters live under `site/`; package reference pages are generated from each package's README. Read the [contributing guide](CONTRIBUTING.md) before proposing changes, and report security issues according to the [security policy](SECURITY.md).

Pulse is licensed under [MIT](LICENSE).

## ⭐ Support Pulse

Pulse is still early, but it already ships 12 releases, a full bilingual documentation site, and runnable benchmarks. If you are building data-driven pipelines in Go, or adding a reliable workflow execution layer to an Agent application, a star helps more people find it - which matters a lot for an independently maintained project.

- Found a problem or want a feature: [open an issue](https://github.com/Luo-root/pulse/issues) or [start a discussion](https://github.com/Luo-root/pulse/discussions)
- Useful but no immediate need: hitting the ⭐ in the top right is enough
- Want to contribute: [contributing guide](CONTRIBUTING.md)

