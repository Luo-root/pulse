# host

两层装配的第二层：**只接跨包的缝**。各包自己的基础装配不在这里——memory 会话栈/条目栈见 [`memory`](../memory/README.md) 根级门面，`llm.Registry` / `observability.Bootstrap` / `toolset/builtins.Register` 各自是一站式入口。host 收敛的是「把各包串起来」的知识。

包文档（godoc）见 `host.go` 包注释；设计票 [#156](https://github.com/Luo-root/pulse/issues/156)（两层装配）。

## 上手

```go
h, err := host.New(host.Options{
    Providers: []host.Provider{host.Provider(openai.Register)}, // 签名对齐 Register，直接转换
    Models:    map[string]llm.Config{"main": {Provider: openai.ProviderCompletions, Model: "gpt-4o-mini", APIKey: os.Getenv("OPENAI_API_KEY")}},
    Tools:     []host.ToolSource{func(c *kernel.Context, reg *toolset.Registry) error {
        _, err := builtins.Register(c, reg, builtins.Options{Root: workspace})
        return err
    }},
    Session: ss, // memory.NewSessionStack 的产物；nil = 纯无状态回合
    Observe: host.ObserveConfig{HostID: "my-app", Sink: mySink}, // Sink nil = 不装观测
})
defer h.Close()

a, err := h.Agent(ctx, host.AgentOptions{Name: "main", Model: "main", System: "..."})
res, err := a.Run(ctx, llm.User(llm.Text("用户输入")))
```

快速开始从约六十行降到约十五行；「framework」称号的回归里程碑即本包合入。

## host.Agent 的三向接线

`host.Agent` 是 `loop.Agent` 的薄包装，在有会话的宿主上，每个 `Run` 完成：

1. **回合前**：`session.Surface()` 折影为 history 传给 loop——调用方不再自己维护历史；
2. **回合前**：`request.header`（system / 工具声明快照 / model）审计落盘——重放与续跑的锚点；
3. **回合后**：本回合输入与产出消息（user / assistant 含 tool call / tool.result）逐条落盘——下一轮 Surface 即含完整历史。

无会话宿主构造的 Agent 退化为纯透传；`RunHistory` 显式传 history 的通道保留（旁路注入），有会话时被 Surface 取代。

## 零新抽象

- `host.Provider` = `func(*kernel.Context, *llm.Registry) error`——`openai.Register` / `anthropic.Register` 直接转换；
- `host.ToolSource` = `func(*kernel.Context, *toolset.Registry) error`——`builtins.Register` 用闭包携带 Options；
- 进阶装配（请求级 scope、事件订阅、自定义服务）经 `h.Kernel()` / `h.Models()` / `h.Tools()` 用各包原生语义——host 不藏内核。

## 安全默认

- 模型/工具/观测/会话全部显式 opt-in：不传就没有；
- `New` 失败即整体失败，已注册部分随 `kernel` Dispose 逆序撤除（可逆效应语义）；
- 会话落盘是明文（JSONL 文件即密钥面），路径宿主拥有。

## 测试

`go test -race ./host/`——无会话透传、三向接线（Surface 角色序列 / tool result / request.header 审计 / 二轮历史注入）各有验收测试。
