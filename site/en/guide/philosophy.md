# Design philosophy

Every trade-off in Pulse has a reason. This page explains **why** — so that when you are picking a library, or hitting a boundary, or wondering "could there just be a flag for this", you know what this project is holding on to.

The full version (each axiom with its costs and boundaries) lives in the repository: [`docs/design/philosophy.md`](https://github.com/Luo-root/pulse/blob/main/docs/design/philosophy.md).

## What it is, in one line

Pulse is a **data-arrival-driven** execution engine for Go, plus a layer of **graph observation**. It does three things:

1. **Declare** — what each node reads and writes;
2. **Advance** — one Key arriving (with a value or an explicit skip) is what makes the nodes waiting on it ready; no edge objects, no topological sort, no scheduling loop;
3. **Explain** — where the run is, who is waiting for whom, and where it failed.

It is not a DAG workflow engine (no scheduler, no retry magic, no implicit compensation); not a business framework (it never does IO and knows no business fields); not a DSL (Go stays Go — declarative assembly exists only in the `pulse/yaml` layer).

## Six ideas you will feel directly

### 1. The topology is derived from data, not drawn by you

A node declares only `Requires` / `Provides`; an edge is derived from "who produces this Key, who consumes it".

**Why**: hand-written edges get written wrong — a missing one, a reversed end. And "who produces this Key" is already unique by declaration (a Key has at most one source, otherwise `ErrDuplicateSource`), so an edge is a **derived view**, not an input.

**What you get**: no wiring table to keep in sync; the cost is that graph-theory shapes like "several distinct edges between the same pair of nodes" are not expressible.

### 2. Declaration and arrival are separate: `pending` is not arrival

A slot has exactly three states: `pending` (not there yet), `ready` (has a value), `skipped` (explicitly skipped). **Ready and skipped are both arrival.**

**Why**: mix the two and a downstream node will think it should skip itself while its input simply has not arrived yet — and "who is waiting for whom" becomes unanswerable (a dashboard would paint "not reached yet" and "waiting for a slot" identically).

**What you get**: `Get` blocks until arrival, `TryGet` peeks without blocking, `WaitAll` reports which routes never arrived; a zero value and "nothing" are two different things — say `skipped` when you mean it, never a zero value.

### 3. Skipping is normal; failure never degrades into a skip

**Why**: "this route has nothing this round" is ordinary in a data flow (optional inputs, a branch not taken, an upstream omitting a value on purpose). Treating it as an exception forces every node to carry boilerplate; dressing a failure up as a skip makes the incident disappear from the incident list.

**What you get**: the gate asks "**did any input bring a value**" — a node runs as soon as one did, so the default is **collect whatever arrives**. To say "one missing route means do not run me", return `WaitAll`'s value — that is the explicit fan-in policy, and the engine will not guess it for you.

### 4. Failure is explicit, and reported at the earliest decidable point

**Why**: problems that only show up once the graph runs are the expensive ones — hangs, timeouts, silently reading a zero value. All three are hard to debug and none points at the line that was actually wrong.

**What you get**: `Start` rejects the statically-decidable dead graphs (a `Requires` nothing provides, a dependency cycle — the cycle comes with **one concrete witness path**); `Validate` lets assembly ask as often as it likes; declarative assembly (`pulse/yaml`) checks child graphs **per reference site**, layer by layer; error messages name the spot (`Sub "b"` and friends) instead of saying "failed".

### 5. Observation shares the engine's semantics, and never changes them

**Why**: two sources of truth drift — the dashboard says A, the log says B, and both are "right". And once observation can change semantics it is no longer observation, it is hidden control flow.

**What you get**: events and state are written **at the same point**; a panicking observer never becomes a node failure; business dimensions can only travel through `Attrs` (`Record` never gains named fields). The cost: there is nowhere to put "a business field the engine knows about" — it goes into `Attrs`.

### 6. A zero-dependency engine, with capabilities layered around it

**Why**: the engine is a foundation inside somebody else's system — it must not drag a dependency tree into their supply chain. Observation and declarative assembly are opt-in layers.

**What you get**: `pulse` uses the standard library only (check it yourself with `go list -deps .`), `gopkg.in/yaml.v3` is used by `pulse/yaml` alone, and the dependency arrows point one way (`pulse` ← `observe` / `yaml`).

## Deliberately not provided

| Not provided | Why |
|---|---|
| A DAG scheduler and topological ordering | Data arrival is scheduling; execution order is a **result** of declarations, not a premise |
| An implicit "missing means skip" policy | Missing must be declared by the node (the gate collects whatever arrives; `WaitAll` is the all-or-nothing opt-in) |
| Rolling back external side effects | The engine only manages its own slots; databases, HTTP calls and file writes need a transaction, an idempotency key or compensation — supplied by you |
| Implicit compensation, opaque transaction magic | Nothing goes in without its own contract and tests — it sounds like a guarantee, but it is luck |
| Business dimensions inside engine types | No named fields on `Record`; `Attrs` is the only way out |
| A DSL in Go | Go stays Go; declarative assembly lives in the YAML layer only |

## One thing that is changing: a graph is a definition, a run is an instance

Today a `Graph` is **both** the definition and one run: `Start` succeeds once, `Wait` releases the context, slots and node state live on the graph — and `Sub` has to build its child graph at run time, so "composed but not yet running, and I cannot see the whole structure" and "build / binding errors surface only when the run reaches them" come as a package.

The target model splits them: **`Graph` is a reusable static definition** (executable many times, sequentially or concurrently, and mountable as a child graph in several places), while **`Execution` carries everything about one run**. This is being designed and rolled out in stages under [#311](https://github.com/Luo-root/pulse/issues/311); the contract draft and the staged plan are in [`docs/design/object-model.md`](https://github.com/Luo-root/pulse/blob/main/docs/design/object-model.md).

Until then, wherever the docs say "a `Graph` is one run's world", they are describing **what exists today** — the target shape lives in the object-model document.
