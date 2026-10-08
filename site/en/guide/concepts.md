# Core concepts

Pulse has only three things: **Key** (a data slot), **Node** (a unit of computation), **Graph** (the world of one run). Understand these and the slot tri-state, and everything else is a corollary.

## The three things

| Concept | In one line | Key property |
|---|---|---|
| **Key** | a typed data slot | `Key[T]` binds a name to a type: `Get` hands you a `T` directly, with no assertion; a Key name must always be registered with the same `T` — no silent "same name, different type" override |
| **Node** | declares what it reads and writes | declares only `Requires` / `Provides`, and **not who the next node is** — dependencies form implicitly from who produces and who consumes each Key |
| **Graph** | the world of one run | the node set + data slots + first error + cancellation. Data is born with `Run` and dies when it ends |

A node looks like this:

```go
pulse.NewNode("summarize",
	pulse.Requires(Docs),     // AND precondition: enters Run only when all inputs have arrived
	pulse.Provides(Summary),  // the slots this node will write
	func(rc *pulse.RunCtx) error {
		docs, err := pulse.Get(rc, Docs)
		if err != nil {
			return err
		}
		return pulse.Set(rc, Summary, join(docs))
	})
```

To take inputs of different types in one node, use `pulse.Deps(pulse.Requires(A), pulse.Requires(B))` — a single `Requires[T]` can only take Keys of one type.

## Slot tri-state

```
pending | ready(value) | skipped
```

**Ready and skipped are both "arrival".** A waiter, once woken, distinguishes these two kinds of arrival, instead of disguising "never arrives" as a fake value. This is the place where engines of this kind most easily go wrong: treating a skip as failure leaves the basic operation of branching with nowhere to live.

Two cascading rules follow:

- any input skipped → **`Run` is not executed**, and all outputs are skipped;
- after `Run` returns successfully, **unwritten `Provides` are auto-skipped** (otherwise downstream would wait forever for an arrival).

This also yields a fact that is easy to state backwards — **a skip is a fact about the slot, not about the node**:

| Case | Final state | Observation records |
|---|---|---|
| The node calls `Skip` on one `Provide` and then returns normally | `completed` | two: wait segment + run segment |
| The node did not execute because **an input was skipped** | `skipped` | **only one** wait record (`err="pulse: skipped [key]"`), no run segment |

See the real output (two nodes: `translate` calls `Skip` on `translated`, and `publish` requires `translated`):

```text
PULSE | 2026/10/08 - 15:39:02.334 | running    |         - | pulse.node_wait_finished | source=observe | pulse.graph=branch-demo pulse.node=translate | host=quickstart | trace=branch-demo
PULSE | 2026/10/08 - 15:39:02.348 | completed  |   13.75ms | pulse.node_run_finished | source=observe | pulse.graph=branch-demo pulse.node=translate | host=quickstart | trace=branch-demo
PULSE | 2026/10/08 - 15:39:02.348 | skipped    |   13.75ms | pulse.node_wait_finished | source=observe | pulse.graph=branch-demo pulse.node=publish | host=quickstart | err="pulse: skipped [translated]" | trace=branch-demo
```

`translate` wrote a skip, but it finished normally itself; `publish` is the node that did not execute.

### How to write a branch

There is no `if` primitive. Branching = **calling `Skip` on the `Provide` you did not choose**:

```go
if cond {
	if err := pulse.Set(rc, OutA, v); err != nil {
		return err
	}
	return pulse.Skip(rc, OutB)
}
if err := pulse.Set(rc, OutB, w); err != nil {
	return err
}
return pulse.Skip(rc, OutA)
```

**Both sides must speak.** Skipping only the branch you did not choose is not enough: after a successful `Run`, any `Provide` you never wrote is auto-skipped, so "A was not chosen" turns into "neither A nor B arrived" — both downstreams stay unexecuted. `Set` the chosen one and `Skip` the other; you need both.

Write semantics: `Set` / `Skip` are both an **idempotent first write** — writing again once the slot is ready is ignored (values are not compared), writing `Skip` again once skipped is ignored too; a `Set` and a `Skip` on the same slot report `ErrConflict`. The same holds for `Seed`.

## One run, one world

> A `Graph` is one instantiation of a template, not a re-runnable container.

A second call to `Start()` returns `ErrGraphStarted`; once a slot has arrived it is closed and does not reopen; `Seed` is rejected after the graph has started. **There is no `Reset`, and there will not be one.**

The criterion, in one sentence: **pulse holds "the data flowing in this run"; it does not hold history.** As soon as it had to keep last run's values around to run a second round, the engine would have to answer "what should stay and what should be cleared" — that is storage semantics, the caller's business. None of the three needs below require the engine to store anything:

| Need | Correct expression |
|---|---|
| Run N independent requests on the same graph | one `New` per request |
| Run 3 candidates inside one request | fan-out within one graph (multiple `Provides`) |
| A long session with many turns | one graph per turn, topology from YAML |

## Explicit failure

`error` and `skipped` take **two separate exits**, never reused:

- any node returning a non-skip error → the **first error** is recorded and the whole graph is canceled; every waiter is woken;
- `Run()` / `Err()` return the original error and **never** rewrite a failure as `ErrSkipped`;
- `panic` does not leak through: a node panic is converted into a node error and takes the same failure path;
- `Err()` does not include a plain skip — **a graph that is skipped end to end is a legal outcome**.

There are two sources of cancellation: the outer `ctx` (the ctx of `pulse.New`), or an aspect timeout (`Timeout`). Either one makes a node still waiting for data return immediately.

## RunCtx: only declared slots

`RunCtx` is the world a node can see during one run: **the slots it has declared** plus this layer's cancelable context. You do not get the whole blackboard:

- `Get` a Key that is not declared in `Requires` → `ErrUndeclared`;
- write a Key that is not in `Provides` → `ErrUndeclared`.

`RunCtx.Fork()` derives a cancelable context only; it **shares** the declaration permissions and the write record with its parent — it is not an independent write transaction.

## Aspects

```go
type Aspect func(rc *RunCtx, next func(*RunCtx) error) error
```

An aspect wraps the node's entire "**wait for input + execute**" span — which is why `Timeout` can interrupt a node that is still waiting for data, not only the execution. Not calling `next` short-circuits.

- `Timeout(d)`: cancels this layer's ctx on timeout;
- `Retry(attempts, delay)`: retries execution errors only; **a cancellation during the wait phase is not retried, and a skipped input is not retried either** (skip is arrival, not failure);
- Order: global aspects (`pulse.WithAspects`) come before node aspects, and **the first one written is the outermost** — so `Timeout` outside, `Retry` inside.

**Latch constraint**: a single node's `Run` must not be entered **concurrently** (two goroutines running the same node at once would necessarily contend for the same slots); violating this returns `ErrNextCalledTwice`. **Sequential re-entry is legal** — `Retry` depends on exactly that (1→0→1). So the criterion is "overlap", not "more than once".

## The three packages

| Package | What it is | Depends on |
|---|---|---|
| `pulse` (root) | the graph engine | **nothing** (standard library only) |
| `pulse/observe` | graph observation: folds the engine's `Observer` callbacks into structured records | `pulse` |
| `pulse/yaml` | declarative graph assembly: YAML → graph | `pulse` + `yaml.v3` |

**The dependency arrow is one-way and must not reverse**: the engine imports no observation package; it exposes a single `Observer` seam. A host that does not need observation imports the root package alone — its dependency closure is empty.

Next: read [Orchestration](/en/guide/orchestration) to see how these are stitched into real topologies, or jump straight to [Graph observation](/en/guide/observability) to wire up an egress.
