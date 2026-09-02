[English](README.md) | [中文](README_zh.md)

# toolset/mcp

The MCP **tool source** adapter layer (Accepted: `docs/design/toolset-v1-design.md` T3).

It mounts "the tool catalog of a given MCP server" into `pulse.tools`: `Register` to go live, `DisposeSource` to go offline. loop still only sees `AsToolSet()`. Sync attaches `DefaultPreview` (an opaque card) to every tool by default; `Config.PreviewFn` can override it for the whole source.

## Abstraction (modeled on the llm Factory)

```text
Client（mock / SDKClient=官方 go-sdk）
    ListTools / CallTool / Close
        ↓
Source.Sync / Detach / Plugin
        ↓
toolset.Registry（Source = "mcp.<id>"）
```

Provided: the **`Client` interface + mock tests + the official go-sdk adapter** (`SDKClient` / `ConnectSDK` / `ConnectCommand`).
Tests default to `InMemoryTransports`; use `ConnectCommand` (stdio) to talk to an external process.

## Deliberately out of scope

| Not doing | Notes |
|---|---|
| resources / prompts | They do not enter the ToolSet |
| Skills | Skill ≠ Source; see [`skills/`](../../skills/README.md) |
| Automatic renaming on conflict | `name_prefix` names tools before Register; a name clash fails |
| Guessing the source from a Name prefix | Bulk revocation only goes through `DisposeSource("mcp.<id>")` |
| Guaranteed SSE / Streamable HTTP | `ConnectSDK` accepts any Transport; tests default to InMemory |

## Getting started

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

Or hand `mcp.Plugin(reg, cfg)` to `kernel.Use`: on unload it automatically Detaches + calls `Client.Close()`.

## Tests

```bash
go test -race ./toolset/mcp/...
```
