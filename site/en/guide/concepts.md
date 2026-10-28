# Core concepts

This page explains Pulse's data model and execution rules. Run the [quick start](quickstart.md) first, then use these concepts to understand branching, aggregation, reuse, and cancellation.

## Definitions and executions

Pulse has four core objects:

| Object | Contents | Lifetime |
| --- | --- | --- |
| `Key[T]` | A data name and Go type | Used with a definition |
| `Node` | ID, description, read/write declarations, function, and aspects | Part of a definition |
| `Graph` | Nodes, ports, dependencies, and mounted children | Reusable across runs |
| `Execution` | This run's inputs, slots, state, errors, and cancellation | One run |

Think of a Graph as a set of processing instructions and an Execution as one request following them. Two requests use the same instructions without sharing execution slots.

Every `g.Run(ctx, in)` creates a fresh execution. To keep the handle, call `g.NewExecution(ctx, in)`, then `e.Start()` and `e.Wait()`. An Execution can successfully start only once.

Execution isolation does not copy business objects. Captured counters and clients, and supplied maps, slices, and pointers, can still be shared. Application code owns their synchronization during concurrent runs.

## Keys: named, typed data

`pulse.NewKey[string]("summary")` identifies a string named `summary`. Keys with the same name and type are equivalent. A graph cannot mix different types for one key name. An uninitialized zero Key cannot serve as an input or port.

A node declares read permissions with `Requires` and write permissions with `Provides`. Reading an undeclared input or writing an undeclared output returns `ErrUndeclared`. Use `pulse.Deps` to combine declarations of different types.

Each key has one source: a declared host inlet or one producing node. Duplicate sources are rejected at assembly. Several downstream nodes can read the same ordinary value; channel readers compete for stream elements, so streams need additional rules. See [streams](streams.md).

## Ports: a graph's public interface

Keys are used for internal reads and writes. Ports determine what the host and parent graph can access:

- `DeclareInput` declares an external input; supply this run's value through `Inputs`.
- `DeclareOutput` declares a result the host can read from this execution.
- A child graph is wired through declared ports. Internal keys do not automatically become public ports.

Declaring one key as both inlet and outlet creates a pass-through interface. If it is optional, remains unsupplied, and no node requires it, it can still be `pending` when execution ends. That is a normal state and is not rewritten as skipped.

See [inputs and outputs](inputs-outputs.md) and [subgraphs](subgraphs.md) for declarations, wiring, and reads.

## Three slot states

A Key identifies data. Its slot in an Execution records whether this run's data has arrived.

| State | Meaning | Arrived? |
| --- | --- | --- |
| `pending` | No result supplied yet | No |
| `ready` | A value supplied, including valid zero values | Yes |
| `skipped` | Explicitly no value in this run | Yes |

Both `ready` and `skipped` finish a wait. `0`, an empty string, or a valid nil value can be business data; they do not stand in for absence. See [inputs and outputs](inputs-outputs.md) for nil input restrictions.

`Set` supplies a value; `Skip` supplies normal absence. Repeating the same state is an idempotent first write: another `Set` does not replace the first value, and another `Skip` does not change a skipped slot. `Set` and `Skip` conflict. Supplying a key twice through `Inputs` is rejected immediately by `Seed` / `SkipSeed`.

When a node returns successfully, its unwritten declared outputs automatically skip. To choose a route, write the selected output and explicitly skip the others. The [branching guide](branching.md) provides a complete example.

## When a node runs

An ordinary node follows these rules:

1. With no input dependencies, it can begin work.
2. With inputs, it waits for **all inputs** to arrive, as either `ready` or `skipped`.
3. Once every input has arrived, at least one `ready` input permits the function to run. If all inputs are `skipped`, the node skips its function and outputs.

If a summary has a value and tags skip, the report node can still enter its function. Use `TryGet` or `Batch` to collect the available values explicitly. If the business rule requires both summary and tags, return `WaitAll`'s result from the function to choose an overall skip.

This does not mean processing immediately after the first input. Aggregation waits for every route to declare its outcome, then applies the chosen policy. [Branches and joins](branching.md) explains both policies and their APIs.

## Skips, failures, and cancellation

**A skip is normal.** A node can skip one output or end the whole node through `NoValue` / a skip result. Even a graph skipped throughout can succeed; `Wait` excludes normal skips from its error result.

**A real error fails the execution.** A node returning a non-skip error causes Pulse to retain the error and cancel this execution. Waiters and cooperative functions can then exit. A node panic becomes an execution error. Failure is never silently changed into absence.

**Cancellation needs cooperation.** Use `rc.Context()` for HTTP, database calls, loops, and blocking operations. Go cannot safely terminate a function that ignores cancellation. `Wait` and `Timeout` must still wait for it to return.

## Sealing and reuse

Successful `Validate`, or a successful start of an Execution, seals the entire definition tree, including mounted children. Sealing fixes nodes and ports so that structure cannot change during execution.

Failed validation seals nothing. Complete the definition and try again. A sealed Graph can still run and be mounted elsewhere: sealing freezes the definition rather than consuming it.

After sealing, static validation structures are reused, including for graphs containing Sub mounts. Each start still checks this run's inputs and skips, effective quotas and aspects, host stream subscriptions, and checkpoint boundaries. Input values, execution slots, and restored records belong only to that Execution.

Mounting one child at two positions creates independent child Executions. The mounting position also appears in `Site.Path`, allowing observation to distinguish instances of a reused module.

## Aspects and observation

An Aspect wraps a node's wait and function, providing explicit retries and timeouts. The first declared aspect is outermost. `Retry` stages outputs per attempt and discards a failed attempt's writes when retrying. External side effects are not rolled back. See [retries and cancellation](retry-cancellation.md) for order and limitations.

`Topology` describes static structure, `Snapshot` describes one execution's state, and Observer provides events. Snapshots contain no business values, and reading a view never advances execution. Read business results from declared outlets and use the optional `observe` package to integrate event logs.

Next: [Choose a usage task](orchestration.md), or consult the [lifecycle reference](../reference/lifecycle.md).
