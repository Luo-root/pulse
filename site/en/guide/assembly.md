# YAML assembly

This page is for developers who want topology in configuration files while keeping business implementation in Go. It requires Go 1.25+ and familiarity with [node declarations](./orchestration.md) and [static subgraphs](./subgraphs.md). Examples use development APIs.

`pulse/yaml` assembles YAML into a `Graph` and `SeedPlan`. YAML declares nodes, dependencies, and child wiring. A registry supplies real key types and named business functions. Registered functions supply Run only: they neither return a Node nor decide topology placement.

## Complete example: assemble a child and read its result

Save the program as `main.go` in an empty directory. The YAML root input is a literal, and the child has a fixed prefix. The host resolves the plan into run inputs and reads the report through a declared output.

```go
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/yaml"
)

const document = `
version: 1
description: Assemble a reusable summary step
seeds:
  - key: {name: topic, type: string}
    from: {kind: literal, value: Pulse}
graphs:
  summarize:
    description: Add a prefix
    seeds:
      - key: {name: child.prefix, type: string}
        from: {kind: literal, value: "about "}
    nodes:
      - id: format
        uses: demo.format
        requires:
          - {name: child.topic, type: string}
          - {name: child.prefix, type: string}
        provides: [{name: child.summary, type: string}]
nodes:
  - id: summary
    description: Run the summary step
    graph: summarize
    in: {child.topic: topic}
    out: {child.summary: report}
    timeout: 2s
    retry: {attempts: 3, delay: 10ms}
`

func run() error {
	topic := pulse.NewKey[string]("topic")
	childTopic := pulse.NewKey[string]("child.topic")
	prefix := pulse.NewKey[string]("child.prefix")
	summary := pulse.NewKey[string]("child.summary")
	report := pulse.NewKey[string]("report")
	reg := pulse.NewRegistry()
	for _, key := range pulse.Keys(topic, childTopic, prefix, summary, report) {
		if err := pulse.RegisterKey(reg, key); err != nil {
			return err
		}
	}
	if err := reg.Register("demo.format", func(rc *pulse.RunCtx) error {
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
		return pulse.Set(rc, summary, p+value)
	}); err != nil {
		return err
	}
	g, plan, err := yaml.Load([]byte(document), reg, yaml.LoadOptions{
		GraphID: "yaml-demo", MaxRunning: 2,
	})
	if err != nil {
		return err
	}
	if err := g.DeclareOutput(pulse.NewPort(report)); err != nil {
		return err
	}
	if err := g.Validate(); err != nil {
		return err
	}
	in, err := plan.Inputs(nil)
	if err != nil {
		return err
	}
	e, err := g.NewExecution(context.Background(), in)
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
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
```

Run in that directory:

```sh
go mod init example.com/pulse-yaml
go get github.com/Luo-root/pulse@1f67045d6c42
go run .
```

Expected output:

```text
report=about Pulse state=ready
```

Load resolves registry entries and assembles definitions; it runs no body and reads no external input. `plan.Inputs` resolves root inputs. Child literals become mount-site Fix bindings. The host adds a root output before Validate, then controls this run through NewExecution/Start/Wait.

## Document fields

| Field | Meaning |
|---|---|
| `version` | Absent, 0, or 1; other versions rejected |
| `description` | Root description; `graphs.<name>.description` describes a child |
| `seeds[].key` | `{name, type}`, matched against the registry |
| `seeds[].from` | kind is literal, env, file, or context; an omitted kind means literal |
| `seeds[].skip` | true declares normal absence; from is not resolved for that entry |
| `nodes[].id` / `description` | Node identity and description |
| `nodes[].uses` | Registered business function name |
| `nodes[].requires` / `provides` | Factory-node key declarations |
| `graphs` / `nodes[].graph` | Reusable graph table and reference; graph and uses are mutually exclusive |
| `nodes[].in` / `out` | Child binds, both written as `child key: parent key` |
| `nodes[].timeout` | Go duration such as 2s; becomes outer Timeout |
| `nodes[].retry` | attempts and delay; becomes inner Retry |
| `observer` | Documentation hint ignored by Load; use options for observers |

A type tag is the registered key's `reflect.Type.String()`, not an arbitrary alias. Query the expected tag with `Registry.TypeTagOf`. Same-name type conflicts, missing tags, unregistered keys, and unregistered uses are errors. YAML has no Produce/Consume/Tee declaration syntax; topology cannot infer hidden channel behavior inside ordinary factories.

## Resolve external inputs in the host

For nonliteral inputs such as env, file, and context, pass a resolver to `plan.Inputs`. This fragment needs `os`, `fmt`, and `yaml`, existing `plan` and `g`, and a host function returning `error`:

```go
in, err := plan.Inputs(func(from yaml.SeedFrom) (any, error) {
	if from.Kind != "env" {
		return nil, fmt.Errorf("unsupported seed kind %q", from.Kind)
	}
	value, ok := os.LookupEnv(from.Env)
	if !ok {
		return nil, fmt.Errorf("environment variable %q is not set", from.Env)
	}
	return value, nil
})
if err != nil {
	return err
}
if err := g.Run(ctx, in); err != nil {
	return err
}
```

Here `ctx` is the host execution context. Resolved values must still match the key type. Generic literal slices and maps are recursively aligned with registered container types. Conversion does not guess map-to-struct fields, convert strings to numbers or vice versa, truncate floats, or narrow out of range. Business conversions belong in a resolver returning the correct type.

## Subgraphs and validation scope

A child node's interface comes from in/out and child seeds. It cannot also declare requires/provides, and a factory node cannot declare in/out. Bind ends must have matching real types. A child input cannot also be produced internally. Child seeds that carry values support literal only; skipped seeds do not resolve from.

Load assembles a child definition per reference site and checks that site's wiring. Mounting one spec twice creates two positions; runtime creates a separate child execution at each. Reference cycles, empty children, invalid wiring, and source problems in referenced subtrees report the mounting path during Load.

An unreferenced spec has no definite interface, so it is not treated as a closed graph requiring every input to have a source. Known source conflicts, duplicate IDs, cycles, and other checks still apply. The host can add root declarations after Load. Root structural checks belong to Validate/Start, while required per-run inputs are checked by NewExecution/Start. Successful Load does not mean the complete root is ready to start.

## Inspect declarations with Analyze

This fragment needs YAML bytes named `data`, `fmt`, and `yaml`, inside a function returning `error`:

```go
topology, err := yaml.Analyze(data, yaml.LoadOptions{GraphID: "preview"})
if err != nil {
	return err
}
for _, problem := range topology.Problems {
	fmt.Println(problem)
}
fmt.Println(topology.Mermaid())
```

Analyze needs no Registry and executes no factories, aspects, resolvers, or ObserverFor. It shares Load assembly rules. Root problems remain in Problems; child or document assembly problems return an error. Returned Type fields retain YAML labels, with empty labels for wiring-only keys that cannot be inferred. Analyze does not validate real Go types, registered factories, or literal assignability. A successful preview still needs Load and Inputs validation.

## Quotas, observers, and common errors

Set the whole declaration tree's quota with `LoadOptions.MaxRunning`; the default is unlimited. Avoid also placing WithMaxRunning in `LoadOptions.Graph`. Only a positive MaxRunning overrides that declaration; zero does not reset an existing quota to unlimited.

Attach the root observer through `LoadOptions.Graph`; children inherit by default. Supply `ObserverFor(path)` only when mounts need different destinations. It is called once per actual child assembly during Load, never for the root, check tree, or Analyze.

Timeout and Retry on a child reference wrap the whole child execution. On a node inside the child, they wrap that step only. Their order is fixed: Timeout outside Retry. When a call fails, distinguish assembly, input resolution, and execution, then follow the [error reference](../reference/errors.md) instead of replacing failures with skips.

## Next steps

[Observation and diagnostics](./observability.md) covers static previews and execution overlays. The [lifecycle reference](../reference/lifecycle.md) defines validation and startup boundaries. [Package documentation](../packages/index.md) links to further public APIs.
