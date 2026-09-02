# Core concepts

Pulse v2's foundation is a **plugin kernel**: every modification to the environment is registered as a reversible Effect, and service-dependency changes drive loading/unloading. Five concepts explain every package's design.

## kernel.Context five-piece

| Concept | One line | Key property |
|---|---|---|
| **Effect** | Environment modifications registered as effects | Unload restores — plugins roll back in reverse registration order |
| **ServiceKey[T]** | Typed service handle | `kernel.Get(ctx, key)` needs no type assertion; services are lazy values |
| **Event** | Four emission modes | Emit / Waterfall / Parallel (whole tree) + EmitLocal / WaterfallLocal (this scope) |
| **Plugin** | Loading unit | Loaded via `kernel.Use(host, plugin)`; Fiber = a dependency-reactive lifecycle domain |
| **Loader** | Incremental reconciliation | Dependency changes automatically load/unload the affected plugin subtree |

Request-level isolation uses **scopes** (`host.Derive()`): Effects/events on a request scope never touch the host, and the scope is reclaimed when the turn ends — `loop.Agent`'s `WithEventScope` hangs exactly there.

## Vocabulary first

The `llm` package is v2's other pillar: the request vocabulary only carries fields with **stable cross-provider semantics**. When a provider wire format has no counterpart, the adapter returns `ErrBadRequest` explicitly — no silently dropped parameters, no `map[string]any` escape hatch. The same request code behaves predictably on OpenAI and Anthropic.

```go
reg.Declare("main", llm.Config{
	Provider: openai.ProviderCompletions,
	Model:    "gpt-4o-mini",
	APIKey:   os.Getenv("OPENAI_API_KEY"),
})
model, err := reg.Open("main") // observed wrapper: emits kernel events automatically
```

## Stateless Agent

`loop.Agent` executes **one turn**: model inference → tool calls → result backfill → final answer, with HITL decision events in between. It holds no history — session storage, compaction, retry and failover belong to the memory layer (`memory/session`, `memory/compaction`) or the host.

## Interception points

Kernel events are the framework's extension surface:

- `before_generate` / `after_response` (Waterfall) — the llm interception seam; the observed wrapper is driven by it;
- `before_tool_call` (WaterfallLocal) — the HITL approval mount point; tool calls can be rejected or rewritten before execution;
- `fiber_state` / `loader_action` — lifecycle events the observability package subscribes to.

See the [observability package docs](/en/packages/observability/) and the [kernel package docs](/en/packages/kernel/).

## Design boundaries

- **Clean break**: the v1 component tree is deleted, no compatibility layer;
- **Plugins are not a slogan**: every environment modification is reversible and auditable;
- **Vocabulary first**: see above;
- **Stateless Agent**: see above.
