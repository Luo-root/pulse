# Package reference

Pulse provides a graph execution core, with optional observation and YAML assembly packages. This page groups their entry points by responsibility so you can look up configuration, events, fields and strict semantics.

## Packages and dependencies

| Import path | Responsibility | Dependencies | Reference |
|---|---|---|---|
| `github.com/Luo-root/pulse` | Graph definitions, run inputs, Execution, nodes and static subgraphs, aspects and state views | Standard library | [Go API documentation](https://pkg.go.dev/github.com/Luo-root/pulse) · [Current source](https://github.com/Luo-root/pulse) |
| `github.com/Luo-root/pulse/observe` | Converts Observer callbacks into structured Records for text, slog, memory or asynchronous output | `pulse` + standard library | [observe package reference](/en/packages/observe/) |
| `github.com/Luo-root/pulse/yaml` | Assembles YAML graphs from registered Run bodies and produces input plans or static topology | `pulse` + `gopkg.in/yaml.v3` + standard library | [yaml package reference](/en/packages/yaml/) |

The dependency direction is `pulse` ← `observe` / `yaml`. The root does not import these packages; its dependency closure contains only the standard library. Observation and YAML assembly can be selected independently.

## Find the right reference

| Question | Entry |
|---|---|
| How does a definition differ from an execution, and how do I declare inputs or read outputs? | [Core concepts](/en/guide/concepts) · [Inputs and outputs](/en/guide/inputs-outputs) |
| How do nodes connect, subgraphs mount, and retries or cancellation work? | [Orchestration](/en/guide/orchestration) · [Static subgraphs](/en/guide/subgraphs) · [Retries and cancellation](/en/guide/retry-cancellation) |
| When is an event emitted, how do TraceID and Execution differ, and when must a sink flush? | [observe package reference](/en/packages/observe/) |
| Which YAML fields exist, how are factories and types registered, and what do Load and Analyze validate? | [yaml package reference](/en/packages/yaml/) |
| How do I integrate observation or declarative assembly from the beginning? | [Graph observation guide](/en/guide/observability) · [Declarative assembly guide](/en/guide/assembly) |

Package references provide complete minimal programs and lookup tables. Guides explain the integration step by step. The observe and yaml site pages come from their bilingual package READMEs; source links help verify the current contracts.

## Documentation version

This website uses `v0.5.0` as its API baseline. The command below pins that version. For subsequent development changes or unmerged branches, select main or matching local source as described in [installation](../guide/installation.md):

```bash
go get github.com/Luo-root/pulse@v0.5.0
```

Requires Go 1.25.0 or later. Released Go API documentation may differ from current source; read against the commit your application uses.
