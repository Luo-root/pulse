# Errors

This page is for developers handling assembly, startup, or runtime errors in the [development version](./migration.md). Identify the stage before choosing to fix a definition, supply inputs, handle a business failure, or recover persistent records. Do not classify behavior solely by error text. Use `errors.Is` for public sentinels and `errors.As` for concrete types.

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

Both ErrNextCalledTwice triggers share a sentinel; the message explains which one occurred. Another execution of the same definition is not node re-entry. Each Execution has independent advance state.

## Normal skipping

Get, WaitAll, and Batch.Get may return `*SkipError`, whose Keys names normally absent inputs. Its chain matches `errors.Is(err, pulse.ErrSkipped)`. An ordinary node returning it finishes skipped, settles unpublished outputs as skipped, and does not fail the run.

`Skip(rc, key)` skips one output only; a nil node return remains completed. NoValue declares normal absence for the whole node. Do not convert real business failures into NoValue, which loses the failure cause.

Reliable checkpoint errors remain failures even when they wrap ErrSkipped. Classify CheckpointError first.

## Classify reliable errors first

This display helper can be added to a program importing `context`, `errors`, and `pulse`. It classifies errors but does not decide persistent retry budgets:

```go
func classify(err error) string {
	if err == nil {
		return "success"
	}
	var checkpointErr *pulse.CheckpointError
	if errors.As(err, &checkpointErr) {
		return "checkpoint failure"
	}
	if errors.Is(err, pulse.ErrSkipped) {
		return "normal absence"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancellation or deadline"
	}
	return "failure"
}
```

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
| Stream has no reader, independent readers, insufficient capacity, or lifecycle cycle | Add actual readers, use Tee/FanOut, adjust quotas or cross-graph completion dependencies |
| ReadOutput before finalization | Wait after successful Start; this error is not ErrGraphNotStarted |
| YAML assembly, registry, or input resolution error | Check GraphID, uses, and name/type, then resolver and value types |
| Checkpoint mode lacks scope/hooks, contains channels, or has an invalid restoration plan | Fix integration, values, or recovery sets; rejected startup restores nothing partially |

Read all static problems from Topology.Problems before confirming with Validate. Successful Validate seals the definition; account for that boundary when inspecting problems.

## Runtime failure, cancellation, and outputs

A real node error latches the first error and cancels the execution. Wait returns that cause after every node ends. Other concurrent nodes may complete or cancel without overwriting the first captured error. A business-body panic becomes a node error.

When Timeout handles expiry itself, it creates an error naming the node and limit, with failed status and no context.DeadlineExceeded wrapper. If the inner result is received first, its original error is retained. Inner completion and deadline expiry can be ready together, allowing context.DeadlineExceeded to return directly with canceled status. A canceled state alone therefore does not prove host cancellation; check the host context, node deadlines, and actual cause.

Host cancellation, host deadline expiry, or cancellation caused by another node's failure can also produce canceled status. Cancellation is cooperative: an unresponsive body may delay Wait. Timeout likewise waits for the inner function and does not forcibly terminate a business goroutine.

ReadOutput first verifies the output declaration, then the execution result. A valid output yields no partial value before finalization or after real failure/cancellation. Its value and state are invalid on error. Normal skipped and optional pass-through pending results have nil error.

## Observation delivery errors

Observer callbacks have no error return, and their panics are isolated. Sink failures do not automatically become business failures. Hosts check LineSink Flush/Err, AsyncSink Flush/Close, and similar interfaces. `observe.ErrNilSink` from NewRecordObserver means a nonnil Sink is required.

## Next steps

Return to [inputs and outputs](../guide/inputs-outputs.md) to verify the host boundary, or read [retry, timeout, and cancellation](../guide/retry-cancellation.md) to choose execution policy. Recovery error handling is in the [checkpoint reference](./checkpoint.md).
