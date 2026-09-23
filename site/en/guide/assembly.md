# Assembly guide (two-layer assembly)

This page is for **people writing application assembly on Pulse** (app authors, not people changing framework packages): after reading it you can assemble an Agent with tools, sessions, approval and long-term memory yourself, and you know which layer each switch comes from and what is missing when it is off. Every snippet on this page is real API — for which snippets have compile backing and which only show shape, see the start of §3 and "Code and compile coverage" at the end of §5.

## 1. What the two layers are

| Layer | Who owns it | What it mounts | Usable on its own |
|---|---|---|---|
| **Layer one**: each package's self-contained facade | Each package itself | Composes the recommended default from the package's public API: `llm.Registry`, `toolset.Registry`, `observability.Bootstrap`, `memory.NewJSONLSessionStack` / `memory.NewMemoryItemStack`, `loop.NewAgent` | Yes. A single package runs standalone — model + one turn needs no host |
| **Layer two**: `host` | The app author | **Only the cross-package seams**: provider and tool-source registration, the `session ↔ loop` three-way wiring, request scope, agent construction and lifecycle | It cannot replace layer one. The components inside host are layer one's products (`h.Models()` is an `llm.Registry`); host only strings them together |

**Layering discipline** (three rules, verifiable in the source):

- `loop` does not import `memory`: the turn executor does not know about sessions — history accumulation, persistence and compaction are someone else's job;
- `host` imports the packages one-way (`kernel` / `llm` / `loop` / `memory` / `observability` / `toolset`), and no non-test package in the repository imports `host`;
- the `memory` root facade (`memory.NewSessionStack` / `memory.NewItemStack`) imports only its own sub-packages (`session` / `store` / `assemble`).

So there is exactly one criterion for picking a layer: **is what you want cross-package?** Model calls, tool registration, session storage and turn execution each live in layer one; cross-package knowledge such as "write the session down by event while the turn runs" is what lives in layer two.

## 2. The fastest path

```go
k := kernel.New() // 内核归应用所有：你的插件（UI、审批、任务队列…）Use 同一个根
defer k.Dispose() // 生命周期归调用方，Host 没有 Close

h, err := host.New(host.Options{
	Kernel:    k,
	Providers: []host.Provider{host.Provider(openai.Register)},
	Models: []host.ModelDecl{
		{Name: "main", Config: llm.Config{Provider: openai.ProviderCompletions, Model: "gpt-4o-mini", APIKey: os.Getenv("OPENAI_API_KEY")}},
	},
	Session: memory.NewMemorySessionStack(),
})
if err != nil {
	panic(err)
}

agent, err := h.DefaultAgent(ctx, host.DefaultAgentOptions{Name: "main", Model: "main", System: "You are a concise assistant."})
if err != nil {
	panic(err)
}
res, err := agent.Run(ctx, llm.UserText("你好"))
if err != nil {
	panic(err)
}
fmt.Println(res.Final.Text())
```

This assembly does four things: it mounts `llm.Registry` and `toolset.Registry` onto your kernel through the official plugin path, declares a model that can be `Open`ed, attaches the session stack, and inside `DefaultAgent` wires the three into a turn executor where "Surface injects history / append by event / a request scope per turn". It is the same assembly as the "one-step assembly" in the [Quick start](/en/guide/quickstart) (with one extra `System` and the closing print); that page also gives a manual-assembly counterpart, to see what each layer does step by step.

## 3. Layer one: each package's default facade

> **Shape only**: the snippets in this section show shape — the minimal form of each package's facade, for judging "what this layer looks like on its own and when host is unnecessary". They are not compiled snippet by snippet (symbols and signatures are checked against each package's source); the verbatim-runnable full assembly with compile backing is in §5.

### kernel — Effect / ServiceKey / events

```go
k := kernel.New()
defer k.Dispose() // 逆序还原：Effect 登记的修改在 Dispose 时回滚
```

No host needed: when what you are writing is a **host plugin** (Provide a service, subscribe with `kernel.On`, declare dependencies with `kernel.Require`), depend on kernel directly — the host components' service repository is that same root, so your plugins and they see each other.

### llm — provider adapters + named model instances

```go
reg := llm.NewRegistry(k) // 拦截事件（before_generate / after_response）的宿主 scope
if err := openai.Register(k, reg); err != nil {
	panic(err)
}
if err := reg.Declare("main", llm.Config{
	Provider: openai.ProviderCompletions,
	Model:    "gpt-4o-mini",
	APIKey:   os.Getenv("OPENAI_API_KEY"),
}); err != nil {
	panic(err)
}
model, err := reg.Open("main") // observed 包装：每次调用发事件，计量/限流/路由因此都是普通监听插件
```

No host needed: for a single **model call** (no ReAct turn, no persistence, no event subscription), a `Registry` + adapter is enough. `host.Options.Providers` / `Models` only write those two steps as declarative fields.

### toolset — reversible tool registration

```go
if _, err := kernel.Use(k, toolset.Plugin()); err != nil { // Provide pulse.tools，卸载时 Close
	panic(err)
}
reg, ok := kernel.Get(k, toolset.ServiceKey) // host 装配下与 h.Tools() 是同一个实例
if !ok {
	panic("pulse.tools not provided")
}
if _, err := reg.Register(k, toolset.Registration{
	Def: llm.ToolDef{
		Name:        "lookup",
		Description: "查一条事实",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	},
	Fn:     func(ctx context.Context, args json.RawMessage) (string, error) { return "…", nil },
	Source: "local.lookup",       // 来源稳定名：撤销（DisposeSource）与归因的锚点
	Risk:   toolset.RiskReadonly, // 必填：Unspecified 会被拒绝，不会静默降级成只读
	// PreviewFn 可选：执行前只读卡片（HITL 用，见 §五·b）
}); err != nil {
	panic(err)
}
```

No host needed: when the tools are the host's own assets and do not need to join an agent loop (say, a one-off model-driven tool choice), call `Register` directly. `host.Options.Tools` is a list of `ToolSource` functions — write the registration logic once and host calls it for you at assembly time.

### observability — Bootstrap + Sink

```go
sink := observability.NewLineSink(os.Stdout) // 默认出口：一行一条人读文本（不过 slog）
defer sink.Flush()                           // 关闭前必须 Flush：最后一批还在缓冲里

if _, err := kernel.Use(k, observability.Bootstrap("my-app", sink)); err != nil { // 必须最先 Use
	panic(err)
}
// 之后的业务插件：llm.Plugin / toolset.Plugin / 你自己的插件
```

No host needed: for the **assembly-time** trajectory alone (`fiber_state` / `loader_action` transitions + the startup banner), one Bootstrap plugin is enough. What `host.ObserveConfig` adds is the three **request-time** mounts (see §5d).

### memory — session and item stacks

```go
sessions := memory.NewMemorySessionStack() // 便捷：进程内（重启即失）
// sessions, err := memory.NewJSONLSessionStack(dir)              // 便捷：JSONL 落盘（blobs + 文件锁 + Flush fsync）
// stack := memory.NewSessionStack(session.NewJSONLStore(dir, /* 选项 */)) // 泛化：注入任意 SessionStore

items := memory.NewMemoryItemStack(assemble.Budget{StableMemoryTokens: 800, RetrievedTokens: 1200})
// items := memory.NewItemStack(myStore, myMeter, assemble.Budget{…}) // 泛化：注入任意 MemoryStore + 计量
```

No host needed: `SessionStack` / `ItemStack` are pure storage surfaces — use them directly for a session-management UI alone, or for a single assembly run to get a context. What host attaches is the "wire them onto a loop turn" part (Surface injects history, append by event).

### loop — stateless turn executor

(`model` / `reg` / `k` are the products of the sections above.)

```go
agent, err := loop.NewAgent(model, "assistant", // id 随事件与观测发出，供归因
	loop.WithToolSet(reg.AsToolSet()),
	loop.WithSystemPrompt("You are a concise assistant."),
	loop.WithEventScope(k), // 事件派发 scope；宿主直连时是根，host 装配下是每回合派生的请求 scope
)
if err != nil {
	panic(err)
}
res, err := agent.Run(ctx, nil, llm.UserText("你好")) // history 明着传；本回合产出从 res.Messages 取
```

No host needed: for a single turn with history you manage yourself and no persistence, loop is the most direct. What host adds on top is exactly the three-way wiring — history folded from `Session.Surface()`, persistence happening synchronously on loop events, and a scope that is independent per turn.

## 4. Layer two: host

### `host.Options`

| Field | Type | Meaning |
|---|---|---|
| `Kernel` | `*kernel.Context` | **Required**. host components mount on this shared root; `Use` the same root in your plugins and they share the service repository and the event bus with them |
| `Providers` | `[]host.Provider` | Provider adapter registration (runs before `Models`). `host.Provider(openai.Register)` converts directly |
| `Models` | `[]host.ModelDecl` | Model declarations `{Name string; Config llm.Config}`. A slice, not a map: declaration order is stable |
| `Tools` | `[]host.ToolSource` | Tool sources — a local builtins closure / an MCP Source / `host.SkillTools(loader)` / your own |
| `Session` | `*memory.SessionStack` | The session stack; `nil` = a pure stateless turn (history passed explicitly by the caller via `RunHistory`) |
| `Observe` | `host.ObserveConfig` | `{HostID string; Sink observability.Sink}`; `Sink == nil` = no observability (zero overhead) |

### `host.AgentOptions` and `host.DefaultAgentOptions`

The two construction paths' knobs are **same-named and same-typed** (pinned by `TestHostOptionsKnobParity` in the repository); they differ only in *where things come from*: `NewAgent` injects every parameter, `DefaultAgent` takes the host's default assembly. In `DefaultAgentOptions`, `Model` is a declared name and `ToolSet` / `Session` come from the host; there is no `ModelName` field (the declared name is filled in automatically).

| Field | Type | Meaning |
|---|---|---|
| `Name` | `string` | Agent identity (the loop instance identity; emitted with events and observability records). **Required** |
| `Model` | `llm.ChatModel` (NewAgent) / `string` (DefaultAgent's declared name) | Any `ChatModel` source, or resolved by name from the host Registry |
| `ModelName` | `string` | The model name audited in `request.header`. **Required with a session** (validated at construction, never left to fail mid-turn) |
| `ToolSet` | `loop.ToolSet` | This agent's tool set (DefaultAgent takes the host Tools aggregate view `h.Tools().AsToolSet()`); `nil` = a pure conversation turn |
| `Session` | `session.Session` | This agent's session (the target of the three-way wiring); `nil` = no persistence |
| `System` | `string` | The system prompt; empty = none |
| `ToolGate` | `host.ToolGate` | The tool-execution gate: `func(ctx context.Context, call llm.ToolCall) (approved bool, reason string)`; `nil` = unguarded |
| `ScopeHook` | `func(scope *kernel.Context) error` | The request-scope mount point: called after each `Run` derives its request scope and before the turn starts; returning an error aborts this turn |
| `OnDelta` | `func(text string)` | This turn's assistant text deltas (streaming UI; a synchronous callback — don't block in it for long) |
| `MaxSteps` | `int` | Per-turn cap on reasoning-action steps (`0` = unlimited). Hitting the cap is not an error: `Result.StoppedBy == loop.StopMaxSteps`, and the turn closes as usual |
| `ContextBuilder` | `func(ctx context.Context, surface, input []*llm.Message) ([]*llm.Message, error)` | The per-turn context-assembly seam (the official landing point for long-term memory and trimming, see §5c) |
| `SessionID` | `string` (DefaultAgent only) | Non-empty = open the existing session and resume; empty = create a new one. Non-empty errors when the host has no `Options.Session` |

### The three-way wiring

With a session stack attached, every `Run` does three things:

1. **Before the turn**: `session.Surface()` folds into the history handed to loop (a pending session on the `RecoverExposePending` tier is rejected here — `ErrPendingEvents`; an unpaired tool call is never fed to the model);
2. **During the turn**: records are appended **synchronously** on loop events — `turn.started` → `request.header` → input messages → `step.started` → assistant (**before** tool execution and HITL approval, and `Flush`ed at this point) → `tool.called` (before approval and execution) → `tool.result` → `request.route` + `request.usage` → `step.ended` → `turn.ended`. Every model-visible step is in the log the moment it happens: if the process dies at any execution point, the log stops at the real scene;
3. **A request scope per turn**: derived from the host kernel and destroyed when done; the observability bridge, `ToolGate` and `ScopeHook` all mount on it — loop / llm dispatch is Local, so agents on the same host never crosstalk.

### Lifecycle

- **Kernel injection**: `Options.Kernel` is required — host never builds its own kernel, otherwise your other plugins and the host components cannot see each other;
- **A failed `New` has no Dispose backstop**: it only returns an error, and the components already mounted stay on the kernel, unwound together in LIFO order by your `k.Dispose()` (a failure is usually a config error — fix it and assemble again);
- **`Host` has no `Close`**: it is stateless and reusable, and its lifecycle belongs to the caller;
- **The registries' lifetime also belongs to the kernel**: `llm.Registry` / `toolset.Registry` load through the official plugin path and are `Close`d with `k.Dispose()` (`kernel.Get(k, llm.ServiceKey)` / `kernel.Get(k, toolset.ServiceKey)` return the very same instances as `h.Models()` / `h.Tools()`);
- **Session handles belong to the caller**: the JSONL implementation needs `Close()` to release the file lock (outside the `Session` interface, reached by type assertion) — close it when done on the normal path, don't rely on the stale timeout.

## 5. Recipes

### a. Tools + a JSONL-persisted session

```go
store, err := session.NewJSONLStore("data/sessions", session.WithRecoverPolicy(session.RecoverExposePending))
if err != nil {
	panic(err)
}

h, err := host.New(host.Options{
	Kernel:    k,
	Providers: []host.Provider{host.Provider(openai.Register)},
	Models:    []host.ModelDecl{{Name: "main", Config: cfg}},
	Tools: []host.ToolSource{
		func(c *kernel.Context, reg *toolset.Registry) error {
			_, err := builtins.Register(c, reg, builtins.Options{Root: "workspace"})
			return err
		},
	},
	Session: memory.NewSessionStack(store), // 泛化构造：store 带策略，门面只收敛构造
})
```

Key points:

- `memory.NewJSONLSessionStack(dir)` is a convenience wrapper and does **not** take a recovery policy; for `RecoverExposePending` use the general constructor `memory.NewSessionStack(session.NewJSONLStore(dir, session.WithRecoverPolicy(...)))`;
- The policy is **Store-level**, and the default tier's synthesis is **genuinely written back to the log** (destructive) — for "a human must adjudicate" scenarios, carry `RecoverExposePending` from the very first `Open`; otherwise the pending state is already synthesized into an `interrupted` closure at open time;
- **Cold-recovery adjudication path**: `sess, err := h.SessionStack().Open(ctx, id)`; `errors.Is(err, session.ErrPendingEvents)` means pending state exists (and `Surface()` refuses to project too). Reach the adjudication surface: `r, ok := sess.(session.Recoverable)` →
  - `r.Pending()` takes a scene snapshot (`PendingState.Calls` carries the call payload from that moment, to resend or to fill in a result by hand);
  - `r.ResolvePending(ctx, session.ResolvePendingOption{ToolCallID: id, Result: &session.ToolResultPayload{…}})` supplies the **real** result (back to the wait point instead of voiding it);
  - `Interrupted: true` takes the default interrupted closure; `r.ResolveAsInterrupted(ctx)` voids every pending entry in one shot;
- **Resuming**: `h.DefaultAgent(ctx, host.DefaultAgentOptions{Name: "main", Model: "main", SessionID: id})` (a non-empty `SessionID` requires `Options.Session`, otherwise construction errors).

### b. HITL approval: gate + permission card

```go
a, err := h.DefaultAgent(ctx, host.DefaultAgentOptions{
	Name: "main", Model: "main",
	// (1) 执行前权限卡片：闸门闭包持 h.Tools()，用 toolset 的预览面取卡片。
	ToolGate: func(gctx context.Context, call llm.ToolCall) (bool, string) {
		card, ok, err := h.Tools().Preview(gctx, call.Name, call.Arguments)
		if err != nil || !ok {
			return false, "no preview card" // 没卡片也照问人，绝不自动放行
		}
		// askHuman 是宿主自己的裁决来源（审批 UI / 策略表 / 终端提示）：
		// 裁决期间就地等在这个 ctx 上——取消与超时随宿主。
		return askHuman(gctx, card), "rejected by approval UI"
	},
	// (2) 要**改写**调用（脱敏 / 路由 / 补默认参数）时挂 waterfall：
	//     BeforeToolCall 是 around 语义，改写 Call 或置 Rejected 短路。
	ScopeHook: func(scope *kernel.Context) error {
		_, err := kernel.OnWaterfall(scope, loop.EventBeforeToolCall,
			func(p *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
				p.Call.Arguments = sanitize(p.Call.Arguments) // 就地改写；只观察也必须委托 next
				return next(p)
			})
		return err
	},
})
```

Key points:

- **Cards**: `Preview` is the pre-execution read-only permission card (`Subject` / `Action` / `Kind` / `Risk` / `Source`, with details on one of `File` / `Command` / `Network` / `Opaque`). `ok == false` means the tool is unregistered or registered no `PreviewFn` — treat it as "empty preview, still ask the human"; when `err != nil` but `ok` is true, that must not pass either;
- **Order (approved = executed)**: the gate mounts on the `before_tool_call` chain **post-order** — it lets the inner hooks finish first (`ScopeHook`'s rewrite applies there), then approves the **final** call; loop executes the tool only after the whole waterfall returns. So the card shows exactly the call that will run, and the gate does not need to re-run `sanitize`;
- **Rejection semantics**: returning `approved=false` short-circuits, the tool does not execute, and the model receives an `IsError` tool result carrying `reason` (the `tool.result` is persisted too). With an empty `reason` the fallback wording comes from loop (`rejected by policy`) — host does not mint a second one;
- **The ctx is `Run`'s**: the gate and the waterfall both run on loop's request goroutine, so waiting for the human is just `<-ctx.Done()` in place; timeouts/cancellation follow the host, and you do not need a separate waterfall to obtain a ctx;
- **An inner rejection does not bother the human again**: when an inner hook already set `Rejected`, the gate is skipped and the inner reason reaches the model as-is.

### c. Long-term memory: the assembly seam

```go
items := memory.NewMemoryItemStack(assemble.Budget{StableMemoryTokens: 800, RetrievedTokens: 1200})
a, err := h.NewAgent(host.AgentOptions{
	// ...
	ContextBuilder: func(ctx context.Context, surface, input []*llm.Message) ([]*llm.Message, error) {
		in := assemble.AssembleInput{
			Namespace: []string{"user-42"},
			Surface:   surface, // 当前会话 surface（无会话时是 RunHistory 的历史）
		}
		if len(input) > 0 { // Run(ctx) 可以不带输入：空 = 只取稳定记忆
			in.Query = input[len(input)-1].Text() // 本轮输入作检索信号
		}
		out, err := items.Assemble(ctx, in)
		if err != nil {
			return nil, err
		}
		return out.Messages, nil // 稳定前缀 → surface 尾部 → 检索记忆 → injected
	},
})
```

Key points:

- `ContextBuilder` is the **per-turn** assembly seam; `host` passes in the current surface and this turn's input, and the slice it returns is the history sent to the model (this turn's input is still appended after it as-is);
- **This turn's input may be empty** (`Run(ctx)` without input is a legal call) — check for empty before reading the retrieval signal;
- **The assembled product is not persisted**: it never enters the session surface, and recalled memory is re-injected every turn (`memory/assemble`'s "retrieved blocks are not persisted");
- **Assembly happens outside the request scope** (before the scope is derived): work done inside the builder carries no TraceID for this turn — observe it with your own tracer;
- For SQLite / custom storage use the general constructor `memory.NewItemStack(store, meter, budget)`; a `nil` `meter` estimates by characters.

The assembled product in this section uses the same calls and fields as the assembly-seam recipe in the host package README (`memory.NewMemoryItemStack` → `assemble.AssembleInput` → `items.Assemble` → `out.Messages`), and is compiled verbatim and genuinely run by `TestHostContextBuilderRecipe` (including the empty-input case).

### d. Observability egress

```go
sink := observability.NewLineSink(os.Stdout) // 默认出口：一行一条人读文本
defer sink.Flush()                           // 进程退出前必须 Flush

h, err := host.New(host.Options{
	// ...
	Observe: host.ObserveConfig{HostID: "my-app", Sink: sink},
})
```

What host does for you: inside `New` it `Use`s `observability.Bootstrap` **first** (a complete loading trajectory requires observability before every business plugin); every `Run` derives a fresh `observability.NewTraceID()` and mounts `observability.AttachCollector` + `llm.Observe` + `loop.Observe` on the same request scope — three mounts, one cfg, one TraceID. Business plugins and `ScopeHook` can take this request's direct-write entry with `kernel.Get(scope, observability.CollectorKey)`, and the records they write share one trace with the loop/llm records.

Choosing an egress:

| Egress | Form | Use when |
|---|---|---|
| `observability.NewLineSink(w)` | Default: one human-readable columnar line per record (never through slog) | Terminal / log file |
| `observability.SlogSink{Logger: log}` | `log/slog` structured (no time of its own — the handler already has one) | Plugging into an existing logger, or JSON for a collector |
| `&observability.MemorySink{}` | In-memory collection | Test assertions and demos |
| `observability.NewAsyncSink(inner)` | **Wrapper**: `Write` only enqueues; a single background goroutine calls `inner` in order | Slow egresses (file / network); a pessimization for already-fast egresses — don't wrap those by default |

On the host side this path is covered by `TestHostObservePerRequest` (`HostID` lands on every record, an independent TraceID per turn) and `TestHostAttachCollectorBusinessWrite` (in-request business direct write to `CollectorKey`).

### e. MCP tool source

```go
Tools: []host.ToolSource{
	func(c *kernel.Context, reg *toolset.Registry) error {
		// 一次 Sync = 拉 ListTools + 按 NamePrefix 定名登记；掉线时 Detach 整源撤销。
		src, err := mcp.NewSource(reg, mcp.Config{
			ID:          "filesystem",          // Source 元数据固定为 "mcp." + ID
			Client:      client,                // mcp.Client：官方 go-sdk / mcp-go / 自建适配
			NamePrefix:  "fs",                  // 非空时模型可见名 = fs_<上游名>
			DefaultRisk: toolset.RiskReadWrite, // 必填；Unspecified 被拒绝
			// PreviewFn 覆盖本源全部工具的预览；nil 用默认的 opaque 卡片。
		})
		if err != nil {
			return err
		}
		return src.Sync(c, context.Background())
	},
},
```

Key points:

- **MCP is a "source", not a "tool"**: `Sync` registers the upstream tools into `toolset.Registry` one by one, the `Source` metadata is `"mcp." + ID`, and going offline revokes the whole source with `DisposeSource` / `Source.Detach` (never guess the source from a name prefix);
- **Preview**: without a `PreviewFn` the default opaque card is used — HITL can still ask the human, the card just carries a summary of the remote effect;
- To have a **plugin** own the lifecycle (`Detach` + `Client.Close()` on unload), use the plugin path: `pl, err := mcp.Plugin(h.Tools(), cfg)` followed by `kernel.Use(k, pl)` — it needs `h.Tools()` after `host.New`, one extra step but a unified unload path.

### f. Skills

```go
loader, err := skills.Open("skills") // 目录下每个含 SKILL.md 的子目录是一个 skill
if err != nil {
	panic(err) // 非法 frontmatter 在装配期暴露，不拖到回合中段
}

h, err := host.New(host.Options{
	// ...
	Tools: []host.ToolSource{host.SkillTools(loader)}, // 注册 list_skills / load_skill 两个只读工具
})
```

Key points: **a Skill is a procedure package, not a tool** — `host.SkillTools` gives two read-only tools for *reading* a procedure (the short table in `list_skills`, the body and resource list via `load_skill` on demand), and does not promote a Skill into something executable; `list_skills` returns name + description only, never tipping local paths into the model context. `skills.Loader` is an interface — inject your own implementation if you don't want filesystem loading.

### Code and compile coverage

`host/guide_recipe_test.go` compiles and genuinely runs this page's **main assembly (§2), sessions (§5a), HITL (§5b) and tool sources (§5e + §5f)** verbatim (the test substitutes a scripted model for the real provider; everything else is verbatim); `§5c` is covered by `TestHostContextBuilderRecipe`, `§5d` by `TestHostObservePerRequest`. The per-package facade snippets in §3 are their minimal shape and are not compiled snippet by snippet — symbols and signatures are checked against each package's source.

## 6. Assembly contract and common pitfalls

1. **Every external side effect is opt-in**: model network, tool execution, persistence, observability — not passed means not there. `Options.Session == nil` is a pure stateless turn, `Observe.Sink == nil` is no observability at zero overhead.
2. **Persistence is fail-closed**: any append failure inside a turn aborts the turn (`Run` returns an error, the log stops at a state consistent with reality, and cold recovery synthesizes the closure on reopen); any other panic (model adapter, `OnDelta`, other listeners) is rethrown untouched — host neither swallows nor relabels it.
3. **Two construction paths**: `h.NewAgent` is the **most general construction** (model / ToolSet / Session all injected — a stub model, a dedicated tool set, an external session); `h.DefaultAgent` is the convenience wrapper (resolves the model by declared name, takes the host tool aggregate, creates/opens on the host session stack). For non-default sources use `NewAgent` directly — don't try to change the host assembly.
4. **Concurrent agents on one host**: construct an **independent Agent** per concurrent unit (`Host` is stateless and reusable); concurrent `Run` calls on one Agent are undefined behaviour — the session brackets (`turn.started` / `turn.ended`) would interleave.
5. **The request scope must be mounted correctly**: loop / llm events are **Local dispatch** (`EmitLocal` / `WaterfallLocal`), visible only inside this scope and its descendants — a listener mounted on the host root receives no turn events at all. This is why the observability bridge, `ToolGate` and `ScopeHook` all mount on the request scope derived inside `Run`, and it is the only reason `ScopeHook` exists.
6. **Turn input accepts user messages only**: putting assistant / tool messages into `Run(ctx, …)` is explicitly rejected (assistant and tool messages are produced by the turn itself and persisted).
7. **`ModelName` is required with a session**: `request.header` records the model name (the replay and resume anchor), validated at construction; `DefaultAgent` fills in the declared name automatically, while `NewAgent` must be given one (no value is an error, never a silent empty fill).
8. **A non-empty `SessionID` requires `Options.Session`**: otherwise construction errors outright — don't count on "find the session by ID" when the host has no session stack attached.

## 7. Next steps

- **Core concepts**: Effect / ServiceKey / events and the loading model → [Core concepts](/en/guide/concepts)
- **Quick start**: the shortest path and the manual-assembly counterpart → [Quick start](/en/guide/quickstart)
- **Memory layer**: sessions, compaction, long-term store and context assembly → [Memory layer](/en/guide/memory)
- **Observability**: Bootstrap / Record / Sink and host-provided egress → [Observability](/en/guide/observability)
- **Per-package docs**: the assembly layer [host](/en/packages/host/), the model layer [llm](/en/packages/llm/), tools [toolset](/en/packages/toolset/) and [toolset/mcp](/en/packages/toolset/mcp/), [skills](/en/packages/skills/), [memory](/en/packages/memory/), execution [loop](/en/packages/loop/), [observability](/en/packages/observability/), [kernel](/en/packages/kernel/)
