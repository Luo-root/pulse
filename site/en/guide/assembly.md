# Declarative assembly

`pulse/yaml` assembles one YAML document into a `*pulse.Graph` + a `SeedPlan`. The division of labour is hard:

> **The YAML owns the topology; a registered factory only supplies `Run`.**

A factory does not return a `*Node`, because it should not decide where it attaches in the graph. The same `Run` can be wired into a different graph by swapping the YAML.

## The two routes

| Route | Where the topology lives | Fits |
|---|---|---|
| Imperative | Go code: `pulse.NewNode(...)` + `g.Add(...)` | Topology shares the code's lifetime and is checked at compile time |
| Declarative | YAML: `id` / `requires` / `provides` | Topology must be editable, shipped per request, shared with people |

Both routes produce a `*Graph` and can be mixed (the factory a declarative `uses` points at is itself an ordinary Go function).

## Complete example

```go
package main

import (
	"context"
	"fmt"

	"github.com/Luo-root/pulse"
	"github.com/Luo-root/pulse/yaml"
)

var (
	Topic   = pulse.NewKey[string]("topic")
	Summary = pulse.NewKey[string]("summary")
	Report  = pulse.NewKey[string]("report")
)

const doc = `
version: 1
seeds:
  - key: {name: topic, type: string}
    from: {kind: literal, value: "slot contract"}
nodes:
  - id: summarize
    uses: demo.summarize
    requires: [{name: topic, type: string}]
    provides: [{name: summary, type: string}]
    timeout: 30s
    retry: {attempts: 3, delay: 100ms}
  - id: report
    uses: demo.report
    requires: [{name: summary, type: string}]
    provides: [{name: report, type: string}]
`

func main() {
	// 1) Register keys (YAML reconciles by {name, type}) and named factories.
	reg := pulse.NewRegistry()
	pulse.MustRegisterKey(reg, Topic)
	pulse.MustRegisterKey(reg, Summary)
	pulse.MustRegisterKey(reg, Report)

	reg.MustRegister("demo.summarize", func(rc *pulse.RunCtx) error {
		topic, err := pulse.Get(rc, Topic)
		if err != nil {
			return err
		}
		return pulse.Set(rc, Summary, "about "+topic)
	})
	reg.MustRegister("demo.report", func(rc *pulse.RunCtx) error {
		summary, err := pulse.Get(rc, Summary)
		if err != nil {
			return err
		}
		return pulse.Set(rc, Report, "report: "+summary)
	})

	// 2) Assemble: the topology comes from the YAML.
	g, plan, err := yaml.Load([]byte(doc), reg, yaml.LoadOptions{
		Context: context.Background(),
		GraphID: "yaml-demo", // required: observation uses it to tell which graph ran
	})
	if err != nil {
		panic(err)
	}

	// 3) The host applies the seed plan, then runs.
	if err := plan.Apply(g, nil); err != nil { // anything beyond literal needs resolve
		panic(err)
	}
	if err := g.Run(); err != nil {
		panic(err)
	}
	fmt.Println("run ok:", g.ID())
}
```

```text
run ok: yaml-demo
```

## YAML fields

| Field | Meaning |
|---|---|
| `version` | Absent or `1`; any other value is rejected |
| `seeds[].key` | `{name, type}` — must match what `RegisterKey` registered |
| `seeds[].from` | `kind`: `literal` / `env` / `file` / `context`; values other than `literal` are supplied by the host's `resolve` |
| `seeds[].skip` | `true` routes this entry through `SkipSeed` (the slot arrives "skipped") |
| `nodes[].id` | Node ID, required; the attribution key for observation |
| `nodes[].uses` | Required; names a factory on the `Registry` |
| `nodes[].requires` / `provides` | `{name, type}` lists |
| `nodes[].timeout` | Assembled into `pulse.Timeout`, **outside** |
| `nodes[].retry` | `{attempts, delay}`, assembled into `pulse.Retry`, **inside** |

A few points:

- **Keys are reconciled by `{name, type}`**. `type` is `reflect.Type.String()` (`string`, `[]string`, `[]Doc`, …) and must match what `RegisterKey` registered; a name that does not line up, or a missing `type`, fails at `Load` time.
- **Time fields** use Go's `ParseDuration` form (`30s` / `100ms`); never write a bare number.
- **Aspect order**: whatever is listed first is further outside → `timeout` outside, `retry` inside, i.e. "the total duration is bounded, each attempt retries on its own".
- **The engine does no IO**. When `from.kind` is not `literal`, `Load` only hands it to `plan.Apply(g, resolve)` as-is; reading files, reading environment variables and pulling a context out of a request are all the host's job.
- The `observer` field is a documentation hint and `Load` ignores it — observers go through `LoadOptions.Graph` (`pulse.WithObserver(...)`).

## Edges and pitfalls

**A `literal` is shape-aligned to the registered type: lists and maps work out of the box.** What the YAML decoder hands over is a generic container (`[]any` / `map[string]any`), and `pulse.SeedByName` converts it recursively into the target type: `value: ["a","b"]` fills a `[]string` and `value: {a: 1}` fills a `map[string]int`, with no `resolve` in sight.

Three things it deliberately does **not** do (each fails loudly — use `resolve` for a correctly typed value). The criterion: **shape can be aligned by a machine; semantics only the caller knows.**

| Case | Error (measured on the author's machine; the type name follows your package) |
|---|---|
| elements are objects and the target is a struct (field mapping is the host's semantics) | `pulse: seed "demo.in": value type []interface {} not assignable to []yaml_test.Doc (element 0: cannot convert map[string]interface {} to yaml_test.Doc)` |
| string ↔ number coercion | `pulse: seed "demo.in": … (element 0: cannot convert string to int)` |
| truncating a float into an int / narrowing out of range | `… (element 0: cannot convert float64 to int)` · `… (element 0: cannot convert int to int8)` |

When you need one of those, use `resolve` to produce a correctly typed value yourself:

```go
plan.Apply(g, func(from yaml.SeedFrom) (any, error) {
	if from.Kind == "file" {
		raw, err := os.ReadFile(from.Path) // the host does the IO
		if err != nil {
			return nil, err
		}
		var docs []Doc
		if err := json.Unmarshal(raw, &docs); err != nil {
			return nil, err
		}
		return docs, nil
	}
	return nil, fmt.Errorf("unsupported seed kind %q", from.Kind)
})
```

Other edges:

- **YAML only**: there is no JSON entry point (one topology keeps one dialect);
- An empty `nodes`, a node missing `id` / `uses`, or a `uses` pointing at an unregistered factory all fail at `Load` time (the latter two name the node id);
- `LoadOptions.GraphID` is required — the graph identity is observation's attribution key;
- This is the only package with a third-party dependency (`gopkg.in/yaml.v3`); neither the root package nor `observe` depends on it.

Package-level API and examples: [yaml package docs](/en/packages/yaml/).
