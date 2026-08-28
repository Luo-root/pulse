# kernel/flow/yaml

E2 声明式装图：把 YAML 装成 `flow.Graph` + `SeedPlan`。

拓扑归属 **A**：YAML 必填 `id` / `uses` / `requires` / `provides`；`uses` 对应 `flow.Registry` 上的 **Run 工厂**（`func(*RunCtx) error`），不返回 `*Node`。

```go
reg := flow.NewRegistry()
flow.MustRegisterKey(reg, In)
reg.MustRegister("demo.step", func(rc *flow.RunCtx) error { /* ... */ return nil })

g, plan, err := flowyaml.Load(yamlBytes, reg, flowyaml.LoadOptions{})
_ = plan.Apply(g, nil) // literal；其它 kind 传 resolve
_ = g.Run()
```

- Timeout 在外、Retry 在内（与设计文一致）
- Key 用 `{name, type}` 对账；`type` = `reflect.Type.String()`（与 `RegisterKey` 一致）
- 本包可依赖 `gopkg.in/yaml.v3`；`kernel/flow` 核心不依赖 yaml

规格：`docs/design/flow-v2-design.md` E2；实现 Issue #32。
