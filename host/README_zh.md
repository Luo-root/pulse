# host

两层装配的第二层：**只接跨包的缝**。各包自己的基础装配不在这里——memory 会话栈/条目栈见 [`memory`](../memory/README.md) 根级门面，`llm.Registry` / `observability.Bootstrap` / `toolset/builtins.Register` 各自是一站式入口。host 收敛的是「把各包串起来」的知识。

包文档（godoc）见 `host.go` 包注释；设计票 [#156](https://github.com/Luo-root/pulse/issues/156)（两层装配）。

## 上手

```go
k := kernel.New() // kernel 归应用所有：你的插件（UI、审批、队列…）也 Use 到这里
defer k.Dispose()

h, err := host.New(host.Options{
    Kernel: k, // 必填：host 组件挂到这个共享内核上，与你的插件互相可见
    Providers: []host.Provider{host.Provider(openai.Register)}, // 签名对齐 Register，直接转换
    Models: []host.ModelDecl{
        {Name: "main", Config: llm.Config{Provider: openai.ProviderCompletions, Model: "gpt-4o-mini", APIKey: os.Getenv("OPENAI_API_KEY")}},
    },
    Tools: []host.ToolSource{
        func(c *kernel.Context, reg *toolset.Registry) error {
            _, err := builtins.Register(c, reg, builtins.Options{Root: workspace})
            return err
        },
        host.SkillTools(loader), // Skills 短表/加载工具（list_skills + load_skill，只读）
    },
    Session: ss, // memory.NewMemorySessionStack() / NewJSONLSessionStack(dir)
    Observe: host.ObserveConfig{HostID: "my-app", Sink: mySink}, // Sink nil = 不装观测
})

a, err := h.DefaultAgent(ctx, host.DefaultAgentOptions{Name: "main", Model: "main", System: "..."})
res, err := a.Run(ctx, llm.User(llm.Text("用户输入")))

// 续跑既有会话（冷恢复语义随会话栈的 Store）：
a2, err := h.DefaultAgent(ctx, host.DefaultAgentOptions{Name: "main", Model: "main", SessionID: id})
```

装配产出的能力都挂在**内核服务仓库**上（host 走官方插件路径装载，不是裸构造）：`llm.ServiceKey`（`"pulse.llm"`）与 `toolset.ServiceKey`（`"pulse.tools"`）经 `kernel.Get` 取回的，就是 `h.Models()` / `h.Tools()` 同一实例；注册中心的生命周期归内核（`k.Dispose()` 时关闭，`closed` 守卫生效），所以应用自己的插件可以照 `toolset` README 那样登记工具：

```go
models, _ := kernel.Get(k, llm.ServiceKey)     // == h.Models()
tools, ok := kernel.Get(k, toolset.ServiceKey) // == h.Tools()
```

装了 `Observe.Sink` 时，每个请求 scope 上还绑了 `observability.CollectorKey`（作用域局部绑定）：业务插件 / `ScopeHook` 在请求内直写观测，写出的记录与 loop/llm 的记录是同一条 TraceID（D10 的业务观测入口）：

```go
a, err := h.NewAgent(host.AgentOptions{
    // ...
    ScopeHook: func(scope *kernel.Context) error {
        c, ok := kernel.Get(scope, observability.CollectorKey) // 请求 scope 上取直写器
        if !ok {
            return nil // 没装 Sink 就没有
        }
        c.Write("order.created", "ok") // 进宿主 Sink，带本请求 TraceID
        return nil
    },
})
```

## 基础构造 + 便捷封装（全库统一的装配分层）

`NewAgent` 是**最泛化构造**：全参数注入——model 可以是任意 `llm.ChatModel` 来源（Registry 产出、stub、宿主自定义），ToolSet / Session 显式传入，不依赖宿主的默认装配：

```go
a, err := h.NewAgent(host.AgentOptions{
    Name:      "worker",
    Model:     myCustomModel,       // 任意 ChatModel 来源
    ModelName: "my-model",          // request.header 审计名；接会话时必填
    ToolSet:   myToolSet,           // nil = 无工具
    Session:   mySession,           // nil = 无会话持久化
    System:    "...",
    ToolGate:  myApproval,          // 工具执行闸门（HITL 最小挂点）；nil = 不设防
    OnDelta:   onText,              // 文本增量（流式 UI）；nil = 不回调
    MaxSteps:  12,                  // 单回合步数上限（0 = 不限）
})
```

`DefaultAgent` 是**基于 NewAgent 的便捷封装**：模型经宿主 Registry 按声明名解析、工具集取宿主 Tools 聚合视图、会话在宿主 SessionStack 上新建（新建时 `header.AgentID` 取 agent 名；`SessionID` 非空则打开既有会话续跑）——三行参数覆盖 90% 场景；非默认来源走 NewAgent，零特例。memory / toolset 同构：memory 门面是 `NewSessionStack(store)` 最泛化 + `NewMemorySessionStack()` / `NewJSONLSessionStack(dir)` 便捷；toolset 是 `Registry.Register` 最泛化 + `builtins.Register` / `host.SkillTools` 便捷。

## host.Agent 的三向接线（事件驱动落盘）

`host.Agent` 在有会话的宿主上，每个 `Run` 完成：

1. **回合前**：`session.Surface()` 折影为 history 传给 loop——调用方不再自己维护历史；未决会话（`RecoverExposePending` 档）在此拒绝，经 `session.Recoverable` 裁决后再跑。恢复策略经通用构造接入：`memory.NewSessionStack(session.NewJSONLStore(dir, session.WithRecoverPolicy(...)))`——`memory.NewJSONLSessionStack(dir)` 便捷封装不接策略；
2. **回合中**：按 loop 事件**同步**落盘——`turn.started` → `request.header` → 输入消息 → `step.started` → assistant（**先于**工具执行与 HITL 审批）→ **`Flush`（HITL 检查点）** → `tool.called`（**先于**审批与执行，被拒绝的调用也记）→ `tool.result` →（回合收尾时）`request.route` + `request.usage` → `step.ended` → `turn.ended`。JSONL 的 `Append` 只 write 不 fsync，崩溃只保证 Flush 点之前——`after_model` 落盘 assistant 后立刻刷一次，掉电/强杀时裁决现场（unpaired tool_call）已在磁盘上；只此一点刷，不逐条刷。`request.route` 记本回合**实际服务**的模型（adapter 从响应回填，未回填退 `ModelName`），`request.usage` 记全回合累计 token（含缓存命中）。model-visible means logged：模型可见的每一步在发生时即已入日志，进程死在任意执行点（工具执行中、审批等待中、模型调用失败），日志都停在真实现场——冷恢复（#158）的官方来源就是这条路径；
3. **回合级 scope**：每回合从宿主 kernel 派生独立请求 scope（观测桥 / ToolGate / ScopeHook 都挂它），用毕即毁——loop/llm 是 Local 派发，同宿主多 Agent 互不串扰。

error / cancel 路径同样落盘：loop 的 `turn_end` 无论何种方式结束都会发出，已发生的产出与输入保留在日志里，闭合事件记 `interrupted`——副作用已经出去了，就不能当没发生。

无会话宿主构造的 Agent 退化为纯透传；`RunHistory` 显式传 history 的通道保留（旁路注入），有会话时被 Surface 取代。装了 `ContextBuilder` 时，顺序是 **Surface → ContextBuilder → loop**：组装产物才作为 history 发给模型。

## 流式文本增量

`AgentOptions.OnDelta` / `DefaultAgentOptions.OnDelta` 是 `loop.Agent.RunStream` 的 onDelta 透传——**唯一的 token 级文本出口**（`llm.EventTextDelta` 只喂这个回调，不上事件总线；总线上的 loop/llm 事件是步骤级与整响应级）。

```go
a, err := h.DefaultAgent(ctx, host.DefaultAgentOptions{
    Name: "main", Model: "main",
    OnDelta: func(text string) { sendToUI(text) }, // 逐段文本
})
res, err := a.Run(ctx, llm.User(llm.Text("...")))  // 仍是阻塞调用：返回时回合已结束
```

契约：

- 回调在 `Run` 的调用栈上**同步**执行（loop 在单 goroutine 里串行派发，不必加锁），但它属请求路径——别在里面长时间阻塞，要转交 UI 就尽快入队或扇出；
- 取消经 `Run` 的 ctx；回调签名不带 ctx（与 loop 一致）；
- panic 原样上抛，host 不吞不标（同「安全默认」）；
- 流式**不是旁路**：会话落盘、观测、闸门照旧，`Run` 仍返回完整 `*loop.Result`；
- 只有 assistant **文本**增量：思维链增量与工具调用参数增量是传输层分片，由各适配器自己的状态机拼成 `llm.Reasoning` part 与 `ToolCall`，随响应一次性到达（要 token 级思维链，用 `h.Models()` 取模型自己 `Stream`）。

## 上下文组装缝（长期记忆的落点）

`AgentOptions.ContextBuilder` / `DefaultAgentOptions.ContextBuilder` 是每回合派发前的组装缝——**长期记忆（`memory/assemble` 的预算组装与检索召回）与上下文裁剪的官方落点**：

```go
items := memory.NewMemoryItemStack(assemble.Budget{StableMemoryTokens: 800, RetrievedTokens: 1200})
a, err := h.NewAgent(host.AgentOptions{
    // ...
    ContextBuilder: func(ctx context.Context, surface, input []*llm.Message) ([]*llm.Message, error) {
        in := assemble.AssembleInput{
            Namespace: []string{"user-42"},
            Surface:   surface, // 当前会话折影
        }
        if len(input) > 0 {     // Run(ctx) 可以不带输入：空 = 只取稳定记忆
            in.Query = input[len(input)-1].Text() // 本轮输入作检索信号
        }
        out, err := items.Assemble(ctx, in)
        if err != nil {
            return nil, err
        }
        return out.Messages, nil // 稳定前缀 → surface 尾部 → 检索 → injected
    },
})
```

契约：

- `surface`：有会话时是 `session.Surface()` 的折影，无会话时是 `RunHistory` 传入的 history；
- `input`：本轮输入（host 已校验的 user 消息），**可能为空**——`Run(ctx)` 不带输入是合法调用，取最后一条前先判空，别直接写 `input[len(input)-1]`；返回值作为 history 交给 loop，**本轮 input 仍原样追加在其后**——组装器只负责「当前消息之前」那一段；
- **组装产物不落盘**：返回的序列只作为本次请求的 history，不进会话 surface——召回的记忆**每轮都要重新注入**，下一次 `Surface()` 也不会把它带回来（与 `memory/assemble` §8.3「检索块不持久化」一致）；
- **组装发生在请求 scope 之外**（派生 scope 之前）：在组装里做的事不带本回合 TraceID；要观测就用自己的 tracer（ctx 在手，缝在宿主侧）；
- 返回 error **中止本回合**，且发生在任何模型调用之前（半成品不会发给模型）；
- nil = 不组装（与不设时同行为）；组装器不认识 host，缝在宿主这一侧——`host` 不 import `memory/assemble`；
- 本配方有编译级兜底：`TestHostContextBuilderRecipe` 逐字用上面这段（没人编译的文档片段正是字段名写错还能躺着的原因）。

## 完整 HITL 配方（权限卡片 / 参数净化 / 取消）

`ToolGate` 是 HITL 的**最小**挂点（`func(ctx context.Context, call llm.ToolCall) (bool, string)`：批准或拒绝；ctx 就是传给 `Run` 的那个——要等人工裁决就地等它，取消 / 超时随宿主）。需要**改写调用**的场景走 `ScopeHook`——它在两条构造路径上都可用，拿到的就是本回合的请求 scope：

```go
a, err := h.DefaultAgent(ctx, host.DefaultAgentOptions{
    Name: "main", Model: "main",
    // ① 执行前权限卡片：闸门闭包持 h.Tools()，用 toolset 的预览面现算卡片。
    ToolGate: func(gctx context.Context, call llm.ToolCall) (bool, string) {
        card, ok, err := h.Tools().Preview(gctx, call.Name, call.Arguments)
        if err != nil || !ok {
            return false, "no preview; ask the human" // 拿不到卡片也要问人，别默认放行
        }
        showToHuman(card)                                     // card.Subject / card.Action / card.Kind…
        return askHuman(gctx, card), "rejected by approval UI" // 等裁决就用 gctx：取消 / 超时随宿主
    },
    // ② 参数净化：在请求 scope 上自挂 before_tool_call waterfall。
    ScopeHook: func(scope *kernel.Context) error {
        _, err := kernel.OnWaterfall(scope, loop.EventBeforeToolCall,
            func(p *loop.BeforeToolCall, next func(*loop.BeforeToolCall) *loop.BeforeToolCall) *loop.BeforeToolCall {
                p.Call.Arguments = sanitize(p.Call.Arguments) // 就地改写：名字与参数都可换
                return next(p)
            })
        return err
    },
})
```

要点：

- **卡片**：`toolset.Registry.Preview(ctx, name, args)` 返回 `(Preview, ok, err)`；`ok=false` 表示工具未登记或没登记 `PreviewFn`——按「空预览，HITL 仍应问人」处理，别当成放行；
- **顺序（批准的 = 执行的）**：闸门挂在 `before_tool_call` 链上（更外层还有落盘的那条 `tool.called` 透传环，见「三向接线」），并取**后序**——先让链跑完内层（`ScopeHook` 挂的改写在这一段生效），再拿**最终**调用去取卡片审批；loop 在整条链返回之后才真正执行工具。所以卡片上看到的就是即将执行的那一份，闸门里不必再跑一遍 `sanitize`。两处代价要知道：①闸门看不到「改写前」的原始调用——要留原始调用请观察 `llm.after_model` 或 `loop.tool_finished`；②**闸门裁决时内层钩子已经跑过了**，拒绝并不能撤销它们已经发生的副作用（日志、审计行、净化记账）——需要「被拒就完全不发生」的工作应放在执行器（工具实现）里，而不是钩子里。内层已置 `Rejected` 时闸门整段跳过，人不会被打扰两次，模型拿到的是内层那句 reason；
- **改写**：`BeforeToolCall` 是 around 语义，可就地改 `Call.Name` / `Call.Arguments`，也可置 `Rejected` 短路（loop 的 waterfall 契约）；
- **取消与超时**：闸门与 waterfall 都跑在 loop 的请求 goroutine 上，拿到的都是宿主传给 `Run` 的那个 ctx——等人工裁决就地等它，取消 / 超时随宿主，不必自己再转一手（`TestHostToolGateReceivesRunContext` 钉住值传播与超时两条）；
- 两条构造路径都能用：`ScopeHook` 在 `NewAgent` 与 `DefaultAgent` 上都有。

**迁移**：`ToolGate` 在 v0.2.x 不带 ctx，下一 minor 起带——旧写法加个首参即可（`func(ctx context.Context, call llm.ToolCall)`；不用 ctx 的闸门写 `_`）。

## 零新抽象

下面每个 `AgentOptions.X` 旋钮在 `DefaultAgentOptions` 上都有**同名同型对偶**（`TestHostOptionsKnobParity` 机械护栏：加旋钮只加一边会让测试红）；两条路径只差「来源」——模型是现成 `ChatModel` 还是声明名、ToolSet / Session 是显式传入还是取宿主默认。

- `host.Provider` = `func(*kernel.Context, *llm.Registry) error`——`openai.Register` / `anthropic.Register` 直接转换；
- `host.ToolSource` = `func(*kernel.Context, *toolset.Registry) error`——`builtins.Register` 用闭包携带 Options；`host.SkillTools(loader)` 也是 ToolSource（skills 短表/加载只读工具对）；
- `host.ToolGate` = `func(ctx context.Context, call llm.ToolCall) (approved bool, reason string)`——工具执行闸门（before_tool_call waterfall 上取后序的一环——审批的是改写后的最终调用；ctx 就是传给 `Run` 的那个），审批 UI / 策略引擎经此接入 DefaultAgent；空 `reason` 的兜底文案**只有一处**（loop 的 `rejected by policy`），闸门不给理由时模型看到的就是它——host 不再自造第二套默认文本；
- `AgentOptions.ScopeHook` = `func(*kernel.Context) error`——每次 Run 派生请求 scope 后调用：应用经 `kernel.On` / `kernel.OnWaterfall` 在请求 scope 上自行订阅 loop/llm 事件（Local 派发只本 scope 可见，挂宿主根收不到）；
- `AgentOptions.OnDelta` = `func(text string)`——loop 的文本增量回调（`RunStream` 的 onDelta），流式 UI 经此接入；
- `AgentOptions.ContextBuilder` = `func(ctx, surface, input) ([]*llm.Message, error)`——每回合的上下文组装缝（`memory/assemble` 的落点）；
- 其余进阶装配（自定义服务、宿主级插件）经 `h.Kernel()` / `h.Models()` / `h.Tools()` 用各包原生语义——host 不藏内核。

## 安全默认

- **kernel 注入制**：host 不私建内核——`Options.Kernel` 必填，应用的其他插件 Use 到同一个 kernel 即可与 host 组件共享服务仓库与事件总线；kernel 生命周期归调用方（Dispose 归你），Host 没有 Close；
- 模型/工具/观测/会话全部显式 opt-in：不传就没有；
- host.New 失败只返回 error、不做 Dispose 兜底：已挂载组件留在 kernel 上随调用方 Dispose 统一回收（失败通常是配置错误，修正后重来即可）；
- 落盘 fail closed：回合内任何 append 失败中断回合并报错，其余 panic（模型适配器 / onDelta / 其他监听器）原样重抛不吞（`appendFail` 私有载荷识别）；日志停在与真实一致处，重开由冷恢复合成闭合；输入只接受 user 消息，其余角色构造期/回合前显式拒绝；
- 会话落盘是明文（JSONL 文件即密钥面），路径宿主拥有。

## 测试

`go test -race ./host/`——无会话透传、三向接线（Surface 角色序列 / 生命周期闭合 / request.header 审计 / 二轮历史注入）、工具执行前日志在位、HITL 检查点 Flush（每步 after_model 恰一次）、error 路径落盘与重开零合成、SessionID 续跑、ToolGate 拒绝、ScopeHook 订阅、每请求独立 TraceID、流式文本增量透传（两条构造路径 + 不设回调照常跑通 + panic 原样上抛）、步数上限（带会话：落盘闭合 + 可续跑）、便捷路径的 ScopeHook、上下文组装缝（产物字面进请求 + 失败在模型调用前中止）、会话 header 归属（`TestHostDefaultAgentSessionHeaderAgentID`），以及两条 HITL 配方（waterfall 改写调用、闸门取权限卡片）；另有四条护栏：闸门**后序**语义（卡片看到的就是将执行的那份，`TestHostToolGateSeesRewrittenCall`）及其短路分支（内层已拒则整段跳过闸门、reason 取内层那句，`TestHostToolGateSkippedWhenInnerRejected`）、两条构造路径旋钮同名同型（`TestHostOptionsKnobParity`，反射比对）、README 组装配方逐字可编译可运行（`TestHostContextBuilderRecipe`，含空 input 档）；再加五条：内核服务键可取回同一实例 + Dispose 后 `closed` 守卫（`TestHostRegistryServiceKeysOnKernel`）、请求 scope 上的业务直写器（`TestHostAttachCollectorBusinessWrite`）、`tool.called` 先于闸门落盘（`TestHostToolCalledBeforeGate`）、审计事件 `request.route` / `request.usage` 的顺序与取值（`TestHostRequestUsageAndRoute`）、空 reason 的兜底文案归 loop（`TestHostGateEmptyReasonFallsBackToLoopText`）、多步回合 `request.route` 取最后一次服务模型（`TestHostRequestRouteLastWinsAcrossSteps`）、`tool.called` 对畸形参数的纵深防御（`TestTurnRecorderDropsMalformedToolArguments`）、闸门拿到的 ctx 就是传给 `Run` 的那个（值传播 + 闸门内等超时，`TestHostToolGateReceivesRunContext`）。
