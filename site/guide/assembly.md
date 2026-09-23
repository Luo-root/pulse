# 装配指南（两层装配）

本页写给**在 Pulse 上写应用装配的人**（应用作者，不是改框架包的人）：读完你能自己装出一个带工具、带会话、带审批、带长期记忆的 Agent，并且知道每个开关来自哪一层、不打开时缺什么。页面里的代码都是真实 API——哪些片段有编译兜底、哪些只示形，见 §三 开头与 §五 末尾的「代码与兜底」。

## 一、两层装配是什么

| 层 | 谁负责 | 装什么 | 单用这一层能不能开箱 |
|---|---|---|---|
| **第一层**：各包自含门面 | 各包自己 | 用本包公开 API 组合出推荐默认：`llm.Registry`、`toolset.Registry`、`observability.Bootstrap`、`memory.NewJSONLSessionStack` / `memory.NewMemoryItemStack`、`loop.NewAgent` | 能。单包可独立跑——只要模型 + 一个回合，就不必上 host |
| **第二层**：`host` | 应用作者 | **只接跨包缝**：供应商与工具来源注册、`session ↔ loop` 三向接线、请求 scope、Agent 构造与生命周期 | 不能替代第一层。host 里的组件就是第一层的产物（`h.Models()` 就是 `llm.Registry`），它只负责把它们串起来 |

**分层纪律**（三条，读源码可核）：

- `loop` 不 import `memory`：回合执行器不认识会话——历史累积、落盘、压缩都是别人的事；
- `host` 单向 import 各包（`kernel` / `llm` / `loop` / `memory` / `observability` / `toolset`），全仓库没有非测试包 import `host`；
- `memory` 根门面（`memory.NewSessionStack` / `memory.NewItemStack`）只 import 自己的子包（`session` / `store` / `assemble`）。

所以选层只有一条判据：**你要的东西是不是「跨包」的**。模型调用、工具登记、会话存储、回合执行各自都在第一层；「回合跑起来时把会话按事件写下去」这种跨包知识才在第二层。

## 二、最快路径

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

这段装配做了四件事：把 `llm.Registry` 与 `toolset.Registry` 经官方插件路径挂到你的 kernel 上、声明一个可 `Open` 的模型、接上会话栈、在 `DefaultAgent` 里把三者接成「Surface 注入 history / 按事件落盘 / 每回合请求 scope」的回合执行器。与 [快速开始](/guide/quickstart) 的「一步装配」是同一份装配（这里多一个 `System` 与收尾打印）；那一页还给了手工装配对照，用来一步步看清每层在做什么。

## 三、第一层：各包默认门面

> **示意**：本节片段只示形——各包门面的最小形态，用来判断「这一层单独用长什么样、什么时候不必上 host」。它们未逐段编译（符号与签名按各包源码核对）；逐字可跑、有编译兜底的完整装配在 §五。

### kernel —— Effect / ServiceKey / 事件

```go
k := kernel.New()
defer k.Dispose() // 逆序还原：Effect 登记的修改在 Dispose 时回滚
```

不必上 host：你要写的是**宿主插件**（Provide 服务、`kernel.On` 订阅事件、`kernel.Require` 声明依赖）时，直接依赖 kernel 即可——host 组件的服务仓库就是同一个根，你的插件与它互相可见。

### llm —— provider 适配器 + 命名模型实例

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

不必上 host：只要**一次模型调用**（不做 ReAct 回合、不落盘、不订阅事件）时，`Registry` + adapter 就够。`host.Options.Providers` / `Models` 只是把这两步写成声明式字段。

### toolset —— 可逆工具登记

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

不必上 host：工具是宿主自己的资产、也不需要接 Agent 循环（比如只做一次模型驱动的工具选择）时，直接 `Register`。`host.Options.Tools` 是 `ToolSource` 函数列表——注册逻辑写一次，host 替你在装配期调用。

### observability —— Bootstrap + Sink

```go
sink := observability.NewLineSink(os.Stdout) // 默认出口：一行一条人读文本（不过 slog）
defer sink.Flush()                           // 关闭前必须 Flush：最后一批还在缓冲里

if _, err := kernel.Use(k, observability.Bootstrap("my-app", sink)); err != nil { // 必须最先 Use
	panic(err)
}
// 之后的业务插件：llm.Plugin / toolset.Plugin / 你自己的插件
```

不必上 host：只要**装配期**轨迹（`fiber_state` / `loader_action` 迁移 + 启动横幅）时，一个 Bootstrap 插件就够。`host.ObserveConfig` 额外做的是**请求期**三处挂载（见 §五·d）。

### memory —— 会话栈与条目栈

```go
sessions := memory.NewMemorySessionStack() // 便捷：进程内（重启即失）
// sessions, err := memory.NewJSONLSessionStack(dir)              // 便捷：JSONL 落盘（blobs + 文件锁 + Flush fsync）
// stack := memory.NewSessionStack(session.NewJSONLStore(dir, /* 选项 */)) // 泛化：注入任意 SessionStore

items := memory.NewMemoryItemStack(assemble.Budget{StableMemoryTokens: 800, RetrievedTokens: 1200})
// items := memory.NewItemStack(myStore, myMeter, assemble.Budget{…}) // 泛化：注入任意 MemoryStore + 计量
```

不必上 host：`SessionStack` / `ItemStack` 是纯存储面——只有会话管理 UI、或只想跑一次组装取上下文时直接用。host 接的是「把它们接到 loop 回合上」那一段（Surface 注入 history、按事件落盘）。

### loop —— 无状态回合执行器

（`model` / `reg` / `k` 就是上面各节的产物。）

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

不必上 host：单回合、history 自己管、不落盘时 loop 最直接。host 在它之上补的正是三向接线——history 由 `Session.Surface()` 折影、落盘按 loop 事件同步发生、scope 每回合独立。

## 四、第二层：host

### `host.Options`

| 字段 | 类型 | 语义 |
|---|---|---|
| `Kernel` | `*kernel.Context` | **必填**。host 组件挂在这个共享根上，你的插件 `Use` 同一个根就与它们共享服务仓库与事件总线 |
| `Providers` | `[]host.Provider` | 供应商适配器注册（先于 `Models` 执行）。`host.Provider(openai.Register)` 直接转换 |
| `Models` | `[]host.ModelDecl` | 模型声明 `{Name string; Config llm.Config}`。用切片不用 map：声明顺序稳定 |
| `Tools` | `[]host.ToolSource` | 工具来源——本地 builtins 闭包 / MCP Source / `host.SkillTools(loader)` / 自建 |
| `Session` | `*memory.SessionStack` | 会话栈；`nil` = 纯无状态回合（history 由调用方经 `RunHistory` 明着传） |
| `Observe` | `host.ObserveConfig` | `{HostID string; Sink observability.Sink}`；`Sink == nil` = 不装观测（零开销） |

### `host.AgentOptions` 与 `host.DefaultAgentOptions`

两条构造路径的旋钮**同名同型**（仓库有 `TestHostOptionsKnobParity` 钉住），差别只在「东西从哪来」：`NewAgent` 全参数注入，`DefaultAgent` 取宿主默认装配。`DefaultAgentOptions` 里 `Model` 是声明名、`ToolSet` / `Session` 由 host 提供，没有 `ModelName` 字段（自动填声明名）。

| 字段 | 类型 | 语义 |
|---|---|---|
| `Name` | `string` | agent 标识（loop 实例身份；随事件与观测记录发出）。**必填** |
| `Model` | `llm.ChatModel`（NewAgent）/ `string`（DefaultAgent 的声明名） | 任意 `ChatModel` 来源，或按名从宿主 Registry 解析 |
| `ModelName` | `string` | `request.header` 审计的模型名。**接会话时必填**（构造期校验，不留到回合中段失败） |
| `ToolSet` | `loop.ToolSet` | 本 agent 的工具集（DefaultAgent 取宿主 Tools 的聚合视图 `h.Tools().AsToolSet()`）；`nil` = 纯对话回合 |
| `Session` | `session.Session` | 本 agent 的会话（三向接线的目标）；`nil` = 不落盘 |
| `System` | `string` | 系统提示词；空 = 无 |
| `ToolGate` | `host.ToolGate` | 工具执行闸门：`func(ctx context.Context, call llm.ToolCall) (approved bool, reason string)`；`nil` = 不设防 |
| `ScopeHook` | `func(scope *kernel.Context) error` | 请求 scope 挂点：每次 `Run` 派生请求 scope 后、回合开始前调用；返回 error 中止本回合 |
| `OnDelta` | `func(text string)` | 本回合的 assistant 文本增量（流式 UI；同步回调，别在里面长阻塞） |
| `MaxSteps` | `int` | 单回合推理-行动步数上限（`0` = 不限）。触发上限不是错误：`Result.StoppedBy == loop.StopMaxSteps`，回合照常闭合 |
| `ContextBuilder` | `func(ctx context.Context, surface, input []*llm.Message) ([]*llm.Message, error)` | 每回合上下文组装缝（长期记忆与裁剪的官方落点，见 §五·c） |
| `SessionID` | `string`（仅 DefaultAgent） | 非空 = 打开既有会话续跑；空 = 新建。宿主未接 `Options.Session` 时非空报错 |

### 三向接线

接上会话栈后，每个 `Run` 做三件事：

1. **回合前**：`session.Surface()` 折影成 history 交给 loop（`RecoverExposePending` 档的未决会话在此拒绝——`ErrPendingEvents`，不把 unpaired tool call 喂给模型）；
2. **回合中**：按 loop 事件**同步**落盘——`turn.started` → `request.header` → 输入消息 → `step.started` → assistant（**先于**工具执行与 HITL 审批，并在这一点 `Flush`）→ `tool.called`（先于审批与执行）→ `tool.result` → `request.route` + `request.usage` → `step.ended` → `turn.ended`。模型可见的每一步在发生时即已入日志，进程死在任意执行点日志都停在真实现场；
3. **每回合请求 scope**：从宿主 kernel 派生、用毕即毁；观测桥、`ToolGate`、`ScopeHook` 都挂在它上面——loop / llm 是 Local 派发，同宿主多 Agent 互不串扰。

### 生命周期

- **kernel 注入制**：`Options.Kernel` 必填——host 不私建内核，否则你的其他插件与 host 组件互相不可见；
- **`New` 失败不做 Dispose 兜底**：只返回 error，已成功挂载的组件留在 kernel 上，随你的 `k.Dispose()` 统一逆序回收（失败通常是配置错误，修正后重来）；
- **`Host` 没有 `Close`**：它无状态可复用，生命周期归调用方；
- **注册中心的生命周期也归内核**：`llm.Registry` / `toolset.Registry` 走官方插件路径装载，`k.Dispose()` 时随之 `Close`（`kernel.Get(k, llm.ServiceKey)` / `kernel.Get(k, toolset.ServiceKey)` 拿到的是与 `h.Models()` / `h.Tools()` 同一个实例）；
- **会话句柄归调用方**：JSONL 实现要 `Close()` 释放文件锁（`Session` 接口之外，经类型断言取），正常路径用完即关，别依赖 stale 超时。

## 五、配方

### a. 带工具 + JSONL 持久化会话

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

要点：

- `memory.NewJSONLSessionStack(dir)` 是便捷封装，**不带**恢复策略；要 `RecoverExposePending` 就走泛化构造 `memory.NewSessionStack(session.NewJSONLStore(dir, session.WithRecoverPolicy(...)))`；
- 策略是 **Store 级**且默认档的合成会**真实写回日志**（破坏性）——「要让人裁决」的场景从第一次 `Open` 起就得带 `RecoverExposePending`，否则未决在打开时就已被合成为 `interrupted` 闭环；
- **冷恢复裁决路径**：`sess, err := h.SessionStack().Open(ctx, id)`；`errors.Is(err, session.ErrPendingEvents)` 表示有未决（此时 `Surface()` 也拒绝投影）。拿裁决面：`r, ok := sess.(session.Recoverable)` →
  - `r.Pending()` 取现场快照（`PendingState.Calls` 带当时的调用载荷，可重发或人工补结果）；
  - `r.ResolvePending(ctx, session.ResolvePendingOption{ToolCallID: id, Result: &session.ToolResultPayload{…}})` 补**真实**结果（回到等待点而不是作废）；
  - `Interrupted: true` 走默认中断闭环；`r.ResolveAsInterrupted(ctx)` 一键把全部未决作废；
- **续跑**：`h.DefaultAgent(ctx, host.DefaultAgentOptions{Name: "main", Model: "main", SessionID: id})`（`SessionID` 非空必须有 `Options.Session`，否则构造期报错）。

### b. HITL 审批：闸门 + 权限卡片

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

要点：

- **卡片**：`Preview` 是执行前的只读权限卡片（`Subject` / `Action` / `Kind` / `Risk` / `Source`，`File` / `Command` / `Network` / `Opaque` 之一带细节）。`ok == false` 表示未登记或该工具没 `PreviewFn`——按「空预览、仍要问人」处理；`err != nil` 时 `ok` 为 true，同样不得放行；
- **顺序（批准的 = 执行的）**：闸门挂在 `before_tool_call` 链上取**后序**——先让内层跑完（`ScopeHook` 的改写在这一段生效），再拿**最终**调用去审批；loop 在整条 waterfall 返回之后才真正执行工具。所以卡片展示的就是即将执行的那一次，闸门不必自己再 sanitize 一遍；
- **拒绝语义**：返回 `approved=false` 即短路，工具不执行，模型收到一条带 `reason` 的 `IsError` 工具结果（`tool.result` 同样落盘）。`reason` 为空时兜底文案由 loop 给出（`rejected by policy`）——host 不自造第二套；
- **ctx 是 `Run` 的那个**：闸门与 waterfall 都跑在 loop 的请求 goroutine 上，等人批就地 `<-ctx.Done()` 即可；超时/取消随宿主，不必为拿 ctx 另挂 waterfall；
- **内层已拒则不再打扰人**：内层钩子已经置 `Rejected` 时闸门被跳过，内层的 reason 原样到模型。

### c. 长期记忆：组装缝

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

要点：

- `ContextBuilder` 是**每回合**的组装缝，`host` 传入当前 surface 与本轮 input，返回的序列就是发给模型的 history（本轮 input 仍原样追加在其后）；
- **本轮输入可能为空**（`Run(ctx)` 不带输入是合法调用）——取检索信号前先判空；
- **组装产物不落盘**：不进会话 surface，召回的记忆每轮重新注入（`memory/assemble` 的「检索块不持久化」）；
- **组装发生在请求 scope 之外**（派生 scope 之前）：在组装里做的事不带本回合 TraceID，要观测就用自己的 tracer；
- 接 SQLite / 自定义存储用泛化构造 `memory.NewItemStack(store, meter, budget)`；`meter` 为 `nil` 时按字符估算。

本段的组装体与 host 包 README 的组装缝配方是同一组调用与字段（`memory.NewMemoryItemStack` → `assemble.AssembleInput` → `items.Assemble` → `out.Messages`），由 `TestHostContextBuilderRecipe` 逐字编译并真跑（含空 input 那一档）。

### d. 观测出口

```go
sink := observability.NewLineSink(os.Stdout) // 默认出口：一行一条人读文本
defer sink.Flush()                           // 进程退出前必须 Flush

h, err := host.New(host.Options{
	// ...
	Observe: host.ObserveConfig{HostID: "my-app", Sink: sink},
})
```

host 替你做的：`New` 里以观测**最先** `Use` 一次 `observability.Bootstrap`（完整装载轨迹要求观测先于一切业务插件）；每次 `Run` 派生独立的 `observability.NewTraceID()`，并在同一个请求 scope 上挂 `observability.AttachCollector` + `llm.Observe` + `loop.Observe`——三处同一个 cfg，同一 TraceID。业务插件与 `ScopeHook` 可以 `kernel.Get(scope, observability.CollectorKey)` 取到本请求的直写入口，写出的记录与 loop/llm 记录共享同一个 trace。

出口选择：

| 出口 | 形态 | 用在 |
|---|---|---|
| `observability.NewLineSink(w)` | 默认：一行一条人读列式文本（不经 slog） | 终端 / 日志文件 |
| `observability.SlogSink{Logger: log}` | `log/slog` 结构化（不自己写 time——handler 已经有一个） | 接宿主既有 logger、要 JSON 喂采集器 |
| `&observability.MemorySink{}` | 内存收集 | 测试断言与演示 |
| `observability.NewAsyncSink(inner)` | **包装器**：`Write` 只入队，单后台协程按序调 `inner` | 手慢的出口（文件 / 网络）；对已经很快的出口是负优化，别默认套 |

host 侧的这一路行为由 `TestHostObservePerRequest`（`HostID` 落到每条记录、每回合独立 TraceID）与 `TestHostAttachCollectorBusinessWrite`（请求内业务直写 `CollectorKey`）覆盖。

### e. MCP 工具来源

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

要点：

- **MCP 是「来源」不是「工具」**：`Sync` 把上游工具逐条登记进 `toolset.Registry`，`Source` 元数据是 `"mcp." + ID`，掉线用 `DisposeSource` / `Source.Detach` 整源撤销（不要用名字前缀猜来源）；
- **预览**：没给 `PreviewFn` 时用默认的 opaque 卡片——HITL 仍能问人，只是卡片上写的是远端效果摘要；
- 想让**插件**持有生命周期（卸载时 `Detach` + `Close` Client）就用插件路径：`pl, err := mcp.Plugin(h.Tools(), cfg)` 后 `kernel.Use(k, pl)`——它要在 `host.New` 之后拿 `h.Tools()`，多一步但卸载路径统一。

### f. 技能

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

要点：**Skill 是规程包，不是工具**——`host.SkillTools` 给的是「读取规程」的两个只读工具（短表在 `list_skills`，正文与资源清单按需 `load_skill`），不把 Skill 升格成可执行件；`list_skills` 只返回 name + description，不把本机路径倒进模型上下文。`skills.Loader` 是接口——不想用文件系统装载，就注入自己的实现。

### 代码与兜底

`host/guide_recipe_test.go` 把本页的**主装配（§二）、会话（§五·a）、HITL（§五·b）、工具来源（§五·e + §五·f）**逐字编译并真跑（测试里用脚本模型顶替真实 provider，其余逐字）；`§五·c` 由 `TestHostContextBuilderRecipe` 覆盖，`§五·d` 由 `TestHostObservePerRequest` 覆盖。§三 各包门面片段是它们的最小形态，未逐段编译——符号与签名按各包源码核对。

## 六、装配契约与常见坑

1. **全部外部副作用 opt-in**：模型网络、工具执行、落盘、观测——不传就没有。`Options.Session == nil` 就是纯无状态回合，`Observe.Sink == nil` 就是零开销不装观测。
2. **落盘 fail closed**：回合内任何 append 失败都会中断本回合（`Run` 返回错误，日志停在与真实一致的状态，重开时由冷恢复合成闭合）；其余 panic（模型适配器、`OnDelta`、其他监听器）原样上抛——host 不吞不标。
3. **两条构造路径**：`h.NewAgent` 是**最泛化构造**（模型 / ToolSet / Session 全注入，可以是 stub 模型、专用工具集、外部会话）；`h.DefaultAgent` 是便捷封装（按声明名解析模型、取宿主工具聚合、在宿主会话栈上 Create/Open）。需要非默认来源时直接用 `NewAgent`，不要试图改宿主装配。
4. **同宿主多 Agent 并发**：为每个并发单元构造**独立 Agent**（`Host` 无状态、可复用）；同一个 Agent 的并发 `Run` 是未定义行为——会话括号（`turn.started` / `turn.ended`）会交错。
5. **请求 scope 必须挂对**：loop / llm 的事件是 **Local 派发**（`EmitLocal` / `WaterfallLocal`），只在本 scope 及其后代可见——把监听挂到宿主根收不到任何回合事件。这就是为什么观测桥、`ToolGate`、`ScopeHook` 挂的都是 `Run` 里派生的请求 scope，也是 `ScopeHook` 存在的唯一理由。
6. **回合输入只接受 user 消息**：`Run(ctx, …)` 里塞 assistant / tool 消息会被显式拒绝（assistant 与 tool 由回合自身产出并落盘）。
7. **接会话时 `ModelName` 必填**：`request.header` 要记模型名（重放与续跑锚点），构造期校验；`DefaultAgent` 自动填声明名，`NewAgent` 要自己给（不给就报错，不会静默补空）。
8. **`SessionID` 非空必须有 `Options.Session`**：否则构造期直接报错——不要在宿主没接会话栈时指望「拿 ID 找回会话」。

## 七、下一步

- **核心概念**：Effect / ServiceKey / 事件与装载模型 → [核心概念](/guide/concepts)
- **快速开始**：最短链路与手工装配对照 → [快速开始](/guide/quickstart)
- **记忆层**：会话、压缩、长期存储与上下文装配 → [记忆层](/guide/memory)
- **可观测性**：Bootstrap / Record / Sink 与宿主自带出口 → [可观测性](/guide/observability)
- **逐包文档**：装配层 [host](/packages/host/)、模型层 [llm](/packages/llm/)、工具 [toolset](/packages/toolset/) 与 [toolset/mcp](/packages/toolset/mcp/)、[skills](/packages/skills/)、[memory](/packages/memory/)、执行 [loop](/packages/loop/)、[observability](/packages/observability/)、[kernel](/packages/kernel/)
