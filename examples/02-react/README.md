# 02-react

验证 `loop.Agent` 的核心语义：**ReAct 工具回合、多轮 history、流式输出**，以及本课主角——**官方观测适配的接入**（`ObserveConfig` + `AttachCollector` + `llm.Observe` / `loop.Observe` 三件装配件，业务事实经 Collector 直写）。装配链在 01 已手写展开，本课复用 `demoapp.Open` 封装版；审批（HITL）是下一课的独立主题——本课只装只读工具，先看清循环本身与观测怎么接。

## 本课依赖

[01-chat](../01-chat/)：kernel 装配链与 `llm` 词汇表。

## ReAct 循环在哪

```text
用户输入 ─▶ Agent.RunStream ─▶ 模型决定「调工具」或「回答」
                 ▲                      │ tool call
                 │                      ▼
                 └──── 结果回填 ◀── ToolSet.Execute
```

`loop.Agent` 是**无状态**的回合执行器：接收 history + 本轮输入，驱动「模型 ↔ 工具」直到模型不再发起调用，返回本回合新增消息。工具调用循环对模型是隐式的，对你是显式的——`res.Steps` 是步数，`res.StoppedBy` 说明结束原因。

## 工具注册：toolset.Registry

工具不直接塞给 loop：先注册进 `toolset.Registry`（kernel 服务 `pulse.tools`），再 `AsToolSet()` 适配：

```go
kernel.Use(host.Ctx, toolset.Plugin())
reg, _ := kernel.Get(host.Ctx, toolset.ServiceKey)
reg.Register(host.Ctx, toolset.Registration{
    Def: llm.ToolDef{Name: "lookup", /* schema */},
    Fn:  func(ctx context.Context, args json.RawMessage) (string, error) { ... },
    Source: "local.lookup",
    Risk:   toolset.RiskReadonly,
})
tools := reg.AsToolSet()
```

Registry 带来两样 MemToolSet 没有的东西：**Risk/Source 元数据**（03 课审批策略的决策依据）与**可逆注销**（`DisposeSource`，卸载即还原的工具版）。

## 每轮请求的标准形态：reqScope + 观测适配 + Agent

```go
reqScope, _ := host.Ctx.Derive()          // 每轮独立子作用域
defer reqScope.Dispose()
cfg := observability.ObserveConfig{Sink: host.Sink, HostID: host.HostID(), TraceID: observability.NewTraceID()}
collector, _ := observability.AttachCollector(reqScope, cfg) // 业务直写面
llm.Observe(reqScope, cfg)                // 官方 llm 适配：generate_finished
loop.Observe(reqScope, cfg)               // 官方 loop 适配：tool/turn_finished
agent, _ := loop.NewAgent(host.Model,
    loop.WithToolSet(tools),
    loop.WithEventScope(reqScope),        // Local 派发：监听与 Agent 同 scope 才听得到
)
```

这是 02 起所有课程的标准形态：tool / turn / llm 事件走 `EmitLocal`/`WaterfallLocal`，请求之间不串扰；`reqScope.Dispose()` 随手清干净本轮监听。03 课起观测接线复用 `demoapp.Host.NewObserve` 封装版。

## 观测接入：官方适配三件 + Collector 直写

本课手写的观测接入（03 课起复用 `demoapp.Host.NewObserve` 封装版——那是你在这里亲手写过一遍的东西），五个设计决定各回答一个问题：

1. **cfg 生命周期为什么 = 请求？** `ObserveConfig{Sink, HostID, TraceID}` 同一请求多适配复用同一值——共享 TraceID 正是 D3 请求级关联；跨请求必须新建（复用旧 cfg 会制造假关联）。
2. **两层标识怎么分？** HostID 宿主稳定（装配期一次生成）；TraceID 每请求独立，由官方默认生成器 `observability.NewTraceID()` 生成（时间戳 + 随机段 + 进程内序号）——适配层从不自造序号；跨宿主对账靠 HostID 字段分层（D3），TraceID 本身无契约语义。
3. **为什么全都挂 reqScope？** `AttachCollector` / `llm.Observe` / `loop.Observe` 的监听走 Local 派发，与 Agent 的 `WithEventScope` 必须同 scope——挂错 scope 什么也听不到；同一 scope 重复调用 `Observe` = 双监听双记录（godoc 显式警告）。`demoapp.InstallAnthropicMaxTokensDefault(reqScope)` 同理：Anthropic 线格式 MaxTokens 必填（nil → ErrBadRequest），在请求 scope 上兜底注入——装配层默认值，不是库 API。
4. **官方 Record 不扩字段，业务事实怎么进？** 运行期事实由官方适配折叠（token 用量进 `Attrs`，key 契约 `llm.model` 等）；**业务自定义事实走 Collector 直写**（D10 直写服务）：`collector.Write("react.summary", …)` 与官方记录走同一 Sink、自动携带 HostID/TraceID——不经事件总线，事件名遵守 `<组件>.<事实>` 点分约定，Sink 聚合时天然分组。
5. **tool 三态谁判定？** `loop.Observe` 单一事实源：`completed` / `rejected` / `failed`（rejected 优先）——**rejected 是 HITL 的拒绝，不算 crash**，是独立状态（03 课接手）。

`llm.Observe` 的 `before_generate` 只作计时起点（waterfall 透传，不改请求）；`after_response` 折成 `llm.generate_finished`（Status=finish_reason，Duration=本次生成）。

## 多轮 history 归属

`Agent` 无状态，history 由 REPL 回调持有：

```go
res, err := agent.RunStream(ctx, onDelta, history, msg)
history = append(history, msg)             // 本轮用户输入
history = append(history, res.Messages...) // 本轮 assistant/tool 全部产出
```

第二轮问「刚才查到什么」，模型能从 history 复述——多轮生效的直接证据。这两个 append 就是后续记忆层要接管的位置（替换它们，而不是改 loop；05/06 课见）。

## RunStream：流式与聚合

`RunStream(ctx, onDelta, history, msg)` 逐 delta 回调文本增量，返回与 `Run` 相同的聚合 `Result`——流式只是输出形态，语义与 `Generate` 一致（llm 包契约）。

## 工具与边界

- `lookup`：只读查询（`RiskReadonly`）。
- 已知边界如实记录：`ToolSet.Execute` 返回 string，工具结果暂不支持多模态回传。

## 运行与测试

```powershell
go run ./examples/02-react
go test ./examples/04-flow/ -v   # 循环与工具的断言在 04 的合并测试与本课的 demoapp 测试
```

无凭据时 ScriptedModel 回放固定脚本（lookup → 总结），`stopped_by` / `steps` / `trace` 打在 stderr。

## 下一课

[03-hitl](../03-hitl/)：给工具调用装上审批闸——denylist / interactive / allowlist / off 四策略与会话信任。
