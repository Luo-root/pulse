# Package docs

Pulse ships exactly three packages:

| Package | What it is | Depends on | Docs |
|---|---|---|---|
| [`pulse`](https://github.com/Luo-root/pulse) (root) | the graph engine | **nothing** (standard library only) | [Core concepts](/en/guide/concepts) · [Orchestration](/en/guide/orchestration) |
| [`pulse/observe`](/en/packages/observe/) | graph observation: engine `Observer` callbacks → structured records | `pulse` | [Graph observation](/en/guide/observability) |
| [`pulse/yaml`](/en/packages/yaml/) | declarative assembly: YAML → graph | `pulse` + `yaml.v3` | [Declarative assembly](/en/guide/assembly) |

The page body of the latter two is the package's `README_en.md` verbatim, synced at build time by `site/scripts/sync-docs.mjs` — **the README is the source; editing it updates the site**.

The root package has no separate page: its API contracts live in godoc (every exported symbol carries one), usage lives in the guide, and the design lives in [`docs/design/pulse.md`](https://github.com/Luo-root/pulse/blob/main/docs/design/pulse.md).

::: tip Dependency direction
`pulse` ← `observe` / `yaml`, one-way and never reversed. The engine imports no observation package; a host that imports only the root package has an empty dependency closure.
:::
