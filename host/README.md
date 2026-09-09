# host

Layer two of the two-layer assembly: **only the cross-package seams**. Each package's own base assembly does not live here — the memory session/item stacks are the [`memory`](../memory/README.md) root-level facade, and `llm.Registry` / `observability.Bootstrap` / `toolset/builtins.Register` are their own one-stop entries. What host consolidates is the knowledge of *wiring the packages together*.

Package docs (godoc) in the `host.go` package comment; design ticket [#156](https://github.com/Luo-root/pulse/issues/156) (two-layer assembly).

## Getting started

```go
k := kernel.New() // the kernel is app-owned: your plugins (UI, approval, queues…) Use the same root
defer k.Dispose()

h, err := host.New(host.Options{
    Kernel: k, // required: host components mount on this shared kernel, visible to your plugins
    Providers: []host.Provider{host.Provider(openai.Register)}, // signature matches Register; convert directly
    Models:    map[string]llm.Config{"main": {Provider: openai.ProviderCompletions, Model: "gpt-4o-mini", APIKey: os.Getenv("OPENAI_API_KEY")}},
    Tools: []host.ToolSource{
        func(c *kernel.Context, reg *toolset.Registry) error {
            _, err := builtins.Register(c, reg, builtins.Options{Root: workspace})
            return err
        },
        host.SkillTools(loader), // skill catalog/loading tools (list_skills + load_skill, read-only)
    },
    Session: ss, // memory.NewMemorySessionStack() / NewJSONLSessionStack(dir)
    Observe: host.ObserveConfig{HostID: "my-app", Sink: mySink}, // nil Sink = no observability
})

a, err := h.DefaultAgent(ctx, host.DefaultAgentOptions{Name: "main", Model: "main", System: "..."})
res, err := a.Run(ctx, llm.User(llm.Text("user input")))
```

The quick start drops from ~60 lines to ~15; the return of the "framework" title is gated on this package landing.

## Base constructor + convenience wrappers (the unified assembly layering)

`NewAgent` is the **most general construction**: everything injected — the model can come from any `llm.ChatModel` source (Registry output, stubs, host-custom), ToolSet / Session are explicit, and nothing depends on the host's default assembly:

```go
a, err := h.NewAgent(ctx, host.AgentOptions{
    Name:      "worker",
    Model:     myCustomModel,       // any ChatModel source
    ModelName: "my-model",          // request.header audit name
    ToolSet:   myToolSet,           // nil = no tools
    Session:   mySession,           // nil = no session persistence
    System:    "...",
})
```

`DefaultAgent` is the **convenience wrapper over NewAgent**: the model is resolved by declared name from the host Registry, the tool set is the host Tools aggregate view, and the session is created on the host SessionStack — three parameters cover 90% of cases; non-default sources use NewAgent with zero special cases. memory / toolset share the same shape: the memory facade has `NewSessionStack(store)` as the general form with `NewMemorySessionStack()` / `NewJSONLSessionStack(dir)` as conveniences; toolset has `Registry.Register` as the general form with `builtins.Register` / `host.SkillTools` as conveniences.

## The three-way wiring in host.Agent

`host.Agent` is a thin wrapper over `loop.Agent`. On a session-equipped host, every `Run` performs:

1. **Before the round**: `session.Surface()` folds into the history passed to loop — callers no longer maintain history themselves;
2. **Before the round**: a `request.header` (system / tool-declaration snapshot / model) is recorded for audit — the anchor for replay and resume;
3. **After the round**: the round's input and produced messages (user / assistant with tool calls / tool.result) are recorded one by one — the next Surface already contains the full history.

An Agent built on a session-less host degrades to a pure passthrough; the `RunHistory` explicit-history channel remains (side-channel injection) and is superseded by Surface when a session exists.

## Zero new abstractions

- `host.Provider` = `func(*kernel.Context, *llm.Registry) error` — `openai.Register` / `anthropic.Register` convert directly;
- `host.ToolSource` = `func(*kernel.Context, *toolset.Registry) error` — wrap `builtins.Register` (and its Options) in a closure; `host.SkillTools(loader)` is also a ToolSource (the skill catalog/loading read-only pair);
- Advanced assembly (request-scoped contexts, event subscriptions, custom services) goes through `h.Kernel()` / `h.Models()` / `h.Tools()` with each package's native semantics — host hides nothing.

## Safe defaults

- **Kernel injection**: host never builds its own kernel — `Options.Kernel` is required, and your plugins Use the same kernel to share the service repository and event bus with host components; the kernel's lifecycle belongs to the caller (Dispose is yours) and Host has no Close;
- Models / tools / observability / sessions are all explicit opt-in: not passed means not there;
- A `New` failure only returns an error with no Dispose backstop: already-mounted components stay on the kernel and are unwound by the caller's Dispose (failures are usually config errors — fix and re-assemble);
- Session persistence is plaintext (a JSONL file is the secret surface), paths are host-owned.

## Tests

`go test -race ./host/` — dedicated acceptance tests for the stateless passthrough and the three-way wiring (Surface role sequence / tool result / request.header audit / second-round history injection).
