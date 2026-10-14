# Lifecycle

This page defines the [development version](./migration.md) contract for developers controlling Execution, implementing Observer, or interpreting snapshots. Usage examples are in [inputs and outputs](../guide/inputs-outputs.md), [subgraphs](../guide/subgraphs.md), and [observation](../guide/observability.md).

## Definition assembly and sealing

A Graph is created, assembled, and sealed. It holds no run context, values, or first error.

| Action | Check or effect | Seals the definition? |
|---|---|---|
| Add, port declarations, Sub | Register nodes, types, unique sources, or mount binds | No |
| Topology | Read structure, ports, binds, children, and Problems | No |
| Failed Validate | Return a structural problem | No layer is sealed |
| Successful Validate | Complete static validation | This graph and its entire mounted subtree |
| NewExecution | Accept inputs and calculate effective configuration | No |
| Failed Start | Return structural, input, or restoration errors | No sealing caused by this failure |
| Successful Start | Begin this run after validation | The entire definition tree |

After sealing, definition changes through Add, DeclareInput, DeclareOutput, and Sub return errors wrapping `ErrGraphStarted`. A sealed child can still be mounted into another unsealed parent, and definitions can run repeatedly or concurrently. Validation and sealing share one assembly boundary, so the sealed definition is the one that was checked.

Static validation checks Requires sources, producers or pass-through inputs for declared outputs, dependency cycles, mount binds, and visible stream relationships. It invokes no business callbacks and cannot validate their computation. Use Topology for a problem list without sealing; successful Validate changes assembly state.

## Control one execution

`NewExecution(ctx, in, opts...)` creates an execution and copies input entries. It checks that every external input required by a node has a value or explicit skip. Effective configuration is fixed at creation; subsequent input entry changes do not affect it.

Start rechecks per-run inputs against the current definition before sealing. On success, it synchronously emits GraphStarted and then launches node goroutines. Start may succeed once only; a second call returns ErrGraphStarted. Rejected startup sets no started flag and emits no start event. Correct a repairable structure and retry startup, or create a new execution when more input data is required.

Wait requires successful Start, otherwise returning ErrGraphNotStarted. It waits for all nodes, latches the final result, emits GraphFinished, and releases the execution context. Concurrent or repeated Wait calls return the same latched result and all wait for GraphFinished to return. Cleanup cancellation does not turn success into cancellation.

With Start but no Wait, the graph snapshot remains running even after nodes finish, and no GraphFinished is emitted. Cancel signals cancellation and does not replace Wait.

## Graph, node, and slot states

| Object | States |
|---|---|
| Graph execution | `pending`, `running`, `completed`, `failed`, `canceled` |
| Node | `pending`, `waiting`, `running`, `completed`, `skipped`, `failed`, `canceled` |
| Data slot | `pending`, `ready`, `skipped` |

There is no graph-level skipped state. An execution with every node normally skipped is completed.

A normal node goes pending → waiting → running → terminal. All-skipped inputs move it directly from waiting to skipped. Aspect short-circuiting and restored nodes may expose only a terminal state; do not invent missing wait or run segments. States change before callbacks, and Retry does not move states backward.

A slot transitions once from pending to ready or skipped. Both represent arrival and never reopen. A repeated same-state write retains the first write; an opposite-state write returns ErrConflict. Duplicate input collection writes are rejected earlier instead of silently using slot idempotence.

## Conditions for entering a node body

1. Wait for every Requires key to arrive as ready or skipped.
2. If inputs exist and all are skipped, skip the node normally.
3. Otherwise acquire quota; a child mounting step consumes no quota itself.
4. Enter the body after cancellation checks pass.

A source node with no Requires is exempt from the all-skipped test. Context cancellation interrupts input and quota waits. Default joins permit partial values; a node requiring all values must explicitly return WaitAll's absence result. Normal skipping does not withdraw already published values.

Unwritten Provides become skipped at finalization. A nil return remains completed; a normal SkipError means skipped. A real error is captured as the first error and cancels the execution. The first captured error among parallel nodes is not guaranteed by declaration order.

## Observer counts and ordering

These counts apply to successfully started executions eventually finalized by Wait:

| Callback | Count | Call location |
|---|---|---|
| GraphStarted | 1 | Start caller goroutine, before all node callbacks |
| GraphFinished | 1 | Wait caller goroutine, after all node callbacks |
| NodeWaiting | At most 1 per node | Node goroutine |
| NodeRunning | At most 1 per node | Node goroutine |
| NodeFinished | 1 per node | Node goroutine |

Retry attempts do not repeat Waiting or Running. Rejected start validation emits no graph or node events. Callbacks are synchronous and may run concurrently across nodes. Observers must be concurrency-safe and avoid prolonged blocking. Panics are isolated, and Observer has no error return that affects execution.

Do not call Start/Wait on the same execution inside a callback; callbacks sit on their call chains and may wait for themselves. Results are finalized in GraphFinished, where ReadOutput is permitted.

## observe events and duration

| Event | Produced when | Duration |
|---|---|---|
| `pulse.graph_started` | Graph starts | 0 |
| `pulse.graph_finished` | Wait finalizes | Wall time between graph callbacks |
| `pulse.node_wait_finished` | Waiting ends through entry or termination | Wait segment |
| `pulse.node_run_finished` | Termination after entering the body | Run segment |
| `pulse.node_finished` | Failure or cancellation without Waiting/Running | 0, not measured |

Normal skipped records have nil Record.Err. A lone completed/skipped Finished generates no node timing record, including restoration and successful short-circuiting. An aspect failing before next produces node_finished to retain attribution.

Durations include synchronous observer and sink costs within the measured window, not just business CPU time. Graph duration excludes assembly, input preparation, and Flush after Wait returns.

## Views and recovery boundaries

Snapshot is available after creation, during execution, and after finalization. Nodes and slots are sampled separately, and child handles are live objects. It is neither an atomic whole-tree sample nor a persistent record. Progress views contain no business values.

A restored node only moves to its terminal state and emits Finished after GraphStarted. It repeats no body, aspects, or Commit. A restored mounting node creates no child execution, so it invents no Child or child events. See [checkpoints](./checkpoint.md) for the reliable restoration contract.

## Common errors and next steps

Confusing sealed Graph with started Execution incorrectly rejects a second run; create a fresh Execution. Omitting Wait loses graph finalization. Waiting for the same execution inside an observer can block. See the [error reference](./errors.md) for meanings and corrective actions.
