# 05-tools-sources

并排演示三种能力来源装配（**不改** `02-react`）：

| 路径 | 进哪 | 本示例做什么 |
|---|---|---|
| **toolset 本地** | `pulse.tools` → `Tools` | `Register` `lookup` / `delete_file` |
| **MCP Source** | 同一 Registry → `Tools` | go-sdk InMemory server，前缀后为 `mcp_echo` |
| **Skills** | **Messages** 短表 + 只读工具 | `skills.Open(examples/skills)`；`list_skills` / `load_skill` |

Skill ≠ Tool ≠ Source：规程短表进 system；真正执行仍是 `tool_call`。  
`list_skills` 走 `skills.Catalog`，只返回 name+description；`load_skill` 直接返回 `skills.Content` JSON（`body` + **`directory`** + 资源首页；超限有 `resources_next`，宿主可用 `ListResources` 翻页）。脚本相对路径以 `directory` 为根；有通用命令行工具即可跑 `scripts/`，不必再造专用 script 工具。

## 跑

```powershell
go run ./examples/05-tools-sources
```

无凭据：内置 `ScriptedModel` 依次调用 `lookup` → `mcp_echo` → `list_skills` → `load_skill(frontend-design)`。

## 你会看到

1. 启动时打印三路说明与完整 `Definitions()`
2. 回合中本地 / MCP / Skills 只读工具的调用与结果摘要
3. Skills Discovery 短表含 `frontend-design`、`pptx`

## 边界

- MCP 用 InMemory，不拉外部进程
- 不做 HITL / REPL（见 02-react）
- `load_skill` 是 **Tool**（RiskReadonly），把 `skills.Content` 以 tool result 塞进 Messages；Catalog 不含目录
