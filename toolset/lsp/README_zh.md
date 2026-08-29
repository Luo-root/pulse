# toolset/lsp

把外部语言服务器（gopls、typescript-language-server、pyright…）挂成工具的可选包（Issue [#64](https://github.com/Luo-root/pulse/issues/64)）。**不进 builtins**：依赖外部进程、按语言显式配置。

单工具 `lsp`（RiskReadonly），op 分发：

| op | 参数 | 行为 |
|---|---|---|
| `diagnostics` | `path` | didOpen 后在 `DiagWindow` 内等 server push，返回该文件诊断（severity/位置/message/source）；超时如实提示 |
| `definition` | `path`,`line`,`column` | 符号定义位置 `path:line:col` |
| `references` | `path`,`line`,`column`,`include_declaration?` | 引用列表 |
| `hover` | `path`,`line`,`column` | 类型/签名文档（markdown 优先） |

## 上手

```go
dispose, err := lsp.Register(host, reg, lsp.Options{
    Root: "/path/to/workspace",
    Servers: map[string]string{
        ".go": "gopls",
        ".ts": "typescript-language-server --stdio",
    },
    // Timeout: 30s；DiagWindow: 3s
})
defer dispose()
```

## 契约要点

- **lazy 生命周期**：首次调用按扩展名 spawn（`strings.Fields` 分词，不支持引号路径）→ `initialize`/`initialized` → `didOpen`；进程常驻到 dispose。启动/握手失败只对该语言报错，下次调用重试，不炸 Register
- **清理双路**：显式 `dispose()` 与 scope Dispose（独立 Effect）都 `shutdown → exit →` 树杀（Windows `taskkill /T /F`；Unix 进程组 SIGKILL）
- **position 口径**：`line`/`column` 0 基；`column` 是 LSP 原生 **UTF-16 code units**（ASCII 场景与字符数一致）
- **只读**：不做 formatting / rename / didChange 写同步——改文件走 builtins 的写工具，改完再 `diagnostics` 验证
- **零新依赖**：手写 JSON-RPC stdio 分帧（Content-Length header）；不引 `golang.org/x/tools`
- **conn 缝**：`spawnServer` 包内变量，测试注入内存 fake 钉协议序（initialize → initialized → didOpen → request）
- Source：`lsp.lsp`；未配置扩展名 / server 未启动失败的错误都明确（不静默空结果）

## 刻意不做

- 写类 LSP op（formatting / rename / codeAction）
- completion / code lens / 诊断订阅事件面
- 多根 workspace；server 自动发现
- `golang.org/x/tools` 依赖

## 测试

```bash
go test -race ./toolset/lsp/...
```
