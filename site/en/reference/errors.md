# Errors

This page is for developers handling assembly, startup, or runtime errors in the [v0.5.1](./migration.md). Identify the stage before choosing to fix a definition, supply inputs, handle a business failure, or recover persistent records. Do not classify behavior solely by error text. Use `errors.Is` for public sentinels and `errors.As` for concrete types.

## Sentinels and corrective actions

| Error | Meaning | Next action |
|---|---|---|
| `ErrUndeclared` | Node accesses an undeclared key or writes outside Provides; host reads an undeclared or mistyped output | Check Requires/Provides or DeclareOutput and reuse the correct Key |
| `ErrConflict` | A ready slot is written skipped, or a skipped slot is written ready | Choose one state per output; check whether Only is mixed with other writes |
| `ErrDuplicateSource` | Two nodes produce one key, or an input port conflicts with an internal producer | Keep one source and use different keys for independent results |
| `ErrGraphStarted` | Execution started twice, or sealed Graph modified | Use a fresh Execution for another run or a new Graph for another topology; the message distinguishes the cases |
| `ErrGraphNotStarted` | Wait called before successful Start | Handle Start errors and Wait only after successful startup |
| `ErrNextCalledTwice` | Overlapping aspect advance or re-entry without a new attempt boundary | Do not call next concurrently; use built-in Retry |
| `ErrSkipped` | Normal absence or explicit node-level absence | Follow the absence policy; do not retry it as an ordinary fault |
| `ErrStreamStopped` | Producer stop signal after explicit reading-group Stop | Manual producers handle it explicitly; ProduceStream succeeds only if actually stopped and its callback returns the exact sentinel |
| `ErrDeliveryExpired` | Nack/Stop expired this delivery | Use the new Delivery; an old credential cannot advance acknowledgement |

Both ErrNextCalledTwice triggers share a sentinel; the message explains which one occurred. Another execution of the same definition is not node re-entry. Each Execution has independent advance state.

## Normal skipping

Get, WaitAll, and Batch.Get may return `*SkipError`, whose Keys names normally absent inputs. Its chain matches `errors.Is(err, pulse.ErrSkipped)`. An ordinary node returning it finishes skipped, settles unpublished outputs as skipped, and does not fail the run.

`Skip(rc, key)` skips one output only; a nil node return remains completed. NoValue declares normal absence for the whole node. Do not convert real business failures into NoValue, which loses the failure cause.

A wrapped skip, or an `errors.Join` / multiple-`%w` tree whose every branch is a normal skip, remains normal arrival. A mixed tree is different: `errors.Join(pulse.NoValue(), businessErr)` still fails because it contains a real failure, even though one path matches ErrSkipped. A wrapper's custom Is cannot hide failing Unwrap branches; a leaf with no underlying cause may still declare normal skipping through Is. Retry continues on the real failure without publishing its failed write set, and checkpointing does not save that failure as success or skip. Ordinary execution does not retract values already published directly before failure.

Reliable checkpoint errors and panics remain failures even when they wrap ErrSkipped. `errors.Is` / `errors.As` only establishes a matching path, not the classification of a mixed tree or execution outcome. Use the actual execution result below for application display.

## Classify the actual execution outcome

This display helper can be added to a program importing `errors` and `pulse`. Call it after successful Start and after Wait returns: the snapshot's graph status already distinguishes completion, failure, and cancellation. It does not decide persistent retry budgets:

```go
func classify(e *pulse.Execution) string {
	state := e.Snapshot().Graph
	switch state.Status {
	case pulse.GraphStatusCompleted:
		return "success"
	case pulse.GraphStatusCanceled:
		return "canceled"
	case pulse.GraphStatusFailed:
		var checkpointErr *pulse.CheckpointError
		if errors.As(state.Err, &checkpointErr) {
			return "checkpoint failure"
		}
		return "failure"
	default:
		return "not finalized"
	}
}
```

Normal skipping does not make the run's Wait return an error. Within a node, match `*pulse.SkipError` with `errors.As` on that Get / WaitAll result when absence needs special handling; exclude `CheckpointError` first and preserve other errors. Do not apply that local absence test to the overall Wait outcome: `panic(pulse.ErrSkipped)` is a failure, and an independent RPC wrapping `context.Canceled` while the execution context remains valid is also a failure. The error chain alone must not downgrade either one.

CheckpointError retains Operation and the original cause. Hook errors or panics stop the entire execution tree. Parent Retry or an outer aspect cannot make the execution succeed by swallowing that error. Storage may have committed without returning confirmation, so reread authoritative records instead of blindly repeating Commit or clearing history.

To identify host interruption, inspect the actual Execution context. A business RPC may wrap context.Canceled while the execution context remains valid; that is still a business failure requiring confirmation. The error chain alone must not refresh recovery budgets. See [checkpoints](./checkpoint.md).

## Errors without dedicated sentinels

Not every contract error has a public sentinel. Diagnose these from their stage and message:

| Stage and problem | Next action |
|---|---|
| New has no graph ID; Add has duplicate/empty node ID or mismatched key types | Correct construction and naming, then reassemble |
| Seed has nil input pointer, zero key, nil interface, or duplicate entry | Use the address of zero-value Inputs, a complete Key, SkipSeed, or new Inputs |
| Supplied key is not an input, or a required run input is missing | Fix ports and prepare complete Inputs, then create a fresh execution |
| Requires lacks a source or an output has no producer | Add a producer or correct port declaration; fix key names |
| Dependency or composition cycle | Change the structure; there is no Remove API, so rebuild an assembled cycle |
| Child input omitted, bound twice, or wrong direction/type | Check In/Out/Fix against child ports |
| Stream has no reader, independent readers, insufficient native quota, or lifecycle cycle | Add actual readers/pre-start host subscriptions, use TeeStream/FanOut, and adjust quotas/completion dependencies for native or managed semantics |
| ReadOutput before finalization | Wait after successful Start; this error is not ErrGraphNotStarted |
| YAML assembly, registry, or input resolution error | Check GraphID, uses, and name/type, then resolver and value types |
| Checkpoint mode lacks scope/hooks, contains channels, or has an invalid restoration plan | Fix integration, values, or recovery sets; rejected startup restores nothing partially |

Read all static problems from Topology.Problems before confirming with Validate. Successful Validate seals the definition; account for that boundary when inspecting problems.

## Concrete managed-stream errors

Messages supports one reader; FanOut workers use Receive. Unopened endpoints, undeclared reads, unfinished reading obligations, incomplete StreamHooks, active sources without continuation promises, and HandleMessage output overflow return concrete errors. Overflow fails immediately without waiting for its own callback. Exhaustion and commit failures are latched; swallowing them cannot produce success.

An exact ErrStreamStopped from a ConsumeStream callback first Acks the successful current item, then Stops; wrapped stop errors remain failures. Ordinary Retry cannot retract actual stream outputs or rewind inputs; proven skipped paths with no data remain normal. Register SubscribeStream before Start for host-only streams. Graph.Validate/Topology omit only missing-reader diagnostics for declared managed root outlets awaiting host connection, preserving checks for unexported streams and native channels. Start still strictly checks actual subscriptions for missing readers and splitting; definition validation does not replace startup checks. A subscribed pass-through inlet needs Seed/SkipSeed before startup.

## Runtime failure, cancellation, and outputs

A real node error latches the first error and cancels the execution. By default, Wait returns that cause after every node ends. Other concurrent nodes may complete or cancel; ordinary errors do not overwrite the first captured error. Checkpoint mode has one exception: after joining registered reliable stream work, Wait gives priority to the tree's first CheckpointError already latched at that boundary and marks the graph failed. Err, GraphFinished, Snapshot, and concurrent/repeated Wait calls reuse that final result without traversing user errors again. Each child execution finalizes at its own boundary; another execution's later failure does not rewrite an established result. A business-body panic becomes a node error. Panics from a custom error's Is/As/Unwrap methods during engine error classification also become node failures, and the engine stops traversing that faulty error. This protection does not cover panics in user-created goroutines.

After the inner function exits, Timeout checks its own time limit and parent cancellation. Actual expiry of that limit produces an error naming the node and limit, with failed status and no context.DeadlineExceeded wrapper, including when both completion paths are ready together. Inner local Cancel cleanup does not overwrite an obtained success, normal skip, or real failure. Parent cancellation still retains the context cancellation cause. Interpret canceled status together with the host context, node deadlines, and actual cause.

Host cancellation, host deadline expiry, or cancellation caused by another node's failure can also produce canceled status. Cancellation is cooperative: an unresponsive body may delay Wait. Timeout likewise waits for the inner function and does not forcibly terminate a business goroutine.

ReadOutput first verifies the output declaration, then the execution result. A valid output yields no partial value before finalization or after real failure/cancellation. Its value and state are invalid on error. Normal skipped and optional pass-through pending results have nil error.

## Observation delivery errors

Observer callbacks have no error return, and their panics are isolated. Sink failures do not automatically become business failures. Hosts check LineSink Flush/Err, AsyncSink Flush/Close, and similar interfaces. `observe.ErrNilSink` from NewRecordObserver means a nonnil Sink is required.

## Next steps

Return to [inputs and outputs](../guide/inputs-outputs.md) to verify the host boundary, or read [retry, timeout, and cancellation](../guide/retry-cancellation.md) to choose execution policy. Recovery error handling is in the [checkpoint reference](./checkpoint.md).
