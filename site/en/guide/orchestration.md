# Orchestration guide

This page is for Go developers who have completed the [quick start](./quickstart.md) and need to organize multiple processing steps. It explains how to choose an orchestration approach and links to inputs, branches, subgraphs, and streams.

The examples on this site use the v0.5.1 object model. Pin the dependency as described in [installation and versions](./installation.md).

## Define a flow through data relationships

Nodes declare `Requires` (the keys they read) and `Provides` (the keys they write). Pulse derives dependencies from these declarations. The host does not create edge objects or arrange the execution order.

A `Key[T]` identifies data with a Go type. Within one graph, each key has one source: an external input port or one node's output. Several nodes may read the same ordinary value. For several results, give the producers distinct output keys.

A node waits until every declared input has arrived before entering its body. Arrival includes `ready`, which carries a value, and `skipped`, which represents normal absence. At least one ready input permits the body to run; when all inputs are skipped, the node also skips. A source node with no inputs is eligible immediately. See [branches and joins](./branching.md).

## Choose a topic

| Goal | Page | Main operations |
|---|---|---|
| Supply request or host data and read the result | [Inputs and outputs](./inputs-outputs.md) | `DeclareInput`, `Seed`, `DeclareOutput`, `ReadOutput` |
| Select a route and combine partial or complete results | [Branches and joins](./branching.md) | `Only`, `Skip`, `FanOut`, `Join`, `WaitAll` |
| Retry a failed step and set execution time limits | [Retry, timeout, and cancellation](./retry-cancellation.md) | `Retry`, `Timeout`, `Execution.Cancel` |
| Use one flow as a step in another | [Static subgraphs](./subgraphs.md) | `Sub`, `In`, `Out`, `Fix` |
| Produce and consume data continuously | [Streams](./streams.md) | `ProduceStream`, `ConsumeStream`, `TeeStream`, `SubscribeStream` |
| Move topology into a declaration file | [YAML assembly](./assembly.md) | `Registry`, `Load`, `SeedPlan`, `Analyze` |
| Inspect progress, record lifecycle events, or draw the graph | [Observation and diagnostics](./observability.md) | `Observer`, `Snapshot`, `Topology.Mermaid` |

## Recommended assembly and execution order

1. Define keys, a graph identity, and a description; create the `Graph`.
2. Declare external inputs and outputs, then add nodes or mount assembled child graphs.
3. Once assembly is complete, call `Validate` to detect missing sources, invalid outputs, dependency cycles, and visible stream problems early.
4. Prepare `Inputs` for this run, then create and start an `Execution`.
5. Call `Wait` until every node has terminated, then read declared outputs.

A `Graph` stores a reusable definition. An `Execution` stores one run's inputs, slots, states, error, and cancellation domain. Every `g.Run(ctx, in)` creates a fresh execution. Use `NewExecution`, `Start`, and `Wait` separately for results or progress. For host-only root streams, you may Validate to freeze the definition, then create the execution, SubscribeStream, and Start. Validate/Topology omit only missing-reader diagnostics for declared managed root outlets, without counting actual host readers, allowing unexported streams, or changing native-channel rules. Start still strictly checks missing readers and splitting against actual subscriptions. Read and acknowledge before Wait, or explicitly Stop. Wait includes required host reading obligations.

A successful `Validate` or `Start` seals the entire definition tree, preventing changes to nodes and ports. A sealed definition can still run repeatedly or be mounted as a child elsewhere. Failed validation seals no layer. `Validate` is therefore not a query without side effects; use `Topology` to inspect structure and problems without sealing.

## Concurrency and configuration

Different nodes may run concurrently once inputs arrive. `WithMaxRunning(n)` limits nodes holding business quota. Input waits and Sub mounts consume none; blocked managed Send/Receive and related waits temporarily release it and reacquire before business resumes, cancellation returns, or panic unwinds. A purely managed pipeline can therefore progress with quota 1. Native channel helpers still hold quota throughout and retain conservative capacity checks. Arbitrary business I/O does not release quota automatically. `n <= 0` means unlimited.

A child without its own quota declaration inherits the parent's quota pool. A child that declares a quota uses an independent pool for that branch, replacing the inherited pool. It is not constrained by both parent and child limits. Each root execution has its own pool; multiple root executions have no automatic shared global limit.

For observers and quotas, precedence is this run's `WithExec*` configuration, the definition's `With*` declaration, then the parent execution environment. Graph aspects are additive: inherited aspects, definition aspects, execution aspects, and node aspects, from outermost to innermost. Put a limit on the relevant node or mounting step when it should affect only that step. See [retry, timeout, and cancellation](./retry-cancellation.md).

## Common assembly errors

- Two nodes produce the same key: use distinct keys for distinct results and join them explicitly; later writes do not overwrite earlier values.
- An external input is also produced inside the graph: choose one source, either host data or internal computation.
- A required input is missing for this run: use `Seed` for a value or `SkipSeed` for explicit absence.
- A cycle is used to express repetition: dependency cycles cannot start; organize a business loop inside a node or start a new execution from the host.
- Assembly continues after successful validation: finish all declarations before validation; build a new definition for another topology.

## Next steps

Read [inputs and outputs](./inputs-outputs.md) to establish the host boundary, then choose a topic above. Exact states, callback counts, and sealing rules are in the [lifecycle reference](../reference/lifecycle.md). Checkpoint integration has a separate [reference](../reference/checkpoint.md).
