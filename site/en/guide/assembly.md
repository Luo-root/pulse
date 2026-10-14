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
		GraphID: "yaml-demo", // required: observation uses it to tell which graph ran
	})
	if err != nil {
		panic(err)
	}

	// 3) The host resolves the seed plan into this run's inputs, then runs.
	in, err := plan.Inputs(nil) // anything beyond literal needs resolve
	if err != nil {
		panic(err)
	}
	if err := g.Run(context.Background(), in); err != nil {
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
- **The engine does no IO**. When `from.kind` is not `literal`, `Load` only hands it to `plan.Inputs(resolve)` as-is; reading files, reading environment variables and pulling a context out of a request are all the host's job.
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

It expands to the **static composition `pulse.Sub`** of [a graph as a node](/en/guide/orchestration): the child graph is **built at `Load` time**, one instance per mounting point; the parent-side node's `Requires` / `Provides` follow from `in` / `out`, and the engine's static validation (sources / cycles / slots) still applies. From that:

- **The whole tree is visible once assembled, without running it**: `g.Topology()` recurses along the static mounting edges, `Nodes[i].Sub` is the child graph mounted at that point (go one level deeper with `.Sub` again), paths are derived from the **mounting position** (`step1`, `step1/toInner`), and the graph identity is `<root id>/<mounting path>`;
- **A child graph's static problems are reported at `Load`**, and the message names the **mounting path**: `pulse/yaml: graph "inner" (mounted at "step1/toInner"): pulse: node "leaf" requires "c.nope" but nothing provides it (declare it with DeclareInput if the value comes from outside)`;
- **The same spec mounted in several places = several independent instances**: each place has its own slots and mounting path, both places run, and observation tells them apart by graph id and `pulse.path`;
- **Cancellation enters the child from the mounting point**: cancel the parent ctx and the child's nodes see it on the spot — not a run that lasts the whole span and swallows the cancellation.

Rules:

- **The boundary is written where the wiring is**: `in` / `out` are always `child key: parent key`; a graph node may **not** also carry `requires` / `provides`.
- **Types and names are both compared at `Load`**: each end is resolved against `{name, type}` with the **same** type token (a parent-side key registered with another type is reported); referencing a graph that does not exist, binding a key no node of the child declares, or a graph with no nodes — all three are reported by name.
- **Reference cycles are caught at `Load`**: nesting a graph inside a graph is allowed with no depth limit, while `a → b → a` is rejected with one concrete path.
- **The boundary has exactly one spelling**: a graph node may not also carry `requires` / `provides`, and a **factory node may not carry `in` / `out`** — writing the wrong side silently does nothing, so both are reported at assembly time.
- **A key fed by `in:` may not also be provided inside the child**: a key has exactly one source, and what `in:` does is declare that key as an inlet of the child — the declaration itself hits `ErrDuplicateSource`, reported at Load.
- **A subgraph's `seeds`**: the ones that actually carry a value may only use `literal` (`env` / `file` / `context` need host IO, and `SeedPlan` belongs to the parent graph; the `from` of a `skip: true` entry has no meaning and follows the top-level `seeds` reading); names and types are reconciled against the registry; **the same key seeded twice in one graph** is also reported — the second write would be silently ignored and which value wins depends on declaration order.
- **Every *referenced* `graphs:` declaration is checked, once per reference site**: assembly first builds a **build-only, never-run** check tree — **the very same assembly code**, every child mounted in its static form, one instance per reference site. Each instance declares its own interface as ports from **that site's** `in:` / `out:` (`DeclareInput` / `DeclareOutput`), which keeps "this was an external input all along" apart from "you forgot to wire it" and makes seeding "fake sources" into the check tree unnecessary; finally the engine's own recursive `Validate` judges it — so "a `Requires` nothing provides (and it is not a declared inlet)", "a declared outlet nobody produces" and "dependency cycle" inside a child are reported at `Load` too, **word for word the message `Start` would give**, naming the **mounting path** (for example: `graph "inner" (mounted at "step1/toInner")`). Per reference site rather than per union: the same spec mounted in two places can have different `in:` sets, and a union check would read "the other site fed it" as "this site fed it as well".
- **An unreferenced spec is not judged for missing sources, but known conflicts are checked**: an unknown Requires may be a future inlet and is declared in the temporary check graph. Literal/skip seeds are known external sources; a node providing the same key makes both Load and Analyze reject it with ErrDuplicateSource. Factory names / empty `nodes` / duplicate IDs / dependency cycles / stream relationships remain checked. Missing sources are judged at the actual site's `in:` / `out:` once referenced.
- **`Load` touches no ctx at all**: a graph is a definition and a definition has no lifetime — which cancellation domain a run lives in is decided at `Run` / `NewExecution` time (`LoadOptions.Context` was deleted for that reason: keeping it would only be a silently dead knob). A child's cancellation still reaches in from the mounting point (at run time the mounting step derives the child execution's ctx from that layer's parent node). A failed `Load` returns `nil, nil, err`, so the caller has no handle to clean anything up — which is why every fallible check runs before the graph is handed out.
- **Aspects land on the parent-side node**: `timeout: 30s` puts a time limit on the whole child graph, and `retry: {attempts: 3, delay: 100ms}` runs that whole child graph up to three times — **every `retry:` attempt builds a fresh child execution**, and the first attempt's bridge back into the parent is discarded along with that attempt's **write staging area**, so the parent only ever sees the **successful** attempt's output (the half-and-half mix cannot happen). Nodes **inside** the child keep their own aspects, and a `retry:` placed there still retries just that node.
- **Each mounting point builds one instance with its own slots and mounting path, and both places run**: the same spec referenced twice is two independent instances, which observation tells apart by graph id and `pulse.path`.
- **The root level's own criteria are still reported the moment a run starts** (`Run` / `NewExecution`; the root layer is not judged at assembly time): the top-level `seeds:` only declares keys as the root graph's inlets, and the values are supplied by the host via `SeedPlan.Inputs` before a run — the root's external input surface can still grow with host-declared inlets after `Load`, so the sources criterion is left to start time.

**Observation inherits by default; optionally choose an egress per mount**: attach the root observer through `LoadOptions.Graph`. Supply `LoadOptions.ObserverFor(path)` only when child mount sites need different observers or record destinations. It is called **once per child mount during Load assembly**: path is the node id for one level and `outer/inner` for deeper levels; the root does not call it. Returning nil inherits the parent's effective observer, and a selected observer may also be inherited by descendants. Every callback already carries `Site.Path`, so one egress serves multiple levels with their own `pulse.path`; normally leave `ObserveConfig.Path` empty. The check tree **does not** call ObserverFor, so extra validation does not change the host's callback count.

## Edges and pitfalls

**A `literal` is shape-aligned to the registered type: lists and maps work out of the box.** What the YAML decoder hands over is a generic container (`[]any` / `map[string]any`), and `pulse.SeedByName` converts it recursively into the target type: `value: ["a","b"]` fills a `[]string` and `value: {a: 1}` fills a `map[string]int`, with no `resolve` in sight.

Three things it deliberately does **not** do (each fails loudly — use `resolve` for a correctly typed value). The criterion: **shape can be aligned by a machine; semantics only the caller knows.**

| Case | Error (measured on the author's machine; the type name follows your package) |
|---|---|
| elements are objects and the target is a struct (field mapping is the host's semantics) | `pulse: seed "demo.in": value type []interface {} not assignable to []yaml_test.Doc (element 0: cannot convert map[string]interface {} to yaml_test.Doc)` |
| string ↔ number coercion | `pulse: seed "demo.in": … (element 0: cannot convert string to int)` |
| truncating a float into an int / narrowing out of range | `… (element 0: cannot convert float64 to int)` · `… (element 0: cannot convert int to int8)` |

When you need one of those, hand `resolve` to `plan.Inputs` to produce a correctly typed value yourself:

```go
in, err := plan.Inputs(func(from yaml.SeedFrom) (any, error) {
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
- **The budget goes through `LoadOptions.MaxRunning` only**: it is the budget of the **whole declaration tree** (`<=0` = unlimited, the default). Do not also put `pulse.WithMaxRunning` into `LoadOptions.Graph` — when both are given the rule is "`MaxRunning > 0` overrides whatever is in `Graph` (even when smaller), and `<= 0` leaves it alone" (`int`'s zero value cannot tell "not set" from "set to 0", so `MaxRunning: 0` cannot mean "unlimited"); the seven combinations are pinned by `TestLoadOptionsQuotaPrecedence`;
- This is the only package with a third-party dependency (`gopkg.in/yaml.v3`); neither the root package nor `observe` depends on it.

Package-level API and examples: [yaml package docs](/en/packages/yaml/).
