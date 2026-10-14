# Inputs and outputs

This page is for developers who need to pass request data into a flow and retrieve business results afterward. First understand [graphs, executions, and keys](./concepts.md) and use the development revision identified in the [migration reference](../reference/migration.md).

Port declarations describe what a graph accepts and produces. `Inputs` and `Execution` hold data for a particular run. Declared outputs provide business results; snapshots provide progress. These use separate APIs.

## Declare the interface and node access

Place this fragment inside an assembly function returning `error`, with `github.com/Luo-root/pulse` imported. `g` is an unsealed graph; `request` and `result` are existing `Key[string]` values.

```go
if err := g.DeclareInput(pulse.NewPort(request)); err != nil {
	return err
}
if err := g.DeclareOutput(pulse.NewPort(result)); err != nil {
	return err
}
if err := g.Add(pulse.NewNode("format", "Format the input",
	pulse.Requires(request), pulse.Provides(result),
	func(rc *pulse.RunCtx) error {
		value, err := pulse.Get(rc, request)
		if err != nil {
			return err
		}
		return pulse.Set(rc, result, "result: "+value)
	})); err != nil {
	return err
}
```

`DeclareInput` allows the host to supply the key. `DeclareOutput` allows the host or parent graph to read it; it does not create a producer. An output must be produced internally or also declared as an input for pass-through. Otherwise `Validate` and `Start` reject it.

`Requires` and `Provides` declare node access: a node can read keys it declares and write only its own `Provides`. Keys with the same name and type are equivalent; the same name with different types is rejected during assembly. Use `pulse.Deps` to combine declarations of different types.

## Prepare inputs for each run

Continue in the same host function; `request` is already declared as an input.

```go
var in pulse.Inputs
if err := pulse.Seed(&in, request, "hello"); err != nil {
	return err
}
```

The zero value of `Inputs` is usable. `Seed` supplies a value; `SkipSeed` declares normal absence. When this run has no request value, replace the `Seed` fragment with the following one. Do not write the same key twice into one input collection.

```go
var in pulse.Inputs
if err := pulse.SkipSeed(&in, request); err != nil {
	return err
}
```

`SkipSeed` declares arrival and allows downstream skip propagation. Omitting a required input entirely is a start-condition error. `Seed` and `SkipSeed` immediately reject nil `*Inputs`, zero keys, and duplicate writes. Host `Seed` also rejects a nil interface value; typed nil pointers or slices remain values. Do not use a nil interface to represent skipping.

Input entries are copied when the execution is created. Adding further entries afterward does not change an existing execution. Slices, maps, and pointers inside business values are not deep-copied; do not mutate them concurrently with execution reads.

## Separate structural and per-run validation

`Validate` checks the definition's structure and seals the entire subtree on success. It requires no values for any particular run.

`NewExecution` checks this run's inputs: supplied keys must be declared inputs, never internally produced keys. Every input required by a node must have a `Seed` or `SkipSeed` entry. Inputs unused by nodes may be omitted.

`Start` checks per-run inputs again before sealing. An unsealed definition can acquire another node requiring a new input between creation and startup. If the execution then lacks that input, startup fails and seals nothing. Prepare complete `Inputs` and create a new execution. Changing the original input variable cannot add data to the old execution.

## Start, wait, and read results

This fragment continues with the assembled `g`, `in`, and declared output `result`. The host function needs a `context.Context` named `ctx` and the `fmt` import.

```go
e, err := g.NewExecution(ctx, in)
if err != nil {
	return err
}
defer e.Cancel()
if err := e.Start(); err != nil {
	return err
}
if err := e.Wait(); err != nil {
	return err
}
value, state, err := pulse.ReadOutput(e, result)
if err != nil {
	return err
}
switch state {
case pulse.SlotStateReady:
	fmt.Println(value)
case pulse.SlotStateSkipped:
	fmt.Println("No result this run")
case pulse.SlotStatePending:
	fmt.Println("Optional pass-through input was not supplied")
}
```

Check `err` before the state. On error, both `value` and `state` are invalid. Successful reads have three outcomes:

| State | Meaning | Host action |
|---|---|---|
| `ready` | A value, including zero numbers, empty strings, and valid nil values | Use the returned value |
| `skipped` | Normal absence; the flow may still succeed | Use a fallback or omit the item |
| `pending` | An unfed optional pass-through output that no node requires | Handle omission according to the interface |

`ReadOutput` does not wait, create slots, or emit observation events. Read after `Wait` finalizes the execution; `OnGraphFinished` may also read the finalized result. Unfinished, failed, or genuinely canceled executions expose no partial business result. Mutable objects remain the original business objects; the host owns subsequent synchronization.

## Common errors

- Seeding without declaring an input: call `DeclareInput` before creating the execution.
- Supplying an internally produced key: supply an upstream input or remove the internal producer.
- Reading host results with `Get`: `Get` requires a node context; hosts use `ReadOutput(e, key)`.
- Treating a zero value as absence: a zero value can be `ready`; check `SlotState`.
- Reading business values from `Snapshot`: snapshots contain no values; declared outputs are the result interface.

## Next steps

[Branches and joins](./branching.md) explains how input skipping propagates. [Static subgraphs](./subgraphs.md) uses ports as parent-child boundaries. Exact lifecycle and error behavior are in the [lifecycle reference](../reference/lifecycle.md) and [error reference](../reference/errors.md).
