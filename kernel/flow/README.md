[English](README.md) | [中文](README_zh.md)

# kernel/flow

`flow` is Pulse v2's data-readiness orchestration package at the timescale of **a single run**.

It is not a DAG executor that maintains an explicit edge table and pre-computes a topological order. Nodes only declare which `Key`s they read and which they produce; at run time all nodes are submitted in one go, and a node proceeds naturally once its input data arrives. `Requires` is an **AND precondition**: the node enters `Run` only after all inputs are ready.

```go
import "github.com/Luo-root/pulse/kernel/flow"
```

For the design contract and acceptance checklist, see [`../../docs/design/flow-v2-design.md`](../../docs/design/flow-v2-design.md).

## Applicability

Suited to organizing work with definite input/output relationships within a single `context.Context` lifetime:

- preparing data in parallel and then converging;
- conditional branches such as classification and routing;
- one-shot tasks that need explicit cancellation, timeout, retry, and a concurrency cap;
- splitting one Agent execution into observable compute nodes.

It is not meant to carry persistent state across runs. Sessions, caches, circuit breakers, resume-from-checkpoint, distributed scheduling, and service governance are out of `flow`'s scope.

## Mental Model

```text
节点声明 Requires / Provides
          │
          ▼
Graph.Start / Run 一次性提交全部节点
          │
          ▼
每个节点等待全部 Requires 到达（AND）
          │
          ├── 全部 ready ──► 进入 Run ──► Set / Skip Provides
          │
          └── 任一 skipped ─► 不进入 Run，全部 Provides 自动 Skip

节点返回 error ─► 记录首错并取消整图
```

A slot has exactly three states:

```text
pending（未到达） | ready（值） | skipped（跳过）
```

Both `ready` and `skipped` are arrivals. Skip is a normal branch outcome, not a failure; only a node error is a failure.

## Minimal Example: a Linear Data Flow

```go
package main

import (
    "context"
    "fmt"

    "github.com/Luo-root/pulse/kernel/flow"
)

var (
    Source = flow.NewKey[string]("example.source")
    Result = flow.NewKey[string]("example.result")
)

func main() {
    g := flow.New(context.Background())

    if err := g.Add(flow.NewNode(
        "prepare",
        nil,
        flow.Provides(Source),
        func(rc *flow.RunCtx) error {
            return flow.Set(rc, Source, "hello")
        },
    )); err != nil {
        panic(err)
    }

    if err := g.Add(flow.NewNode(
        "format",
        flow.Requires(Source),
        flow.Provides(Result),
        func(rc *flow.RunCtx) error {
            value, err := flow.Get(rc, Source)
            if err != nil {
                return err
            }
            return flow.Set(rc, Result, value+" world")
        },
    )); err != nil {
        panic(err)
    }

    if err := g.Run(); err != nil {
        panic(err)
    }

    // 输出槽通常由后续节点消费。此例将 Result 再交给外部调用方即可。
    fmt.Println("flow completed")
}
```

Inside a node, `Get` / `Set` may only access declared keys:

- `Get` / `TryGet` / `WaitAll` can read the node's `allowed` set (Requires∪Provides);
- `Set` / `Skip` can only write `Provides`;
- undeclared keys, same name with a different type, and similar errors are returned explicitly; data must not be silently written onto a shared blackboard.
- convention: do not `Get` a Provide you have not written yourself (it would wait forever); reading Provides is mainly for aspects/diagnostics.

When one node needs multiple inputs or outputs of different generic types, declare them together with `Deps`:

```go
flow.NewNode(
    "compose",
    flow.Deps(flow.Requires(Title), flow.Requires(Count)),
    flow.Provides(Summary),
    func(rc *flow.RunCtx) error {
        // ...
        return nil
    },
)
```

## External Input: Seed / SkipSeed

The external caller writes inputs before the run:

```go
g := flow.New(ctx)
if err := flow.Seed(g, Request, input); err != nil {
    return err
}
if err := g.Run(); err != nil {
    return err
}
```

If an external input marks a branch as inapplicable, use `SkipSeed`:

```go
if err := flow.SkipSeed(g, OptionalInput); err != nil {
    return err
}
```

### Source Identity Rules

Each key has **at most one source identity**:

1. an external `Seed` / `SkipSeed`; or
2. one node's `Provides`.

The two cannot coexist; a conflict returns `ErrDuplicateSource`. The same external source may be declared repeatedly: repeated `Seed` follows the slot's first-write rule, repeated `SkipSeed` is idempotent; a value/skip conflict between `Seed` and `SkipSeed` returns `ErrConflict`.

Therefore an external input must not also be computed by a node:

```go
// 错误：Request 已被外部来源占用，Add 返回 ErrDuplicateSource。
_ = flow.Seed(g, Request, input)
_ = g.Add(flow.NewNode("load", nil, flow.Provides(Request), load))
```

## Skip: Conditional Branches and Automatic Wrap-Up

Conditional branches need no OR scheduling. A classifier node `Set`s the selected branch and `Skip`s the unselected ones; any downstream node that depends on a skipped input never runs, and its outputs continue to Skip.

```go
var (
    Left  = flow.NewKey[string]("left")
    Right = flow.NewKey[string]("right")
)

split := flow.NewNode(
    "split",
    nil,
    flow.Deps(flow.Provides(Left), flow.Provides(Right)),
    func(rc *flow.RunCtx) error {
        if chooseLeft() {
            if err := flow.Set(rc, Left, "selected"); err != nil {
                return err
            }
            return flow.Skip(rc, Right)
        }
        if err := flow.Skip(rc, Left); err != nil {
            return err
        }
        return flow.Set(rc, Right, "selected")
    },
)
```

The rules:

- `Set` is an idempotent first write; a second `Set` is silently ignored.
- `Set` and `Skip` cannot be mixed on the same slot; a conflict returns `ErrConflict`.
- when any of a node's `Requires` is skipped, the framework does not run that node's `Run`; instead it marks all of the node's `Provides` as skipped.
- if `Run` returns normally but omits some `Provides`, the framework automatically `Skip`s the unwritten outputs so downstream nodes do not wait forever.
- when a node returns an error: `Graph.Run` / `Err` still return that error (**never** disguised as `ErrSkipped`); the framework may still Skip **unwritten Provides**, only to release waiters (cancellation cleanup) — it does not translate failure into a skip.
- `ErrSkipped` is not `Graph.Run`'s failure result; it only means some read or wait encountered a skip.

`Get` on a skipped slot returns an error matched by `errors.Is(err, flow.ErrSkipped)`. `TryGet` checks without blocking:

```go
value, ready, skipped, err := flow.TryGet(rc, Input)
```

## Parallel Convergence

All nodes are submitted; waiting for data consumes no run slot. Nodes without dependencies can finish in parallel, and a join node runs only once all inputs are ready:

```go
join := flow.NewNode(
    "join",
    flow.Deps(flow.Requires(LeftOutput), flow.Requires(RightOutput)),
    flow.Provides(Merged),
    func(rc *flow.RunCtx) error {
        left, err := flow.Get(rc, LeftOutput)
        if err != nil {
            return err
        }
        right, err := flow.Get(rc, RightOutput)
        if err != nil {
            return err
        }
        return flow.Set(rc, Merged, left+right)
    },
)
```

By default there is no limit on how many nodes are actually inside the user `Run`. To cap run concurrency:

```go
g := flow.New(ctx, flow.WithMaxRunning(4))
```

In `WithMaxRunning(n)`, `n <= 0` means unlimited concurrency (the default). Waiting for `Requires` takes no slot; a slot is taken only once all inputs have arrived.

## Lifecycle: Run, Start, Wait, and Cancellation

```go
// 同步执行：Start 后 Wait。
if err := g.Run(); err != nil {
    return err
}

// 异步执行：适合调用方需要并行做其他工作。
if err := g.Start(); err != nil {
    return err
}
if err := g.Wait(); err != nil {
    return err
}
```

- `Run` = `Start` + `Wait`.
- a graph starts at most once; once started, `Add`, `Seed`, and `SkipSeed` all return `ErrGraphStarted`.
- cancelling the `ctx` passed at Graph creation interrupts all waits and any cooperating node.
- when a node returns any error other than `ErrSkipped`, the Graph records the first error, cancels the whole graph, and returns that error from `Run` / `Wait`.
- `Graph.Err()` returns the first node error or the upstream context's cancellation reason; a plain Skip never makes it fail.

## Aspects: Timeout and Retry

`Aspect` is isomorphic to `kernel.Waterfall`: an aspect short-circuits everything after it by not calling `next`. Within a single `Around`, **concurrent/overlapping** calls to `next` are forbidden (`ErrNextCalledTwice`); **sequential repeated** calls are allowed (`Retry` needs this). Global aspects are installed via `flow.WithAspects`; node aspects are passed as trailing arguments to `flow.NewNode`. Global aspects sit on the outside, node aspects on the inside. E1 `Observer` lifecycle events are guarded by a per-node latch, so Waiting/Running still fire at most once.

```go
g := flow.New(ctx,
    flow.WithAspects(loggingAspect),
)

node := flow.NewNode(
    "call-model",
    flow.Requires(Prompt),
    flow.Provides(Answer),
    callModel,
    flow.Timeout(10*time.Second),
    flow.Retry(3, 200*time.Millisecond),
)
```

Built-in behavior and optional aspects:

| Capability | Semantics |
|---|---|
| Recovery | Built in and cannot be turned off. A `Run` panic becomes a node error and cancels the whole graph. |
| `Timeout(d)` | Covers both waiting for inputs and node execution; a timeout cancels the node's `RunCtx`, so it can interrupt `Get` / `WaitAll`. |
| `Retry(attempts, delay)` | Retries inner execution errors; `attempts <= 0` normalizes to 1. Cancellation during the input-wait phase is not retried. |

`RunCtx.Fork()` derives only a cancellable context and **shares** declared permissions and the write record; it is not an independent write transaction. Custom aspects can obtain the current context via `rc.Context()`.

## Lifecycle Observation (E1)

`Observer` is flow's **own** typed observer, no-op by default. It does not go through `kernel.Emit`, nor write to `observability.Sink` — the official observability package knows nothing about flow; an assembly-layer bridge (e.g. `demoapp.FlowObserver`) subscribes and then folds events into two Records.

```go
obs := flow.ObserverFunc{
    Waiting:  func(id string) { /* 进入 WaitAll 前 */ },
    Running:  func(id string) { /* acquire 后、用户 Run 前 */ },
    Finished: func(id string, reason flow.NodeFinishReason, err error) { /* skip 清理后 */ },
}
g := flow.New(ctx, flow.WithObserver(obs))
```

| Event | When | Notes |
|---|---|---|
| `OnNodeWaiting` | Before entering `WaitAll` | ≤ 1 per node |
| `OnNodeRunning` | After `WaitAll` succeeds and `acquire` | Skip / timeout interrupting the Wait → **not emitted** |
| `OnNodeFinished` | After the terminal state is settled and skip cleanup | Reasons: `completed` / `skipped` / `failed` / `canceled` |

Contract highlights:

- Waiting/Running/Finished fire at most once per node; multiple `Retry` attempts do not re-emit.
- an observer panic **must not** become a node failure.
- the usual bridge implementation: the two Records `flow.node_wait_finished` / `flow.node_run_finished`, each carrying `Duration`; node identity goes in `FiberName` (the official envelope is not extended). See `examples/04-flow` (record definitions in `examples/internal/demoapp/bridge.go`; the 04-flow README is already tabulated).

## Declarative Graph Assembly (E2)

**YAML only** graph assembly lives in the subpackage [`yaml`](yaml/README.md): `Load` / `SeedPlan`. Topology belongs to A — YAML must carry `id` / `uses` / `requires` / `provides`; `uses` maps to a Run factory on the `Registry`. No JSON parser will be added.

- duration fields use the Go `ParseDuration` form: `30s`, `100ms` (do not write bare numbers whose seconds reading is ambiguous).
- `observer:` is a documentation hint; `Load` **ignores** it; observers go through `LoadOptions.Graph` (e.g. `WithObserver`).
- `version` is omitted or `1`; other values are rejected.

### No Public Slot-Read API After the Graph Ends

`Graph` has no public `Get` after the run. If a leaf can neither double-Provide the same output key (single producer) nor converge two Finals through AND (Skip would cascade through and swallow the join node), the example layer may write terminal results out through a closure — this is a **contract workaround**, not the "all outputs go through closures" convention. For the three constraints on "closure-written Final", see [`examples/04-flow/README.md`](../../examples/04-flow/README.md).

## Declaration-Time Validation

`Graph.Add` rejects the following invalid declarations early, before start:

- nil nodes, empty node IDs, duplicate node IDs;
- duplicate `Requires` or duplicate `Provides` on the same node;
- the same node declaring both `Requires` and `Provides` for the same key;
- multiple nodes declaring `Provides` for the same key;
- keys with the same name but different generic types;
- node sources conflicting with `Seed` / `SkipSeed` sources.

This keeps producer races, same-name/type conflicts, and similar problems out of concurrent run time.

## Exported API

| Category | Symbols | Purpose |
|---|---|---|
| Graph | `New` / `Graph.Add` / `Run` / `Start` / `Wait` / `Err` | Construct, declare, and execute a single run |
| Options | `WithMaxRunning` / `WithAspects` / `WithObserver` | Run concurrency, global aspects, lifecycle observation |
| Registry (E2) | `Registry` / `NewRegistry` / `Register` / `RegisterKey` / `Lookup` / `ResolveKey` / `SeedByName` | Named Run factories + key type reconciliation; instances are not global |
| Key | `Key[T]` / `NewKey` / `Name` | Typed slot identifiers |
| Node | `Node` / `NewNode` / `ID` | Declare nodes |
| Dependencies | `Requires` / `Provides` / `Deps` | Declare node inputs and outputs |
| External input | `Seed` / `SkipSeed` | Resolve slots before the run |
| Node read/write | `Get` / `TryGet` / `WaitAll` / `Set` / `Skip` | Read, wait, write, or skip declared keys |
| Aspects | `Aspect` / `AspectFunc` / `Timeout` / `Retry` | Wrap node waiting and execution |
| Observer | `Observer` / `ObserverFunc` / `MultiObserver` / `WithObserver` | E1 node lifecycle (Waiting/Running/Finished); no-op by default |
| Run context | `RunCtx.Context` / `NodeID` / `Fork` / `Cancel` | Read the node context and write aspects |
| Errors | `ErrSkipped` / `ErrConflict` / `ErrUndeclared` / `ErrDuplicateSource` / `ErrNextCalledTwice` / `ErrGraphStarted` / `ErrGraphNotStarted` | Match expected error categories |

## Non-Goals

`flow` currently deliberately does not provide:

- explicit edge objects and a topological sorter;
- OR dependencies, `WaitAny`, race-based convergence;
- node re-runs, `SetOrUpdate`, or a continuously reactive graph;
- persistence, resume-from-checkpoint, distributed execution;
- cross-run service governance such as circuit breaking and error swallowing by default;
- blackboard-style arbitrary reads/writes on undeclared keys;
- depending on a yaml decoding library in the **core package** (E2 parsing lives in the subpackage [`yaml`](yaml/): `Load` / `SeedPlan`; topology belongs to A: the Factory only yields a Run).
