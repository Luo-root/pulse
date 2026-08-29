# toolset/builtins

通用基础工具，经 `toolset.Registry` 挂入 `pulse.tools`。

中性命名：`read` / `ls` / `glob` / `grep` / `exec` / `edit` / `write` / `web_fetch` / `web_search` / `question`。

## 上手

```go
host := kernel.New()
defer host.Dispose()
_ = kernel.Use(host, toolset.Plugin())
reg, _ := kernel.Get(host, toolset.ServiceKey)

dispose, err := builtins.Register(host, reg, builtins.Options{
    Root: "/path/to/workspace",
    // WriteRoots: nil → 仅 Root
    // Enabled: []string{"read","grep"} → 子集
})
defer dispose()
```

## 契约要点

| 项 | 行为 |
|---|---|
| 路径 | 相对路径相对 `Root`；symlink 解析到最终落点再 confine；读写根分家；`ForbidRead` 拒绝窥视 |
| `read` | 行号前缀；`offset`/`limit`；超限返回 truncated 续读提示 |
| `ls`/`glob`/`grep` | **先收集并稳定排序再切页**；超限 trailer 带 `after` 游标 |
| `edit`/`write`(覆盖) | **同进程须先 `read`**；mtime 更新则 stale 拒绝；`edit` 默认唯一匹配 |
| `exec` | **Windows = PowerShell**；Unix = `sh -c`；timeout + 输出头尾截断；RiskDangerous |
| `web_fetch` | http(s) GET → 抽文本 → 按行 `offset`/`limit`（默认 limit=`ReadLimit`）；超 `MaxLineRunes` 的行截断加 `…`（与 `read` 同口径）。超限 trailer `pass offset=N`；每次续读再 GET。拦 file/ftp/data、NUL 二进制、云 metadata；**Dial 时**对实际解析 IP 再检一次（防 redirect / DNS rebinding），连已检 IP 而非主机名。私网默认允许（`BlockPrivate` 才拒）。不是渲染后的浏览器 DOM |
| `web_search` | 默认可注入 `Searcher`；nil 则 DuckDuckGo Lite（HTML 解析，可能被反爬） |
| `question` | 向人提问；需 `Asker`。**不是** HITL 批准 |
| `glob`/`grep` | P0 **不**应用 `.gitignore`（显式）；非法正则返回 error |
| Source | `builtins.<name>`；`Register` 返回的 `dispose()` 可逆 |

## 刻意不做

- **写前 diff 进 HITL**：`edit`/`write`/`exec` 登记 `PreviewFn`（只读盘）；卡片给 `before_tool_call` 监听器，不进 tool result。见 Issue [#56](https://github.com/Luo-root/pulse/issues/56)
- `apply_patch` / LSP / `job_output`（另票）
- 真浏览器渲染（chromedp）；`web_fetch` 只抽 HTTP 正文
- OS 级 sandbox（bwrap / Seatbelt）
- 改 examples（示例统一另规划）
- Skill 自动变 Tool
- 第二套执行事件总线

## 测试

```bash
go test -race ./toolset/builtins/...
```
