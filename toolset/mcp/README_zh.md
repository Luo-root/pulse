# toolset/mcp

MCP **工具来源**适配层（Accepted：`docs/design/toolset-v1-design.md` T3）。

把「某个 MCP server 的工具目录」挂进 `pulse.tools`：上线 `Register`，掉线 `DisposeSource`。loop 仍然只看见 `AsToolSet()`。Sync 默认给每条工具挂 `DefaultPreview`（opaque 卡片）；`Config.PreviewFn` 可整源覆盖。

## 抽象（对标 llm Factory）

```text
Client（mock / SDKClient=官方 go-sdk）
    ListTools / CallTool / Close
        ↓
Source.Sync / Detach / Plugin
        ↓
toolset.Registry（Source = "mcp.<id>"）
```

已提供 **Client 接口 + mock 测试 + 官方 go-sdk 适配**（`SDKClient` / `ConnectSDK` / `ConnectCommand`）。
测试默认用 `InMemoryTransports`；对接外部进程用 `ConnectCommand`（stdio）。

## 刻意不做

| 不做 | 说明 |
|---|---|
| resources / prompts | 不进 ToolSet |
| Skills | Skill ≠ Source；见 T4 |
| 冲突自动改名 | `name_prefix` 在 Register 前定名，撞名失败 |
| Name 前缀猜来源 | 批量撤销只走 `DisposeSource("mcp.<id>")` |
| SSE / Streamable HTTP 必达 | `ConnectSDK` 可接任意 Transport；默认测 InMemory |

## 上手

```go
// 外部 MCP Server（stdio）
client, err := mcp.ConnectCommand(ctx, exec.Command("my-mcp-server"))
// 或测试 / 自定义 Transport：
// client, err := mcp.ConnectSDK(ctx, transport)

reg, _ := kernel.Get(host, toolset.ServiceKey)
src, err := mcp.NewSource(reg, mcp.Config{
    ID:          "fs",
    Client:      client,
    NamePrefix:  "fs", // 可空；非空 → fs_read
    DefaultRisk: toolset.RiskReadonly,
})
_ = src.Sync(host, ctx) // 上线
src.Detach()             // 掉线
_ = client.Close()
```

或 `mcp.Plugin(reg, cfg)` 交给 `kernel.Use`：卸载时自动 Detach + `Client.Close()`。

## 测试

```bash
go test -race ./toolset/mcp/...
```
