# 包文档

Pulse 只有三个包：

| 包 | 是什么 | 依赖 | 文档 |
|---|---|---|---|
| [`pulse`](https://github.com/Luo-root/pulse)（根） | 图引擎 | **零**（只用标准库） | [核心概念](/guide/concepts) · [编排](/guide/orchestration) |
| [`pulse/observe`](/packages/observe/) | 图观测：引擎 `Observer` 回调 → 结构化记录 | `pulse` | [图观测](/guide/observability) |
| [`pulse/yaml`](/packages/yaml/) | 声明式装图：YAML → 图 | `pulse` + `yaml.v3` | [声明式装图](/guide/assembly) |

后两个包的页面正文 = 仓库中该包的 `README.md`（中文）原文，由 `site/scripts/sync-docs.mjs` 在构建时同步——**与代码同源，改 README 即改站点**。

根包没有单独的包页：它的 API 契约写在 godoc 里（每个导出符号都带一条），用法在指南页，设计在[`docs/design/pulse.md`](https://github.com/Luo-root/pulse/blob/main/docs/design/pulse.md)。

::: tip 依赖方向
`pulse` ← `observe` / `yaml`，单向且不许反向。引擎不 import 任何观测包；只 import 根包的宿主，依赖闭包是空的。
:::
