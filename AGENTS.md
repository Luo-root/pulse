# AGENTS.md

## What this is

Go library (`github.com/Luo-root/pulse`) — an AI Agent framework with model abstraction, tool registry, memory, MCP client, workflow engine, sandbox, and skill system. All component code lives under `components/`. `pulse.go` at root is a stub (`package pulse`); it is not an entrypoint.

## Build & test

```bash
go build ./...          # verify compilation
go test ./...           # run all tests
go test ./components/agent/...          # single package
go test -run TestAgent_Send ./components/agent/...  # single test
```

- Requires **Go 1.25.0+** (toolchain auto-downloads if missing).
- `openai` package tests call a live API and will fail without valid credentials — this is expected.
- `mcp` and `sandbox` tests spawn external processes (Node, Python, Go compilers); skip with `-run` if unavailable.
- No Makefile, linter config, or CI pipeline exists. `go test ./...` is the only verification step.

## Test files are gitignored

`*_test.go` is listed in `.gitignore`. Tests exist on disk but are **not tracked in git**. When creating or modifying test files, be aware they won't appear in `git status` or commits unless `.gitignore` is changed first.

## Repo layout

```
pulse.go                    # stub: package pulse (ignore)
components/
  agent/                    # Agent core: multi-turn loop, tool calling, usage tracking
  chatmodel/                # BaseModel interface + openai/, anthropic/, mock_model.go
  schema/                   # Message, ToolCall, StreamReader, Tool definitions
  tools/                    # ToolRegistry + built-in tools (file, command, web, chromium, etc.)
  memory/                   # 3-layer memory: system prompt, short-term window, long-term (GORM+SQLite+HNSW)
  mcp/                      # MCP client (JSON-RPC 2.0 over stdio), multi-server Manager
  sandbox/                  # Process-based code execution sandbox (Python/Node/Go/Shell)
  flowchart/                # DAG workflow engine with topological parallel execution
    node/                   # Node interface, functional nodes, interceptors (retry/timeout/circuit-breaker)
  skill/                    # Markdown+YAML frontmatter skill definitions
  stream/                   # Stream multicast (one-to-many broadcast)
  flow/                     # FlowContext, DataSlot, SafeMap — workflow data plumbing
skills/                     # Example skill definitions (*.md with YAML frontmatter)
```

## Key conventions

- **No `internal/` or `cmd/`** — this is a library, not a binary. All packages are public API.
- **Functional options pattern** used throughout (`agent.WithSessionID()`, `tools.WithPermission()`, etc.).
- **Chinese comments and doc** are the norm; preserve them when editing.
- **Path safety**: `tools/file.go` enforces all file ops stay within the working directory via `safePath()`. Don't bypass this.
- **Dangerous command blocklist** in `tools/command.go` intercepts destructive shell commands.
- **`chat.db`** is a SQLite file created at runtime for long-term memory and user config. It's gitignored.
- **Coverage**: `coverage.out` exists at root from prior runs. Use `go test -coverprofile=coverage.out ./...` to regenerate.

## MCP config

MCP server configs are JSON files loaded via `mcp.LoadConfig()`. Format:
```json
{"mcpServers": [{"name": "...", "transport": {"command": "...", "args": [...], "env": [...]}}]}
```
Environment variables use `${VAR}` syntax (not `$VAR`).

## Skills

Skills are `SKILL.md` files with YAML frontmatter (`name`, `description`, `allowed-tools`, `parameters`, `timeout`). The `skills/` directory is gitignored. Use `skill.ParseSkillMarkdown()` to parse; `SkillLoader.LoadFromDir()` to batch-load.
