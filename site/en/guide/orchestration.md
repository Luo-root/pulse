# Orchestration

All of orchestration is this: **declare what you read and what you write, then let data arrival drive execution**. This page covers how the topology forms, and the semantics of concurrency, timeout, retry, rate limiting and cancellation.

## Where the topology comes from

There are no edge objects between nodes and no topological-sort step. Dependencies are fixed at assembly time by a Key's **production and consumption**:

```
        ┌── zh ──┐
topic ──┤        ├── join
        └── en ──┘
```

- A Key allows **one source**: `Seed`/`SkipSeed`, or exactly one node's `Provides`;
- The same Key `Provides`d in two nodes → `Add` returns `ErrDuplicateSource` outright;
- A node that both `Requires` and `Provides` the same Key → `Add` fails (a self-loop is not a legal data flow).

## Source nodes and Seed

A graph's inputs have only two origins:

```go
// 1) Host injection: write the initial value before the run
_ = pulse.Seed(g, Topic, "slot contract")

// 2) Source node: no Requires; it runs as soon as it is submitted
_ = g.Add(pulse.NewNode("zh", nil, pulse.Provides(Words), func(rc *pulse.RunCtx) error {
	return pulse.Set(rc, Words, []string{"data", "arrival", "is", "scheduling"})
}))
```

`Seed` is an **idempotent first write**: seeding the same Key again is ignored, while a `Seed` that conflicts with a `SkipSeed` returns `ErrConflict`. After the graph has started, `Seed` is rejected (`ErrGraphStarted`).

## fan-out / fan-in

fan-out is simply "several nodes writing different Keys", fan-in is simply "one node `Requires` several Keys" — **no extra primitive is involved**. For inputs of more than one type, splice one declaration together with `Deps` (`Requires[T]` takes Keys of a single type at a time):

```go
_ = g.Add(pulse.NewNode("join",
	pulse.Deps(pulse.Requires(Words), pulse.Requires(Label)),
	pulse.Provides(Joined),
	func(rc *pulse.RunCtx) error {
		words, err := pulse.Get(rc, Words)
		if err != nil {
			return err
		}
		label, err := pulse.Get(rc, Label)
		if err != nil {
			return err
		}
		return pulse.Set(rc, Joined, fmt.Sprintf("%s:%d", label, len(words)))
	}))
```

`Requires` is an AND: the node enters `Run` only when both inputs have arrived. On a clean run `g.Err()` is `nil`.

## Branching: call Skip on the path you did not take

There is no `if` primitive — branching is "mark the `Provide`s you did not take as skipped":

```go
if lang == "zh" {
	return pulse.Skip(rc, Translated) // downstream does not run because its input was skipped
}
return pulse.Set(rc, Translated, translate(summary))
```

It matters to tell the two terminal states apart: **the node that wrote the skip is itself `completed`**; it is the **downstream** node that never executed — because its input was skipped — that is `skipped`. Details and real records: [Core concepts · slot tri-state](/en/guide/concepts).

## Timeout

```go
pulse.NewNode("wait-forever", requires, provides, run, pulse.Timeout(50*time.Millisecond))
```

The aspect wraps the whole "**wait for input + execute**" span, so a timeout can interrupt a node that is **still waiting for data**, not just the execution. In the example below `Never` has no producer at all (its slot stays pending forever):

```text
timeout err = pulse: node wait-forever timeout after 50ms
```

A timeout is a **failure** and takes the failure path: it cancels the whole graph, and `Run` returns the error above.

## Retry

```go
pulse.NewNode("flaky", nil, pulse.Provides(Topic), run, pulse.Retry(3, 10*time.Millisecond))
```

`Retry` retries **execution errors** only. Skips are **not** retried — a skip is arrival, not failure; cancellation during the waiting phase is not retried either. Measured: the third attempt succeeds:

```text
retry   attempts = 3 err = <nil>
```

## First error cancels

Any node returning a non-skip error → record the **first error** + cancel the whole graph (including nodes still waiting for data):

```text
run err = boom
pulse.node_wait_finished       node=boom     status=running    err=<nil>
pulse.node_run_finished        node=boom     status=failed     err=boom
pulse.node_wait_finished       node=waiter   status=canceled   err=context canceled
```

Three things hold at once: `Run` returns the **original error** (it is not rewritten as a skip); the failing node is `failed`; the waiting node that got killed is `canceled`, and it has **only the waiting-segment** record.

## Rate limiting

```go
g, _ := pulse.New(ctx, "demo", pulse.WithMaxRunning(4))
```

`WithMaxRunning(n)` caps how many nodes are **inside `Run` at the same time**. **Waiting for data does not take a slot** — otherwise rate limiting would degrade into deadlock (every node holding a slot is waiting for data, and nobody can make progress).

**Queuing for a slot is interruptible too**: when the ctx is canceled (or a first error cancels the graph), nodes still queued never enter `Run` and finish as `canceled` — with only their wait record — so a freed slot is never handed to an already-canceled node.

## Aspect order

```
global aspects (pulse.WithAspects) → node aspects (NewNode's aspects…) → the core (wait for input + execute)
```

Whatever is listed first is further outside. So `pulse.Timeout(d)` placed ahead of `pulse.Retry(...)` gives you "the **total** duration is bounded, each attempt retries on its own" — timeout outside, retry inside. Written the other way round it is "each attempt gets its own limit".

**The latch**: a single node's `Run` must not be entered concurrently (violating it → `ErrNextCalledTwice`), yet sequential re-entry is legal — that is exactly what `Retry` relies on.

## Where to read results

`Graph` exposes no slot reads after `Run`. The products belong to the caller; two usual approaches:

```go
var report string // 1) the node closure writes it out — use this for the terminal product
// ...

if err := g.Run(); err != nil { return err } // 2) failures come back as the return value
```

`Run()` / `Err()` semantics: they return the **first error** or the ctx cancellation reason, and **never a plain skip**; an all-skipped graph is a legal outcome (`Err()` is `nil`).

Next: the topology does not have to live in Go either — see [Declarative assembly](/en/guide/assembly); to see how long each node spent waiting and how long executing, see [Graph observation](/en/guide/observability).
