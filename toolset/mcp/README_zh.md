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
| Skills | Skill ≠ Source；见 [`skills/`](../../skills/README_zh.md) |
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

## 装配期与结果语义

- `Config.Timeout`（默认 `DefaultTimeout` = 30s）只约束**装配期** `Sync` 的 `ListTools`：对端不响应时 `kernel.Use` / `Sync` 必定在期限内返回错误（超时错误里带旋钮值），而不是永久挂住启动路径。父 ctx 已有的更紧 deadline 优先。
- 工具调用本身跑调用方的回合 ctx，不受 `Config.Timeout` 约束；`Client.Close` 没有 ctx（接口如此），需要上限的实现自己在里面带（如 `ConnectCommand` 的进程 kill）。
- `CallTool` 优先取 `Content` 文本；只回 `structuredContent`（SEP-2106）的 server 用它的 JSON 文本兜底，避免「有结果的工具」被折成空串。

## 测试

```bash
go test -race ./toolset/mcp/...
```
