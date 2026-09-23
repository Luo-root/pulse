[English](README.md) | [中文](README_zh.md)

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
    Models: []host.ModelDecl{
        {Name: "main", Config: llm.Config{Provider: openai.ProviderCompletions, Model: "gpt-4o-mini", APIKey: os.Getenv("OPENAI_API_KEY")}},
    },
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

// Resume an existing session (cold-recovery semantics follow the stack's Store):
a2, err := h.DefaultAgent(ctx, host.DefaultAgentOptions{Name: "main", Model: "main", SessionID: id})
```

## Base constructor + convenience wrappers (the unified assembly layering)

`NewAgent` is the **most general construction**: everything injected — the model can come from any `llm.ChatModel` source (Registry output, stubs, host-custom), ToolSet / Session are explicit, and nothing depends on the host's default assembly:

```go
a, err := h.NewAgent(host.AgentOptions{
    Name:      "worker",
    Model:     myCustomModel,       // any ChatModel source
    ModelName: "my-model",          // request.header audit name; required with a session
    ToolSet:   myToolSet,           // nil = no tools
    Session:   mySession,           // nil = no session persistence
    System:    "...",
    ToolGate:  myApproval,          // tool-execution gate (minimal HITL mount); nil = unguarded
    OnDelta:   onText,              // text deltas (streaming UI); nil = no callback
    MaxSteps:  12,                  // per-round step cap (0 = unlimited)
})
```

`DefaultAgent` is the **convenience wrapper over NewAgent**: the model is resolved by declared name from the host Registry, the tool set is the host Tools aggregate view, and the session is created on the host SessionStack (a non-empty `SessionID` opens the existing session to resume) — three parameters cover 90% of cases; non-default sources use NewAgent with zero special cases. memory / toolset share the same shape: the memory facade has `NewSessionStack(store)` as the general form with `NewMemorySessionStack()` / `NewJSONLSessionStack(dir)` as conveniences; toolset has `Registry.Register` as the general form with `builtins.Register` / `host.SkillTools` as conveniences.

## The three-way wiring in host.Agent (event-driven persistence)

On a session-equipped host, every `Run` performs:

1. **Before the round**: `session.Surface()` folds into the history passed to loop — callers no longer maintain history themselves; a pending session (`RecoverExposePending`) is rejected here — resolve via `session.Recoverable` first. The recovery policy plugs in through the general constructor: `memory.NewSessionStack(session.NewJSONLStore(dir, session.WithRecoverPolicy(...)))` — the `memory.NewJSONLSessionStack(dir)` convenience does not take a policy;
2. **During the round**: records are appended **synchronously** on loop events — `turn.started` → `request.header` → input messages → `step.started` → assistant (**before** tool execution and HITL approval) → **`Flush` (HITL checkpoint)** → `tool.result` → `step.ended` → `turn.ended`. A JSONL `Append` only writes, it does not fsync, and crashes only guarantee everything before the last Flush — so the assistant row is flushed right after it is written: on power loss / SIGKILL the adjudication scene (the unpaired tool call) is already on disk. Only this one point is flushed, never every event. Model-visible means logged: every model-visible fact is in the log the moment it happens; if the process dies at any execution point (mid-tool, awaiting approval, model failure), the log stops at the real scene — this path is the official source of cold recovery (#158);
3. **Per-round scope**: each Run derives an isolated request scope from the host kernel (the observability bridge / ToolGate / ScopeHook all mount there), disposed when the round ends — loop/llm dispatch is Local, so agents on the same host never crosstalk.

Error / cancel paths persist too: loop emits `turn_end` on every exit; what already happened stays in the log and the closure is recorded as `interrupted` — side effects that already went out are not treated as never-happened.

An Agent built on a session-less host degrades to a pure passthrough; the `RunHistory` explicit-history channel remains (side-channel injection) and is superseded by Surface when a session exists. With a `ContextBuilder` the order is **Surface → ContextBuilder → loop**: what the builder returns is what the model receives as history.

## Streaming text deltas

`AgentOptions.OnDelta` / `DefaultAgentOptions.OnDelta` pass `loop.Agent.RunStream`'s onDelta through — the **only token-level text exit** (`llm.EventTextDelta` is fed to that callback alone and never reaches the event bus; what the bus carries is step-level and whole-response-level).

```go
a, err := h.DefaultAgent(ctx, host.DefaultAgentOptions{
    Name: "main", Model: "main",
    OnDelta: func(text string) { sendToUI(text) }, // one call per delta
})
res, err := a.Run(ctx, llm.User(llm.Text("...")))  // still blocking: the round is over when it returns
```

Contract:

- The callback runs **synchronously** on `Run`'s call stack (loop dispatches deltas serially from one goroutine, so no locking is needed), but it is part of the request path — never block in it for long; hand off to your UI quickly;
- Cancellation goes through `Run`'s ctx; the callback carries no ctx (same as loop);
- A panic propagates untouched — host neither swallows nor relabels it (see "Safe defaults");
- Streaming is **not a bypass**: session persistence, observability and the gate all still apply, and `Run` still returns the full `*loop.Result`;
- Assistant **text** only: reasoning deltas and tool-call argument deltas are transport-level fragments consumed by each adapter's own state machine, which assembles them into `llm.Reasoning` parts and `ToolCall`s delivered once with the response (for token-level reasoning, take the model from `h.Models()` and drive `Stream` yourself).

## The context-assembly seam (where long-term memory lands)

`AgentOptions.ContextBuilder` / `DefaultAgentOptions.ContextBuilder` is the per-round assembly seam — **the official landing point for long-term memory (`memory/assemble`'s budgeted assembly and retrieval) and context trimming**:

```go
items := memory.NewMemoryItemStack(assemble.Budget{StableTokens: 800, RetrievedTokens: 1200})
a, err := h.NewAgent(host.AgentOptions{
    // ...
    ContextBuilder: func(ctx context.Context, surface, input []*llm.Message) ([]*llm.Message, error) {
        out, err := items.Assemble(ctx, assemble.AssembleInput{
            Namespace: []string{"user-42"},
            Surface:   surface,                    // the current session surface
            Query:     input[len(input)-1].Text(), // this round's input as the retrieval signal
        })
        if err != nil {
            return nil, err
        }
        return out.Messages, nil                   // stable prefix → surface tail → retrieved → injected
    },
})
```

Contract:

- `surface` is the folded `session.Surface()` when a session is attached, or the history passed to `RunHistory` otherwise;
- `input` is this round's input (user messages, already validated); the returned slice becomes the history handed to loop, and **this round's input is still appended after it** — the builder owns everything *before* the current message;
- returning an error **aborts the round before any model call** (a half-built context is never sent);
- nil = no assembly (same behaviour as not setting it); the assembler does not know host — the seam lives host-side, and `host` never imports `memory/assemble`.

## The complete HITL recipe (permission cards / argument sanitising / cancellation)

`ToolGate` is the **minimal** mount (`func(llm.ToolCall) (bool, string)`: approve or reject, no ctx). The three richer jobs go through `ScopeHook`, which is available on both construction paths and receives this round's request scope:

```go
a, err := h.DefaultAgent(ctx, host.DefaultAgentOptions{
    Name: "main", Model: "main",
    // (1) Pre-execution permission card: the gate closure holds h.Tools() and
    //     computes the card from toolset's preview surface.
    ToolGate: func(call llm.ToolCall) (bool, string) {
        card, ok, err := h.Tools().Preview(ctx, call.Name, call.Arguments)
        if err != nil || !ok {
            return false, "no preview; ask the human" // no card still means ask — never auto-allow
        }
        showToHuman(card)                              // card.Subject / card.Action / card.Kind…
        return askHuman(card), "rejected by approval UI"
    },
    // (2) Argument sanitising / (3) cancellation: mount your own
    //     before_tool_call waterfall on the request scope.
    ScopeHook: func(scope *kernel.Context) error {
        _, err := kernel.OnWaterfall(scope, loop.EventBeforeToolCall,
            func(p *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
                p.Call.Arguments = sanitize(p.Call.Arguments) // in-place rewrite: name and args
                return next(p)
            })
        return err
    },
})
```

Key points:

- **Cards**: `toolset.Registry.Preview(ctx, name, args)` returns `(Preview, ok, err)`; `ok=false` means the tool is unregistered or registered no `PreviewFn` — treat it as "empty preview, HITL should still ask", never as a pass;
- **Rewriting**: `BeforeToolCall` is around-semantics — rewrite `Call.Name` / `Call.Arguments` in place, or set `Rejected` to short-circuit (loop's waterfall contract);
- **Cancellation**: the waterfall runs on loop's request goroutine, so wait for the human with your own ctx (the one passed to `Run`). `ToolGate` carrying no ctx is deliberate (it stays the minimal mount); the complete form owns its ctx;
- Both construction paths work: `ScopeHook` is on `NewAgent` and `DefaultAgent` alike.

## Zero new abstractions

- `host.Provider` = `func(*kernel.Context, *llm.Registry) error` — `openai.Register` / `anthropic.Register` convert directly;
- `host.ToolSource` = `func(*kernel.Context, *toolset.Registry) error` — wrap `builtins.Register` (and its Options) in a closure; `host.SkillTools(loader)` is also a ToolSource (the skill catalog/loading read-only pair);
- `host.ToolGate` = `func(llm.ToolCall) (approved bool, reason string)` — the tool-execution gate (mount point of the before_tool_call waterfall); approval UIs / policy engines plug into DefaultAgent through it;
- `AgentOptions.ScopeHook` = `func(*kernel.Context) error` — called with the per-Run request scope: subscribe to loop/llm events yourself via `kernel.On` / `kernel.OnWaterfall` (Local dispatch is scope-local; mounting on the host root hears nothing);
- `AgentOptions.OnDelta` = `func(text string)` — loop's text-delta callback (the `RunStream` onDelta); streaming UIs plug in here;
- `AgentOptions.ContextBuilder` = `func(ctx, surface, input) ([]*llm.Message, error)` — the per-round context-assembly seam (where `memory/assemble` lands);
- Other advanced assembly (custom services, host-level plugins) goes through `h.Kernel()` / `h.Models()` / `h.Tools()` with each package's native semantics — host hides nothing.

## Safe defaults

- **Kernel injection**: host never builds its own kernel — `Options.Kernel` is required, and your plugins Use the same kernel to share the service repository and event bus with host components; the kernel's lifecycle belongs to the caller (Dispose is yours) and Host has no Close;
- Models / tools / observability / sessions are all explicit opt-in: not passed means not there;
- A `New` failure only returns an error with no Dispose backstop: already-mounted components stay on the kernel and are unwound by the caller's Dispose (failures are usually config errors — fix and re-assemble);
- Persistence is fail-closed: any append failure inside a round aborts the round with an error, while any other panic (model adapter / onDelta / other listeners) is rethrown untouched — recognized via the private `appendFail` payload (the log stops at a state consistent with reality; cold recovery synthesizes the closure on reopen); turn input accepts only user messages — other roles are rejected explicitly;
- Session persistence is plaintext (a JSONL file is the secret surface), paths are host-owned.

## Tests

`go test -race ./host/` — dedicated acceptance tests for the stateless passthrough, the three-way wiring (Surface role sequence / lifecycle closure / request.header audit / second-round history injection), tool-call-logged-before-execution, the HITL checkpoint Flush (exactly one per `after_model` step), error-path persistence with zero synthesis on reopen, SessionID resume, ToolGate rejection, ScopeHook subscription, per-request TraceIDs, streaming text deltas (both construction paths, the nil-callback no-op, panic propagation), the step cap, ScopeHook on the convenience path, the context-assembly seam (the assembled product reaching the request literally, and failures aborting before the model call), and both HITL recipes (rewriting a call from a waterfall, taking a permission card from the gate).
