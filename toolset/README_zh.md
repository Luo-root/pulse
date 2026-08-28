# toolset

pulse v2 的可逆工具注册面（Accepted：[`docs/design/toolset-v1-design.md`](../docs/design/toolset-v1-design.md)）。

给本地工具、未来 MCP 来源一个统一的 `pulse.tools` 注册中心；loop 仍然只看见 `loop.ToolSet`。审批继续挂在请求级 `before_tool_call`，**不**另开 `before_execute` 总线。

## 刻意不做

| 不做 | 归谁 |
|---|---|
| 模型回合 / HITL UI | `loop` + 装配层 |
| 真实 MCP SDK / stdio 子进程 | 下一票换 `mcp.Client` 实现；本包已有 Source 抽象 |
| Skills 装载器 | T4 另文；Skill ≠ Tool / ≠ Source |
| 权限写进 `llm.ToolDef` | Risk/Source 是宿主侧元数据 |
| 独立脚本 Sandbox | 脚本若要跑须映射到已注册工具 |

## 上手

```go
host := kernel.New()
defer host.Dispose()
_ = kernel.Use(host, toolset.Plugin())

reg, _ := kernel.Get(host, toolset.ServiceKey)
_, err := reg.Register(host, toolset.Registration{
    Def: llm.ToolDef{
        Name:        "lookup",
        Description: "查找本地知识",
        Parameters:  json.RawMessage(`{"type":"object"}`),
    },
    Fn:     lookupFn,
    Source: "local.lookup",
    Risk:   toolset.RiskReadonly, // 必填；零值拒绝
})
if err != nil {
    panic(err)
}

agent, err := loop.NewAgent(model,
    loop.WithToolSet(reg.AsToolSet()),
    loop.WithEventScope(reqScope),
)
```

## 契约要点

- **主键** = `Def.Name`（全局扁平唯一）；冲突失败，不静默改名。
- **`DisposeSource(source)`** 按来源批量撤销；禁止用 Name 前缀猜来源。
- **`AsToolSet()`** 是 live 视图；同一回合的 Definitions 快照由 loop 在 Run 开始时取一次。
- **`LookupMeta`** 供 HITL/策略查 Source/Risk；查不到应 fail-closed。
- **依赖**：`toolset` → `loop`；`loop` 不 import `toolset`。`MemToolSet` 仍可用于无 kernel 单测。

## MCP 来源

子包 [`mcp`](mcp/README_zh.md)：`Client` 抽象 + `Source`/`Plugin`。本仓库先用 mock Client 钉契约；换官方 go-sdk / mcp-go 只换 Client 实现。Skills **不是** Source 插件。

## 测试

```bash
go test -race ./toolset/...
```
