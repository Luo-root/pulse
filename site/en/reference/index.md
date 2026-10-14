# Reference

This section is for developers already using Pulse who need exact behavior or integration boundaries. Steps and complete programs are in the [guides](../guide/orchestration.md). Reference pages cover lifecycle, errors, checkpoint protocols, and migration.

## Version scope

These documents describe the development object model on current main. The latest published version is **v0.4.1**; the next release has not been published. To run site examples, use the fixed development commit in the [migration reference](./migration.md). Do not mix the released API from an unversioned installation with this reference.

## Find a contract

| Question | Page |
|---|---|
| When definitions seal, which states exist, how often callbacks occur | [Lifecycle](./lifecycle.md) |
| What errors mean and whether to change declarations or handle a runtime failure | [Errors](./errors.md) |
| When outputs are confirmed and published, what recovery restores, how retry budgets persist | [Checkpoints](./checkpoint.md) |
| How to migrate from v0.4.1 or transitional development APIs | [Migration](./migration.md) |

## Objects and interfaces

| Object or entry | Responsibility | Usage |
|---|---|---|
| `Graph` | Reusable definition, ports, static mounts, default configuration | New, DeclareInput/Output, Add, Sub, Validate |
| `Inputs` | External inputs for one run | Seed, SkipSeed |
| `Execution` | One run's states, slots, error, and cancellation | NewExecution, Start, Wait, Cancel |
| `ReadOutput` | Declared outputs after successful finalization | Check error first, then handle the three states |
| `Topology` | Definition structure and static problems | Topology, Mermaid; no execution or sealing |
| `Snapshot` | Value-free state sample of one execution | Execution.Snapshot; not a recovery plan |
| `Observer` | Lifecycle callbacks | WithObserver or WithExecObserver |
| `CheckpointHooks` | Synchronous reliable confirmation and recovery bridge | WithExecCheckpoint, usually integrated by Durable |

Ordinary hosts need only the root package. Record adaptation and YAML assembly live in `pulse/observe` and `pulse/yaml`; dependencies always point from these packages toward the root. The independent Durable layer owns storage, codecs, persistent RunIDs, and run ownership.

## Inspection and troubleshooting order

First confirm the version, then identify the stage: definition assembly, per-run inputs, start validation, node execution, output reading, or checkpoint recovery. Error names and states have precise meaning only with these conditions established.

- Missing sources, invalid outputs, or dependency cycles: fix structure and validate again.
- Required run input omitted: prepare complete Inputs and create a new execution.
- Normal skipped state: follow the interface's absence policy; the run may succeed.
- Real failure or cancellation: preserve the cause, wait for every node to terminate, then decide the business follow-up.
- Uncertain reliable confirmation: stop publication and recover from authoritative persistent records.

Follow [package documentation](../packages/index.md) for complete public symbol types. This reference does not replace historical design documents. Design background is available in the repository's [current core design](https://github.com/Luo-root/pulse/blob/main/docs/design/pulse.md) and [object model](https://github.com/Luo-root/pulse/blob/main/docs/design/object-model.md).

## Next steps

Start with [lifecycle](./lifecycle.md) for execution control or [errors](./errors.md) for failure handling. If upgrading a project, read [migration](./migration.md) before returning to the corresponding guide.
