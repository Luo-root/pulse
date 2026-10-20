# Checkpoints

This page is for developers implementing durable execution integration, storage, or codecs. First understand [lifecycle](./lifecycle.md), [Retry write sets](../guide/retry-cancellation.md), and [static subgraphs](../guide/subgraphs.md), and use the [development version](./migration.md). Business hosts needing recovery should normally use the independent [pulse-durable](https://github.com/Luo-root/pulse-durable) Run/Resume APIs instead of assembling reliable hooks themselves.

Durable provides checkpointed Run/Resume, a local filestore, and storage and process recovery for managed streams; integration is tracked in [Durable #21](https://github.com/Luo-root/pulse-durable/issues/21). This page specifies Core's bridge. Use matching local checkouts; the maintainer's tags/releases determine release status. Durable waits and distributed run management remain future capabilities. Dependencies are one-way: Durable imports Pulse; Pulse does not import Durable.

## Confirm before publishing

`WithExecCheckpoint(scope, hooks)` enables checkpoint mode for the entire execution tree. It is independent of Aspect and cannot be disabled inside a subtree through WithoutAspects.

A node that is not restored follows this sequence:

1. Synchronously open or reclaim the persistent attempt for the complete node chain.
2. Create an output write set outside the entire aspect chain; body and aspect Set/Skip calls stage there.
3. After complete-chain success or normal skipping, settle unwritten Provides as skipped. Successful short-circuiting of ordinary business nodes also waits for declared dependencies; Sub commits follow live In wiring rules.
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
| `WithExecCheckpoint` | Enable complete-chain reliable mode for an execution and its children; managed streams additionally need StreamHooks |
| `Execution.RestoreNodes` | Validate and replace the whole pre-start recovery plan without installing outputs yet |

For stable schema, finish assembly and pass Validate first. Graph.Validate/Topology omit only missing-reader diagnostics for declared managed root outlets awaiting host connection; they count no actual host, permit no unexported stream, and change no native-channel checks. Schema may therefore be frozen before BeforeStart registers subscriptions; Start still strictly checks actual readers and splitting. Returned containers are isolated from declarations, but business values are not deep-copied. Integrations must encode synchronously, never mutate values or retain references asynchronously and confirm early.

The hook type below requires `context` and Pulse payload types. This is an interface definition, not a ready-to-use storage implementation:

```go
type CheckpointHooks struct {
	OpenAttempt func(context.Context, CheckpointAttemptRequest) (CheckpointAttempt, error)
	FailAttempt func(context.Context, string) error
	OpenChild   func(context.Context, CheckpointChild) ([]NodeCheckpoint, error)
	Commit      func(context.Context, CheckpointCommit) error
	StreamHooks *CheckpointStreamHooks // Optional; all six functions are required when supplied.
}
```

All four hooks and a nonempty root scope are required. Hooks run synchronously outside engine locks, may be concurrent, and must be concurrency-safe. OpenAttempt returns persistent Scope, Index, and Exhausted; an active complete-node root attempt may also carry the Values subset described below. FailAttempt confirms a real failure before another attempt opens. OpenChild verifies the actual child and inputs and returns results for that scope. Commit confirms the complete node chain's result.

OpenChild has one strict business-result signal: after fully validating history and reliably confirming a final business failure in the scope, the bridge may directly return `*CheckpointScopeFailure{Scope: request.Attempt}` with no results. Before installing a restore plan, Core returns that failure to the parent chain: parent Retry may advance within its remaining budget, and a parent aspect may catch it. Confirmed aliases do not rerun and failed streams do not reopen. Only a nonnil, nonempty, exact-scope direct signal is accepted; wrapping it, returning a wrong scope or accompanying results, or using the type from another hook remains a reliable failure. For a root scope the integration returns the same business failure before decoding, restore events, and Start; the execution has not started, so no completed status is invented. Recovery does not reconstruct the original business error text or identity.

A confirmed stream alias across deeper Sub layers may also restore its source's confirmed failed descriptor, but only as a local read-only failed endpoint with no messages and no Open call. Receiving propagates business failure rather than successful EOF; production and acknowledgement remain forbidden. Only Sub aliases may carry this failed reference; an ordinary source node cannot present it as a successful checkpoint. Canceled references are still not accepted as confirmed business failure. The integration must verify historical identity, the failed source root, and valid ancestry without relaxing Open/Send/Ack.

## Child scopes and retry budgets

A persistent Scope is neither a mounting Path nor a process-local Execution ID. The integration assigns stable attempt identities. The same mount path has a different scope under another parent attempt. After restart, an active attempt reclaims its Scope/Index; advancing Index requires a confirmed failure.

Suppose a parent step has download, extract, and summarize child nodes. If the process exits after download and extract are confirmed, Resume creates new parent and child executions, restores those two nodes, and restarts summarize from its function entry. If all children are confirmed but the parent step is not, parent wrapping and output bridging rerun without repeating child business work.

If parent Retry confirms failure and starts another attempt, it cannot borrow old child results. A failed ancestor invalidates descendants regardless of physical deletion. OpenChild verifies Graph, Path, actual Fix values, and the stable In sources in Bindings. In may remain pending; it must not wait for every input. Keys in Bindings do not participate in value fingerprints; already-arrived In entries in Inputs remain for observation compatibility. The persistent layer combines root-input fingerprints, topology/Version, and binding identities to reject incompatible results. A process-local Execution ID is not a stable identity.

Persistent Retry boundaries are distinguished by effective aspect-chain position and parent Scope, not function pointers or this invocation's call count. The complete root chain has Limit 1. Once final failure is confirmed, Resume must not allocate a fresh root scope or replenish retry budgets. A new complete business run needs a new RunID.

## Validate recovery as a whole

NodeCheckpoint Reason accepts completed or skipped only. Outputs must exactly cover all of that node's Provides, including implicit skipped outputs. States are ready/skipped only, with matching names, real types, and assignable values. Pending is not a confirmed result. Valid nil interfaces and typed nil values remain ready.

Recovery sets must be closed over ordinary-value dependencies: internal ordinary producers required by a restored business node need their own results in the set. Managed streams recover through independent source records without requiring the writer's complete node to have finished. A Sub's Requires describes In wiring rather than a completion gate for the whole mount; unused inlets may remain pending. RestoreNodes validates and replaces the entire plan, retaining the previous plan on failure. Start revalidates against the actual definition within the validation/sealing boundary. Only a fully valid plan installs outputs, seals, and starts; an invalid last record cannot partially restore earlier records.

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

The bridge may directly return `*CheckpointNotStarted` only when the call context was canceled before any reliable operation began. Core additionally requires the applicable business or control context for this call to be canceled before retaining interruption semantics; cancellation from the same parent can reach these contexts in either order. If both remain healthy, or the signal is wrapped, it remains a reliable failure, as does an ordinary context error. Never use this signal after calling Store: even context.Canceled can follow an unacknowledged write. The signal confirms no business failure and replenishes no budget.

## Failure, interruption, and uncertain confirmation

Runtime hook or restoration-protocol failures use CheckpointError. A chain containing ErrSkipped or context sentinels still means reliable failure. A shared tree failure latch prevents parent Retry or aspects from swallowing it into success. Pre-start configuration and plan errors instead return contract errors without starting.

Host cancellation, process exit, and reliable-hook faults leave active attempts for authoritative recovery records to resolve. A node returning nil or normal skip after local Cancel also remains active if the complete node chain's own context is canceled: no success/skip commit, no business-failure confirmation, and no persistent-budget consumption. Cleaning up an inner local context while the complete chain's context remains valid preserves the obtained result. Actual expiry of this layer's Timeout and parent cancellation still apply.

A real business failure still requires confirmation using the owning Execution's context even after node-local cleanup; cleanup must not replenish its budget. An RPC returning context.Canceled/DeadlineExceeded while the owning execution context remains valid is also a business failure. The RPC error chain alone cannot decide this classification.

Before accepting a reliable operation, check its applicable business and control contexts; cancellation in either prevents a new operation. Real failure confirmation still uses the owning Execution, independently of node-local cleanup. Once accepted, the entire synchronous call uses the control context at the corresponding layer. Internal business fail-fast immediately wakes business nodes without canceling an already entered reliable call. Host cancellation, Execution.Cancel, explicit parent-node cancellation, and applicable deadlines or Timeouts still interrupt reliable waits, with context values preserved. Shared stream Open belongs to the root execution; canceling one Receive only interrupts that wait. Host Ack uses the host Execution's contexts, while parent-side tail confirmation uses the actual caller's contexts. If storage commits but returns an error, this run stops without automatically retrying Commit or rolling the record back. Resume rereads authoritative results. Later cancellation does not withdraw confirmed records. Persistent attempts need not save raw error text that may contain sensitive business data.

A node's completed or normal skipped conclusion is fixed before reliable Commit. After Commit and publication succeed, node finalization and engine observation reuse the confirmed conclusion without inspecting that business error again. Later host cancellation or real failures from other nodes still determine the run's terminal state; confirmed records remain available.

## Incremental managed-stream confirmation

The ordinary four-hook configuration remains valid. `Key[Stream[T]]` additionally requires complete StreamHooks. Hooks execute synchronously outside engine locks with the same concurrency, panic, and reliable-failure-latching contract:

| StreamHooks entry | Required confirmation |
|---|---|
| `Open` | Open or reclaim the descriptor, message tail, continuous acknowledgement frontier, and source cursor |
| `Append` | Atomically save a message with its SendAt cursor before local delivery |
| `OpenMessage` | Reclaim this message's persistent attempt budget and last committed explicit state |
| `FailMessage` | Confirm a real failed attempt before advancing its budget |
| `Commit` | Confirm input Ack, explicit state, ordinary results, output messages, or Stop together |
| `Finish` | Confirm the actual writer's complete-chain failure; success is atomic with the complete node Commit |

Messages, cursors, acknowledgements, state, and outputs have independent records. Local channels are never serialized. Complete NodeCheckpoint stream outputs use a `CheckpointOutput.Stream` descriptor, mutually exclusive with Value. `CheckpointCommit.Streams` contains only actual owned writers' terminal results and confirms them atomically with the node. Sub aliases do not own the writer and cannot publish EOF early. A stream-only pass-through Sub may complete before its actual source; an empty terminal in its historical reference does not mean production has ended. Recovery must reclaim the authoritative source through Open: known successful terminals cannot regress, and active sources require Resumed plus a declaration that they can continue from Position. Restored aliases and a continuing writer share one endpoint across the execution tree, with a single Open, tail installation, and delivery ledger. Every later alias still validates the stable source, generation, and known terminal.

HandleMessage is an explicit finite transaction. Failed outputs remain unpublished; exhausted failure cannot be swallowed into success. Defaults are one attempt and 64 staged outputs, with each output batch no larger than its stream capacity. One reliable Commit confirms input acknowledgement, explicit `GetMessageState` / `SetMessageState` state, ordinary results, and output messages before local stream publication and Ack. Ordinary Set/Skip results also join the complete-chain outer frame and remain unpublished until that chain is confirmed. Closures, referenced-object mutations, and external effects are outside the rollback boundary. State APIs work only inside persistent HandleMessage callbacks; construct replacement values and Set them explicitly.

When messages have been confirmed but the complete node has not committed, `CheckpointAttempt.Values []CheckpointOutput` allows an **active complete-node root attempt** to return the ordinary-output subset contributed by those confirmed messages, without stream endpoints. Before the business chain, Core restores these values into the checkpoint outer frame. It does not publish execution slots early; publication still requires successful complete Node Commit. This is not a complete NodeCheckpoint and does not fill outputs without contributions. Retry attempts and the nested Attempt.Values returned by OpenMessage must be empty. No-stream behavior and failed-ancestor invalidation remain unchanged.

Stream identity combines Scope/NodeID/Key/Group and persistent Generation. Execution ID is observation only. Aliases share one ledger; stale deliveries cannot advance another generation. AckedThrough is a continuous frontier, not the largest acknowledged sequence. Persistent message budgets use the logical source, group, sequence, processing node, and parent attempt; raising the generation during recovery does not replenish attempts.

A completed source does not rerun its body, but its restored branch rebuilds the endpoint, Done, Snapshot, and unacknowledged tail before Finished. Normal child cleanup does not withdraw the tail. An active source must explicitly promise Position/SendAt continuation through WithStreamSource; restoring an active source without that promise fails. See the [stream guide](../guide/streams.md) for finite message retry versus progressive tokens.

## Limits and common errors

Native channel declarations and helpers, plus actual channels hidden inside ordinary values, interfaces, or containers remain rejected. Declare managed endpoints independently as Key[Stream[T]]; hiding them in ordinary values cannot bypass the protocol. Static declarations are checked recursively before startup; dynamic values are checked at input, restoration, and commit boundaries. Cyclic objects are not rejected solely for being cyclic; codec support is a separate question.

The persistent layer owns business Version, codec ID/version, topology compatibility, input-byte fingerprints, and run ownership. Topology cannot recognize changes to closure, fixed-value, or aspect semantics automatically. Hosts must update the business version when semantics change. Reading, decoding, and version errors must stop recovery, never appear as empty history.

MemoryStore proves in-process contracts only. Local filestore supports record recovery after process exit and OS-exclusive locking. It does not promise power-loss durability, cross-machine leases, or distributed dispatch.

Checkpoints restore neither Go call stacks nor arbitrary source lines. Unconfirmed nodes restart from function entry and may repeat external effects, so this is not an exactly-once or deterministic-replay guarantee. Business code still needs idempotency, transactions, deduplication, or an outbox.

## Next steps

For application integration, continue with [Durable documentation and source](https://github.com/Luo-root/pulse-durable). Core design background is in the [checkpoint design](https://github.com/Luo-root/pulse/blob/main/docs/design/checkpoint.md). See the [error reference](./errors.md) for failure handling.
