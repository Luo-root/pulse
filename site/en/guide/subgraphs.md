# Static subgraphs

This page is for developers who want to encapsulate a flow as a reusable step. It requires Go 1.25+ and familiarity with [input and output ports](./inputs-outputs.md) and [join policies](./branching.md). The example uses development APIs.

`Sub` mounts an assembled child definition into a parent at assembly time. The child exchanges data through declared ports only. Before running, the parent can inspect and validate the complete subtree.

## Complete example: mount one child twice

This program gives the same topic two prefixes and joins the results. Both mounts share the definition, while inputs, outputs, and child executions are isolated per mount. An observer attached to the parent execution is inherited by the children.

Save the program as `main.go` in an empty directory:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/observe"
)

func run() error {
	topic := pulse.NewKey[string]("topic")
	childTopic := pulse.NewKey[string]("child.topic")
	prefix := pulse.NewKey[string]("child.prefix")
	label := pulse.NewKey[string]("child.label")
	first := pulse.NewKey[string]("first")
	second := pulse.NewKey[string]("second")
	report := pulse.NewKey[string]("report")
	child, err := pulse.New("labeler", "Add a topic prefix")
	if err != nil {
		return err
	}
	if err := child.DeclareInput(pulse.NewPort(childTopic), pulse.NewPort(prefix)); err != nil {
		return err
	}
	if err := child.DeclareOutput(pulse.NewPort(label)); err != nil {
		return err
	}
	if err := child.Add(pulse.NewNode("label", "Build a label",
		pulse.Requires(childTopic, prefix), pulse.Provides(label),
		func(rc *pulse.RunCtx) error {
			if err := pulse.WaitAll(rc, pulse.Requires(childTopic, prefix)...); err != nil {
				return err
			}
			value, err := pulse.Get(rc, childTopic)
			if err != nil {
				return err
			}
			p, err := pulse.Get(rc, prefix)
			if err != nil {
				return err
			}
			return pulse.Set(rc, label, p+value)
		})); err != nil {
		return err
	}
	parent, err := pulse.New("sub-demo", "Reuse a labeling subgraph", pulse.WithMaxRunning(2))
	if err != nil {
		return err
	}
	if err := parent.DeclareInput(pulse.NewPort(topic)); err != nil {
		return err
	}
	if err := parent.DeclareOutput(pulse.NewPort(report)); err != nil {
		return err
	}
	if err := pulse.Sub(parent, "first", "Build the first label", []pulse.SubBind{
		pulse.In(topic, childTopic), pulse.Fix(prefix, "A: "), pulse.Out(label, first),
	}, child); err != nil {
		return err
	}
	if err := pulse.Sub(parent, "second", "Build the second label", []pulse.SubBind{
		pulse.In(topic, childTopic), pulse.Fix(prefix, "B: "), pulse.Out(label, second),
	}, child); err != nil {
		return err
	}
	if err := pulse.Join(parent, "collect", "Combine both labels", pulse.Keys(first, second), report,
		func(rc *pulse.RunCtx, batch pulse.Batch[string]) (string, error) {
			if err := batch.WaitAll(); err != nil {
				return "", err
			}
			return strings.Join(batch.Values(), " | "), nil
		}); err != nil {
		return err
	}
	if err := parent.Validate(); err != nil {
		return err
	}
	var in pulse.Inputs
	if err := pulse.Seed(&in, topic, "Pulse"); err != nil {
		return err
	}
	sink := &observe.MemorySink{}
	obs, err := observe.NewRecordObserver(observe.ObserveConfig{
		Sink: sink, HostID: "docs", TraceID: observe.NewTraceID(),
	})
	if err != nil {
		return err
	}
	e, err := parent.NewExecution(context.Background(), in, pulse.WithExecObserver(obs))
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
	value, state, err := pulse.ReadOutput(e, report)
	if err != nil {
		return err
	}
	fmt.Printf("report=%s state=%s\n", value, state)
	fmt.Printf("records=%d\n", sink.Len())
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
```

Run in that directory. For examples from an unmerged branch, use the matching source checkout with the [installation page's local replacement steps](./installation.md); `@main` below only installs APIs already merged into main.

```sh
go mod init example.com/pulse-subgraphs
go get github.com/Luo-root/pulse@main
go run .
```

Expected output:

```text
report=A: Pulse | B: Pulse state=ready
records=16
```

The root has two run records plus two records for each of its three nodes. Each child execution has two run records plus two for its one node, for 16 in total. Record order between concurrent nodes is not fixed. Both children have `pulse.graph=labeler`, paths `first` and `second`, and distinct `pulse.execution` values.

## Understand bind direction

| Bind | Data direction | Parent-side declaration |
|---|---|---|
| `In(parentKey, childKey)` | A parent value enters a child input | Becomes the mounting node's `Requires` |
| `Out(childKey, parentKey)` | Ordinary values bridge after successful child.Wait; Stream / native channels bind early | Becomes the mounting node's `Provides` |
| `Fix(childKey, value)` | Supplies a fixed input at this mount | Uses no parent key |
| `FixSkip(childKey)` | Supplies normal absence at this mount | Uses no parent key |

`In` and `Out` both list the source before the destination. Typed binds require matching types; dynamic `InRef` and `OutRef` compare actual Go types during assembly. Every child-side key must be a declared port of the appropriate direction.

Each required child input must be bound once at each mount. `In` and `Fix` cannot both supply the same child input. One ordinary parent value may feed two distinct child inputs; one child output may also feed two distinct parent output keys.

A child key declared as both input and output provides pass-through. When a mount uses `Out` for that pass-through output, it must supply the input through `In`, `Fix`, or `FixSkip`. Unused optional inputs may remain unbound. Every mount needs at least one bind, and its node ID cannot contain the path separator `/`.

Fixed values belong to the mount and are used as its inputs on every run. Mutable objects are not deep-copied. Produce fresh per-run values through nodes instead of sharing mutable Fix values.

## Validation, sealing, and reuse

`parent.Topology()` exposes the static subtree, ports, and binds without running nodes or sealing definitions. Paths come from mount positions, with nested paths such as `first/inner`.

`parent.Validate()` recursively validates the entire subtree and seals every layer on success. A successful Start seals it too. Sealed definitions accept no new nodes, ports, or mounts, but still run repeatedly; a sealed child can be mounted into another unsealed parent. Failed validation seals nothing.

Every mount creates an independent child execution at runtime. Reusing a definition sequentially or concurrently shares no slots, first error, or cancellation domain. Mutable objects captured by business closures remain application-owned shared state and require synchronization. Composition cycles are rejected during assembly; mounting the same child at different sites is legal reuse.

## Runtime, aspects, and quotas

Sub does not wait for all parent inputs or omit the child when every inlet is skipped. In binds stable source slots that may remain pending. Internal nodes activate according to their own Requires, and independent source branches may run. Stream and native-channel Out bindings are installed before child.Wait and share live endpoints across layers. Ordinary values still bridge after successful child.Wait. The mount itself waits for final child completion and preserves real failure. All skipped bound outputs produce a normal skipped mount.

The child context derives from the mounting node, propagating parent cancellation and local Timeout. The actual writer's complete aspect chain determines managed Done/status; an ancestor mount is not an EOF boundary. Normal child cleanup does not withdraw the accepted tail. The [stream guide](./streams.md) includes cross-Sub and host-subscription fragments.

Retry on a mount creates a new child execution per attempt. A complete finite pipeline wholly inside the child, bridging only ordinary results, can be retried together. Actual external stream inputs/outputs reject ordinary Retry before I/O; proven skipped paths with no data remain normal. Do not inherit Retry into internal stream nodes. Children default to the parent observer and quota pool. The mount consumes no quota; managed waits temporarily release it. A child quota declaration uses an independent pool.

## Common errors and next steps

Binding an internal key without declaring it as an output, omitting a required child input, binding one input twice, or reversing `In`/`Out` arguments causes an assembly or validation error. Finish the child interface before mounting and validating to expose these problems before execution.

See [YAML assembly](./assembly.md) for declarative subgraphs. Streaming across boundaries also requires the completion and output-bridge rules in [streams](./streams.md). Read the [checkpoint reference](../reference/checkpoint.md) when child progress must survive interruption.
