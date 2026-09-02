[English](README.md) | [中文](README_zh.md)

# kernel/flow/yaml

E2 declarative graph assembly (**YAML only**): loads YAML into a `flow.Graph` + `SeedPlan`. No JSON assembly is provided.

Topology belongs to **A**: YAML must carry `id` / `uses` / `requires` / `provides`; `uses` maps to a **Run factory** (`func(*RunCtx) error`) on the `flow.Registry`, not to a `*Node`.

```go
reg := flow.NewRegistry()
flow.MustRegisterKey(reg, In)
reg.MustRegister("demo.step", func(rc *flow.RunCtx) error { /* ... */ return nil })

g, plan, err := flowyaml.Load(yamlBytes, reg, flowyaml.LoadOptions{})
_ = plan.Apply(g, nil) // literal；其它 kind 传 resolve
_ = g.Run()
```

- Timeout on the outside, Retry on the inside (consistent with the design doc)
- keys are reconciled by `{name, type}`; `type` = `reflect.Type.String()` (consistent with `RegisterKey`)
- duration is written `30s` / `100ms` (`time.ParseDuration`)
- the `observer:` field is ignored by Load; attach `WithObserver` via `LoadOptions.Graph`
- `version` is omitted or `1`
- this package may depend on `gopkg.in/yaml.v3`; the `kernel/flow` core does not depend on yaml

Spec: `docs/design/flow-v2-design.md` E2; implementation Issue #32.
