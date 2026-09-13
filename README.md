[English](README.md) | [中文](README_zh.md)

<div align="center">
  <a href="https://luo-root.github.io/pulse/">
    <img alt="Pulse" src=".github/assets/logo.svg" width="260" />
  </a>
</div>

<div align="center">
  <h3>Go AI agent framework — reversible effects, reactive service loading.</h3>
</div>

<div align="center">
  <a href="https://go.dev/"><img alt="Go 1.25.0" src="https://img.shields.io/badge/Go-1.25.0-blue.svg" /></a>
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/License-MIT-green.svg" /></a>
  <a href="https://github.com/Luo-root/pulse/releases/tag/v0.2.2"><img alt="Release v0.2.2" src="https://img.shields.io/badge/release-v0.2.2-2563eb.svg" /></a>
  <a href="https://luo-root.github.io/pulse/"><img alt="Docs: English | 中文" src="https://img.shields.io/badge/docs-English%20%7C%20%E4%B8%AD%E6%96%87-2563eb.svg" /></a>
</div>

<br />

**Pulse** is a Go AI agent framework built around a plugin kernel, shipping its v2 core as v0.2.2.

The v2 kernel is built on reversible effects and dependency-reactive loading. The core rewrite has landed: a plugin kernel, a provider-neutral model layer, a stateless ReAct turn executor, the tool & skills system, the memory layer (sessions, compaction, long-term store, assembly), a dual-foundation observability stack (envelope + per-package folding adapters with a direct-write Collector), declarative flow orchestration, and the two-layer assembly (the `memory` facade + `host` cross-package wiring). The v1 Agent, legacy model adapters, DAG, memory, HITL, and telemetry implementations were removed entirely with no compatibility layer.

## Release & Compatibility

Pulse ships under the 0.x SemVer convention. From **v0.2.0**:

- **Breaking changes ride minor releases only** — a patch release never breaks. Every breaking change is listed at the top of its Release notes.
- **Frozen contracts** (a change to any of these requires a minor release and a Release-notes entry):
  - the `llm` request vocabulary contract: unsupported parameter → `ErrBadRequest`, never silently dropped;
  - the `kernel/flow` slot contract: `pending` / `ready` / `skipped`, skip is arrival rather than failure, node errors cancel the graph;
  - kernel plugin lifecycle semantics: same-name supersede without restore, event listeners are Effects, generational commits;
  - the session event stream format (header v1/v2);
  - the `MemoryStore` / `SessionStore` method sets and the sentinel-error semantics of optional capability interfaces (`Seeder`, `ImportStore`).
- No second full rewrite: there will be no v1→v2-style tree removal without a compatibility path.

## Current Capabilities

| Package | Role | Start here |
|---|---|---|
| [`kernel`](kernel/README.md) | Plugin kernel: Context, reversible Effects, typed services, events, Fiber, Loader | `kernel.New()` / `kernel.Use()` |
| [`llm`](llm/README.md) | Provider-neutral message vocabulary, request/stream events, error classification, model Registry | `llm.NewRegistry()` / `llm.ChatModel` |
| [`llm/openai`](llm/openai/README.md) | OpenAI Chat Completions + Responses official SDK adapter | `openai.Register()` |
| [`llm/anthropic`](llm/anthropic/README.md) | Anthropic Messages official SDK adapter | `anthropic.Register()` |
| [`loop`](loop/README.md) | Stateless ReAct turn executor with tool calls and HITL decision events | `loop.NewAgent(model, name)` |
| [`toolset`](toolset/README.md) | Reversible tool Registry (`pulse.tools`), `AsToolSet()` adapts to loop; builtins / mcp / lsp sub-packages | `toolset.Plugin()` / `Registry.Register` |
| [`skills`](skills/README.md) | Agent Skills loader (agentskills.io; procedure packages, not Tools) | `skills.Open()` / `List`/`Load`/`ReadFile` |
| [`textsplit`](textsplit/README.md) | Text chunking: size budget + separator priority + byte offsets | `textsplit.Split` |
| [`kernel/flow`](kernel/flow/README.md) | Data-ready driven node orchestration (three slot states, Skip, E1 Observer) | `flow.New(ctx, graphID)` |
| [`kernel/flow/yaml`](kernel/flow/yaml/README.md) | E2 declarative YAML graph loading (topology home A: Factory only exposes Run) | `flowyaml.Load` |
| [`memory`](memory/README.md) | P2 memory & sessions (9 sub-packages + root-level assembly facade): session / compaction / store / assemble / selfedit / index / candidate / reflection | `memory/README.md` global map |
| [`host`](host/README.md) | Layer two of the two-layer assembly: cross-package seams (kernel injection + session↔loop event-driven persistence + observability / tool gate) | `host.New(host.Options{...})` |
| [`observability`](observability/README.md) | Official observability package: Bootstrap + Record + Sink + NewTraceID (depends only on kernel) | `observability.Bootstrap()` |
| [`eval`](eval/README.md) | Evaluation suite: engineering-capability property tests + layered benchmarks + cross-framework comparison suite (`eval/war`) | `go test -race ./eval/` |

## The three-question mental model

Three questions answer most of what a new user needs:

1. **How do models / tools get wired?** The shortest path is one `host.New(Options)` call (the [`host`](host/README.md) package: kernel injection → provider → model declarations → tool sources → optional session stack and observability; `DefaultAgent` hands you an agent). To see every step of the wiring, the Quick Start below still walks the manual chain: `kernel.New()` → `llm.NewRegistry(host)` → `openai.Register(...)` → `reg.Declare(...)` → `reg.Open(...)`.
2. **How does one turn run?** `agent.Run(ctx, input)` executes one stateless ReAct round: model ↔ tools until the model stops. History accumulation, retry/failover, and session persistence are owned by the caller — `loop` deliberately owns none of them.
3. **Where does state live?** Three stores, by lifetime: conversation events in the session log (`memory/session`), long-term facts in the item store (`memory/store`), service instances in the kernel's service repository. Everything else is stateless and replaceable.

## Quick Start: Model + ReAct Tool Round

Two paths: **the recommended one-call `host` assembly**, and **manual assembly** (every step spelled out). Provide the API key via environment variables; never hard-code credentials.

### One-call assembly (host)

```go
package main

import (
    "context"
    "fmt"
    "os"

    "github.com/Luo-root/pulse/host"
    "github.com/Luo-root/pulse/kernel"
    "github.com/Luo-root/pulse/llm"
    "github.com/Luo-root/pulse/llm/openai"
)

func main() {
    k := kernel.New() // the kernel is app-owned: your other plugins Use the same root
    defer k.Dispose()

    h, err := host.New(host.Options{
        Kernel:    k,
        Providers: []host.Provider{host.Provider(openai.Register)},
        Models: []host.ModelDecl{{
            Name: "main",
            Config: llm.Config{
                Provider: openai.ProviderCompletions,
                Model:    "gpt-4o-mini",
                APIKey:   os.Getenv("OPENAI_API_KEY"),
            },
        }},
    })
    if err != nil {
        panic(err)
    }

    agent, err := h.DefaultAgent(context.Background(), host.DefaultAgentOptions{
        Name:   "assistant",
        Model:  "main",
        System: "You are a concise assistant.",
    })
    if err != nil {
        panic(err)
    }

    res, err := agent.Run(context.Background(), llm.UserText("Introduce yourself in one line"))
    if err != nil {
        panic(err)
    }
    fmt.Println(res.Final.Text())
}
```

### Manual assembly (step by step)

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

	agent, err := loop.NewAgent(model, "assistant",
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

Framework infrastructure overhead is quantified with same-machine, same-task comparisons: [`eval/war`](eval/war/README.md) (standalone nested module, [Issue #103](https://github.com/Luo-root/pulse/issues/103)) — Pulse full production assembly vs Eino v0.9.19's official production entry, equally-thin stub models, and a correctness sentinel asserting every task really runs (i9-14900HX / Go 1.25, `-count=2`; **compare magnitudes and multiplier ranges, not single digits**):

| Task | Pulse | Eino v0.9.19 | Multiplier range |
|---|---|---|---|
| T1 text round (reused: assemble once, pure runtime) | 3.2–3.7 µs / 22 allocs | 31.2–36.9 µs / 407 allocs | **~8.5–11.4×** |
| T1 text round (cold start: full rebuild each run) | 8.8–10.9 µs / 125 allocs | 30.5–31.3 µs / 425 allocs | **~2.8–3.6×** |
| T2 tool round-trip (cold-start upper bound) | 12.8–13.9 µs / 163 allocs | 94.2–96.0 µs / 1364 allocs | **~6.8–7.5×** |
| T3 linear-chain orchestration (3 passthrough nodes) | 8.3–8.4 µs / 73 allocs | 17.6–17.9 µs / 323 allocs | **~2.1×** |
| T4 fan-out/fan-in DAG (1 source → 2 branches → AND join) | 8.7 µs / 73 allocs | 30.5–35.7 µs / 411–462 allocs (Graph keyed fan-in / Workflow field-mapping variants) | **~3.5–4.1×** |

Accounting, reproduction commands, and the full reading live in [`eval/war/README.md`](eval/war/README.md). All gaps are negligible against a real LLM call (seconds) — this quantifies the base price of an architectural choice, not an "unusable" verdict. Three takeaways:

1. **Orchestration fan-out is free**: flow's AND slots make branch joins nearly free — the DAG (T4) costs the same as the linear chain (T3), same allocs; Eino's join scheduling runs ~1.7× over its own linear chain, and `compose.Workflow` field mapping adds another +12–17%.
2. **Explicit DAG dataflow**: flow nodes declare Requires / Provides at construction (Key + three-state slots), so join dependencies live on the node signature; the same topology in `compose.Graph` relies on runtime machinery — AllPredecessor triggering + `WithOutputKey` keying + default map merging — and `compose.Workflow` adds a field-mapping layer on top, leaving the dataflow semantics more implicit at equal topology.
3. **Layered assembly with kernel**: flow does not import kernel and runs graphs standalone (T3/T4 are exactly that form); when needed, the assembly layer injects the kernel host / services into node closures, and orchestration steps consume registered capabilities directly (T1/T2's Agent rounds are the full kernel form). [`kernel/flow/yaml`](kernel/flow/yaml/README.md) adds declarative graph loading — standalone runs, kernel assembly, and YAML declaration are orthogonal usages, combined per scenario.

## v2 Architecture

```text
Caller
  │
  ├── host (cross-package assembly, optional): host.New → DefaultAgent
  │     ├── session ↔ loop three-way wiring (Surface/history · event-driven persistence · request scope)
  │     ├── observability bridge (llm.Observe + loop.Observe, per-request TraceID)
  │     └── ToolGate (minimal HITL mount) / ScopeHook (subscribe to loop/llm events yourself)
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
- **Agents are stateless**: `loop.Agent` runs exactly one turn; history, session storage, retry, and failover belong to the layer above — the official v2 assembly is `memory/session` (event log + cold recovery) plus `host` (three-way wiring and request-scoped mounts).
- **v1 components are gone**: tools / MCP / Skills are rewritten in v2 (`toolset/builtins`, `toolset/mcp`, `skills`); the old packages are not resurrected. The command-execution sandbox boundary belongs to the host deployment layer (see the "three boundary layers" section in `toolset/builtins`).

## Build & Test

```powershell
# Requires Go 1.25+
go build ./...
go test ./...

# v2 core regression (no real API)
go test -race -skip TestLive ./kernel/... ./llm/... ./loop/ ./toolset/... ./skills/ ./textsplit/... ./memory/... ./host/ ./observability/

# eval property tests (main module; fixed seeds, ~10s)
go test -race -count=1 ./eval/

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
textsplit/                 text chunking (shared by index/openai and long-text modules)
memory/                    P2 memory & sessions (session / compaction / store / assemble / selfedit / index / candidate / reflection)
host/                      two-layer assembly, layer two: cross-package seams (kernel injection + session↔loop event-driven persistence)
observability/             v2 official observability package (Bootstrap / Record / Sink)
eval/                      evaluation suite: property tests + layered benchmarks + cross-framework comparison suite (`eval/war`)
docs/design/               architecture & migration docs (Accepted)
```

## License

[MIT License](LICENSE)
