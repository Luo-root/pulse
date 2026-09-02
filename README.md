[English](README.md) | [中文](README_zh.md)

# Pulse

[![Go Version](https://img.shields.io/badge/Go-1.25.0-blue.svg)](https://go.dev/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

**Pulse** is a Go AI Agent framework undergoing a v2 rewrite.

The v2 kernel is built on reversible effects and dependency-reactive loading: a plugin kernel, a model adapter layer, and a stateless ReAct turn executor land first. The v1 Agent, legacy model adapters, DAG, memory, HITL, and telemetry implementations have been removed entirely; everything going forward is rewritten around the v2 kernel with no compatibility layer.

## Current Capabilities

| Package | Role | Start here |
|---|---|---|
| [`kernel`](kernel/README.md) | Plugin kernel: Context, reversible Effects, typed services, events, Fiber, Loader | `kernel.New()` / `kernel.Use()` |
| [`llm`](llm/README.md) | Provider-neutral message vocabulary, request/stream events, error classification, model Registry | `llm.NewRegistry()` / `llm.ChatModel` |
| [`llm/openai`](llm/openai/README.md) | OpenAI Chat Completions + Responses official SDK adapter | `openai.Register()` |
| [`llm/anthropic`](llm/anthropic/README.md) | Anthropic Messages official SDK adapter | `anthropic.Register()` |
| [`loop`](loop/README.md) | Stateless ReAct turn executor with tool calls and HITL decision events | `loop.NewAgent()` |
| [`toolset`](toolset/README.md) | Reversible tool Registry (`pulse.tools`), `AsToolSet()` adapts to loop; builtins / mcp / lsp sub-packages | `toolset.Plugin()` / `Registry.Register` |
| [`skills`](skills/README.md) | Agent Skills loader (agentskills.io; procedure packages, not Tools) | `skills.Open()` / `List`/`Load`/`ReadFile` |
| [`textsplit`](textsplit/README.md) | Text chunking: size budget + separator priority + byte offsets | `textsplit.Split` |
| [`kernel/flow`](kernel/flow/README.md) | Data-ready driven node orchestration (three slot states, Skip, E1 Observer) | `flow.New(ctx)` |
| [`kernel/flow/yaml`](kernel/flow/yaml/README.md) | E2 declarative YAML graph loading (topology home A: Factory only exposes Run) | `flowyaml.Load` |
| [`memory`](memory/README.md) | P2 memory & sessions (9 sub-packages): session / compaction / store / assemble / selfedit / index / candidate / reflection | `memory/README.md` global map |
| [`observability`](observability/README.md) | Official observability package: Bootstrap + Record + Sink (depends only on kernel) | `observability.Bootstrap()` |
| [`examples`](examples/README.md) | Progressive lessons 00–07: kernel ground / assembly + vocabulary / ReAct / HITL / flow / session memory / long-term memory / production | `go run ./examples/00-hello-kernel` |
| [`eval`](eval/README.md) | Evaluation suite: engineering-capability property tests + layered benchmarks + cross-framework comparison suite (`eval/war`) | `go test -race ./eval/` |

## Quick Start: Model + ReAct Tool Round

The shortest v2 path. Provide the API key via environment variables; never hard-code credentials.

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/llm/openai"
	"github.com/Luo-root/pulse/loop"
)

func main() {
	host := kernel.New()
	defer host.Dispose()

	reg := llm.NewRegistry(host)
	if err := openai.Register(host, reg); err != nil {
		panic(err)
	}
	if err := reg.Declare("main", llm.Config{
		Provider: openai.ProviderCompletions,
		Model:    "gpt-4o-mini",
		APIKey:   os.Getenv("OPENAI_API_KEY"),
	}); err != nil {
		panic(err)
	}
	model, err := reg.Open("main")
	if err != nil {
		panic(err)
	}

	tools := loop.NewMemToolSet()
	_ = tools.Register(llm.ToolDef{
		Name:        "echo",
		Description: "echoes the arguments back",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
	}, func(ctx context.Context, args json.RawMessage) (string, error) {
		return string(args), nil
	})

	agent, err := loop.NewAgent(model,
		loop.WithToolSet(tools),
		loop.WithSystemPrompt("You are a concise assistant."),
		loop.WithEventScope(host),
	)
	if err != nil {
		panic(err)
	}

	res, err := agent.Run(context.Background(), nil, llm.UserText("call the echo tool with text=hello"))
	if err != nil {
		panic(err)
	}
	fmt.Println(res.Final.Text())
}
```

For more models, streaming, multimodal input, reasoning parameters, the capability matrix, and error handling see [`llm/README.md`](llm/README.md); for HITL event examples see [`loop/README.md`](loop/README.md).

## Performance Benchmarks

Framework infrastructure overhead is quantified with same-machine, same-task comparisons: [`eval/war`](eval/war/README.md) (standalone nested module, [Issue #103](https://github.com/Luo-root/pulse/issues/103)) — Pulse full production assembly vs Eino v0.9.19's official production entry, equally-thin stub models, and a correctness sentinel asserting every task really runs (i9-14900HX / Go 1.25; **compare magnitudes and multiplier ranges, not single digits**):

| Task | Pulse | Eino v0.9.19 | Multiplier range |
|---|---|---|---|
| T1 text round (reused: assemble once, pure runtime) | 3.6 µs / 22 allocs | 36.6–40.9 µs / 407 allocs | **~10–11×** |
| T1 text round (cold start: full rebuild each run) | 10.5–10.7 µs / 139 allocs | 38.8–39.0 µs / 425 allocs | **~3.7×** |
| T2 tool round-trip (cold-start upper bound) | 15.1–15.9 µs / 177 allocs | 116.4–117.7 µs / 1364 allocs | **~7.4–7.7×** |
| T3 linear-chain orchestration (3 passthrough nodes) | 8.6–8.9 µs / 73 allocs | 17.9–18.1 µs / 323 allocs | **~2.0×** |
| T4 fan-out/fan-in DAG (1 source → 2 branches → AND join) | 9.0–9.3 µs / 73 allocs | 30.3–37.9 µs / 411–462 allocs (Graph keyed fan-in / Workflow field-mapping variants) | **~3.3–4.1×** |

Accounting, reproduction commands, and the full reading live in [`eval/war/README.md`](eval/war/README.md). All gaps are negligible against a real LLM call (seconds) — this quantifies the base price of an architectural choice, not an "unusable" verdict. Three takeaways:

1. **Orchestration fan-out is free**: flow's AND slots make branch joins nearly free — the DAG (T4) costs the same as the linear chain (T3), same allocs; Eino's join scheduling runs ~1.7× over its own linear chain, and `compose.Workflow` field mapping adds another +15–20%.
2. **Explicit DAG dataflow**: flow nodes declare Requires / Provides at construction (Key + three-state slots), so join dependencies live on the node signature; the same topology in `compose.Graph` relies on runtime machinery — AllPredecessor triggering + `WithOutputKey` keying + default map merging — and `compose.Workflow` adds a field-mapping layer on top, leaving the dataflow semantics more implicit at equal topology.
3. **Layered assembly with kernel**: flow does not import kernel and runs graphs standalone (T3/T4 are exactly that form); when needed, the assembly layer injects the kernel host / services into node closures, and orchestration steps consume registered capabilities directly (T1/T2's Agent rounds are the full kernel form). [`kernel/flow/yaml`](kernel/flow/yaml/README.md) adds declarative graph loading — standalone runs, kernel assembly, and YAML declaration are orthogonal usages, combined per scenario.

## v2 Architecture

```text
Caller
  │
  ├── kernel.Context
  │     ├── ServiceKey: typed services
  │     ├── Effect: unload reverts
  │     ├── Event: Emit/Waterfall/Parallel (whole tree) + EmitLocal/WaterfallLocal (this scope)
  │     └── Plugin / Fiber / Loader: dependency-reactive loading
  │
  ├── observability.Bootstrap   # Use first; side-channel subscription to fiber_state / loader_action
  │
  ├── llm.Registry
  │     └── ChatModel
  │           ├── openai: Chat Completions / Responses
  │           └── anthropic: Messages (MaxTokens required)
  │
  ├── loop.Agent (mount on a request scope)
  │     ├── model inference (llm.WithEventScope → Local)
  │     ├── ToolSet tool calls
  │     └── before_tool_call WaterfallLocal: HITL mount point
  │
  └── kernel/flow (+ flow/yaml)
        ├── Graph: AND / Skip / Observer
        └── YAML loading: Registry + SeedPlan (the assembly layer performs IO)
```

The design blueprint and the v1 → v2 migration order live in [`docs/design/plugin-kernel-v2.md`](docs/design/plugin-kernel-v2.md); request-scoped local event dispatch in [`docs/design/kernel-local-events.md`](docs/design/kernel-local-events.md).

## Design Boundaries

- **Hard breaking change**: the v1 model abstraction and everything depending on it is deleted; no compatibility layer.
- **Vocabulary first**: `llm` only accepts fields with stable cross-provider semantics; when a wire format has no counterpart, the adapter returns an explicit `ErrBadRequest` — never silently drops parameters.
- **Plugins are not a slogan**: every mutation of the environment is registered as a reversible Effect; service dependency changes drive Fiber load / unload.
- **Agents are stateless**: `loop.Agent` runs exactly one turn; history, session storage, retry, and failover belong to the caller or later v2 components.
- **v1 components are gone**: tools / MCP / sandbox / Skills are rewritten as v2 plugins; the old packages are not resurrected.

## Build & Test

```powershell
# Requires Go 1.25+
go build ./...
go test ./...

# v2 core regression (no real API)
go test -race -skip TestLive ./kernel/... ./llm/... ./loop/ ./toolset/... ./skills/ ./textsplit/... ./memory/... ./observability/ ./examples/04-flow/ ./examples/07-production/

# Provider adapters separately
go test -race -skip TestLive ./llm/openai/
go test -race -skip TestLive ./llm/anthropic/
```

Live API smoke tests for OpenAI / Anthropic / MiMo are gated by environment variables (`PULSE_OPENAI_*` / `PULSE_ANTHROPIC_*` / `PULSE_MIMO_*`); without credentials they skip automatically. MiniMax goes through the OpenAI-compatible generic path via `PULSE_OPENAI_BASE_URL` (see the llm/openai README). Never commit `.env` files, tokens, or private keys.

## Repository Layout

```text
kernel/                    v2 plugin kernel
  flow/                    data-ready driven node graph + Observer
  flow/yaml/               E2 declarative YAML graph loading
llm/                       v2 model vocabulary, Registry, provider adapters
loop/                      v2 stateless ReAct turns
toolset/                   reversible tool registry (builtins / mcp / lsp sub-packages)
skills/                    Agent Skills loader (agentskills.io)
textsplit/                 text chunking (shared by index/openai and future long-text modules)
memory/                    P2 memory & sessions (session / compaction / store / assemble / selfedit / index / candidate / reflection)
observability/             v2 official observability package (Bootstrap / Record / Sink)
eval/                      evaluation suite: property tests + layered benchmarks + cross-framework comparison suite (`eval/war`)
docs/design/               architecture & migration docs (Accepted)
examples/                  progressive lessons 00–07 + internal/demoapp assembly layer
```

## License

[MIT License](LICENSE)
