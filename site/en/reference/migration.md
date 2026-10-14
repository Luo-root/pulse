# Migration

This page is for maintainers upgrading from published v0.4.1 or transitional development commits to the reusable Graph definition and independent Execution APIs. Retain existing tests and verify the module version before upgrading.

## Published releases and development documentation

The latest published tag is **v0.4.1**. The object model and checkpoint bridge on current main have not been released. This site describes the development model; it does not announce v0.5.0. Released-version users should use documentation for v0.4.1. To try site examples, pin the public commit already merged into main that matches current production source:

```sh
go get github.com/Luo-root/pulse@1f67045d6c42
go list -m github.com/Luo-root/pulse
```

The project requires Go 1.25+. Go reports the corresponding pseudo-version. Verify its commit rather than inventing a pseudo-version manually, and do not expect development APIs from an unversioned installation. Select a published tag from release notes when the next release becomes available.

## Migrate object responsibilities first

| Concern | v0.4.1 model | Current development model |
|---|---|---|
| Graph | One run's definition, slots, context, and error | Reusable static definition |
| Context | Bound when constructing the graph | Supplied to NewExecution or Run |
| External data | Written into the graph object | Declare an input, then write this run's Inputs |
| Execution control | Graph handles start, wait, and error | Execution handles Start, Wait, Err, Cancel |
| Repeated runs | Rebuild the run object | Create multiple Executions from one Graph |
| Observer callbacks | Identified by graph ID | Site carries Graph, Execution, Path |

Do not add Reset to a definition or cache the last Execution as its state. Cross-run business state belongs to the host, not implicitly to Graph slots.

## Current construction entries

Place this fragment in an assembly function returning `error`, with `pulse` imported. Description fields may be empty but their argument positions must remain:

```go
request := pulse.NewKey[string]("request")
result := pulse.NewKey[string]("result")
g, err := pulse.New("migration-demo", "Reusable processing flow")
if err != nil {
	return err
}
if err := g.DeclareInput(pulse.NewPort(request)); err != nil {
	return err
}
if err := g.DeclareOutput(pulse.NewPort(result)); err != nil {
	return err
}
if err := g.Add(pulse.NewNode("copy", "Forward the request",
	pulse.Requires(request), pulse.Provides(result),
	func(rc *pulse.RunCtx) error {
		value, err := pulse.Get(rc, request)
		if err != nil {
			return err
		}
		return pulse.Set(rc, result, value)
	})); err != nil {
	return err
}
var in pulse.Inputs
if err := pulse.Seed(&in, request, "hello"); err != nil {
	return err
}
```

Use `g.Run(ctx, in)` and handle its error when no result handle is needed. For results or progress, create an Execution, Start successfully, Wait, then call `pulse.ReadOutput(e, result)`. The complete host sequence is in [inputs and outputs](../guide/inputs-outputs.md).

The first argument to Seed, SkipSeed, and both name-based input entries is now the address of Inputs. Ports belong to definitions; values belong to this run. Missing required inputs fail at NewExecution/Start rather than waiting for timeout. Zero keys, duplicate input writes, and nil interface inputs are rejected explicitly.

## Migrate topology and children

Successful Validate seals the entire subtree, as does successful Start. Move declarations that could previously be appended before the first successful validation. Use Topology to query structure and problems without assuming definitions remain mutable after validation.

Sub mounts an assembled child definition using an ID, description, binds, and child. Its interface must consist of DeclareInput/DeclareOutput ports. In carries parent values into the child; Out bridges child outputs back. Fix/FixSkip supply mount-specific inputs. Every run and mount has an independent child execution with cancellation derived from the parent node context.

If you followed transitional development commits, dynamic child construction entries have been removed. Build static subtrees at assembly time instead. This is a transitional API migration, not a capability originally present in v0.4.1. Do not store per-run fixed values in the child definition.

## Migrate observation, results, and retry

Every custom Observer callback receives Site. Preserve Execution and nonempty Path in records rather than grouping concurrent runs by Graph alone. Give each run a fresh TraceID. WithExecObserver overrides are suitable when reusing a definition.

Snapshots belong to Execution and contain no business values. Static structure belongs to Topology; pass WithMermaidSnapshot to Topology for state diagrams. Hosts read declared outputs only, checking error before ready, skipped, or optional pass-through pending. Callers of transitional value-bearing snapshots should move to ReadOutput.

Failed Retry attempts now stage and discard outputs, so downstream nodes never see failed versions. Success or normal skipping commits the write set. Custom aspects that repeatedly call next are no longer legal retries; use built-in Retry. Business code still owns external-effect idempotency. See [retry, timeout, and cancellation](../guide/retry-cancellation.md).

Timeout cancels its inner context at expiry and waits for the body to exit. Code that depended on forced return at the deadline must make blocking operations honor rc.Context and finish its own work before the node returns.

## Migrate YAML and recovery integrations

YAML Load returns a definition and SeedPlan, with no execution context in assembly options. Resolve plan.Inputs for each run, then supply ctx and Inputs to execution. Complete added root declarations before Validate.

Analyze takes YAML bytes and LoadOptions without Registry. Its types are document labels; Load and Inputs still verify actual types and values. Install observers and per-run configuration through definition or Execution entries. Declaration preview does not execute those callbacks.

Recovery uses explicit WithExecCheckpoint and RestoreNodes. Snapshot is not a persistent recovery interface. Stable RunIDs, versions, codecs, storage, and ownership belong to Durable. Ordinary executions need no reliable hooks. See [checkpoints](./checkpoint.md).

## Validate after upgrading

1. Compile all call sites, checking context placement, descriptions, input addresses, and execution handles.
2. Verify business results for present values, normal skipping, real failure, and cancellation separately.
3. Run one definition twice and check captured business state under concurrent runs or multiple child mounts.
4. Verify that failed Retry writes are no longer visible and review external idempotency boundaries.
5. Update both languages and copyable examples, then run project tests.

Compilation alone cannot prove that skipping and failure stay distinct or that shared business objects are concurrency-safe. For execution failures, consult the [error reference](./errors.md), then [lifecycle](./lifecycle.md) for startup and finalization conditions.

## Next steps

Use the [guides](../guide/orchestration.md) to update individual topics. When reading implementation or historical designs, use this page's version scope to distinguish released APIs, current development behavior, and future plans.
