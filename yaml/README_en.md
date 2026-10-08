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

See [`docs/design/pulse.md`](../docs/design/pulse.md) §5 for the design.
