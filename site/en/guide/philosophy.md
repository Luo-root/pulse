# Design philosophy

Every trade-off in Pulse has a reason. This page explains **why** — so that when you are picking a library, or hitting a boundary, or wondering "could there just be a flag for this", you know what this project is holding on to.

The full version (each axiom with its costs and boundaries) lives in the repository: [`docs/design/philosophy.md`](https://github.com/Luo-root/pulse/blob/main/docs/design/philosophy.md).

## What it is, in one line

Pulse aims to be a **data-driven, observable, composable workflow-engine foundation for Go**. Today, `pulse` provides an embeddable execution core for a single process and graph observation. The separate `pulse-durable` project is planned to add node checkpoint recovery; durable waits and distributed run management remain future designs, not implemented capabilities. See the [development direction in #320](https://github.com/Luo-root/pulse/issues/320) for the responsibilities and roadmap.

The three responsibilities below describe the **execution core**. Task claiming, wake-ups and failover belong to durable run management:

1. **Declare** — what each node reads and writes;
2. **Advance** — one Key arriving (with a value or an explicit skip) is what makes the nodes waiting on it ready; no edge objects, no "sort first, then run" scheduling phase, no scheduling loop;
3. **Explain** — where the run is, who is waiting for whom, and where it failed.

It is not a **traditional centrally-scheduled DAG workflow engine**: Pulse derives the dependency graph from `Requires` / `Provides`, rejects dependency cycles and exposes a topology view just like one does — the difference is that progress is driven by Key arrival rather than by a central scheduler. Retry must be declared explicitly (the `Retry` aspect) — and once declared, the engine does own that layer's slot writes along the attempt boundary (a failed attempt is not published downstream); but the engine still performs no implicit compensation, and it will never undo what you already did in the outside world. It is not a business framework (it never does IO and knows no business fields), and not a DSL (Go stays Go — declarative assembly exists only in the `pulse/yaml` layer).

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

**What you get**: the moment a run starts (`Run` / `NewExecution` / `Start()` share one set of criteria) it rejects the statically-decidable dead graphs (a `Requires` nothing provides, a dependency cycle — the cycle comes with **one concrete witness path**); `Validate` lets assembly ask as often as it likes; declarative assembly (`pulse/yaml`) validates child graphs at `Load` time too, per reference site, the message naming the **mounting path**; error messages name the spot (`Sub "b"` and friends) instead of saying "failed".

### 5. Observation shares the engine's semantics, and never changes them

**Why**: two sources of truth drift — the dashboard says A, the log says B, and both are "right". And once observation can change semantics it is no longer observation, it is hidden control flow.

**What you get**: events and state are written **at the same point**; a panicking observer never becomes a node failure; business dimensions can only travel through `Attrs` (`Record` never gains named fields). The cost: there is nowhere to put "a business field the engine knows about" — it goes into `Attrs`.

### 6. A zero-dependency engine, with capabilities layered around it

**Why**: the engine is a foundation inside somebody else's system — it must not drag a dependency tree into their supply chain. Observation and declarative assembly are opt-in layers.

**What you get**: `pulse` uses the standard library only (check it yourself with `go list -deps .`), `gopkg.in/yaml.v3` is used by `pulse/yaml` alone, and the dependency arrows point one way (`pulse` ← `observe` / `yaml`).

## Deliberately not provided

These boundaries describe the execution core. Future checkpointing, durable waits, task claiming and failover belong to run management; the absence of a central scheduler in the local core does not prohibit a durable scheduling protocol in that layer.

| Not provided | Why |
|---|---|
| A central scheduler and "sort first, then run" | Data arrival is what advances the graph; execution order is a **result** of declarations, not a premise; the topology view is a read-only exit that never drives progress |
| An implicit "missing means skip" policy | Missing must be declared by the node (the gate collects whatever arrives; `WaitAll` is the all-or-nothing opt-in) |
| Rolling back external side effects | The engine only manages its own slots; databases, HTTP calls and file writes need a transaction, an idempotency key or compensation — supplied by you |
| Implicit compensation, opaque transaction magic | Nothing goes in without its own contract and tests — it sounds like a guarantee, but it is luck |
| Business dimensions inside engine types | No named fields on `Record`; `Attrs` is the only way out |
| A DSL in Go | Go stays Go; declarative assembly lives in the YAML layer only |

## Landed: a graph is a definition, a run is an instance

Both halves of the "definition vs. one run" split have landed. **Composition and static validation**: `Sub` — composition's one public entry — mounts a **ready-made** child graph built at assembly time, so once assembled the whole tree is visible recursively along the static mounting edges without running anything (`Topology()`), and a child's static problems (no source / a declared outlet nobody produces / dependency cycle) are reported the moment it is mounted, per reference site, the message naming the **mounting path**; ports (`DeclareInput` / `DeclareOutput`) are the only wiring surface, and `Validate()` recurses through the tree and **seals the definition on success** — the whole subtree, mounted children included.

**Execution**: a `Graph` is a **reusable static definition**, and one run's ctx / input values / slots / node state / first error / cancellation domain all live in a new `Execution` — one definition can run many times sequentially, many times concurrently, and be mounted as a child graph in several places (each mounting position gets a child execution of its own); one run's cancellation / failure / completion pollutes neither the definition nor any other run. The inputs (`Inputs` + `Seed` / `SkipSeed`) belong to **that one run**; the definition only declares "this key comes from outside" (`DeclareInput`).

Stage 4 (snapshot / observation / YAML alignment) is implemented (#319; main-branch and release status follow the PRs and releases): Topology exposes the complete static interface, Execution snapshots read this run's progress, Mermaid can overlay states, and YAML Analyze needs no registry. Stage 5 (migration & release) continues under [#311](https://github.com/Luo-root/pulse/issues/311); **stages 1 (static composition), 2 (the `Execution` split) and 3 (transactional `Retry`) are landed**, and the contract plus per-stage landing status are in [`docs/design/object-model.md`](https://github.com/Luo-root/pulse/blob/main/docs/design/object-model.md).
