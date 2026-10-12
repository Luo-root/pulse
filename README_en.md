[English](README_en.md) | [中文](README.md)

<div align="center">
  <a href="https://luo-root.github.io/pulse/">
    <img alt="Pulse" src=".github/assets/logo.svg" width="260" />
  </a>
</div>

<div align="center">
  <h3>A graph engine for Go — data-arrival scheduling, explicit failure.</h3>
</div>

<div align="center">
  <a href="https://go.dev/"><img alt="Go 1.25.0" src="https://img.shields.io/badge/Go-1.25.0-blue.svg" /></a>
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/License-MIT-green.svg" /></a>
  <a href="https://github.com/Luo-root/pulse/releases/tag/v0.4.1"><img alt="Release v0.4.1" src="https://img.shields.io/badge/release-v0.4.1-2563eb.svg" /></a>
  <a href="https://luo-root.github.io/pulse/en/"><img alt="Docs" src="https://img.shields.io/badge/docs-%E4%B8%AD%E6%96%87%20%7C%20English-2563eb.svg" /></a>
  <a href="docs/design/pulse.md"><img alt="Design doc" src="https://img.shields.io/badge/design-pulse.md-2563eb.svg" /></a>
</div>

<br />

**Pulse** is a data-arrival-driven graph engine plus a graph-observation layer. Nodes declare which keys they read and write; the topology is implied by data production and consumption. There are no edge objects, no "sort first, then run" scheduling phase, no scheduler loop.

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
	g, err := pulse.New("demo", "demo graph")
	if err != nil {
		panic(err)
	}
	// DeclareInput: declare docs as an inlet of this graph — declare it before feeding it.
	if err := g.DeclareInput(pulse.NewPort(Docs)); err != nil {
		panic(err)
	}

	var in pulse.Inputs
	_ = pulse.Seed(&in, Docs, []string{"a", "b"})

	_ = g.Add(pulse.NewNode("summarize", "fold docs into one summary",
		pulse.Requires(Docs),
		pulse.Provides(Summary),
		func(rc *pulse.RunCtx) error {
			docs, err := pulse.Get(rc, Docs)
			if err != nil {
				return err
			}
			return pulse.Set(rc, Summary, fmt.Sprintf("%d docs", len(docs)))
		}))

	if err := g.Run(context.Background(), in); err != nil {
		panic(err)
	}
	// Summary has arrived.
}
```

## Design in one screen

- **Data arrival is scheduling.** `Requires` is an AND precondition: the gate is judged once all inputs have arrived (ready or skipped); **collect whatever arrives** — as soon as one input arrived with a value the node enters `Run`, and only when no input brought a value does it skip itself.
- **Slots have three states**: `pending` | `ready(value)` | `skipped`. Ready and skipped are *both* arrival — skipping is not failure. Branching is written by calling `Skip` on the `Provide` you did not choose (and `Set` on the one you did); **both sides must speak**, since skipping one and forgetting to `Set` the chosen one auto-skips that output too, leaving both downstreams unexecuted.
- **Failure is explicit.** A node error records the first error and cancels the whole run; it is never rewritten as `ErrSkipped`.
- **Reusable definition, one run, one world.** A `Graph` is a **reusable static definition** (nodes, dependencies, ports, mounted child graphs); everything about one run — ctx / input values / slots / first error / cancellation domain — lives in a new `Execution`. One definition can run many times sequentially, many times concurrently, and be mounted as a child graph in several places; one run's cancellation / failure / completion pollutes neither the definition nor any other run. To run it once, call `g.Run(ctx, in)`; for step-by-step control, build `g.NewExecution(ctx, in)` yourself and drive `Start` / `Wait`. Cross-run state (history, caches, sessions) still belongs to the caller, not the engine: pulse holds "the data flowing in this run" and no history. The remaining migration stages (transactional `Retry`, …) are tracked under [#311](https://github.com/Luo-root/pulse/issues/311), with the contract in [`docs/design/object-model.md`](docs/design/object-model.md).
- **Aspects wrap the whole "wait for input + execute" span**, so `Timeout` can interrupt a node that is still waiting for data. **Graph-level aspects are inherited into child graphs and stack downwards** (inherited ones outside, the child's own inside; `pulse.WithoutAspects()` opts out) — which is exactly why **graph-level aspects should be used sparingly and node-level ones preferred**: one graph-level `Timeout(d)` applies at the same time to the parent's ordinary nodes, to the step mounting a child graph (= the whole child graph) and to every node inside the child, while **a child graph times out far more easily than a plain node**. The child's **total** time is cut short by the `d` on the mounting step first — it does not become "`d` per node, `N*d` in total"; and when slots are tight a timeout can even fire *before* the child ever gets a slot — that step takes none, yet its `d` starts ticking the moment it enters `Run`.
- **A graph as a node: a graph can be one step.** Composition has **one public entry, `pulse.Sub`** — it mounts a ready-made child graph **built at assembly time**, so the parent can see the whole subtree recursively **without running** (`Topology().Nodes[i].Sub`, paths derived from the mounting position), and the child's static problems (no source / declared outlet nobody produces / dependency cycle) are reported at assembly time too, the message naming the **mounting position**; **ports are the only wiring surface** — `DeclareInput` / `DeclareOutput` declare the inlets and outlets, and wiring to a **key internal to the child** is a static error (outlets carry one more check: one declared but never produced is reported at assembly time instead of waiting for the run to say `never arrived` — an inlet and an outlet sharing a name is a **pass-through**, which does not count, so `In(parent, k)` + `Out(k, parent2)` on one port **is legal**); **the bind-shape criteria exist once** (shared by `Sub` / `Validate` / `Topology().Problems`): a child inlet may be bound **once** (a second one would be silently eaten by first-write-wins), and a binding onto an inlet that already has a source inside the child is rejected outright (a node inside provides it → that value collides with an existing source) — readable off the declarations, so it does not wait for the step to actually run; composition cycles are rejected at assembly time (the criterion looks only at the ancestors on the current recursive path, so mounting one child graph in several places is legal reuse; "walk the subtree" and "mount it" happen inside one global assembly critical section, so two opposing mounts cannot slip a cycle in either). The run semantics are the same as hand-written nesting: `In` / `Out` declare the parent-side boundary (read as **arrows** — source first, destination second) and the parent graph's static validation (sources / cycles / slots) still applies; a child with no observer of its own **inherits the parent's** (forgetting that by hand is **silent** — the parent runs fine and `Run` returns `nil` while not one child record appears). **The mounting position (`pulse.path`) now arrives with every callback** (`Site.Path`), so one inherited egress carries the layer straight from it — no egress per layer needed. A child failure fails this node with the **first error propagating untouched** (`errors.Is` holds); ready outputs are `Set`, skipped ones `Skip`, and when **every** output skipped the node finishes as `skipped` (not a failure). **Slots are shared tree-wide**: `WithMaxRunning(n)` means at most n nodes of the whole tree sit inside `Run` at once (**as long as no layer declares its own**) — a child that declares nothing inherits the parent's share, and the step mounting a child graph itself takes no slot (it waits for its child from start to finish); declaring `WithMaxRunning(m)` inside a child makes **that branch leave the parent's table and run on its own** (`m<=0` = unlimited there) — not a tighter cap stacked on top of the parent's. **A child graph is a definition and can be mounted in several places**: mount it at two sites and both run, each with its own child execution (ctx / slots / first error kept apart); in the YAML layer a `graph:` node that declares `retry:` is **rejected at `Load`** — the bridge back into the parent happens inside the first attempt and those parent slots are idempotent first-write, so a retried attempt's writes are silently dropped and `Retry` reports only the last attempt's error, masking the real cause of the first failure (per-attempt write sets are stage 3; put `retry:` on the node inside the child graph that may actually fail); on the Go side `Sub(..., pulse.Retry(...))` cannot work either, only the rejection point differs (`Aspect` is a bare func type and a closure has no queryable identity, so assembly cannot recognise it): `Retry` refuses it as **the step's first act inside `Run`**, before that step has read its parent-side inputs or written anything. A node id must not contain `/` — it is the layer separator in `pulse.path`, so `a/b` would collide with "a nesting b".
- **The assembly sugar only collects names; it never changes semantics.** `pulse.FanOut` / `pulse.Join` pull "one input → N parallel instances" and "N routes of one type → one batch" into the function signature (`pulse.Keys(...)` + `pulse.Batch[T]`): every route carries its **source name** (`Batch.Items`, missing routes included), the whole batch can be made strict with `m.WaitAll()`, a single route with `m.Get(k)` (a skipped route yields `*SkipError`, an undeclared one `ErrUndeclared`), and both callbacks receive **this node's own `*RunCtx`** (long jobs watch `rc.Context()` for cancellation). `pulse.NoValue()` is a **node-level** skip — not the same thing as `Skip(rc, key)`, which skips one output slot; and `FanOut`'s N workers are validated and committed as **one atomic batch**, so an incomplete batch fails entirely and leaves no half fan-out on the graph. The graph the sugar builds and the graph you wire by hand yield **field-for-field identical observation records** (pinned by an equivalence-anchor test). **The compiler locks element types and arity, not the order of same-typed slots** (`Keys(a,b)` and `Keys(b,a)` both compile — measured) — taking a route by its `Key` is unaffected; that boundary is written down in the design doc. **Beyond wiring there are three more**: `pulse.Only(rc, k, v)` states an exclusive branch in one call (write this one, void every other output of the node — hand-written omissions leave *both* downstreams unrun while the run still reports success); `pulse.Produce` / `pulse.Consume` fold a stream's channel creation, close responsibility, cancellation response and loop into one call (the producer **stays alive until it is done**; the consumer uses `select`, not `for range` — the latter drains the buffer after cancellation and still reports success); and `pulse.Tee` **copies** one stream to N downstreams (plain values do not need it — one `Provides` with N `Requires` is already a broadcast; only channels get split). The streaming trio does a quick assembly-time check and `Start` re-validates the **whole stream graph**: every stream node and every reader of a stream output (a consumer may be hand-written) must fit in the slot budget at once, and every stream output needs exactly one consumer (a dangling downstream, or two readers silently splitting the values, is rejected at `Start` instead of hanging at runtime).
- **Observation rides a single seam.** The engine emits two run-level callbacks (start / finish, bracketing the run) plus at most three per node; folding them into records is `observe`'s job.

Full design: [`docs/design/pulse.md`](docs/design/pulse.md) (Chinese; describes today's implementation — the object model is being migrated).

## Design philosophy

The section above is about **shape**; this one is about **why** — the full version is [`docs/design/philosophy.md`](docs/design/philosophy.md), and the site has a [Design philosophy](https://luo-root.github.io/pulse/en/guide/philosophy) page.

- **The topology is derived from data, not drawn by you.** A node declares what it reads and writes; edges come from "who produces this Key, who consumes it" — no edge objects, hence no wiring table to drift.
- **`pending` is not arrival.** A slot has three states — `pending` (not there yet) / `ready` (has a value) / `skipped` (explicitly skipped) — and ready and skipped are *both* arrival.
- **Skipping is normal; failure never degrades into a skip.** The gate asks "did any input bring a value" — collect whatever arrives; "one missing route means do not run me" is an explicit `WaitAll`, never the default.
- **Failure is explicit, reported at the earliest decidable point.** `Start()` (the start of one run; `Run` goes through it too) rejects the statically-decidable dead graphs (no source, a declared outlet nobody produces, a dependency cycle), `Validate` lets assembly ask repeatedly (**passing seals the whole subtree**: this definition, mounted children included, never changes again — a successful `Start()` seals the tree too; a failed validation seals **nothing**, in no layer — adding the missing piece and asking again is exactly what it is there for), and declarative assembly validates child graphs at `Load` time too — one report per **reference site**, the message naming the **mounting path** (with one exception: a spec nothing references has no mounting site, hence no interface, so "nothing feeds this key" is not decidable there — it is checked for factory names / duplicate ids / source conflicts / cycles / streams only).
- **Observation shares the engine's semantics and never changes them.** Events and state are written at the same point; a panicking observer never becomes a node failure; business dimensions travel through `Attrs` only.
- **A zero-dependency engine, capabilities layered around it.** `pulse` uses the standard library only; observation lives in `observe`, declarative assembly in `yaml`, and the dependency arrows point one way.

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
- Philosophy and design thinking: [`docs/design/philosophy.md`](docs/design/philosophy.md)
- Object model and contracts (`Graph` / `Execution`, migration in flight): [`docs/design/object-model.md`](docs/design/object-model.md)
- API contracts live in godoc; every exported symbol carries one.

## License

MIT — see [LICENSE](LICENSE).
