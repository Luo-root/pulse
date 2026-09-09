# host

Layer two of the two-layer assembly: **only the cross-package seams**. Each package's own base assembly does not live here — the memory session/item stacks are the [`memory`](../memory/README.md) root-level facade, and `llm.Registry` / `observability.Bootstrap` / `toolset/builtins.Register` are their own one-stop entries. What host consolidates is the knowledge of *wiring the packages together*.

Package docs (godoc) in the `host.go` package comment; design ticket [#156](https://github.com/Luo-root/pulse/issues/156) (two-layer assembly).

## Getting started

```go
h, err := host.New(host.Options{
    Providers: []host.Provider{host.Provider(openai.Register)}, // signature matches Register; convert directly
    Models:    map[string]llm.Config{"main": {Provider: openai.ProviderCompletions, Model: "gpt-4o-mini", APIKey: os.Getenv("OPENAI_API_KEY")}},
    Tools:     []host.ToolSource{func(c *kernel.Context, reg *toolset.Registry) error {
        _, err := builtins.Register(c, reg, builtins.Options{Root: workspace})
        return err
    }},
    Session: ss, // from memory.NewSessionStack; nil = purely stateless rounds
    Observe: host.ObserveConfig{HostID: "my-app", Sink: mySink}, // nil Sink = no observability
})
defer h.Close()

a, err := h.Agent(ctx, host.AgentOptions{Name: "main", Model: "main", System: "..."})
res, err := a.Run(ctx, llm.User(llm.Text("user input")))
```

The quick start drops from ~60 lines to ~15; the return of the "framework" title is gated on this package landing.

## The three-way wiring in host.Agent

`host.Agent` is a thin wrapper over `loop.Agent`. On a session-equipped host, every `Run` performs:

1. **Before the round**: `session.Surface()` folds into the history passed to loop — callers no longer maintain history themselves;
2. **Before the round**: a `request.header` (system / tool-declaration snapshot / model) is recorded for audit — the anchor for replay and resume;
3. **After the round**: the round's input and produced messages (user / assistant with tool calls / tool.result) are recorded one by one — the next Surface already contains the full history.

An Agent built on a session-less host degrades to a pure passthrough; the `RunHistory` explicit-history channel remains (side-channel injection) and is superseded by Surface when a session exists.

## Zero new abstractions

- `host.Provider` = `func(*kernel.Context, *llm.Registry) error` — `openai.Register` / `anthropic.Register` convert directly;
- `host.ToolSource` = `func(*kernel.Context, *toolset.Registry) error` — wrap `builtins.Register` (and its Options) in a closure;
- Advanced assembly (request-scoped contexts, event subscriptions, custom services) goes through `h.Kernel()` / `h.Models()` / `h.Tools()` with each package's native semantics — host hides nothing.

## Safe defaults

- Models / tools / observability / sessions are all explicit opt-in: not passed means not there;
- A `New` failure fails the whole assembly; already-registered parts are unwound in reverse by the kernel Dispose (reversible-effect semantics);
- Session persistence is plaintext (a JSONL file is the secret surface), paths are host-owned.

## Tests

`go test -race ./host/` — dedicated acceptance tests for the stateless passthrough and the three-way wiring (Surface role sequence / tool result / request.header audit / second-round history injection).
