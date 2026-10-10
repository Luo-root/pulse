[English](README_en.md) | [中文](README.md)

# yaml

`pulse/yaml` is declarative graph assembly (**YAML only**, no JSON): it loads YAML into a `pulse.Graph` + `SeedPlan`.

**Topology belongs to the YAML**: a node must carry `id` / `uses` / `requires` / `provides`; `uses` maps to a named Run factory (`func(*pulse.RunCtx) error`) on the `pulse.Registry` — a factory only supplies `Run` and **does not return a `*Node`**, because it must not decide where it sits in the graph.

```go
reg := pulse.NewRegistry()
pulse.MustRegisterKey(reg, In)
reg.MustRegister("demo.step", func(rc *pulse.RunCtx) error { /* ... */ return nil })

g, plan, err := yaml.Load(doc, reg, yaml.LoadOptions{GraphID: "demo"})

// For any from.kind other than literal the host supplies the value — the engine does no IO.
resolve := func(from yaml.SeedFrom) (any, error) { return loadDocs(from.Path) }
_ = plan.Apply(g, resolve)

_ = g.Run()
```

```yaml
version: 1
seeds:
  - key: {name: docs, type: "[]Doc"}
    from: {kind: file, path: docs.json}
nodes:
  - id: summarize
    uses: demo.step
    requires: [{name: docs, type: "[]Doc"}]
    provides: [{name: summary, type: string}]
    timeout: 30s
    retry: {attempts: 3, delay: 100ms}
```

Notes:

- Node aspect order: **Timeout outside, Retry inside** (whichever is written first is the outer one).
- Keys are reconciled by `{name, type}`; `type` is `reflect.Type.String()`, matching `RegisterKey`.
- Duration fields use Go's `ParseDuration` form (`30s` / `100ms`) — do not write bare numbers.
- **Literals are shape-aligned to the registered type**: a `literal` value arrives as whatever the YAML decoder produced (generic containers `[]any` / `map[string]any`), and `pulse.SeedByName` converts it recursively into the target type — `value: ["a","b"]` fills a `[]string` and `value: {a: 1}` fills a `map[string]int` without any `resolve`. What it deliberately does **not** do: guessing struct fields from a map, coercing strings to numbers, or truncating floats into ints (those fail loudly — use `resolve` to supply a correctly typed value).
- Any `Seed.from.kind` other than `literal` requires the host to pass a `resolve` callback — **the engine does no IO**: reading files, env or request context is the host's job.
- This package depends on `gopkg.in/yaml.v3`; the root `pulse` package does not depend on yaml.

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

It expands to the engine's `pulse.Sub` (see §5 "a graph as a node" in the design doc): the parent-side node's `Requires` / `Provides` follow from `in` / `out`, and the engine's static validation (sources / cycles / slots) still applies. Rules:

- **The boundary is written where the wiring is**: `in` / `out` are always **`child key: parent key`**; a graph node may **not** also carry `requires` / `provides` — the boundary has exactly one statement.
- **Types are compared at Load**: both ends are resolved against `{name, type}` using the **same** `type` token — if the parent-side key is registered with another type, Load reports it. On the Go side that is a compile error; here it is an assembly error.
- **Names are compared at Load**: referencing a graph that does not exist, binding a key no node of the child declares, or a graph with no nodes — all three are reported by name.
- **Reference cycles are caught at Load**: nesting a graph inside a graph is allowed with no depth limit, but a cycle that cannot be built (`a → b → a`) is rejected with one **concrete** path (same shape as the engine's dependency-cycle error).
- **A key fed by `in:` may not also be provided inside the child** → reported at Load: a key has exactly one source, so running that step would hit `ErrDuplicateSource`.
- **A subgraph's `seeds` may only use `literal`**: `env` / `file` / `context` need host IO, and `SeedPlan` belongs to the **parent** graph — reported at Load rather than halfway through a run. Names and types are reconciled against the registry, and **the same key seeded twice in one graph** is reported too (the second write would be silently ignored, and which value wins depends on declaration order).
- **A subgraph seeds a key the parent also feeds with `in`** → reported at Load (the same slot would be seeded twice and the second write silently ignored, while both declarations look reasonable).
- **Every `graphs:` declaration is checked**, including ones nothing references, and **once per reference site**: only the root level's nodes are actually added to the graph, since a child is built when the run reaches it — check the root alone and a mistake two levels down only blows up halfway through a run, while an unreferenced spec never blows up at all. The mechanism is a **build-only, never-run** throwaway graph per reference site going through the very same assembly code (the engine's assembly checks: node ids, duplicates, source conflicts, bindings), with that site's available sources seeded (the spec's own `seeds` plus the keys the parent feeds with `in:`), and finally the engine's own read-only check (`Graph.Validate`, see design doc §5) — so "a `Requires` nothing provides or seeds" and "dependency cycle" inside a subgraph are reported at `Load` as well, **word for word the same message `Start` would give**. Per reference site rather than per spec: the same graph referenced twice can have different `in:` sets, and a union check would read "the other site fed it" as "this site fed it as well".
- **Aspects**: a graph node may still carry `timeout` / `retry`, and they land on the **parent-side node** — `timeout: 30s` puts a time limit on the whole child graph; nodes **inside** the child keep their own.
- **One subgraph referenced twice = two independent instances**: a fresh graph is built per run (the one-shot contract comes for free), and observation separates them by graph id and `pulse.path`.

**Observation is per level**: `LoadOptions.ObserverFor(path)` builds an egress for each level, where `path` is that level's path (empty for the root, the node id for one level, `outer/inner` for two) — hand it to `observe.ObserveConfig.Path` and every record of that level carries `pulse.path`. Returning nil means the level follows whatever `LoadOptions.Graph` attached (the child inherits the parent's observer, just without a layer).

See [`docs/design/pulse.md`](../docs/design/pulse.md) §5 for the design.
