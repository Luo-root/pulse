# Package docs

Full bilingual READMEs for all 27 packages are generated from the repository single source by the build script (**same source as the code, refreshed at build time**). The sidebar groups them by domain; this page is the quick index.

| Domain | Packages |
|---|---|
| Kernel | [kernel](/en/packages/kernel/) · [kernel/flow](/en/packages/kernel/flow/) · [kernel/flow/yaml](/en/packages/kernel/flow/yaml/) |
| Model layer | [llm](/en/packages/llm/) · [llm/openai](/en/packages/llm/openai/) · [llm/anthropic](/en/packages/llm/anthropic/) |
| Execution | [loop](/en/packages/loop/) |
| Tools & skills | [toolset](/en/packages/toolset/) · [toolset/builtins](/en/packages/toolset/builtins/) · [toolset/mcp](/en/packages/toolset/mcp/) · [toolset/lsp](/en/packages/toolset/lsp/) · [skills](/en/packages/skills/) |
| Memory | [memory](/en/packages/memory/) · session · compaction · store · assemble · selfedit · index · index/openai · reflection · candidate |
| Observability | [observability](/en/packages/observability/) |
| Text | [textsplit](/en/packages/textsplit/) |
| Evaluation | [eval](/en/packages/eval/) · [eval/war](/en/packages/eval/war/) |
| Examples | [examples](/en/packages/examples/) |

::: tip Content source
Each package page's body = the package's `README_zh.md` (Chinese site) / `README.md` (English site) verbatim, synced at build time by `site/scripts/sync-docs.mjs` — repository doc updates flow to the site automatically, with no second copy to maintain.
:::
