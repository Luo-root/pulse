# Retry, timeout, and cancellation

This page is for developers calling external services, handling transient faults, or controlling execution duration. First understand [inputs and outputs](./inputs-outputs.md) and normal skipping, and use the [development APIs](../reference/migration.md).

`Retry` and `Timeout` are `Aspect` functions that wrap a node's input wait, quota wait, and body. Their order determines the scope of time limits and attempts. Cancellation is a signal that the body must cooperate with.

## Add aspects to an external call

This assembly function can be added to an existing program. It needs `context`, `time`, and `github.com/Luo-root/pulse`. `fetch` is a host-supplied business function and must use its context for the request. `input` must have a source; `result` must not already have another producer.

```go
func addFetch(g *pulse.Graph, input, result pulse.Key[string],
	fetch func(context.Context, string) (string, error)) error {
	return g.Add(pulse.NewNode("fetch", "Call an external service",
		pulse.Requires(input), pulse.Provides(result),
		func(rc *pulse.RunCtx) error {
			request, err := pulse.Get(rc, input)
			if err != nil {
				return err
			}
			value, err := fetch(rc.Context(), request)
			if err != nil {
				return err
			}
			return pulse.Set(rc, result, value)
		},
		pulse.Timeout(2*time.Second),
		pulse.Retry(3, 100*time.Millisecond)))
}
```

`Timeout` is outside `Retry`: at most three attempts, with 100 ms between attempts, share one two-second limit. Input waiting, quota waiting, calls, and retry delays are all within that limit. Retry does not continue once its own layer's context is canceled.

Putting `Retry` outside `Timeout` gives each attempt a separate two-second limit. Total duration may approach three attempt limits plus delays. The host execution context still constrains every attempt.

`Retry(attempts, delay)` counts the initial call as an attempt. `attempts <= 0` means one attempt. Ordinary errors are retried, and the last error is returned if all attempts fail. Normal skipping is not retried. Execution cancellation while waiting is not retried either.

## Failed attempt outputs are not published

Each Retry attempt has an independent output write set: a staging area for that attempt's `Set` and `Skip`. Suppose the body writes a result before a later check fails:

| Stage | Attempt staging | Downstream visibility |
|---|---|---|
| First attempt writes `"draft"`, then returns an error | First write set discarded | `"draft"` was never visible |
| Second attempt writes `"final"` and returns nil | Successful write set committed | `"final"` becomes visible |
| Every attempt fails | Every write set discarded | Execution fails; hosts receive no partial result |

A successful or normally skipped attempt commits its existing writes. Skipping does not withdraw values already produced. An inner Retry commits into the outer write set; publication proceeds only when the outer attempt succeeds. Writes by aspects outside Retry are outside its attempt boundary.

All outputs are checked before a batch commits; if any conflicts, none of that batch is published. Publication itself still happens key by key, with no guarantee that all slots become visible at the same instant. Keep writes on the synchronous node and aspect call chain. Do not leave background goroutines writing after a node returns or write the same output concurrently.

An attempt can read its own staged writes. A later attempt cannot read discarded values. Do not depend on a previous attempt's output as implicit state.

## Business code must protect external effects

Write sets manage Pulse outputs only. Completed database commits, requests, and file writes are not rolled back. For example, a request may succeed while its response is lost; returning an error can cause Retry to issue the request again.

Use business idempotency keys, database transactions, deduplication, an outbox, or compensation where duplicates matter. Moving the external operation to the end of the function does not remove the window in which it succeeds but its confirmation is lost. Checkpoints share this boundary; see the [checkpoint reference](../reference/checkpoint.md).

## Timeout waits for the body to exit

`Timeout(d)` sets an inner deadline. Expiry cancels that context, and the aspect always waits for the inner function to return. If the body ignores context and runs for 300 ms, a 40 ms limit may still return only after that body exits. Timeout limits permission to continue work; it does not forcibly terminate a goroutine. Waiting prevents node finalization from racing with a still-running body on the same outputs.

After the inner function exits, Timeout checks its own context on both completion paths. An expired deadline produces an error naming the node and limit, classified as `failed` without wrapping `context.DeadlineExceeded`. Inner code returning nil after observing expiry, or completion and expiry becoming ready together, cannot turn the timeout into success. Before cancellation or expiry, the original inner error is retained; parent cancellation returns the context cancellation cause.

Do not infer host interruption from a context sentinel or `canceled` status alone. Check the host context and the installed deadlines as well. `Timeout(0)` creates an already-expired deadline; it does not disable the timeout. Omit the aspect when no limit is needed.

Ordinary outputs already published are not withdrawn by Timeout. Use Retry staging to isolate failed attempts, or checkpoint mode to publish only after complete-chain confirmation.

## Cancel one execution

This host fragment needs existing `g`, `in`, and `ctx` and belongs in a function returning `error`:

```go
e, err := g.NewExecution(ctx, in)
if err != nil {
	return err
}
defer e.Cancel()
if err := e.Start(); err != nil {
	return err
}
// The application decides to stop this execution.
e.Cancel()
return e.Wait()
```

`Cancel` signals cancellation; `Wait` waits for every node and callback. Cancel does not replace Wait. Canceling one execution changes neither the definition nor other executions of that graph. HTTP, database operations, and business loops should use `rc.Context()`. Cancellation cannot forcibly interrupt an uncooperative blocking operation.

## Subgraphs, graph aspects, and streams

Timeout on a mounting step limits the whole child graph, including child quota waiting. Retry on a mounting step creates a new child execution per attempt. A failed attempt discards the staged outputs bridged back into the parent.

Graph aspects declared with `WithAspects` wrap every node at that layer and are inherited by child nodes. The same Timeout may limit ordinary nodes, whole subgraph mounting steps, and individual child nodes. Put different limits on the relevant nodes or mounts when their scales differ. `WithoutAspects` cuts inheritance inside the child; it does not remove aspects from the parent-side mounting step.

`ProduceStream`, `ConsumeStream`, `TeeStream`, and the original native helpers accept `WithStreamAspects(aspects...)` through the ordinary node chain. Logging, observation, and cooperative Timeout work here. A timeout waits for inner work to exit and preserves failure; published prefixes remain visible.

Managed streams use explicit `HandleMessage` attempts for the current item, or `WithMessageHandling(WithMessageRetry(...))` on ConsumeStream/TeeStream. A failed item is retried before the next item, and its failed staged outputs remain unpublished. Defaults are one attempt and 64 outputs; every batch must fit its output capacity, with immediate overflow failure. Exhausted failure cannot be swallowed, and closures, references, and external effects do not roll back. Send progressive tokens outside this finite boundary with Send/SendAt, continuing by an explicit source cursor or starting a new Execution, without automatically splicing generated versions.

Ordinary Retry write sets cannot rewind actual external stream inputs or retract published outputs, so they reject these before I/O. Proven skipped paths with no data remain normal. A complete finite pipeline inside a Sub bridging only ordinary results can be retried together, without inheriting Retry into its internal stream nodes. Native Produce/Tee still reject channel publication staging; rerunning native Consume provides no Ack or rewind guarantees. See [streams](./streams.md) for examples and the migration matrix.

## Common errors and next steps

A custom aspect must not call `next` concurrently or implement retries by repeatedly calling `next`. Re-entry without a fresh attempt boundary triggers `ErrNextCalledTwice`. Use built-in Retry, and let a custom aspect advance at most once per invocation or short-circuit explicitly.

Read [static subgraphs](./subgraphs.md) to reuse a complete processing stage. See the [error reference](../reference/errors.md) for classification and follow-up actions, and the [lifecycle reference](../reference/lifecycle.md) for states and callback order.
