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
in, err := plan.Inputs(resolve)

_ = g.Run(ctx, in)
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

It expands to the engine's **`pulse.Sub`** (static composition, see §5 "a graph as a node" in the design doc): the child graph is **built at `Load` time** (one instance per mounting point), the parent-side node's `Requires` / `Provides` follow from `in` / `out`, and the engine's static validation (sources / cycles / slots) still applies. From that:

- **The whole tree is visible once assembled, without running it**: `g.Topology()` recurses along the static mounting edges, `Nodes[i].Sub` is the child graph at that point (go deeper with `.Sub` again), paths are derived from the **mounting position** (`step1`, `step1/toInner`), and the graph identity is `<root id>/<mounting path>`;
- **A child graph's static problems are reported at `Load` time**, and the message names the **mounting path**: `pulse/yaml: graph "inner" (mounted at "step1/toInner"): pulse: node "leaf" requires "c.nope" but nothing provides it (declare it with DeclareInput if the value comes from outside)`;
- **One spec mounted in several places = several independent instances**: each place has its own slots and mounting path, both places run, and observation tells them apart by graph id and `pulse.path`;
- **Cancellation enters the child from the mounting point**: cancel the parent context and the child's nodes see it on the spot — not a run that lasts the whole span and swallows the cancellation.

Rules:

- **The boundary is written where the wiring is**: `in` / `out` are always **`child key: parent key`**; a graph node may **not** also carry `requires` / `provides` — and conversely a **factory node may not carry `in` / `out`**: writing the wrong side silently does nothing, so both are reported at Load.
- **Types are compared at Load**: both ends are resolved against `{name, type}` using the **same** `type` token — if the parent-side key is registered with another type, Load reports it. On the Go side that is a compile error; here it is an assembly error.
- **Names are compared at Load**: referencing a graph that does not exist, binding a key no node of the child declares, or a graph with no nodes — all three are reported by name.
- **Reference cycles are caught at Load**: nesting a graph inside a graph is allowed with no depth limit, but a cycle that cannot be built (`a → b → a`) is rejected with one **concrete** path (same shape as the engine's dependency-cycle error).
- **A key fed by `in:` may not also be provided inside the child** → reported at Load: a key has exactly one source, and what `in:` does is declare that key as an inlet of the child — the declaration itself hits `ErrDuplicateSource` (no need to run it).
- **Pass-through**: one child-side key sitting in **both** `in:` and `out:` (with no node inside producing it) — the same rule as the engine's "an inlet and an outlet sharing a name is a pass-through" (`problemsOutputsLocked`). The value the parent feeds in *is* the value of that outlet, so neither "some node inside must read it" nor "some node inside must produce it" applies; **a producer inside still makes it an error** (the bullet above: two sources for one slot).
- **A subgraph's `seeds`**: the entries that actually carry a value may only use `literal` (`env` / `file` / `context` need host IO, and `SeedPlan` belongs to the **parent** graph — reported at Load rather than halfway through a run; the `from` of a `skip: true` entry has no meaning and follows the top-level `seeds` reading). Names and types are reconciled against the registry, and **the same key seeded twice in one graph** is reported too (the second write would be silently ignored, and which value wins depends on declaration order).
- **A subgraph seeds a key the parent also feeds with `in`** → reported at Load (the same slot would be seeded twice and the second write silently ignored, while both declarations look reasonable).
- **Every `graphs:` declaration is checked**; the referenced ones **once per reference site**: assembly first builds a **build-only, never-run** check tree (the very same assembly code, with every child mounted in its static form), one instance per reference site — so "does this key inside the child have a source" holds **per reference site**: mount the same spec in two places and feed that `in:` in only one of them, and the report points at the site that did not feed it (a union check would read "the other site fed it" as "this site fed it as well"). Each instance's interface is declared as ports from **that site's** `in:` / `out:` (`DeclareInput` / `DeclareOutput`), so "this was an external input all along" and "you forgot to wire it" stay apart; the criterion is the engine's own (`Graph.Validate`, see design doc §5), and **word for word the same message `Start` would give** — including the new one: an outlet declared in `out:` that nothing inside the child produces is reported at Load time, instead of waiting for the child to run and say `never arrived`.
- **A spec nothing references is *not* judged for sources**: it has **no mounting site**, hence **no interface** — the keys nothing provides there are either "the site that eventually mounts it will feed them with `in:`" or "a typo", and assembly cannot tell (judging it as a closed graph reports every one of the former, so one template not wired up yet would fail the whole document's `Load`). Such a spec is therefore checked for the things that **are** decidable — factory names / empty `nodes` / duplicate ids / source conflicts / dependency cycles / the streaming set — and its sources are judged once it is **actually referenced**, per **that site's** `in:` / `out:`.
- **`Load` touches no ctx at all**: a graph is a definition and a definition has no lifetime — which cancellation domain a run lives in is decided at `Run` / `NewExecution` time (`LoadOptions.Context` was deleted for that reason: keeping it would only be a silently dead knob). A child's cancellation still reaches in from the mounting point (at run time the mounting step derives the child execution's ctx from that layer's parent node). A failed `Load` returns `nil, nil, err`, so the caller has no handle to clean anything up — which is why every fallible check runs before the graph is handed out.
- **The root level's own criteria are still reported the moment a run starts** (`Run` / `NewExecution`; the root layer is not judged at assembly time): the top-level `seeds:` only declares keys as the root graph's inlets, and the values are supplied by the host via `SeedPlan.Inputs` before a run — the root's external input surface can still grow with host-declared inlets after `Load`, so the sources criterion is left to start time.
- **Aspects**: a graph node may carry `timeout` and `retry`, and both land on the **parent-side node** — `timeout: 30s` puts a time limit on the whole child graph, `retry: {attempts: 3, delay: 100ms}` runs that whole child graph up to three times; nodes **inside** the child keep their own. Every `retry:` attempt builds a **fresh child execution**, and the first attempt's bridge into the parent is discarded along with that attempt's **write set** — the parent only ever sees the **successful** attempt's output, never a half-and-half mix (write sets are defined in [`docs/design/pulse.md`](../docs/design/pulse.md) §4). A `retry:` placed on a node **inside** the child still retries just that node.

**Observation is per level (optional)**: `LoadOptions.ObserverFor(path)` can build an egress per level (different destinations per layer, say), where `path` is that level's path (empty for the root, the node id for one level, `outer/inner` for two). A child graph gets that call **once per mounting point, when `Load` mounts it at that level** (one call per mount, and the egress returned belongs to that level alone). Returning nil means the level follows whatever `LoadOptions.Graph` attached — the child inherits the parent's observer, and the **mounting position still arrives with every callback** (`Site.Path`), so the inherited egress carries `pulse.path` too: reach for `ObserverFor` only when you need a distinct egress instance. The build-only, never-run check tree **does not** call it — the host's callback count does not change just because an extra validation pass happened.

See [`docs/design/pulse.md`](../docs/design/pulse.md) §5 for the design.
