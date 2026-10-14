# Orchestration

All of orchestration is this: **declare what you read and what you write, then let data arrival drive execution**. This page covers how the topology forms, and the semantics of concurrency, timeout, retry, rate limiting and cancellation.

## Where the topology comes from

There are no edge objects between nodes and no topological-sort step. Dependencies are fixed at assembly time by a Key's **production and consumption**:

```
        ┌── zh ──┐
topic ──┤        ├── join
        └── en ──┘
```

- A Key allows **one source**: a declared inlet (`DeclareInput`; this run's value is supplied by `Seed` / `SkipSeed`), or exactly one node's `Provides` — and the two are **mutually exclusive**: declaring an inlet for a key some node of the same graph provides (or, the other way round, `Provide`ing a key already declared as an inlet) hits a source conflict at the moment it is declared, so a graph never carries a dead inlet nobody could feed;
- The same Key `Provides`d in two nodes → `Add` returns `ErrDuplicateSource` outright;
- A node that both `Requires` and `Provides` the same Key → `Add` fails (a self-loop is not a legal data flow).
- Conversely, **every `Requires` must have a source**: a slot nobody writes can never arrive, so that graph can never finish — `Start()` (which `Run` goes through) rejects it on the spot and names the node and the Key (left to runtime it is just a hang, and a hang is not something you can debug).
- **The dependency graph must also be acyclic**: having a source is not the same as being satisfiable. In a cycle every `Requires` has a producer, yet no node can enter `Run` first (the gate waits for all inputs), so every slot stays `pending` forever — `Start()` rejects that as well, reporting one concrete cycle: `pulse: dependency cycle: A -> B -> A (A requires "y", B requires "x")`. A declared inlet's value is supplied by this run's `Seed` / `SkipSeed` before the run starts, so it forms no edge.

Those two judgments also have a **read-only entry point**, `(*Graph).Validate()`: it runs the same checks but **starts nothing and builds no goroutines**, so `Run` / `NewExecution` still work afterwards (inputs are **one run's** values, not part of the definition). **Passing seals the definition** — after that `Add` / `DeclareInput` / `DeclareOutput` / mounting a child graph are all rejected (`ErrGraphStarted`; sealing says "this **definition** will not change any more") — and it seals the **whole subtree**: a mounted child is part of this definition, so it is sealed too (that same child can still be mounted elsewhere — mounting does not change it). A **failed validation seals nothing, in no layer**, and adding the missing piece and validating again is exactly the workflow it is there to support. It exists for **assembly time**: declarative assembly like [YAML](/en/guide/assembly) mounts its child graphs at `Load` time, and `Validate` follows the static mounting edges **recursively**, checking per reference site and naming the **mounting path** — and the answer you get is word for word the one `Start` would have given.

## Source nodes and Seed

A graph's inputs have only two origins:

```go
// 1) Host injection: declare the key an inlet first, then write this run's value into Inputs
_ = g.DeclareInput(pulse.NewPort(Topic))
var in pulse.Inputs
_ = pulse.Seed(&in, Topic, "slot contract")

// 2) Source node: no Requires; it runs as soon as it is submitted
_ = g.Add(pulse.NewNode("zh", "seed the word list", nil, pulse.Provides(Words), func(rc *pulse.RunCtx) error {
	return pulse.Set(rc, Words, []string{"data", "arrival", "is", "scheduling"})
}))
```

Within one `Inputs` a key may be written **once** (a value or a skip) — a second write is rejected right there: it would collide with exactly the slot's idempotent first write, which would silently drop it at run time. The values are copied into an internal read-only table the moment `NewExecution` is created, so changing the `Inputs` afterwards has no effect on an execution already created; a different set of values means another run.

## fan-out / fan-in

fan-out is simply "several nodes writing different Keys", fan-in is simply "one node `Requires` several Keys" — **no extra primitive is involved**. For inputs of more than one type, splice one declaration together with `Deps` (`Requires[T]` takes Keys of a single type at a time):

```go
_ = g.Add(pulse.NewNode("join", "join the words with the label",
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

`Requires` is an AND: **all inputs must arrive** (ready or skipped) for that gate to pass; past the gate it is **collect whatever arrives** — one input with a value is enough to enter `Run`, and only when no input brought a value does the node not execute (see [Core concepts](/en/guide/concepts)). On a clean run `g.Run(ctx, in)` returns `nil`.

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

A graph can be embedded into a larger graph as **a single node** — flows compose in layers instead of growing into one ever-wider flat graph. Composition has **one public entry, `pulse.Sub`**: it mounts a **ready-made** child graph, so the child's shape is fixed at assembly time and the parent can see the whole subtree **before it runs**.

```go
child, _ := pulse.New("enrich", "the enrichment step")
// …add the child's nodes: it reads topicKey, writes summaryKey…
if err := child.DeclareInput(pulse.NewPort(topicKey)); err != nil { // inlet: the parent may feed it in
	return err
}
if err := child.DeclareOutput(pulse.NewPort(summaryKey)); err != nil { // outlet: the parent may take it out
	return err
}
err := pulse.Sub(parent, "step1", "one step handed to a child graph",
	[]pulse.SubBind{pulse.In(topic, topicKey), pulse.Out(summaryKey, summary)}, child)
```

**Ports are the only wiring surface**: the child-side end of `In(parent, child)` must be an **inlet** declared with `DeclareInput`, and the child-side end of `Out(child, parent)` an **outlet** declared with `DeclareOutput`; wiring to a **key internal to the child** (one some node `Provides` but nobody declared as an outlet) is a **static error** — to let the parent use that key, declare it an outlet (the intent becomes explicit); leaving it undeclared means "this is my implementation detail, not a promise". A declared inlet **no longer counts as "no source"** in the static checks: that is how `Validate` tells "you forgot to wire it" apart from "this was an external input all along", and in the static topology its source is `pulse.SourceInput` (`SourceSeed` was deleted with stage 2 — an external value belongs to **one run**'s `Inputs`, so this is the only spelling of "external source" a definition has). **Outlets carry one more criterion, symmetrically**: a `DeclareOutput` key that **nothing produces** (no node `Provides` it, and it is not a pass-through of a same-named inlet) is a **static error** — reported at assembly time instead of waiting for the child to run and for `bridgeOutputs` to say `never arrived`. Ports are **static**: they take part in topology and validation only, and carry no value of any single run.

**The bind-shape criteria exist once** (shared by `Sub` / `Validate` / `Topology().Problems`): (1) a child **inlet** may be bound **once** — a second one would be **silently eaten** by the idempotent first write; (2) a binding onto an inlet that **already has a source** inside the child is rejected outright — a key the child produces internally is **not a bindable port** (the message says `is not a input port of the child graph`) — readable off the declarations, so it does not wait for the step to actually run. And the state "declared inlet + produced inside" **cannot be built at all**: `DeclareInput` and `Add` reject it from both sides at the moment of the declaration (`ErrDuplicateSource`; a key has exactly one source), so a declared inlet is always a usable inlet. **Every inlet the child actually needs must be fed at each mount site** — an inlet some node `Requires`, or the only source of a pass-through outlet this site binds with `Out`; otherwise the failure only shows up once the parent is running (`has no value for this run` when the child execution is created, or `never arrived` after the child finished), by which time sibling nodes may already have run and `Validate()` has sealed the definition. An inlet nobody needs stays **optional** — no caller is forced to invent a value. Outlets are **not** deduped: pushing one outlet to two parent keys loses nothing (the same parent key twice is rejected by `Add`), so **`In(parent, k)` + `Out(k, parent2)` on one port is legal** — that is the pass-through.

One ordinary parent value may feed two distinct child inlets with `In(p,a), In(p,b)`. The parent Requires contains it once, both binds remain, and each child inlet still permits only one binding. Dynamic `InRef` / `OutRef` compare actual reflect.Type at assembly; a zero Key cannot be seeded or declared as a port. A node-written or restored nil interface remains ready through Get/TryGet and internal In bridges; host Seed still rejects a nil interface.

**Visible once assembled, no run needed**: `Topology()` recurses along the static mounting edges (`Nodes[i].Sub` is a whole subtree, `Binds` is the wiring at this point), paths are derived from the **mounting position** (`outer` / `outer/inner`), and **reading it changes no definition**; `Validate()` recurses the same way — a child's static problems are reported on the **parent**, **one report per reference site**, naming the mounting position:

```text
pulse: subgraph "inner" (mounted at "step1/toInner"): pulse: node "leaf" requires "c.nope" but nothing provides it (declare it with DeclareInput if the value comes from outside)
```

**A composition cycle is rejected at assembly time**: the moment `Sub` mounts, it walks the child's subtree asking "is the parent already inside it" (mounting a graph into itself counts), and reports `pulse: subgraph cycle: A -> B -> A`. The criterion looks only at the **set of ancestors on the current recursive path** — mounting one child graph in **several places** is legal reuse, and a global visited set would wrongly reject it (that is reuse, not a cycle).

**Sealing rules**: `Validate()` **seals only on success, and it seals the whole subtree** — after that `Add` / `DeclareInput` / `DeclareOutput` / mounting a child graph are all rejected (`ErrGraphStarted`), for the mounted children too (they are part of this definition); **a failed validation seals nothing**, in no layer — adding the missing piece and validating again is exactly the workflow to support. There is no `Seal()`: it would overlap with "success seals". Sealing only blocks **definition changes** — it does not block runs: one definition still runs through `Run` / `NewExecution` many times (inputs are each run's values, not part of the definition), and a sealed child **can still be mounted elsewhere** (mounting does not change its definition).

**Three things you no longer have to remember** (wire it by hand and missing any of them is silent):

| Wiring by hand means remembering | What you get if you forget | What one `Sub` call gives you |
|---|---|---|
| attach the observer to the child graph | the parent graph runs fine and `Run()` returns `nil`, while **not one child record appears** | a child with no observer of its own **inherits the parent's** (one of its own wins) |
| hang the child execution off the parent node's layer | hand-wired, the child runs in a cancellation domain of its own: a parent cancel is invisible and it runs to completion anyway — measured: the parent reports **success** after 300ms | `Sub` derives the child execution's ctx from that parent-node layer, so cancellation travels down by construction |
| hand-write both bridges | child → parent can only go through a **closure variable** (`Graph` has no public slot read API), and who is bound to whom lives in your head | declare `In` / `Out` once; the bridge happens when the child finishes |

**Run semantics are the same as hand-written nesting**: `In` / `Out` read as **arrows** (source first, destination second), and the parent-side node's `Requires` / `Provides` follow from them — the boundary is written where the wiring is, and both ends of one `T` are locked at compile time. Child succeeds → this node succeeds (ready outputs are `Set`, skipped ones `Skip`); **every output skipped** → this node finishes as `skipped` (not a failure); child fails → this node fails and the **first error propagates untouched** (`errors.Is` holds); cancellation → the child sees it. Two boundaries remain:

1. **A child graph is a definition and can be mounted in several places**: mount it at two sites and both run — each mounting position gets its own **child execution** (its own ctx / slots / state / first error), so their inputs and outputs stay apart; the definition can also run many times sequentially or concurrently. A `Retry` on that step is fine: every attempt builds a **fresh child execution**, and the bridge back into the parent belongs to that attempt's write staging area (see "Retry").
2. **The cancellation domain**: the child execution's ctx is **derived from that parent-node layer** (including a `Timeout` on this step), so cancellation travels down by construction. Without that derivation the child would run the whole span and swallow the cancellation — measured on the old hand-wired version: the parent takes 300ms and reports **success**; with derivation it is 51ms + `context canceled`.

`aspects` land on **the parent-side node** — `pulse.Timeout(30*time.Second)` puts a time limit on the whole child graph. A parent's **graph-level** aspects (`WithAspects`) are also **inherited into the child** (prepended outside the child node's own aspects, stacked; `WithoutAspects()` opts out) — but do not use that to bound a subtree, see [Aspect order](#aspect-order).

**One quota is shared by the whole tree**: `WithMaxRunning(n)` = at most n nodes of this tree working inside `Run` at once — a child that declares nothing inherits the parent's share (the same count), and the step mounting a child graph (`Sub`) takes no slot of its own (from start to finish it is waiting for the child). Declaring `WithMaxRunning(m)` inside a child makes **that branch leave the parent's table and be counted on its own** (that layer and its descendants switch to its share; `m<=0` = that subtree unlimited) — **not** "one more, tighter cap on top of the parent's": with a parent of `n=1` and a child of `m=2`, that branch can still run 2 at a time. Declarative assembly gives the whole tree its budget via `LoadOptions.MaxRunning`.

**Two boundaries to remember**: ① the observer is inherited automatically, and the **mounting position (`pulse.path`) now arrives with every callback** (`Site.Path`) — the inherited egress reads it straight from there, so every layer's records carry their own layer and there is no need to build an egress per layer (the old spelling, `ObserveConfig.Path` on a per-layer egress, is now only a fallback — see "Nesting depth" in [Observability](/en/guide/observability)). ② a node id must not contain `/` — that is the layer separator in a path, so writing `a/b` for the step mounting a child and "a nesting b" produce the **same** path (the graph id included); the mounting step rejects it outright at assembly (a plain node id may contain `/` — it never enters a path).

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
- **The consumer uses `select` to respond to cancellation**: `for range` may keep processing buffered values after cancellation; `select` lets a node waiting to read exit after cancellation. Wait independently reports cancellation of the execution context, so body cooperation and the run's final result are separate decisions.
- **An empty stream is not a skip**: nothing sent, outputs closed normally → the downstream still enters `Run` (zero callbacks, `completed`, the run succeeds).
- **Slots**: both ends of a stream must be live at once — at assembly time the sugar can only do a quick local check, and `Start()` re-checks the **whole stream graph**: every stream node *and every reader of a stream output* (a consumer may be hand-written) must fit in the slot budget at the same time, otherwise the graph is rejected outright instead of hanging at runtime. That also rejects "small stream + big buffer" configurations — whether they work depends on the data volume, so they must not be relied on. On top of that, **every stream output needs a consumer — exactly one**: a dangling output is rejected at `Start()` (a sender blocking on an unread output waits forever), and a second reader is rejected too, because two independent readers silently split the values between them. To broadcast, give each downstream its own `Tee` output; to compete, use one `FanOut`.
- **Don't confuse `FanOut` with `Tee`**: use `FanOut` to *compete* (each item handled once), `Tee` to give every downstream everything. Plain values do not need `Tee` — one `Provides` with N `Requires` is already a broadcast; only channels get split.

Bindings are followed across children to the actual readers. Sub itself takes no slot: `Produce + Tee + two Consume nodes inside a child` needs 4 slots in the shared pool. A child quota declaration creates a separate pool, even if its number matches the parent; separate mounts of one definition are counted separately. Renaming one channel into two independently consumed inlets still triggers the split-values rejection.

Closure and bridging also form dependencies: Sub publishes Out only after the entire child finishes, Consume waits for channel closure, and Tee needs its readers to start before synchronous sending can finish. If A's mount waits for child B's done, B waits for Tee to close, and Tee waits for A to start reading, Validate/Start report a stream lifecycle cycle even with unlimited quota or finite buffers. The check covers known built-in stream and static binding relationships; hidden waits in callbacks/Aspects and manual close timing remain the host's responsibility. A declared root Produce/Tee outlet also needs an actual consumer.

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
pulse.NewNode("wait-forever", "waits until the timeout fires", requires, provides, run, pulse.Timeout(50*time.Millisecond))
```

The aspect wraps the whole "**wait for input + execute**" span, so a timeout can interrupt a node that is **still waiting for data**, not just the execution. The shape here is "a producer exists, but it is slower than the timeout" — upstream will write eventually, it just has not yet. The "never arrives" graph cannot even be built: a Key nobody writes is rejected by `Start()`.

```text
timeout err = pulse: node wait-forever timeout after 50ms
```

A timeout is a **failure** and takes the failure path: it cancels the whole graph, and `Run` returns the error above.

A timeout is **cooperative**: on expiry it cancels this layer's ctx and then **waits for the node body to return** — `Run` never returns while the body is still running (an aspect shares the write target and the advance ledger with its parent, and returning early would let the finish path and the still-running execution touch the same slots concurrently). When the body ignores ctx (a bare `time.Sleep`, blocking IO), `Timeout` can only wait for it to finish — do not use it as a hard watchdog. **Published slots are not rolled back either**: output the body `Set` before the deadline may already have been consumed downstream — a timeout is a failure, not a rollback. (A `Timeout` placed **inside** a `Retry` is a different story: that is just one failed attempt, and its whole write set is discarded — see "Retry" below.)

## Retry

```go
pulse.NewNode("flaky", "fails occasionally: shows Retry", nil, pulse.Provides(Topic), run, pulse.Retry(3, 10*time.Millisecond))
```

`Retry` retries **execution errors** only. Skips are **not** retried — a skip is arrival, not failure; cancellation during the waiting phase is not retried either. Measured: the third attempt succeeds:

```text
retry   attempts = 3 err = <nil>
```

**A failed attempt's writes are not published** (the engine has owned this since stage 3): `Retry` opens a **fresh write staging area per attempt** (the attempt write set), so that attempt's `Set` / `Skip` land there and **never touch the execution slots** — if the attempt fails with retries left, the whole thing is **discarded** and downstream never saw anything it wrote (the slots were never touched, so nobody waiting on them was even woken). Only a successful attempt — or one ending in a skip, since a skip is arrival — hands the staging area over; and before handing it over the **whole batch is checked first**, so a key that cannot land (the target already arrived in the opposite state) means **nothing is published** — no "half a batch". Past that check the keys go out **one by one**, with no promise of strict atomic visibility.

The nested-`Retry` boundary: **an inner result belongs to the outer attempt** (an inner commit merely hands it to the outer staging area), so when the outer attempt fails the inner writes are discarded with it.

Three boundaries worth remembering:

1. **A cross-attempt read does not see a discarded write** — that key never had a value in this attempt, so reading it waits for a slot nobody will write (until this layer's ctx is cancelled); a **read-back inside the attempt** is self-consistent (staging wins);
2. **The transaction boundary is the layer `Retry` wraps**: writes made outside `Retry` (by an outer aspect) belong to no attempt and are not discarded;
3. **Only engine-managed slots can be discarded**: database commits / HTTP calls / file writes that already happened cannot be taken back — move them to the last successful attempt, or use idempotency keys / an outbox / compensation.

**It can be put on a step that mounts a child graph** (a node built by `pulse.Sub`): every attempt builds a **fresh child execution**, and the first attempt's bridge into the parent is discarded along with that attempt's staging area — so the parent only ever sees the **successful** attempt's output, and the half-and-half mix cannot happen. [Declarative assembly](/en/guide/assembly) is the same: `retry:` on a `graph:` node is assembled and runs (a fresh child execution per attempt).

**What it cannot sit on is a stream producer**: `Produce` / `Tee` are shaped as "`Set` a channel first, then send into it", and with an attempt boundary that channel would only be published once the attempt **commits** — while its consumer waits for the channel before it can do anything. Sender and receiver wait for each other and the whole run hangs until the ctx expires. So that case is a **named error at run time, spelling out the fix** (take the write-staging aspect off this layer), not a silent hang; at bottom, a retry **restarts a stream**, and values already sent cannot be taken back. The criterion is "is there a write set", never the aspect's identity — `Aspect` is a bare func type that assembly cannot identify.

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
g, _ := pulse.New("demo", "demo graph", pulse.WithMaxRunning(4))
```

`WithMaxRunning(n)` caps how many nodes are **doing work inside `Run` at the same time** (the step mounting a child graph does not count: `Sub` waits for the child from start to finish), and that budget is **shared by the whole graph tree**: a child that declares nothing inherits the parent's share — so **as long as no layer declares its own**, declaring 4 on the root means at most 4 nodes of the tree are working at once. A parent's node and a child's node compete for the **same** share and never yield to each other: with budget to spare they run concurrently, and only then do they queue; while the steps mounting a child graph wait, an observer sees **more than n** rows in `running`, which is not concurrency exceeding the budget. Declaring `WithMaxRunning(m)` inside a child makes **that branch leave the parent's table and run on its own** (`m<=0` = unlimited there, which is also leaving) — **not** "one more, tighter cap on top of the parent's": with a parent of 1 and a child of 2, that branch still runs 2 at a time. Declarative assembly takes the tree-wide budget as `LoadOptions.MaxRunning`. **Waiting for data does not take a slot** — otherwise rate limiting would degrade into deadlock (every node holding a slot is waiting for data, and nobody can make progress).

**Queuing for a slot is interruptible too**: when the ctx is canceled (or a first error cancels the graph), nodes still queued never enter `Run` and finish as `canceled` — with only their wait record — so a freed slot is never handed to an already-canceled node.

## Aspect order

```
global aspects (pulse.WithAspects) → node aspects (NewNode's aspects…) → the core (wait for input + execute)
```

Whatever is listed first is further outside. So `pulse.Timeout(d)` placed ahead of `pulse.Retry(...)` gives you "the **total** duration is bounded, each attempt retries on its own" — timeout outside, retry inside. Written the other way round it is "each attempt gets its own limit".

**Across graphs**: a parent's graph-level aspects are **inherited into the child**, prepended **outside** the child node's own aspects (stacked, not replaced). So in a three-level nest the innermost node carries the tree's graph-level aspects (prepended) plus its own — it does **not** stack once per level. A child that wants none of the parent's batches writes `pulse.WithoutAspects()` (aspects the child declares itself still apply; writing only that means no graph-level aspects at this layer at all).

**Use graph-level aspects sparingly; prefer node-level ones.** `WithAspects` lands on **every** node of the layer at once — including the step mounting a child graph (a `Sub` step — a whole child graph sits behind it) — and it keeps stacking onto every node inside the child. `Timeout` is the aspect most easily bitten by that mismatch of scale: one graph-level `Timeout(d)` is at the same time "each ordinary node of the parent's limit", "the mounting step's limit (i.e. the whole child graph)" and "each node inside the child's limit" — three things that are not the same order of magnitude, sharing one `d`, while **a child graph times out far more easily than a plain node**. Put a limit on the one node that needs it: `NewNode(..., pulse.Timeout(...))` for an ordinary node, the `aspects` of a `Sub` step for a whole child graph. Keep graph-level aspects for the ones you genuinely want applied uniformly across the tree (tracing / metrics). Note also that **cutting only detaches "inheritance onto the child's nodes"**: the parent's own mounting step is still wrapped by the parent's graph-level aspects — "this subtree has to run long" cannot be fixed by cutting alone.

Two accounts that are easy to get wrong: (1) the child's **total** time is cut short by the `d` on the step mounting it first — it does **not** become "`d` per node, `N*d` in total": the inherited `d` does wrap every child node, but it is only a segment inside that outer `d`; (2) when slots are tight, **a timeout can fire before the child ever gets a slot** — the mounting step takes no slot, yet its `d` starts ticking the moment it enters `Run`, while the child's nodes are still queued for quota.

**The advance gate**: a single node's `Run` must not be entered **concurrently** (violating it → `ErrNextCalledTwice`); **sequential re-entry is allowed only when a fresh attempt boundary was opened** — the criterion is "did this advance swap in a new write staging area", not "how many times": `Retry` swaps one per attempt, so its re-entry is legal; a retry aspect you write yourself opens no boundary, so its second advance is rejected on the spot, and the message carries the **first** advance's failure reason too. It limits the advances of **this node in this execution** — another `Run` is a fresh execution context and its count starts at zero.

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

- **Ignoring ctx delays termination**: after external cancellation, a body that never checks `rc.Context()` may continue working, and the engine waits for it to return. If cancellation happens before finalization, even a nil body return can leave the node completed while the run is canceled; Run / Wait return the context error. Cooperative cancellation controls when the body exits, not whether cancellation counts as success.
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

if err := g.Run(ctx, in); err != nil { return err } // 2) failures come back as the return value
```

`Run(ctx, in)` / `Execution.Err()` return the **first error**, and **never a plain skip**; an all-skipped graph is a legal outcome. Cancellation is cooperative: the body responds to ctx itself, and the engine waits for it to exit without forcibly stopping its goroutine. Wait then latches the terminal result: absent a first error, execution cancellation before finalization still becomes the result, even if no body returned it as an error. A completed node and a canceled run may coexist. Context cleanup after finalization does not change the result, and repeated Wait never recaptures that cleanup cancellation.

Next: the topology does not have to live in Go either — see [Declarative assembly](/en/guide/assembly); to see how long each node spent waiting and how long executing, see [Graph observation](/en/guide/observability).
