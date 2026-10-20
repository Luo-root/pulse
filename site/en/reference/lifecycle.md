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

Wait requires successful Start, otherwise returning ErrGraphNotStarted. It waits for all nodes plus required host stream reading groups registered before Start to complete and Ack or explicitly Stop, and joins registered reliable stream work. It then latches the result, emits GraphFinished, and releases the context. Checkpoint mode gives priority to the tree's first CheckpointError already latched at this boundary and marks the graph failed; otherwise the ordinary first error or real cancellation remains. Err and graph observation reuse this final result. Each child applies the same rule at its own finalization boundary; another execution's later failure does not rewrite an established result. Wait does not drain for the host. For host-only streams, use NewExecution → SubscribeStream → Start, consume and acknowledge, then Wait. Concurrent or repeated Wait calls return the same result and all wait for GraphFinished to return. Cleanup cancellation does not turn success into cancellation.

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

This input gate applies to ordinary business nodes. Sub has no aggregate input gate: In stably binds pending/ready/skipped source slots and each internal node judges its own inputs. All skipped inlets still create a child whose independent source branches may run. Live Stream/native-channel Out binds early; ordinary Out bridges after successful child.Wait. The mount still waits for final child completion without quota.

A source node without Requires is exempt from the all-skipped test. Context cancellation interrupts input and quota waits. Managed blocking waits temporarily release quota and reacquire before business resumes, including cancellation returns and panic unwinding. Serialize I/O on one RunCtx, including Forks. Default joins permit partial values; a node requiring all values must explicitly return WaitAll's absence result. Normal skipping does not withdraw already published values.

Unwritten Provides become skipped at finalization. A nil return remains completed; a normal SkipError means skipped. A real error is captured as the first error and cancels the execution. The first captured error among parallel nodes is not guaranteed by declaration order; the final execution result follows the reliable-error priority rule above.

## Two managed-stream completion facts

A ready stream slot means the endpoint is available, not that messages are finished. The actual writer's complete aspect chain closes Done and sets completed/failed/canceled/stopped and Err. An ancestor Sub is not an EOF boundary. Snapshot separately reports the continuous acknowledgement frontier, in-flight count, and reading-group Stopped. Done may precede tail Ack; successful child cleanup does not withdraw that tail.

Receive does not acknowledge. Ack releases capacity, Nack requeues the same sequence, and cancellation never automatically Acks. Receive returns io.EOF after normal production with no remaining obligations, or explicit Stop; real failure preserves its cause. Stop clears unacknowledged obligations without advancing the frontier. Native Messages supports one reader; wrap select waiting in a wait-only WaitStream callback. FanOut competition uses Receive. See the [stream guide](../guide/streams.md).

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

## Optional aspect facts

`AspectObserver.OnAspectEvent(AspectEvent)` supplements actual aspect control points without extending Observer's five methods or changing the counts above. Install through the same WithObserver/WithExecObserver environment. Children inherit it; MultiObserver fans out only to members implementing the optional interface, isolating each panic. Callbacks run outside engine locks, can read Snapshot, and cannot turn observer panic into business failure.

Retry reports attempt_started after allocation/reclamation and attempt_finished on completion. Only actual subsequent allocation emits retry. Normal skipping never retries or counts as failure. Timeout derives deadline from its own timer cause; parent cancellation or an outer timeout cannot impersonate it. Finished follows actual inner exit, retaining panic/reliable failure as failed. NamedAspect explicitly registers custom entry/exit observation. Without an observer, no logs are emitted.

`RunCtx.Site()` shares the actual Site used by callbacks and execution snapshots. Boundaries are unique within this node execution, with direct parent boundary and attempt association. Persistent scopes and recovery markers come from the checkpoint bridge; complete restoration replays no history. See the [observe package reference](../packages/observe/index.md) for fields and phases.

## observe events and duration

| Event | Produced when | Duration |
|---|---|---|
| `pulse.graph_started` | Graph starts | 0 |
| `pulse.graph_finished` | Wait finalizes | Wall time between graph callbacks |
| `pulse.node_wait_finished` | Waiting ends through entry or termination | Wait segment |
| `pulse.node_run_finished` | Termination after entering the body | Run segment |
| `pulse.node_finished` | Failure or cancellation without Waiting/Running | 0, not measured |
| `pulse.aspect` | Optional aspect facts | Measured invocation, attempt, wait or expiry reporting; entry and retry use 0 |

Normal skipped records have nil Record.Err. A lone completed/skipped Finished generates no node timing record, including restoration and successful short-circuiting. An aspect failing before next produces node_finished to retain attribution.

Durations include synchronous observer and sink costs within the measured window, not just business CPU time. Graph duration excludes assembly, input preparation, and Flush after Wait returns.

## Views and recovery boundaries

Snapshot is available after creation, during execution, and after finalization. Nodes and slots are sampled separately, and child handles are live objects. It is neither an atomic whole-tree sample nor a persistent record. Progress views contain no business values.

A restored node only moves to its terminal state and emits Finished after GraphStarted. It repeats no body, aspects, or Commit. A restored mounting node creates no child execution, so it invents no Child or child events. See [checkpoints](./checkpoint.md) for the reliable restoration contract.

## Common errors and next steps

Confusing sealed Graph with started Execution incorrectly rejects a second run; create a fresh Execution. Omitting Wait loses graph finalization. Waiting for the same execution inside an observer can block. See the [error reference](./errors.md) for meanings and corrective actions.
