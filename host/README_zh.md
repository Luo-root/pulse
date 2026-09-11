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
})
```

`DefaultAgent` 是**基于 NewAgent 的便捷封装**：模型经宿主 Registry 按声明名解析、工具集取宿主 Tools 聚合视图、会话在宿主 SessionStack 上新建（`SessionID` 非空则打开既有会话续跑）——三行参数覆盖 90% 场景；非默认来源走 NewAgent，零特例。memory / toolset 同构：memory 门面是 `NewSessionStack(store)` 最泛化 + `NewMemorySessionStack()` / `NewJSONLSessionStack(dir)` 便捷；toolset 是 `Registry.Register` 最泛化 + `builtins.Register` / `host.SkillTools` 便捷。

## host.Agent 的三向接线（事件驱动落盘）

`host.Agent` 在有会话的宿主上，每个 `Run` 完成：

1. **回合前**：`session.Surface()` 折影为 history 传给 loop——调用方不再自己维护历史；未决会话（`RecoverExposePending` 档）在此拒绝，经 `session.Recoverable` 裁决后再跑。恢复策略经通用构造接入：`memory.NewSessionStack(session.NewJSONLStore(dir, session.WithRecoverPolicy(...)))`——`memory.NewJSONLSessionStack(dir)` 便捷封装不接策略；
2. **回合中**：按 loop 事件**同步**落盘——`turn.started` → `request.header` → 输入消息 → `step.started` → assistant（**先于**工具执行与 HITL 审批）→ **`Flush`（HITL 检查点）** → `tool.result` → `step.ended` → `turn.ended`。JSONL 的 `Append` 只 write 不 fsync，崩溃只保证 Flush 点之前——`after_model` 落盘 assistant 后立刻刷一次，掉电/强杀时裁决现场（unpaired tool_call）已在磁盘上；只此一点刷，不逐条刷。model-visible means logged：模型可见的每一步在发生时即已入日志，进程死在任意执行点（工具执行中、审批等待中、模型调用失败），日志都停在真实现场——冷恢复（#158）的官方来源就是这条路径；
3. **回合级 scope**：每回合从宿主 kernel 派生独立请求 scope（观测桥 / ToolGate / ScopeHook 都挂它），用毕即毁——loop/llm 是 Local 派发，同宿主多 Agent 互不串扰。

error / cancel 路径同样落盘：loop 的 `turn_end` 无论何种方式结束都会发出，已发生的产出与输入保留在日志里，闭合事件记 `interrupted`——副作用已经出去了，就不能当没发生。

无会话宿主构造的 Agent 退化为纯透传；`RunHistory` 显式传 history 的通道保留（旁路注入），有会话时被 Surface 取代。

## 零新抽象

- `host.Provider` = `func(*kernel.Context, *llm.Registry) error`——`openai.Register` / `anthropic.Register` 直接转换；
- `host.ToolSource` = `func(*kernel.Context, *toolset.Registry) error`——`builtins.Register` 用闭包携带 Options；`host.SkillTools(loader)` 也是 ToolSource（skills 短表/加载只读工具对）；
- `host.ToolGate` = `func(llm.ToolCall) (approved bool, reason string)`——工具执行闸门（before_tool_call waterfall 的挂载点），审批 UI / 策略引擎经此接入 DefaultAgent；
- `AgentOptions.ScopeHook` = `func(*kernel.Context) error`——每次 Run 派生请求 scope 后调用：应用经 `kernel.On` / `kernel.OnWaterfall` 在请求 scope 上自行订阅 loop/llm 事件（Local 派发只本 scope 可见，挂宿主根收不到）；
- 其余进阶装配（自定义服务、宿主级插件）经 `h.Kernel()` / `h.Models()` / `h.Tools()` 用各包原生语义——host 不藏内核。

## 安全默认

- **kernel 注入制**：host 不私建内核——`Options.Kernel` 必填，应用的其他插件 Use 到同一个 kernel 即可与 host 组件共享服务仓库与事件总线；kernel 生命周期归调用方（Dispose 归你），Host 没有 Close；
- 模型/工具/观测/会话全部显式 opt-in：不传就没有；
- host.New 失败只返回 error、不做 Dispose 兜底：已挂载组件留在 kernel 上随调用方 Dispose 统一回收（失败通常是配置错误，修正后重来即可）；
- 落盘 fail closed：回合内任何 append 失败中断回合并报错，其余 panic（模型适配器 / onDelta / 其他监听器）原样重抛不吞（`appendFail` 私有载荷识别）；日志停在与真实一致处，重开由冷恢复合成闭合；输入只接受 user 消息，其余角色构造期/回合前显式拒绝；
- 会话落盘是明文（JSONL 文件即密钥面），路径宿主拥有。

## 测试

`go test -race ./host/`——无会话透传、三向接线（Surface 角色序列 / 生命周期闭合 / request.header 审计 / 二轮历史注入）、工具执行前日志在位、HITL 检查点 Flush（每步 after_model 恰一次）、error 路径落盘与重开零合成、SessionID 续跑、ToolGate 拒绝、ScopeHook 订阅、每请求独立 TraceID，各有验收测试。
