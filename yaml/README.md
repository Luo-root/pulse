[English](README_en.md) | [中文](README.md)

# yaml

`pulse/yaml` 是声明式装图（**YAML only**，不提供 JSON）：把 YAML 装成 `pulse.Graph` + `SeedPlan`。

**拓扑归 YAML**：节点必填 `id` / `uses` / `requires` / `provides`；`uses` 对应 `pulse.Registry` 上的具名 Run 工厂（`func(*pulse.RunCtx) error`）——工厂只给 Run，**不返回 `*Node`**，因为它不该决定自己接在图的哪里。

```go
reg := pulse.NewRegistry()
pulse.MustRegisterKey(reg, In)
reg.MustRegister("demo.step", func(rc *pulse.RunCtx) error { /* ... */ return nil })

g, plan, err := yaml.Load(doc, reg, yaml.LoadOptions{GraphID: "demo"})

// from.kind 不是 literal 时取值由宿主给出——引擎不做 IO。
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

要点：

- 节点切面顺序：**Timeout 在外、Retry 在内**（先写的更靠外）。
- Key 用 `{name, type}` 对账；`type` = `reflect.Type.String()`，与 `RegisterKey` 一致。
- 时间字段用 Go 的 `ParseDuration` 形式（`30s` / `100ms`），不写裸数字。
- **字面量按登记类型做形状对齐**：`literal` 的取值由 YAML 解码器给出（泛型容器 `[]any` / `map[string]any`），`pulse.SeedByName` 会把它递归转成目标类型——`value: ["a","b"]` 直接填 `[]string`、`value: {a: 1}` 直接填 `map[string]int`，不需要 `resolve`。**不做**的：map→struct 的字段猜测、字符串↔数字互转、浮点截断成整数（这些一律报错，用 `resolve` 给出类型正确的值）。
- `Seed.from.kind` 除 `literal` 外需要宿主传 `resolve` 回调——**引擎不做 IO**：读文件、读 env、从请求取 context 都是宿主的事。
- 本包依赖 `gopkg.in/yaml.v3`；根包 `pulse` 不依赖 yaml。

设计见 [`docs/design/pulse.md`](../docs/design/pulse.md) §5 装配。
