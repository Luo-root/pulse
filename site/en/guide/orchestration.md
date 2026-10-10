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
- Conversely, **every `Requires` must have a source**: a slot nobody writes can never arrive, so that graph can never finish — `Start()` rejects it on the spot and names the node and the Key (left to runtime it is just a hang, and a hang is not something you can debug).
- **The dependency graph must also be acyclic**: having a source is not the same as being satisfiable. In a cycle every `Requires` has a producer, yet no node can enter `Run` first (the gate waits for all inputs), so every slot stays `pending` forever — `Start()` rejects that as well, reporting one concrete cycle: `pulse: dependency cycle: A -> B -> A (A requires "y", B requires "x")`. Keys from `Seed`/`SkipSeed` have already arrived before start, so they form no edge.

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

`Requires` is an AND: **all inputs must arrive** (ready or skipped) for that gate to pass; past the gate it is **collect whatever arrives** — one input with a value is enough to enter `Run`, and only when no input brought a value does the node not execute (see [Core concepts](/en/guide/concepts)). On a clean run `g.Err()` is `nil`.

## Sugar: FanOut / Join

Wiring a node by hand aligns the same name three times (the declaration in `NewNode`, the `Get`, the `Set`). The sugar pulls the names into the **function signature**:

```go
// fan-in: N inputs of one type → one batch
err = pulse.Join(g, "collect", pulse.Keys(a, b, c), out,
	func(rc *pulse.RunCtx, m pulse.Batch[string]) (Report, error) {
		headline, err := m.Get(a) // per-route strict: no `a` and this node skips
		if err != nil {
			return Report{}, err
		}
		return report(headline, m.Values(), m.Missing()), nil // Values(): collect whatever arrived
	})

// fan-out: one input → N parallel instances, each with its own output slot
// (nodes are named id-1 … id-N)
err = pulse.FanOut(g, "worker", docs, pulse.Keys(r1, r2, r3),
	func(rc *pulse.RunCtx, shard int, doc string) (Result, error) {
		if nothingFor(shard) {
			return Result{}, pulse.NoValue() // this shard has no output: the whole instance skips, that is not a failure
		}
		return work(shard, doc)
	})
```

It is called `FanOut` rather than `Spread` because it does **not split data**: all N instances see the same input, and "one piece each" is up to `shard`.

Sugar is only sugar — the graph it produces and the graph you wire by hand yield **field-for-field identical observation records** (the `observe` package pins this with an equivalence anchor: same topology, two wirings, same event names / node attribution / terminal states / `Err` / envelope).

| What you write | Expands to | Semantics |
|---|---|---|
| `Join(..., Keys(a,b,c), out, fn)` | one node: `Requires(a,b,c)` + `Provides(out)` | the gate is "collect whatever arrives"; when every input skipped, `fn` does not run and the node skips itself |
| `FanOut(..., in, Keys(r1,r2), fn)` | two nodes (`worker-1` / `worker-2`), each `Requires(in)` | one goroutine each; the instance count is fixed at assembly time; the whole batch is committed **in one step**, so an incomplete batch fails entirely and leaves no half fan-out on the graph |
| `pulse.NoValue()` | a `*SkipError` | "no value this time": the **whole node** skips, its output slot skips with it; a downstream `Join` sees it in `Batch.Missing()` |
| `m.WaitAll()` | `return` it directly | the explicit strict fan-in: one missing route and the node finishes as skipped (not a failure) |

Both callbacks receive **this node's own `*RunCtx`** as their first parameter: an instance that talks to HTTP or a database uses `rc.Context()` to notice cancellation (the engine wakes it when the graph is canceled), and `rc.NodeID()` for attribution.

`pulse.NoValue()` is not the same thing as `Skip(rc, key)`: the latter marks just that one output slot as skipped and the node still finishes as `completed`; the former is a **node-level** terminal declaration — return it and the whole node ends as `skipped`.

**Missing routes *and* their sources are visible in the type**: `pulse.Batch[T]` is a roster — `Items []BatchItem[T]` (`Key` + `Value` + `Present`, in declaration order) — **every declaration is in it**, missing routes included; `Len()` / `Values()` / `Missing()` / `Get(k)` are four ways to read that roster. A bare slice of values would leave the host unable to tell "this route has no value" from "this route was never part of the batch" — or from "which slot did this value come from".

`Get(k)` takes a **`Key` object**, not a name (a typo in a string literal would silently become a zero value), and its three outcomes stay far apart: a value on arrival gives the value; a route that arrived skipped gives `*SkipError` (return it and this node skips); a `Key` that is not part of this batch gives `ErrUndeclared` — "you asked for the wrong thing" never mixes with "this route has no value".

**What the compiler locks**: the element type (`Keys(...)` and `Batch[T]` must share one `T`) and the arity (`Join` takes one batch, `FanOut` opens one instance per output slot). It does **not** lock the order of same-typed slots — `Keys(a, b)` and `Keys(b, a)` (both `Key[string]`) **both compile**. Measured: positional reading shifts (`Values()[0]` goes from `"from-a"` to `"from-b"`) while `Get(a)` returns `"from-a"` either way — that is exactly what the source names on `Batch` buy, and swapping the order only changes declaration order.

## A graph as a node: one graph, one step

A graph can be embedded into a larger graph as **a single node** — flows compose in layers instead of growing into one ever-wider flat graph.

```go
err := pulse.Sub(parent, "step1",
	[]pulse.SubBind{pulse.In(topic, childIn), pulse.Out(childOut, summary)},
	func(sc *pulse.SubCtx) (*pulse.Graph, error) {
		child, err := pulse.New(sc.Context(), sc.GraphID(), pulse.WithMaxRunning(2))
		if err != nil {
			return nil, err
		}
		if err := child.Add( /* …nodes that read childIn and write childOut… */ ); err != nil {
			return nil, err
		}
		return child, nil
	})
```

`In` / `Out` read as **arrows**: source first, destination second (`In(parent, child)`, `Out(child, parent)`). What the parent-side node declares follows entirely from them — **the boundary is written where the wiring is**, so reading the parent graph tells you what this step eats and what it produces. Both ends must share one `T`, which the compiler locks.

**Three things you no longer have to remember** (wire it by hand and missing any of them is silent):

| Wiring by hand means remembering | What you get if you forget | What `Sub` does |
|---|---|---|
| attach the observer to the child graph | the parent graph runs fine and `Run()` returns `nil`, while **not one child record appears** | a child with no observer of its own **inherits the parent's** (one of its own wins) |
| derive the child ctx from `rc.Context()` | build the child from `context.Background()` and a parent cancel is invisible — it runs to completion anyway | `sc.Context()` derives from this node |
| hand-write both bridges | child → parent can only go through a **closure variable** (`Graph` has no public slot read API), and who is bound to whom lives in your head | declare `In` / `Out` once; the bridge happens when the child finishes |

Terminal states map exactly like hand-written nesting: child succeeds → this node succeeds (ready outputs are `Set`, skipped ones `Skip`); **every output skipped** → this node finishes as `skipped` (not a failure); child fails → this node fails and the **first error propagates untouched** (`errors.Is` holds); cancellation → the child sees it.

**Slots are per graph**: the parent's `WithMaxRunning` governs *how many child graphs run at once* (the `Sub` node holds one parent slot for the whole child run), while concurrency inside the child is governed by the child's own `WithMaxRunning` — so nesting **exceeds the parent's cap**. Measured: parent `maxRun=1` + child `maxRun=2` → peak of **3** nodes inside `Run` at the same time (1 parent + 2 child).

**A child graph is one-shot**: `build` must return a new graph on every run. Build `pulse.New` outside the closure and reuse it, and you get a sentence that tells you how to fix it (`ErrGraphStarted` stays in the error chain):

```
pulse: Sub "b": the child graph was already started:
a graph runs once, so build must return a new one on every run (pulse: graph already started)
```

`aspects` land on **the parent-side node** — `pulse.Timeout(30*time.Second)` puts a time limit on the whole child graph.

## Streaming: Produce / Consume / Tee

`Key[<-chan T]` always worked; what was missing is not "one more channel wrapper" but the six things every producing / consuming node has to hand-write (creation and publication, close responsibility, the loop, cancellation, error propagation, backpressure and slots). The classic mistake is **`Set`-ing the channel and then returning**: the node is already `completed`, the actual sending lives on in an unobserved background goroutine — errors never reach the graph and cancellation cannot wake it.

```go
// The simplest shape is a Produce + Consume pair: one output, one consumer
err := pulse.Produce(g, "src", stream, func(rc *pulse.RunCtx, send func(int) error) error {
	for _, v := range values {
		if err := send(v); err != nil { // cancellation escapes a blocked send
			return err
		}
	}
	return nil
})
// For N downstreams, insert a Tee: it copies one stream into N outputs, one consumer each
err = pulse.Tee(g, "fan", stream, pulse.Keys(sA, sB)) // broadcast: both downstreams see everything
err = pulse.Consume(g, "sinkA", sA, func(rc *pulse.RunCtx, v int) error {
	return handle(v)
})
err = pulse.Consume(g, "sinkB", sB, func(rc *pulse.RunCtx, v int) error {
	return archive(v)
})
```

- **The producer stays alive until it is done**: the channel is closed only in the `defer` that runs when the node returns (success / error / cancellation / panic all take it, so there is no double close), and `send` selects between sending and `rc.Context().Done()`.
- **The consumer uses `select`, not `for range`**: the latter keeps computing the buffered values after a cancellation *and still reports success*; `select` exits the moment the run is cancelled and returns the cancellation reason.
- **An empty stream is not a skip**: nothing sent, outputs closed normally → the downstream still enters `Run` (zero callbacks, `completed`, the run succeeds).
- **Slots**: both ends of a stream must be live at once — at assembly time the sugar can only do a quick local check, and `Start()` re-checks the **whole stream graph**: every stream node *and every reader of a stream output* (a consumer may be hand-written) must fit in the slot budget at the same time, otherwise the graph is rejected outright instead of hanging at runtime. That also rejects "small stream + big buffer" configurations — whether they work depends on the data volume, so they must not be relied on. On top of that, **every stream output needs a consumer — exactly one**: a dangling output is rejected at `Start()` (a sender blocking on an unread output waits forever), and a second reader is rejected too, because two independent readers silently split the values between them. To broadcast, give each downstream its own `Tee` output; to compete, use one `FanOut`.
- **Don't confuse `FanOut` with `Tee`**: use `FanOut` to *compete* (each item handled once), `Tee` to give every downstream everything. Plain values do not need `Tee` — one `Provides` with N `Requires` is already a broadcast; only channels get split.

## Branching: call Skip on the path you did not take

There is no `if` primitive — branching is "mark the `Provide`s you did not take as skipped":

```go
if lang == "zh" {
	return pulse.Skip(rc, Translated) // the downstream needs only this one → no value arrived → it does not run
}
return pulse.Set(rc, Translated, translate(summary))
```

That is a **single-slot** optional output: both branches speak about the same slot (write or skip), hence a single downstream. A multi-slot branch must **speak on both sides** — `Set` the chosen one, `Skip` the other; skip only one and the unwritten ones are auto-skipped, leaving both downstreams unexecuted (see the branch example in [Core concepts](/en/guide/concepts)).

`pulse.Only` says that sentence in one call: `return pulse.Only(rc, OutA, v)` writes A and voids every other output of the node. N outputs go from "1 `Set` + N−1 `Skip`s" to one line, and **the caller no longer writes `Skip` at all** — the omission described above cannot take that shape any more. It **does not test any condition**: which branch to take is still your `if`. The flip side is worth knowing too: repeated statements get loud — `Set` another output first and then `Only` yields `ErrConflict` (two hand-written `Set`s are silently ignored).

It matters to tell the two terminal states apart: **the node that wrote the skip is itself `completed`**; it is the **downstream** node that never executed — because no input brought a value — that is `skipped`. Details and real records: [Core concepts · slot tri-state](/en/guide/concepts).

## Timeout

```go
pulse.NewNode("wait-forever", requires, provides, run, pulse.Timeout(50*time.Millisecond))
```

The aspect wraps the whole "**wait for input + execute**" span, so a timeout can interrupt a node that is **still waiting for data**, not just the execution. The shape here is "a producer exists, but it is slower than the timeout" — upstream will write eventually, it just has not yet. The "never arrives" graph cannot even be built: a Key nobody writes is rejected by `Start()`.

```text
timeout err = pulse: node wait-forever timeout after 50ms
```

A timeout is a **failure** and takes the failure path: it cancels the whole graph, and `Run` returns the error above.

A timeout is **cooperative**: on expiry it cancels this layer's ctx and then **waits for the node body to return** — `Run` never returns while the body is still running (an aspect shares the write record with its parent, and returning early would let the finish path and the still-running execution touch the same slots concurrently). When the body ignores ctx (a bare `time.Sleep`, blocking IO), `Timeout` can only wait for it to finish — do not use it as a hard watchdog. **Published slots are not rolled back either**: output the body `Set` before the deadline may already have been consumed downstream — a timeout is a failure, not a rollback.

## Retry

```go
pulse.NewNode("flaky", nil, pulse.Provides(Topic), run, pulse.Retry(3, 10*time.Millisecond))
```

`Retry` retries **execution errors** only. Skips are **not** retried — a skip is arrival, not failure; cancellation during the waiting phase is not retried either. Measured: the third attempt succeeds:

```text
retry   attempts = 3 err = <nil>
```

**Preconditions for a safe retry**: the failing attempt must not have written any `Provide`, and must have no non-reentrant side effects. Slots are published on arrival with an idempotent first write: once an attempt has `Set`/`Skip`ped a slot, downstream has already been woken, later attempts' writes are **silently ignored**, and the slot is not rolled back — rolling back would mean reopening a slot, which contradicts "one run, one world". If you need transactional retry, move the output to the attempt that is guaranteed to succeed: **compute first, `Set` last**.

## First error cancels

Any node returning a non-skip error → record the **first error** + cancel the whole graph (including nodes still waiting for data):

```text
run err = boom
pulse.graph_started            node=-        status=running   err=<nil>
pulse.node_wait_finished       node=boom     status=running   err=<nil>
pulse.node_run_finished        node=boom     status=failed    err=boom
pulse.node_wait_finished       node=waiter   status=canceled  err=context canceled
pulse.graph_finished           node=-        status=failed    err=boom
```

Three things hold at once: `Run` returns the **original error** (it is not rewritten as a skip); the failing node is `failed`; the waiting node that got killed is `canceled`, and it has **only the waiting-segment** record. A failed run still gets both ends: the two run-level records are emitted as usual, with `pulse.graph_finished` carrying `Status = failed` and the first error in `Err`.

The failure path also marks the failing node's **unwritten `Provide`s** as skipped — that only unblocks downstreams still waiting, it is not their finish reason. Their finish reason follows **"cancellation wins"**: once ctx is canceled a wait always returns `ctx.Err()`, including when arrival and cancellation become ready together. So a downstream that did not run *because of the first error* is stably `canceled` and never flips between `skipped` and `canceled` depending on scheduling.

The same `canceled` covers the other two ways "this run was torn down from outside": the parent ctx being canceled **or hitting its deadline**, and being canceled while queued for a slot. Conversely, a node's **own** `Timeout` expiring is its `failed` — that node did not finish within its limit. The criterion for the four terminal states lives in the godoc of `pulse.NodeFinishReason`.

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

## Wiring conventions: make the silent mistakes unwritable

The sections above explain **how the engine judges**; this one is about **how you write it**. Five rules, each one earned from a measured **silent** mistake — no error, no warning, and the run still returns `nil`. Nothing enforces these for you (there are no lint / vet hints: that would mean a separate binary and a CI change on your side, and it cannot recognise the case where `rc` is passed into a helper that reads the key), so they live here.

**More than one output: state the whole batch, never half of it.** A node with outputs A / B that means to take A, but only writes `Skip(B)` and forgets `Set(A)`:

```go
return pulse.Skip(rc, OutB) // forgot Set(rc, OutA, v)
```

Measured: **neither downstream runs** (A was never written, B was skipped) and the run still returns `nil`. Write it with `Only` instead — take A, void every other output of this node:

```go
return pulse.Only(rc, OutA, v) // no hand-written Skip left, so "forgetting one" has no shape
```

Need several values at once? Keep hand-writing `Set` — `Only` says "this is the one".

**Same-typed slots: take them by their source key, not by position.** `Keys(a, b)` and `Keys(b, a)` both compile — declaration order cannot be locked down at compile time. Measured: reading `b.Values()[0]` flips from `"from-a"` to `"from-b"` when you write the declaration backwards, and you will not notice when the values look alike; taking by key is immune:

```go
v, err := b.Get(SourceA) // independent of declaration order; *SkipError if that route arrived empty
```

**Keep the declaration next to the read.** A key declared in `Requires(X)` should be read in the same `Run`. Note that **"wait for arrival only, never take the value" is legitimate** (measured: declare it without reading and the node still executes) — that is exactly what a gate node is; what you must avoid is leaving readers unable to tell which kind you meant. If it really is just a gate, say so in one comment line.

**Long jobs watch `rc.Context()`; do not `Set` and return.** Two measurements:

- **Nobody looks at ctx → cancellation is swallowed as success**: cancel externally while the node works, and if the node never looks at `rc.Context()` then `Run()` returns `nil` (no node ever saw the cancellation — see [Core concepts](/en/guide/concepts)).
- **Handing the work to a background goroutine → the error has nowhere to land**: `Set` the channel and return, leaving the actual sending to an unobserved goroutine. Measured: the run reports success (`Run() = nil`) while that goroutine *fails* after finishing its work — not a trace of it on the graph. Forgetting the `defer close` is worse: a downstream `for range` waits for the close and `Run()` never returns (an external ctx timeout cannot save you — the node does not return, so the run does not return).

So: `select` on `rc.Context().Done()` inside long loops, and use `Produce` / `Consume` / `Tee` for streams — they own the close responsibility.

**One stream output feeds one downstream.** Two consumers on the same output will **silently split** the values (both run, each getting a share). `Start()` now rejects that outright (naming the nodes), but write it right the first time — **broadcast** with `Tee` (one output per downstream), **compete** with `FanOut` (workers of the same group sharing an output is an explicit statement):

```go
err := pulse.Produce(g, "src", stream, sendAll)        // one stream
err = pulse.Tee(g, "fan", stream, pulse.Keys(sA, sB))  // copied into two outputs
err = pulse.Consume(g, "sinkA", sA, handle)            // one consumer per output
err = pulse.Consume(g, "sinkB", sB, archive)
```

And make the consumer a **`select`, not `for range`**: in the same measured scenario (producer also ignoring ctx, buffer 8, 5 values, cancel at 15 ms) `for range` computed all 5 values and reported success, while `select` stopped at the 4th and returned `context canceled`. One honest caveat: whether the values still sitting in the buffer get computed is **best-effort** — when both branches are ready `select` picks at random, so it is not a hard guarantee.

## Where to read results

`Graph` exposes no slot reads after `Run`. The products belong to the caller; two usual approaches:

```go
var report string // 1) the node closure writes it out — use this for the terminal product
// ...

if err := g.Run(); err != nil { return err } // 2) failures come back as the return value
```

`Run()` / `Err()` semantics: they return the **first error**, and **never a plain skip** (an all-skipped graph is a legal outcome — `Err()` is `nil`). **Cancellation is cooperative**: the engine only promises that nodes still waiting for data or for a slot return immediately; whether a node already inside `Run` looks at the ctx is up to that node. So a cancellation **becomes the run result only when some node sees it and turns it into an error** — a run whose nodes never looked up and each returned normally counts as completed (`Err()` stays `nil`), even if the parent ctx was cancelled midway.

Next: the topology does not have to live in Go either — see [Declarative assembly](/en/guide/assembly); to see how long each node spent waiting and how long executing, see [Graph observation](/en/guide/observability).
