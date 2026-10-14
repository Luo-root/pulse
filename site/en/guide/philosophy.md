# Design philosophy

This page explains Pulse's principal trade-offs: whether it fits a project and which layer should handle a boundary. See the [usage guides](orchestration.md) for operations and the [reference](../reference/index.md) for precise behavior.

## Dependencies follow data declarations

A node declares the keys it reads and produces. With one source per key, Pulse derives execution dependencies, structure views, and static diagnostics from these declarations.

This reduces the information you must keep in sync. Adding an input dependency means updating its declaration. The trade-off is that the workflow must be expressed through data relationships; conflicting hand-written edges and read/write declarations cannot coexist.

Topology supports inspection and validation; data arrival advances execution. Static checks still reject cycles, missing sources, and invalid ports. Data-driven execution retains structural validation.

## Definitions are reusable; state belongs to a run

Keep a Graph and create an Execution per request. Inputs, cancellation, and failure then belong to that request, without requiring a policy for resetting slots.

Isolation covers Pulse-managed runtime state. Captured business objects, caches, database clients, and mutable values belong to the host. The host supplies synchronization for sharing and a persistence layer for cross-process state.

Static subgraphs follow the same model: reuse the definition and create executions for each mounting position and attempt.

## Normal absence has its own meaning

An unselected branch or an optional operation with no result can be a normal business outcome. `skipped` means explicit absence, `pending` means not yet arrived, and `ready` means a value exists.

Zero values retain their business meaning, and downstream nodes can tell whether every route has finished. Aggregation waits for all routes, then permits processing available values by default. A node requiring every value explicitly chooses that policy with `WaitAll`.

Real errors retain their failure identity. Normal absence does not fail the execution, and a reliable storage failure cannot become normal absence merely because its error chain includes a skip cause.

## Report decidable errors early

Missing sources, duplicate sources, invalid wiring, and cycles can be identified before running. Leaving them to runtime produces waits, timeouts, and late errors that obscure the original mistake.

Assembly entries, `Validate`, and execution startup therefore check the facts available at their respective boundaries. Successful validation seals the whole definition tree so that the checked definition is the one executed. Failed validation leaves it editable.

Static analysis has limits. Waits hidden inside callbacks, external service behavior, and shared application data cannot be proven from declarations alone; they need application design and testing.

## Retry guarantees follow the data boundary

`Retry` stages each attempt's outputs independently. An attempt that fails and will retry discards its writes; success or a normal skip commits them. Downstream nodes cannot receive a failed retry attempt's results.

This guarantee concerns Pulse data. If the node has already charged a payment, sent a notification, or committed an external transaction, discarding slot writes does not undo that operation. Use business idempotency keys, transactions, an outbox, or compensation as appropriate.

The checkpoint layer additionally requires storage confirmation before publishing outputs. Saved node results still do not imply a single external call: unconfirmed functions may run again after recovery.

## Observation follows actual execution facts

Static Topology, execution Snapshot, and event records answer different questions: what the structure is, where it is now, and what has happened.

The engine updates state before emitting events, and observers are not hidden control flow. Default snapshots and records contain no business values. The host reads results through declared outlets and adds business dimensions through explicit attributes.

Snapshots sample individual entries rather than capture an atomic whole-graph instant. The host's recording and storage layers own durable audit and stable history.

## Responsibilities remain layered

| Layer | Responsibility |
| --- | --- |
| Pulse core | Single-process execution, data flow, composition, validation, state, and the checkpoint bridge |
| `observe` | Structured records, logging sinks, rendering, and buffering |
| `yaml` | Declarative topology and type/factory assembly |
| Durable | Checkpoint encoding, storage, ownership, and recovery |
| Application | Business IO, shared objects, idempotency, external transactions, and version policy |

Core uses only the standard library. Optional layers depend on core in one direction. Durable's local checkpoint recovery is implemented; durable waits, task claiming, and distributed failover remain future work.

For full rationale and historical decisions, see the [original philosophy](https://github.com/Luo-root/pulse/blob/main/docs/design/philosophy.md), [object model](https://github.com/Luo-root/pulse/blob/main/docs/design/object-model.md), and [cross-repository responsibilities](https://github.com/Luo-root/pulse/blob/main/docs/design/workflow-foundation.md).

Next: [Lifecycle reference](../reference/lifecycle.md) or the [checkpoint bridge](../reference/checkpoint.md).
