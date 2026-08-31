# 02-react

验证 `loop.Agent` 的核心语义：**ReAct 工具回合、多轮 history、流式输出**。前一层（01-chat）的 kernel + Registry 装配在本层原样复用，不重复验证。审批（HITL）是下一课的独立主题——本课只装只读工具、零监听，先看清循环本身。

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

## 每轮请求的标准形态：reqScope + Bridge + Agent

```go
reqScope, _ := host.Ctx.Derive()   // 每轮独立子作用域
defer reqScope.Dispose()
bridge, _ := host.NewBridge(reqScope)  // 运行期观测桥（trace_id 每轮独立）
agent, _ := loop.NewAgent(host.Model,
    loop.WithToolSet(tools),
    loop.WithEventScope(reqScope), // Local 派发：监听与 Agent 同 scope 才听得到
)
```

这是 02 起所有课程的标准形态：tool / turn / llm 事件走 `EmitLocal`/`WaterfallLocal`，请求之间不串扰；`reqScope.Dispose()` 随手清干净本轮监听。

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
