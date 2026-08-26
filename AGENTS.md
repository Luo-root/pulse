# AGENTS.md

## What this is

Go library (`github.com/Luo-root/pulse`) — an AI Agent framework under v2 reconstruction. The v2 core is `kernel/` (plugin kernel), `llm/` (provider-neutral model vocabulary + adapters) and `loop/` (stateless ReAct turn executor). Legacy `components/agent`, `components/chatmodel`, `components/flowchart`, `components/memory`, `components/hitl` and `components/telemetry` have been removed; the remaining `components/*` (tools, mcp, sandbox, skill, schema, stream, bufutil) are pre-v2 implementations kept until they are rewritten as v2 plugins. `pulse.go` at root is a stub (`package pulse`); it is not an entrypoint.

## Build & test

```bash
go build ./...          # verify compilation
go test ./...           # run all tests
go test -race -skip TestLive ./kernel/ ./llm/... ./loop/   # v2 core regression (no live API)
```

- Requires **Go 1.25.0+** (toolchain auto-downloads if missing).
- Provider adapter live-API smoke tests (`TestLive*` in `llm/openai`, `llm/anthropic`) are gated by environment variables (`PULSE_OPENAI_*`, `PULSE_ANTHROPIC_*`, `PULSE_MIMO_*`); without credentials they skip automatically.
- `mcp` and `sandbox` tests spawn external processes (Node, Python, Go compilers); skip with `-run` if unavailable.
- No Makefile, linter config, or CI pipeline exists. `go test ./...` is the only verification step.

## Repo layout

```
pulse.go                    # stub: package pulse (ignore)
kernel/                     # v2 plugin kernel: Context/ServiceKey/events/Plugin+Fiber/Loader
llm/                        # v2 model layer: content-block vocabulary, ChatModel, Registry, openai/ + anthropic/ adapters
loop/                       # v2 stateless ReAct turn executor: ToolSet, HITL decision events
docs/design/                # design docs (plugin-kernel-v2.md is the v2 blueprint)
components/
  bufutil/                  # capped buffer helpers
  mcp/                      # MCP client (JSON-RPC 2.0 over stdio/SSE) — pre-v2, rewrite pending
  sandbox/                  # Process-based code execution sandbox — pre-v2
  schema/                   # legacy data structures still used by remaining components
  skill/                    # Markdown+YAML frontmatter skill definitions
  stream/                   # Stream multicast (one-to-many broadcast)
  tools/                    # ToolRegistry + built-in tools — pre-v2, rewrite pending
skills/                     # Example skill definitions (*.md with YAML frontmatter, gitignored)
```

## Key conventions

- **No `internal/` or `cmd/`** — this is a library, not a binary. All packages are public API.
- **Functional options pattern** used throughout (`loop.WithToolSet()`, `tools.WithPermission()`, etc.).
- **Chinese comments and doc** are the norm; preserve them when editing.
- **v2 vocabulary contract**: `llm.GenerateRequest` only carries cross-provider stable fields; when a provider wire format has no counterpart, the adapter returns `ErrBadRequest` — never silently drop parameters and never add `map[string]any` escape hatches.
- **Path safety**: `tools/file_ops.go` enforces all file ops stay within the working directory via `safePath()`. Don't bypass this.
- **Dangerous command blocklist** in `tools/command.go` intercepts destructive shell commands.
- **Secrets**: never commit `.env`, API keys or tokens. Live tests are env-gated.

## MCP config

MCP server configs are JSON files loaded via `mcp.LoadConfig()`. Format:
```json
{"mcpServers": [{"name": "...", "transport": {"command": "...", "args": [...], "env": [...]}}]}
```
Environment variables use `${VAR}` syntax (not `$VAR`).

## Skills

Skills are `SKILL.md` files with YAML frontmatter (`name`, `description`, `allowed-tools`, `parameters`, `timeout`). The `skills/` directory is gitignored. Use `skill.ParseSkillMarkdown()` to parse; `SkillLoader.LoadFromDir()` to batch-load.
