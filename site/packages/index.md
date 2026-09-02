# 包文档

全仓 27 个包的双语完整 README 由构建脚本从仓库单源同步生成（**与代码同源，构建时刷新**）。左侧侧边栏按域分组；本页是快速索引。

| 域 | 包 |
|---|---|
| 内核 | [kernel](/packages/kernel/) · [kernel/flow](/packages/kernel/flow/) · [kernel/flow/yaml](/packages/kernel/flow/yaml/) |
| 模型层 | [llm](/packages/llm/) · [llm/openai](/packages/llm/openai/) · [llm/anthropic](/packages/llm/anthropic/) |
| 执行 | [loop](/packages/loop/) |
| 工具与技能 | [toolset](/packages/toolset/) · [toolset/builtins](/packages/toolset/builtins/) · [toolset/mcp](/packages/toolset/mcp/) · [toolset/lsp](/packages/toolset/lsp/) · [skills](/packages/skills/) |
| 记忆 | [memory](/packages/memory/) · session · compaction · store · assemble · selfedit · index · index/openai · reflection · candidate |
| 可观测 | [observability](/packages/observability/) |
| 文本处理 | [textsplit](/packages/textsplit/) |
| 评测 | [eval](/packages/eval/) · [eval/war](/packages/eval/war/) |
| 示例 | [examples](/packages/examples/) |

::: tip 内容来源
每个包页面的正文 = 仓库中该包的 `README_zh.md`（中文站）/ `README.md`（英文站）原文，由 `site/scripts/sync-docs.mjs` 在构建时同步——仓库文档更新后站点自动跟进，无需二次维护。
:::
