# 07-production

生产形态聚合课：**多来源工具 + 反思 + 指标面**。本课把 00–06 的组件装到同一个宿主上——Pulse 的「默认关」哲学在这里收口：MCP/Skills 是显式注册的 Source 不是插件魔法，反思无后台循环（触发归宿主），指标面是三处快照不是隐藏的全局单例。

## 本课依赖

[03-hitl](../03-hitl/)（工具与审批）、[04-flow](../04-flow/)（编排）、[06-memory-agent](../06-memory-agent/)（记忆闭环）。

## 段 1：三路工具装配（sources.go）

同一个 `toolset.Registry` 里的工具来自三个来源：

| 来源 | 装法 | 产物 |
|---|---|---|
| 本地注册 | `Registry.Register` | `lookup` / `delete_file` |
| MCP Server | `toolset/mcp.ConnectSDK`（官方 go-sdk，InMemory transport）| `mcp_echo`（NamePrefix 定名） |
| Skills 装载器 | `skills.Open` → Messages 短表 + 只读 `list_skills`/`load_skill` | 规程注入，非 Tool（Skill ≠ Source） |

一次聚合工具回合：模型在同一回合内调用本地 lookup、MCP echo、list_skills/load_skill——调用方不感知工具来自哪一路。撞名不自动改名（`Name_prefix` 在 Register 前定名）；批量撤销走 `DisposeSource("mcp.<id>")`。

## 段 2：反思与指标面（reflection.go）

```go
reflector, _ := reflection.New(reflection.Options{
    Pipeline:      cand,        // candidate.Pipeline（模型路由 = Extractor seam）
    MaxInputChars: 2000,        // 预算门：超限头部丢整条消息（尾部保留）
})
res, _ := reflector.Reflect(ctx, surface) // 会话末由宿主调——默认关，无后台循环
```

- **输出只到候选**：`ReflectionResult{Items, Report, InputChars, TruncatedChars}` 是审计原料；反思不自动晋升，Active 仍需宿主审批（人盖章）。
- **指标面 = 三处快照**（D4 六项指标全貌）：`candidate.Metrics`（提炼率/批准率/撤销率/污染拒绝率）+ `reflection.Metrics`（token 成本 v1 = Runs/字符数）+ `index.Counted`（召回命中，06 课）。
- **审计接法**：memory/* 不 import observability——快照由宿主桥进监控栈（`request.usage` 同先例）。

## 运行

```powershell
go run ./examples/07-production
go test ./examples/07-production/ -v
```

## 课程完结

到这里你已经见过 Pulse 的全部面：kernel 装配（00/01）、ReAct 与工具（02）、审批（03）、编排（04）、会话真相（05）、长期记忆（06）、生产聚合（07）。深入各组件语义请回到对应包的 `README_zh.md`——那里是事实源；架构全貌见 [`memory/README_zh.md`](../../memory/README_zh.md) 与设计文档 `docs/design/`。
