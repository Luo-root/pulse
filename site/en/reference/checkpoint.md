# Checkpoints

This page is for developers implementing durable execution integration, storage, or codecs. First understand [lifecycle](./lifecycle.md), [Retry write sets](../guide/retry-cancellation.md), and [static subgraphs](../guide/subgraphs.md), and use the [development version](./migration.md). Business hosts needing recovery should normally use the independent [pulse-durable](https://github.com/Luo-root/pulse-durable) Run/Resume APIs instead of assembling reliable hooks themselves.

Durable currently implements checkpointed Run/Resume and a local filestore. Durable waits and distributed run management are future capabilities. Dependencies are one-way: Durable imports Pulse; Pulse does not import Durable.

## Confirm before publishing

`WithExecCheckpoint(scope, hooks)` enables checkpoint mode for the entire execution tree. It is independent of Aspect and cannot be disabled inside a subtree through WithoutAspects.

A node that is not restored follows this sequence:

1. Synchronously open or reclaim the persistent attempt for the complete node chain.
2. Create an output write set outside the entire aspect chain; body and aspect Set/Skip calls stage there.
3. After complete-chain success or normal skipping, settle unwritten Provides as skipped. Successful short-circuiting also waits for declared dependencies to arrive.
4. Validate the complete terminal result, every output, and publication targets.
5. Call Commit synchronously for persistent confirmation of the complete node record.
6. Publish outputs key by key only after confirmation, making them available downstream.

Even if an inner Retry succeeds, a later outer-aspect failure prevents complete-chain publication. A nil node return, including individual Skip calls, is completed. NoValue or all-skipped inputs means skipped. Normal skipping still saves complete results; zero-output nodes save terminal status too.

The persistent layer must atomically save terminal status and all outputs as one record. Confirming a complete record does not make multiple in-memory slots visible at one instant; publication remains sequential by key. Ordinary execution has no complete-chain staging and retains immediate Set/Skip behavior.

## Public bridge entries

| Entry | Purpose |
|---|---|
| `Graph.CheckpointNodes` | Recursively copy static nodes, mount paths, Requires/Provides, and real reflect.Type |
| `Graph.CheckpointPorts` | Copy every declared port, including unused and pass-through ports |
| `Execution.CheckpointInputs` | Copy actually supplied external ready/skipped inputs; no internal output reads |
| `WithExecCheckpoint` | Enable nonstream reliable mode for one execution and its children |
| `Execution.RestoreNodes` | Validate and replace the whole pre-start recovery plan without installing outputs yet |

For stable schema, finish assembly and pass Validate first. Returned containers are isolated from declarations, but business values are not deep-copied. Integrations must encode synchronously, never mutate values or retain references asynchronously and confirm early.

The hook type below requires `context` and Pulse payload types. This is an interface definition, not a ready-to-use storage implementation:

```go
type CheckpointHooks struct {
	OpenAttempt func(context.Context, CheckpointAttemptRequest) (CheckpointAttempt, error)
	FailAttempt func(context.Context, string) error
	OpenChild   func(context.Context, CheckpointChild) ([]NodeCheckpoint, error)
	Commit      func(context.Context, CheckpointCommit) error
}
```

All four hooks and a nonempty root scope are required. Hooks run synchronously outside engine locks, may be concurrent, and must be concurrency-safe. OpenAttempt returns persistent Scope, Index, and Exhausted. FailAttempt confirms a real failure before another attempt opens. OpenChild verifies the actual child and inputs and returns results for that scope. Commit confirms the complete node chain's result.

## Child scopes and retry budgets

A persistent Scope is neither a mounting Path nor a process-local Execution ID. The integration assigns stable attempt identities. The same mount path has a different scope under another parent attempt. After restart, an active attempt reclaims its Scope/Index; advancing Index requires a confirmed failure.

Suppose a parent step has download, extract, and summarize child nodes. If the process exits after download and extract are confirmed, Resume creates new parent and child executions, restores those two nodes, and restarts summarize from its function entry. If all children are confirmed but the parent step is not, parent wrapping and output bridging rerun without repeating child business work.

If parent Retry confirms failure and starts another attempt, it cannot borrow child results from the old attempt. A failed ancestor logically invalidates all descendants regardless of physical record deletion. OpenChild must verify Graph, Path, and actual bound inputs; changed inputs must not mix with old results.

Persistent Retry boundaries are distinguished by effective aspect-chain position and parent Scope, not function pointers or this invocation's call count. The complete root chain has Limit 1. Once final failure is confirmed, Resume must not allocate a fresh root scope or replenish retry budgets. A new complete business run needs a new RunID.

## Validate recovery as a whole

NodeCheckpoint Reason accepts completed or skipped only. Outputs must exactly cover all of that node's Provides, including implicit skipped outputs. States are ready/skipped only, with matching names, real types, and assignable values. Pending is not a confirmed result. Valid nil interfaces and typed nil values remain ready.

Recovery sets must also be closed over internal dependencies: the producers required by a restored node need their own results in the set. RestoreNodes validates and replaces the entire plan, retaining the previous plan on failure. Start revalidates against the actual definition within the validation/sealing boundary. Only a fully valid plan installs outputs, seals, and starts; an invalid last record cannot partially restore earlier records.

This integration fragment assumes host-supplied `g`, `ctx`, `in`, `scope`, complete `hooks`, and decoded `records`, inside a function returning `error`:

```go
e, err := g.NewExecution(ctx, in, pulse.WithExecCheckpoint(scope, hooks))
if err != nil {
	return err
}
defer e.Cancel()
if err := e.RestoreNodes(records); err != nil {
	return err
}
if err := e.Start(); err != nil {
	return err
}
return e.Wait()
```

Restored nodes execute no body, aspects, or Commit. After GraphStarted, they only move to terminal state and emit Finished once. They invent no Waiting, Running, uncreated Child, or child lifecycle. Snapshot contains no values and is not a recovery-plan interface.

## Failure, interruption, and uncertain confirmation

Runtime hook or restoration-protocol failures use CheckpointError. A chain containing ErrSkipped or context sentinels still means reliable failure. A shared tree failure latch prevents parent Retry or aspects from swallowing it into success. Pre-start configuration and plan errors instead return contract errors without starting.

A business function returning context.Canceled while its actual Execution context remains valid is a known business failure requiring failure and budget confirmation. Host cancellation, process exit, and reliable-hook faults leave active attempts for authoritative recovery records to resolve. The RPC error chain alone cannot decide this classification.

If storage commits but returns an error, this run stops without automatically retrying Commit or rolling the record back. Resume rereads authoritative results. Later cancellation does not withdraw confirmed records. Persistent attempts need not save raw error text that may contain sensitive business data.

## Limits and common errors

Checkpoint mode rejects stream nodes, channel declarations, and actual channels hidden in interfaces or containers. Static declarations are checked recursively before startup; dynamic values are checked at input, restoration, and commit boundaries. Cyclic objects are not rejected solely for being cyclic; codec support is a separate question.

The persistent layer owns business Version, codec ID/version, topology compatibility, input-byte fingerprints, and run ownership. Topology cannot recognize changes to closure, fixed-value, or aspect semantics automatically. Hosts must update the business version when semantics change. Reading, decoding, and version errors must stop recovery, never appear as empty history.

MemoryStore proves in-process contracts only. Local filestore supports record recovery after process exit and OS-exclusive locking. It does not promise power-loss durability, cross-machine leases, or distributed dispatch.

Checkpoints restore neither Go call stacks nor arbitrary source lines. Unconfirmed nodes restart from function entry and may repeat external effects, so this is not an exactly-once or deterministic-replay guarantee. Business code still needs idempotency, transactions, deduplication, or an outbox.

## Next steps

For application integration, continue with [Durable documentation and source](https://github.com/Luo-root/pulse-durable). Core design background is in the [checkpoint design](https://github.com/Luo-root/pulse/blob/main/docs/design/checkpoint.md). See the [error reference](./errors.md) for failure handling.
