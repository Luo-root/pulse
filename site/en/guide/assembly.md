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
| `graphs` | Declarations of reusable subgraphs (names are unique within the document); a subgraph does not carry its own `graphs` |
| `nodes[].graph` | This node = assemble one subgraph (mutually exclusive with `uses`) |
| `nodes[].in` / `out` | The boundary of a graph node, always **`child key: parent key`** |
| `nodes[].timeout` | Assembled into `pulse.Timeout`, **outside** |
| `nodes[].retry` | `{attempts, delay}`, assembled into `pulse.Retry`, **inside** |

A few points:

- **Keys are reconciled by `{name, type}`**. `type` is `reflect.Type.String()` (`string`, `[]string`, `[]Doc`, …) and must match what `RegisterKey` registered; a name that does not line up, or a missing `type`, fails at `Load` time.
- **Time fields** use Go's `ParseDuration` form (`30s` / `100ms`); never write a bare number.
- **Aspect order**: whatever is listed first is further outside → `timeout` outside, `retry` inside, i.e. "the total duration is bounded, each attempt retries on its own".
- **The engine does no IO**. When `from.kind` is not `literal`, `Load` only hands it to `plan.Apply(g, resolve)` as-is; reading files, reading environment variables and pulling a context out of a request are all the host's job.
- The `observer` field is a documentation hint and `Load` ignores it — observers go through `LoadOptions.Graph` (`pulse.WithObserver(...)`) and `LoadOptions.ObserverFor` (per level, see below).

## Subgraphs: one step = one graph

`graphs:` declares reusable subgraphs and `nodes[].graph` points a step at one — **a subgraph is the unit of topology reuse**, so reusing a flow does not force you to write a Go factory:

```yaml
version: 1
graphs:
  enrich:
    nodes:
      - id: work
        uses: demo.enrich
        requires: [{name: sg.topic, type: string}]
        provides: [{name: sg.summary, type: string}]
seeds:
  - key: {name: sg.input, type: string}
    from: {kind: literal, value: "slot contract"}
nodes:
  - id: step1
    graph: enrich
    in:  {sg.topic: sg.input}      # child key: parent key (parent reads → child is seeded)
    out: {sg.summary: sg.result}   # child key: parent key (child produces → parent is set)
```

It expands to `pulse.Sub` from [a graph as a node](/en/guide/orchestration): the parent-side node's `Requires` / `Provides` follow from `in` / `out`, and the engine's static validation (sources / cycles / slots) still applies. Rules:

- **The boundary is written where the wiring is**: `in` / `out` are always `child key: parent key`; a graph node may **not** also carry `requires` / `provides`.
- **Types and names are both compared at `Load`**: each end is resolved against `{name, type}` with the **same** type token (a parent-side key registered with another type is reported); referencing a graph that does not exist, binding a key no node of the child declares, or a graph with no nodes — all three are reported by name.
- **Reference cycles are caught at `Load`**: nesting a graph inside a graph is allowed with no depth limit, while `a → b → a` is rejected with one concrete path.
- **The boundary has exactly one spelling**: a graph node may not also carry `requires` / `provides`, and a **factory node may not carry `in` / `out`** — writing the wrong side silently does nothing, so both are reported at assembly time.
- **A key fed by `in:` may not also be provided inside the child**: a key has exactly one source, so running that step would hit `ErrDuplicateSource`.
- **A subgraph's `seeds`**: the ones that actually carry a value may only use `literal` (`env` / `file` / `context` need host IO, and `SeedPlan` belongs to the parent graph; the `from` of a `skip: true` entry has no meaning and follows the top-level `seeds` reading); names and types are reconciled against the registry; **the same key seeded twice in one graph** is also reported — the second write would be silently ignored and which value wins depends on declaration order.
- **Every `graphs:` declaration is checked**, including ones nothing references, and **once per reference site**: only the root level's nodes get added to the graph, since a child graph is built when the run reaches it — check the root alone and a mistake two levels down only blows up halfway through the run. The mechanism is a **build-only, never-run** throwaway graph per reference site going through the same assembly code, with that site's available sources seeded (the spec's own `seeds` plus the keys the parent feeds with `in:`), and finally the engine's own read-only check — so "a `Requires` nothing provides or seeds" and "dependency cycle" inside a subgraph are reported at `Load` too, **word for word the same message `Start` would give**. Per reference site rather than per spec: the same graph referenced twice can have different `in:` sets, and a union check would read "the other site fed it" as "this site fed it as well".
- **Check graphs never touch the host's context, and the root graph is built last**: the check graphs all run on `context.Background()`; a failed `Load` returns `nil, nil, err`, so the caller has no handle to clean anything up — which is why every fallible check has to pass before a root graph is created. The root graph still inherits the host context (so cancellation reaches it) and the host releases it when `Wait` returns.
- **Aspects land on the parent-side node**: `timeout: 30s` puts a time limit on the whole child graph, while nodes **inside** the child keep their own.
- **One subgraph referenced twice = two independent instances**: a fresh graph is built per run, and observation separates them by graph id and `pulse.path`.

**Observation is per level**: `LoadOptions.ObserverFor(path)` builds an egress for each level (empty for the root, the node id for one level, `outer/inner` for two) — hand it to `observe.ObserveConfig.Path` and every record of that level carries `pulse.path`, which is what makes nested logs and traces fold back into a tree.

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
