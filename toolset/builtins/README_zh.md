# toolset/builtins

通用基础工具（P0），经 `toolset.Registry` 挂入 `pulse.tools`。

中性命名：`read` / `ls` / `glob` / `grep` / `exec` / `edit` / `write`。

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
| 路径 | 相对路径相对 `Root`；读写必须落在 Root / WriteRoots；`ForbidRead` 拒绝窥视 |
| `read` | 行号前缀；`offset`/`limit`；超限返回 truncated 续读提示 |
| `edit`/`write`(覆盖) | **同进程须先 `read`**；mtime 更新则 stale 拒绝；`edit` 默认唯一匹配 |
| `exec` | **Windows = PowerShell**；Unix = `sh -c`；timeout + 输出头尾截断；RiskDangerous |
| `glob`/`grep` | P0 **不**应用 `.gitignore`（显式）；非法正则返回 error |
| Source | `builtins.<name>`；Dispose 可逆 |

## 刻意不做（本包 P0）

- `apply_patch` / web / LSP / job_output（另票）
- OS 级 sandbox（bwrap / Seatbelt）
- 改 examples（示例统一另规划）
- Skill 自动变 Tool
- 第二套执行事件总线

## 测试

```bash
go test -race ./toolset/builtins/...
```
