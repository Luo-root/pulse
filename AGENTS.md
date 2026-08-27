# AGENTS.md

## What this is

Go library (`github.com/Luo-root/pulse`) — an AI Agent framework under v2 reconstruction. The v2 core is `kernel/` (plugin kernel + `kernel/flow` dataflow), `llm/` (provider-neutral model vocabulary + adapters) and `loop/` (stateless ReAct turn executor). The entire v1 `components/` tree has been removed. `pulse.go` at root is a stub (`package pulse`); it is not an entrypoint.

## Build & test

```bash
go build ./...          # verify compilation
go test ./...           # run all tests
go test -race -skip TestLive ./kernel/... ./llm/... ./loop/   # v2 core regression (no live API)
```

- Requires **Go 1.25.0+** (toolchain auto-downloads if missing).
- Provider adapter live-API smoke tests (`TestLive*` in `llm/openai`, `llm/anthropic`) are gated by environment variables (`PULSE_OPENAI_*`, `PULSE_ANTHROPIC_*`, `PULSE_MIMO_*`); without credentials they skip automatically.
- No Makefile, linter config, or CI pipeline exists. `go test ./...` is the only verification step.

## Repo layout

```
pulse.go                    # stub: package pulse (ignore)
kernel/                     # v2 plugin kernel: Context/ServiceKey/events/Plugin+Fiber/Loader
  flow/                     # data-driven node graph: typed keys, skip-as-arrival, aspects
llm/                        # v2 model layer: content-block vocabulary, ChatModel, Registry, openai/ + anthropic/ adapters
loop/                       # v2 stateless ReAct turn executor: ToolSet, HITL decision events
docs/design/               # design docs (plugin-kernel-v2.md, flow-v2-design.md, kernel-local-events.md, observability-v1-design.md)
examples/                   # 渐进装配示例（chat / react / flow）与观测原型
skills/                     # Example skill definitions (*.md with YAML frontmatter, gitignored)
```

## Key conventions

- **No `internal/` or `cmd/`** — this is a library, not a binary. All packages are public API.
- **Functional options pattern** used throughout (`loop.WithToolSet()`, `flow.WithMaxRunning()`, etc.).
- **Chinese comments and doc** are the norm; preserve them when editing.
- **v2 vocabulary contract**: `llm.GenerateRequest` only carries cross-provider stable fields; when a provider wire format has no counterpart, the adapter returns `ErrBadRequest` — never silently drop parameters and never add `map[string]any` escape hatches.
- **flow contract**: slots are pending | ready | skipped; skip is arrival, not failure; node error cancels the graph and is never rewritten as skip.
- **Secrets**: never commit `.env`, API keys or tokens. Live tests are env-gated.
