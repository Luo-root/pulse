# toolset/mcp

MCP **工具来源**适配层（Accepted：`docs/design/toolset-v1-design.md` T3）。

把「某个 MCP server 的工具目录」挂进 `pulse.tools`：上线 `Register`，掉线 `DisposeSource`。loop 仍然只看见 `AsToolSet()`。

## 抽象（对标 llm Factory）

```text
Client（mock / 未来 go-sdk / mcp-go）
    ListTools / CallTool / Close
        ↓
Source.Sync / Detach / Plugin
        ↓
toolset.Registry（Source = "mcp.<id>"）
```

本票只提供 **Client 接口 + mock 测试**；不引入社区 SDK。下一票换真实 Client 实现即可对接 MCP Server。

## 刻意不做

| 不做 | 说明 |
|---|---|
| resources / prompts | 不进 ToolSet |
| Skills | Skill ≠ Source；见 T4 |
| 冲突自动改名 | `name_prefix` 在 Register 前定名，撞名失败 |
| Name 前缀猜来源 | 批量撤销只走 `DisposeSource("mcp.<id>")` |

## 上手

```go
reg, _ := kernel.Get(host, toolset.ServiceKey)
src, err := mcp.NewSource(reg, mcp.Config{
    ID:          "fs",
    Client:      myClient, // mock 或 SDK 适配器
    NamePrefix:  "fs",     // 可空；非空 → fs_read
    DefaultRisk: toolset.RiskReadonly,
})
_ = src.Sync(host, ctx) // 上线
src.Detach()             // 掉线
```

或 `mcp.Plugin(reg, cfg)` 交给 `kernel.Use`：卸载时自动 Detach + `Client.Close()`。

## 测试

```bash
go test -race ./toolset/mcp/...
```
