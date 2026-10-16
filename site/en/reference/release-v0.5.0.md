# Pulse v0.5.0 release notes

This page describes changes from v0.4.1 to v0.5.0. **It is currently a release preparation draft. v0.5.0 becomes installable only after its [GitHub Release](https://github.com/Luo-root/pulse/releases) and tag actually exist.** Until then, follow [installation](../guide/installation.md) for a merged main-branch revision or matching local source.

## Breaking changes first

v0.5.0 is a minor release during 0.x, rather than a patch that preserves existing call sites. See [migration](migration.md) for the full procedure.

- **Definitions and runs are separate.** `Graph` holds a reusable definition; `Execution` owns one run's context, inputs, slots, errors, and state. `New` no longer takes a context. Run with `g.Run(ctx, in)`, or use `NewExecution`, `Start`, and `Wait`. Do not retain runtime state on Graph or add Reset.
- **Inputs and subgraph wiring are explicit.** Definitions declare inlets/outlets; this run's values live in `Inputs`. `Seed` / `SkipSeed` and name-based entries take its address. `Sub` mounts an assembled child through ports and `In` / `Out` / `Fix` / `FixSkip`. Successful validation seals the whole subtree. Dynamic subgraph entries from transitional development versions have been removed.
- **Retry isolates attempt outputs.** Failed attempts no longer publish writes downstream; success or normal skipping commits them. Custom aspects cannot repeatedly advance `next` within the same attempt boundary. An error chain containing `CheckpointError` stops Retry even if checkpoints are disabled on this graph.
- **Observer callbacks carry execution location.** Migrate custom Observers to `Site`, containing Graph, Execution, and Path. Snapshots belong to Execution and contain no business values; Topology includes all declared ports. Users of transitional value-bearing snapshots should use declared-output `ReadOutput`. Structures containing port slices no longer support whole-value `==` comparison.
- **Node helpers gain descriptions.** `Join` / `FanOut` / `Produce` / `Consume` / `Tee` take `description` after `g, id`. Configure local stream aspects with `WithStreamAspects`; Consume's function type gains variadic StreamOption.
- **YAML follows the definition/run model.** `Load` is separate from per-run SeedPlan input resolution; `Analyze` needs no Registry. Unknown fields, additional YAML documents, and fractional values for integer configuration are rejected explicitly rather than ignored.
- **Text encoding is stricter.** Quoting/escaping for non-ASCII whitespace, controls, invalid UTF-8, and replacement characters aligns with slog.TextHandler. Migrate parsers and assertions relying on fixed log bytes. Ordinary Chinese and printable emoji remain unchanged.

## New ways to use Pulse

### Run one definition repeatedly

A Graph can create independent sequential or concurrent Executions and be mounted at multiple Sub sites. Inputs, cancellation, and state are execution-local. Callers still synchronize mutable objects captured by business closures.

After successful Wait, use `ReadOutput` for declared outlets, distinguishing ready, normal skipped, and optional pass-through pending. Real failure does not deliver partial values. Use Topology for structure, a value-free Snapshot for execution progress, and Mermaid for structure with execution state.

### Managed messages and live subgraphs

`ProduceStream` / `ConsumeStream` / `TeeStream` provide explicit Ack/Nack, in-flight capacity, quota-releasing business waits, and finite message attempts. Purely managed pipelines can progress with business quota 1. Register `SubscribeStream` before Start for live host reading, then acknowledge or Stop.

Sub connects stream outputs live; ordinary values still bridge after successful child completion. Native channels and older helpers keep compatibility behavior without acquiring acknowledgement, replay, or persistence automatically.

### Complete-node and incremental-stream checkpoint bridges

Core provides explicit whole-tree checkpoint mode, complete-chain staging, confirmed-before-publish, startup restoration checks, and managed-message/state/cursor bridge boundaries. The separate [Pulse Durable](https://github.com/Luo-root/pulse-durable) library owns storage, Codec, stable RunID, and Run/Resume.

Recovery reuses confirmed results rather than restoring Go call stacks. Retry budgets persist; Resume does not replenish exhausted budgets. Applications still prevent duplicate external effects. Checkpoints do not automatically provide external exactly-once behavior.

### Automatically observe actual aspect control facts

Aspects can read location with `rc.Site()`. With `observe.NewRecordObserver` installed, built-in Retry / Timeout record attempts, an actual next retry, backoff, and the current timeout's expiry. `NamedAspect` explicitly records custom aspect entry/exit.

Existing graph/node lifecycle callback counts remain unchanged. Exception protection determines outcomes before terminal reporting. Log errors or Observer panics do not change business execution. Durable's bridge contributes recovery identity; a process-local Execution ID is not a stable RunID.

### Approvals, batches, and Agent loops

The [composition guide](../guide/composition.md) explains approval aspects, host wait registration with Cancel/Resume, and reusable Graph tools called from a loop node. Business code or the host can handle input-dependent batch sizes without modifying a running shared definition.

These compositions do not automatically add a task catalog, wake-up service, per-round durable records, or cross-machine leases. Provide the corresponding host protocols when an application needs them.

## Reliability fixes

This version includes lock-safe stream error-chain diagnostics, exception terminal reporting and Timeout cleanup, normal-absence versus real-failure handling, confirmed publication and reader-obligation fixes, typed-input and assembly validation, plus observation-output isolation and release of dropped payloads. Regression and composition tests remain in the repository.

The root runtime dependency closure remains standard-library-only. `observe` adds no third-party dependencies; `yaml` continues to use yaml.v3. Durable still depends on Core in one direction. Internal file consolidation adds no package paths and preserves those boundaries.

## Upgrade and publication order

1. Migrate call sites with the [migration guide](migration.md) and check values, skips, failure, and cancellation separately.
2. Inspect captured business state when reusing definitions and protect external effects against duplication.
3. Tag the complete integration on `main`. A MERGED label on every stacked PR does not prove main contains every layer: a PR merged into an earlier feature branch can still be absent from main.
4. Once the Core release tag is downloadable, align Durable with that version and recheck the public dependency combination. Retain historical measurement baselines; they are not new-version measurements.

Go 1.25+ is required. The released commit and CI records establish actual build, race, documentation, and cross-repository recovery results. This release continues 0.x development and does not represent a final v1.0 API freeze.
