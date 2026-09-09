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
    Models:    map[string]llm.Config{"main": {Provider: openai.ProviderCompletions, Model: "gpt-4o-mini", APIKey: os.Getenv("OPENAI_API_KEY")}},
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
```

快速开始从约六十行降到约十五行；「framework」称号的回归里程碑即本包合入。

## 基础构造 + 便捷封装（全库统一的装配分层）

`NewAgent` 是**最泛化构造**：全参数注入——model 可以是任意 `llm.ChatModel` 来源（Registry 产出、stub、宿主自定义），ToolSet / Session 显式传入，不依赖宿主的默认装配：

```go
a, err := h.NewAgent(ctx, host.AgentOptions{
    Name:      "worker",
    Model:     myCustomModel,       // 任意 ChatModel 来源
    ModelName: "my-model",          // request.header 审计名
    ToolSet:   myToolSet,           // nil = 无工具
    Session:   mySession,           // nil = 无会话持久化
    System:    "...",
})
```

`DefaultAgent` 是**基于 NewAgent 的便捷封装**：模型经宿主 Registry 按声明名解析、工具集取宿主 Tools 聚合视图、会话在宿主 SessionStack 上新建——三行参数覆盖 90% 场景；非默认来源走 NewAgent，零特例。memory / toolset 同构：memory 门面是 `NewSessionStack(store)` 最泛化 + `NewMemorySessionStack()` / `NewJSONLSessionStack(dir)` 便捷；toolset 是 `Registry.Register` 最泛化 + `builtins.Register` / `host.SkillTools` 便捷。

## host.Agent 的三向接线

`host.Agent` 是 `loop.Agent` 的薄包装，在有会话的宿主上，每个 `Run` 完成：

1. **回合前**：`session.Surface()` 折影为 history 传给 loop——调用方不再自己维护历史；
2. **回合前**：`request.header`（system / 工具声明快照 / model）审计落盘——重放与续跑的锚点；
3. **回合后**：本回合输入与产出消息（user / assistant 含 tool call / tool.result）逐条落盘——下一轮 Surface 即含完整历史。

无会话宿主构造的 Agent 退化为纯透传；`RunHistory` 显式传 history 的通道保留（旁路注入），有会话时被 Surface 取代。

## 零新抽象

- `host.Provider` = `func(*kernel.Context, *llm.Registry) error`——`openai.Register` / `anthropic.Register` 直接转换；
- `host.ToolSource` = `func(*kernel.Context, *toolset.Registry) error`——`builtins.Register` 用闭包携带 Options；`host.SkillTools(loader)` 也是 ToolSource（skills 短表/加载只读工具对）；
- 进阶装配（请求级 scope、事件订阅、自定义服务）经 `h.Kernel()` / `h.Models()` / `h.Tools()` 用各包原生语义——host 不藏内核。

## 安全默认

- **kernel 注入制**：host 不私建内核——`Options.Kernel` 必填，应用的其他插件 Use 到同一个 kernel 即可与 host 组件共享服务仓库与事件总线；kernel 生命周期归调用方（Dispose 归你），Host 没有 Close；
- 模型/工具/观测/会话全部显式 opt-in：不传就没有；
- host.New 失败只返回 error、不做 Dispose 兜底：已挂载组件留在 kernel 上随调用方 Dispose 统一回收（失败通常是配置错误，修正后重来即可）；
- 会话落盘是明文（JSONL 文件即密钥面），路径宿主拥有。

## 测试

`go test -race ./host/`——无会话透传、三向接线（Surface 角色序列 / tool result / request.header 审计 / 二轮历史注入）各有验收测试。
