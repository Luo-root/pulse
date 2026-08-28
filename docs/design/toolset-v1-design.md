# Toolset v1 设计：可逆工具注册 + loop.ToolSet 适配 + MCP 来源 + Skills 业界对齐

> 状态：**Accepted**（Issue [#38](https://github.com/Luo-root/pulse/issues/38)，PR [#39](https://github.com/Luo-root/pulse/pull/39) 评审定案 2026-08-28）
> 包位置（拟）：`toolset/`（公开库包）；Skills 装载与 MCP 客户端作为来源插件，不进 `loop/`
> 前置：kernel / Local 事件 / loop.ToolSet / observability v1 / flow v2 已落地
> 对齐：路径 A（注册归 toolset、执行与 HITL 仍归 loop）；Skills 跟 [Agent Skills](https://agentskills.io/home) 通行标准，不做 Pulse 特例
> 实现：按 §8 T1→T2 另开实现 PR；T3 MCP / T4 Skills 装载器另票

## 0. 一句话定位

给 Pulse 一个**可插拔、可逆**的工具注册面：本地代码工具、MCP server、未来 fs/command/web 插件都往同一个 `pulse.tools` 注册；loop 仍然只看见 `loop.ToolSet`；审批继续挂在请求级 `before_tool_call`。Skills 是**规程/知识包**（`SKILL.md` + 渐进披露），不是第二套工具执行管线。

## 1. 决策记录（为什么是现在这个形状）

| # | 决策 | 被否掉的替代方案 | 理由 |
|---|---|---|---|
| D1 | **路径 A**：`toolset` 管注册/来源/生命周期，并适配出 `loop.ToolSet` | 把执行与审批迁进 `toolset.before_execute`；或让 loop 直接依赖 Registry | loop 已有稳定 `before_tool_call`/`after_tool_call` + HITL demo；再开一套执行事件 = 第二总线 |
| D2 | **不引入** `before_execute` / `after_execute` | 照搬 `plugin-kernel-v2.md` P1 原文事件名 | P1 写于 Local 事件与 HITL 落地之前；现事实源是 `pulse.loop.before_tool_call`（WaterfallLocal）与 `after_tool_call`（EmitLocal） |
| D3 | 模型可见定义继续只用 `llm.ToolDef` | 另造 `toolset.ToolSpec` 暴露给模型 | 词汇表单一；权限/来源是**宿主侧元数据**，不进模型线格式 |
| D4 | Skills 对齐 [agentskills.io](https://agentskills.io/specification)：文件夹 + `SKILL.md`（`name`/`description` 必填）+ 渐进披露 | 把 Skill 做成可 `Execute` 的伪工具；或 Pulse 专有 frontmatter 当规范 | 用户明确「跟业界一致、不要特例独行」；业界标准已是开放格式，被 Claude Code / Codex / Copilot 等采用 |
| D5 | MCP = **工具来源插件**（注册一批 `llm.ToolDef` + handler），掉线即 Effect 撤销 | MCP 另起执行环；或把 MCP 工具直接塞进 `MemToolSet` 无归属 | 可逆生命周期与「server 掉线 → 能力降级」需要来源归属；归属在 Registry，不在 loop |
| D6 | 权限分级是 Registry 条目元数据；策略监听仍挂 `before_tool_call` | 把权限写进 `llm.ToolDef`；或在 toolset 内建审批器 | `ToolDef` 是跨 provider 稳定字段，加权限会污染词汇表；审批必须请求隔离（Local） |
| D7 | observability 仍零业务依赖；工具审计由装配层 Bridge 订阅 loop 事件 | observability import toolset/loop；或给 Record 加 `map` payload | 与 observability v1 D1/D2 一致；工具名等细节走桥侧 slog 附加键，不扩官方信封成万能袋 |
| D8 | `MemToolSet` 保留为轻量内存实现；Registry 适配器是生产默认 | 删除 `MemToolSet`；或强制所有测试走 kernel | 单测与无 kernel 场景仍需要零依赖 ToolSet；契约兼容 |

评审（PR #39）：D1–D8 **Accepted**。

### 对 P1 原文的显式修订

`docs/design/plugin-kernel-v2.md` §P1 中「`before_execute → execute → after_execute`」与「审批挂 before_execute」在本设计定案后应改为：

```text
注册/来源/可逆生命周期 → toolset（pulse.tools）
执行前审批 / 执行后轨迹 → loop.before_tool_call / after_tool_call（Local）
MCP / 本地工具插件 → 向 Registry 注册；dispose / DisposeSource = 撤销注册
```

P1 措辞已在本 Accepted 文档 PR 同批修订 `plugin-kernel-v2.md`，避免双事实源。

## 2. 分层与归属

```text
┌──────────────────────────────────────────────────────────────┐
│ 装配层（demoapp / 未来宿主）                                   │
│  Use(toolset.Plugin)                                         │
│  装本地工具插件 / MCP 来源插件 /（可选）Skills 装载器           │
│  reqScope: Bridge + HITL On(before_tool_call)                │
│  loop.NewAgent(WithToolSet(registry.AsToolSet()), ...)       │
└───────────────┬───────────────────────────────┬──────────────┘
                │                               │
┌───────────────▼──────────────┐   ┌────────────▼──────────────┐
│ toolset/                     │   │ skills（业界格式，非执行面）│
│  Registry = pulse.tools      │   │  SKILL.md 元数据+指令      │
│  Source 插件可逆注册         │   │  scripts/references/assets│
│  AsToolSet() → loop.ToolSet  │   │  渐进披露进模型上下文      │
│  条目元数据：Source/Risk/... │   │  allowed-tools 仅声明依赖  │
└───────────────┬──────────────┘   └───────────────────────────┘
                │ Definitions / Execute
┌───────────────▼──────────────┐
│ loop/                        │
│  ToolSet 消费                │
│  before_tool_call WaterfallLocal（HITL/策略）│
│  ToolSet.Execute             │
│  after_tool_call EmitLocal   │
└──────────────────────────────┘
```

三层职责一句话：

| 层 | 负责 | 不负责 |
|---|---|---|
| **toolset** | 注册、来源归属、可逆 Effect、聚合成 `ToolSet`、宿主侧元数据 | 模型回合、HITL UI、第二套执行事件 |
| **loop** | 模型决策、调用前局部审批、调用、调用后轨迹、错误回传模型 | 工具从哪来、MCP 连接、Skill 文件格式 |
| **Skills** | 规程知识包、何时用/怎么做、可选脚本与资料 | 自己变成 `ToolSet.Execute`；另起事件总线 |

### 依赖方向（写死）

`AsToolSet() loop.ToolSet` 与 `Fn loop.ToolFunc` ⇒ **`toolset` import `loop`**。`loop` **不** import `toolset`。这是路径 A 的代价，比把 `ToolSet` 再抽到 `llm` 或新建 `tooliface` 更小。实现时禁止为「依赖好看」搬接口。

## 3. 数据契约

### 3.1 保持不变的消费面

```go
// loop — 唯一执行消费接口（已落地）
type ToolSet interface {
    Definitions() []llm.ToolDef
    Execute(ctx context.Context, call llm.ToolCall) (string, error)
}
```

```go
// llm — 唯一模型可见工具声明（已落地）
type ToolDef struct {
    Name        string
    Description string
    Parameters  json.RawMessage // nil = 无参
}
```

### 3.2 Registry（拟）

```go
package toolset

var ServiceKey = kernel.NewServiceKey[*Registry]("pulse.tools")

// Risk 是宿主侧分级，不进入 llm.ToolDef。
// 禁止用 iota 零值表示 Readonly：漏填不得 fail-open。
type Risk int

const (
    RiskUnspecified Risk = iota // 非法；Register 必须拒绝
    RiskReadonly
    RiskReadWrite
    RiskDangerous
)

type Registration struct {
    Def    llm.ToolDef
    Fn     loop.ToolFunc
    Source string // 来源稳定名，如 "local.lookup" / "mcp.filesystem"；元数据，非 Name 主键
    Risk   Risk   // 必填；不得为零值 RiskUnspecified
}

type Registry struct { /* 并发安全；主键 = Def.Name（全局扁平唯一） */ }

func Plugin() kernel.Plugin // Provide(ServiceKey) + Close Effect

// Register：Name 全局唯一；Risk 必填；在 c 上登记可逆 Effect。
// dispose 撤销「这一次」注册（该 Name）。重复 dispose 安全。
func (r *Registry) Register(c *kernel.Context, reg Registration) (dispose func(), err error)

// DisposeSource：按 Source 元数据批量撤销该来源下全部注册。
// 供 MCP 掉线使用。禁止用 Def.Name 前缀匹配猜测来源。
func (r *Registry) DisposeSource(source string)

func (r *Registry) AsToolSet() loop.ToolSet // live 视图；回合快照在 loop
func (r *Registry) LookupMeta(name string) (source string, risk Risk, ok bool)
```

契约要点：

1. **同名冲突**：后注册失败（与现 `MemToolSet` 一致），不静默覆盖、不自动改名；MCP 重连应先 `DisposeSource`（或 dispose 旧注册）再 Register。
2. **Definitions 顺序**：按 `Name` 字典序，对齐 `MemToolSet`。
3. **Execute**：未知工具（含本回合开始后已撤销的名）返回 error；loop 折成 `IsError` 工具结果，不终止回合。
4. **AsToolSet**：live 适配视图，不把权限策略塞进 Execute；策略在 `before_tool_call`。
5. **LookupMeta**：供 HITL/策略查 Risk/Source；查不到 = 未注册，策略 fail-closed。实现时用 waterfall **当时**的 `Call.Name`（监听器可改名）。
6. **Risk 必填**：`RiskUnspecified` 或漏填 → Register 返回 error。三级够用；不为「以后多级」留空洞枚举。不要「默认 Readonly + 来源覆盖」——那是第二套策略。
7. **主键 = `Def.Name`（扁平）**：`Source` 只做元数据 + `DisposeSource`，不是命名空间主键。

### 3.2.1 同一回合 Definitions 是快照

`loop.RunStream` 在回合开始调用一次 `Definitions()`，整回合 `GenerateRequest.Tools` 使用该切片（见 `loop/loop.go`）。因此：

- **`AsToolSet` 是 live 视图**；**快照发生在 loop**，不在 Registry。
- MCP 中途掉线 / `DisposeSource`：
  - **本回合**：模型侧工具列表**不收缩**；若模型仍调用已撤销的名 → `Execute` 失败 → 折成 `IsError` 回传（loop 既有行为）。
  - **下一回合**：再取 `Definitions` 时不再包含。
- 这与「来源掉线 = 能力降级」兼容：降级对**后续回合的模型可见列表**生效；当前回合只保证 Execute 不再成功命中已撤销 handler。

### 3.3 来源插件最小形状

```go
// 本地示例：在 Plugin Apply 里 Register，靠返回 dispose / Context.Effect 可逆
func LookupPlugin() kernel.Plugin

// MCP：持有 client；OnConnected → Register 一批工具（Register 前按配置定名）；
// OnDisconnected / Fiber dispose → DisposeSource(source) 或逐个 dispose
func MCPSourcePlugin(cfg MCPConfig) kernel.Plugin
```

不做：来源插件私自 `Emit` 一套 tool 执行事件；执行事实只从 loop Local 事件出。  
不做：对 `Def.Name` 做前缀匹配来批量撤销——批量撤销只走 `DisposeSource` 或插件自持的 dispose 列表。

### 3.4 Skills（业界对齐，本版只定边界）

对齐 [Agent Skills Specification](https://agentskills.io/specification)：

| 项 | 口径 |
|---|---|
| 形态 | 目录 + 必选 `SKILL.md` |
| 必填 frontmatter | `name`, `description` |
| 可选 | `license`, `compatibility`, `metadata`, `allowed-tools`（实验性） |
| 加载 | 渐进披露：启动只吃 name/description → 命中再读正文 → 按需读 scripts/references/assets |
| 与工具关系 | Skill **指导**何时用哪些已注册工具；`allowed-tools` 是声明，**不是**自动注册新 `ToolDef` |
| 脚本 | **若要跑，必须映射到已注册工具**；本版**没有**独立脚本执行面 / Sandbox API |

`allowed-tools` 的 `Bash(git:*)` 等写法视为**不透明声明**（Claude Code 方言）。Pulse v1 **不解析、不预批准、不据此注册工具**。

**本版明确不做的 Skills 实现**（避免假实现）：

- 不把仓库里现有 `skills/` 示例的 Pulse 私有字段（`category` / `language` / `timeout` / `parameters` / `env_vars`）升格为规范；
- **不在本文实现装载器**；Skills 装载器另开设计文（见 §10 / T4）；
- 不把 Skill 正文当 tool arguments schema 喂给模型工具列表；
- 不暗示 v1 已有「宿主沙箱跑 skill 脚本」运行时。

现有 `skills/`（gitignore）仅作本地试验材料；规范化时按 agentskills.io 收敛。

### 3.5 事件与 HITL（不改语义）

| 事件 | 作用域 | 用途 |
|---|---|---|
| `pulse.loop.before_tool_call` | WaterfallLocal | 改写 Call、Rejected 短路、HITL、按 Risk 策略 |
| `pulse.loop.after_tool_call` | EmitLocal | 审计轨迹（含 Rejected / Err / Duration） |

装配约束（已有，重申）：

- HITL / 策略监听必须与 Agent 挂在同一 `reqScope`；
- 禁止在宿主全树 `Waterfall` 上挂请求级审批（串扰）；
- toolset 注册生命周期挂装配/来源 scope，**不要**挂到单次 reqScope（否则每轮重建注册）。

### 3.6 observability 桥

继续由 `demoapp.Bridge`（或未来宿主桥）订阅 `EventAfterToolCall` → `loop.tool_finished` Record。

本版允许桥用 **slog 附加键**输出 `tool` / `source`（若策略层注入），**不**给 `observability.Record` 增加 Attributes map，也**不**让 observability import toolset。

`before_tool_call` 的拒绝结果已体现在 `AfterToolCall.Rejected`，无需强制双记；若宿主要审批等待时长，自行在 HITL 插件打点，不进官方信封。

## 4. 生命周期

```text
Host Use(toolset.Plugin)
  → Provide(pulse.tools)
  → Use(本地工具插件…) / Use(MCPSource…)
       → Registry.Register × N（各带 Effect；Risk 必填）
  → 请求：Derive reqScope → HITL → Agent(AsToolSet()) → Dispose reqScope
       （注册保留；仅摘除请求级监听）
       （本回合 Tools 快照在 loop 回合开始时固定）
  → MCP 掉线 / 卸载来源插件
       → DisposeSource(source) 或 dispose
       → 本回合：列表不收缩；Execute 已撤销名 → error
       → 下一回合：Definitions 不再包含
  → Host Dispose
       → Registry Close + 全部 Effect 撤销
```

不变式：

1. **注册即可逆**：没有「只 Register 不 dispose」的公共 API 逃逸路径（测试可用显式 dispose / `DisposeSource`）。
2. **来源掉线 = 能力降级**：不伪造占位 tool；不按 Name 前缀猜来源。
3. **请求隔离 ≠ 工具隔离**：工具目录是宿主级（或长生命周期 scope）；审批是请求级。
4. **回合内不收缩工具列表**；撤销影响 Execute 与后续回合的 Definitions。

## 5. MCP 边界（v1）

v1 目标：

- 一个 MCP server = 一个 `Source` 字符串（如 `mcp.<serverID>`），**不是** Name 前缀协议；
- list tools → 映射为 `llm.ToolDef` + 转发 call 的 `Fn`；
- **定名在 Register 之前**：配置项 `name_prefix`（可空）。空 = 用上游原名；非空 = 宿主指定格式拼出 `Def.Name`（例如 `prefix + "_" + orig`）。撞名 → Register error + 装配日志，**禁止冲突后静默改名**；
- 连接失败/断开 → `DisposeSource(source)`（或插件自持 dispose 列表一次性撤销）；
- 工具名主键仍是全局扁平的 `Def.Name`，与 `MemToolSet` 短名习惯一致。

v1 不做：

- 把 MCP resources/prompts 塞进 ToolSet；
- provider 原生 MCP 线格式与 Pulse Registry 双轨自动同步（若模型侧已有 provider MCP，由宿主二选一，避免双重工具列表）；
- 「冲突自动加前缀」（模型调用名漂移）。

## 6. 迁移与兼容

| 现状 | v1 后 |
|---|---|
| `loop.MemToolSet` | 保留；文档标明「轻量/测试」 |
| `WithToolSet(mem)` | 继续合法 |
| 生产装配 | `WithToolSet(registry.AsToolSet())` |
| demo HITL | 可增强为 `LookupMeta` 读 Risk；默认行为不变 |
| P1 文档措辞 | 已随本设计 Accepted **同批**修订 |

无强制大爆炸迁移：旧代码不改也能跑；新能力走 Registry。

## 7. 明确不做

1. 第二套 `before_execute` / `after_execute` 事件总线。
2. `map[string]any` 工具 Options / 元数据逃生舱。
3. 把权限、Source、MCP session 写进 `llm.ToolDef`。
4. Skills 特例执行面（Skill ≠ Tool）。
5. 以仓库现有非标准 skill frontmatter 当 API。
6. observability 依赖 toolset/loop。
7. 无消费者的空接口占位（例如先留 `Sandbox` 接口不实现）；也**不**提供独立脚本执行面。
8. 本设计 PR 夹带记忆层实现或提交 `memory-layer-research-and-v2-design.md`。
9. 对 `Def.Name` 前缀匹配批量撤销；冲突后自动改名。
10. 把 `ToolSet` / `ToolFunc` 为依赖方向再抽一层包。

## 8. 分阶段落地（实现顺序，仍属本设计范围）

| 阶段 | 交付 | 验收 |
|---|---|---|
| **T0 文档** | 本设计 Accepted + Issue #38 + PR #39；P1 措辞同批修订 | D1–D8 Accepted；§10 已拍板；契约缝入库；P1 无双事实源 |
| **T1 Registry** | `toolset` 包：Plugin、Register/dispose、`DisposeSource`、AsToolSet、Risk 必填、冲突/排序/回合语义测试 | `go test -race ./toolset/...`；可用两个假来源插件测掉线撤销（不必等 MCP） |
| **T2 示例迁移** | demoapp 可选改用 Registry 注册 lookup；HITL 可选读 Risk | `02-react` 行为不变；可与 T1 同 PR |
| **T3 MCP 来源** | **另开实现 Issue/PR**：最小 MCP client 来源插件（可先 mock transport） | 上线注册 / `DisposeSource` / 前缀配置 / 冲突失败 |
| **T4 Skills 装载器** | 设计文 [`skills-v1-design.md`](skills-v1-design.md) + Issue [#46](https://github.com/Luo-root/pulse/issues/46)；不在本文追加实现规格 | 按 agentskills.io；与 toolset 仅「工具已存在」交互 |

记忆层仍排在工具侧主路径之后。

## 9. 测试计划摘要

- Register 可逆：dispose 后 live `Definitions` 不再包含该名；重复 dispose 安全。
- `DisposeSource`：只撤该 Source；不影响其他 Source；不依赖 Name 前缀。
- Risk：`RiskUnspecified` / 漏填 → Register error。
- 同名冲突：第二次 Register 返回 error，第一次仍可用；无自动改名。
- `AsToolSet`：顺序稳定；Execute 转发；ctx 取消传递；live 视图语义。
- 回合快照（与 loop 集成或文档化单测）：回合开始后 DisposeSource，本回合 Tools 切片仍旧；Execute 已撤名失败；下一回合 Definitions 更新。
- 与 loop 集成：mock 模型多轮工具 + HITL reject 仍只走 Local 事件；`LookupMeta` 用改名后的 `Call.Name`。
- 双 reqScope：策略不串扰（复用既有 Local 事件测试模式）。
- MCP 源（T3）：连接→可见；断开→不可见；重连同名成功；`name_prefix` 定名在 Register 前。
- 负面：禁止测试依赖「Skill 被当成 ToolDef 自动出现在 Definitions」。

## 10. 已拍板（原评审四问）

> PR #39 审核推荐口径，写入正文后不再保留开放问题。

1. **Risk**：三级够用（Readonly / ReadWrite / Dangerous）。Register **必填**；采用 `RiskUnspecified = 0` 且 Register 拒绝零值，禁止漏填变成 Readonly（fail-open）。不要「默认 Readonly + 来源覆盖」。
2. **命名**：模型可见名**全局扁平且唯一**（主键 = `Def.Name`）。`Source` 只做元数据 + `DisposeSource`。MCP 用配置 `name_prefix`（可空）在 **Register 之前**定名；冲突失败 + 装配日志；**禁止**冲突后静默改名或强制 `source/name` 进 `ToolDef.Name`。
3. **PR 拆分**：T3 MCP 与 T1 **拆 PR**。T1（+ 可选 T2）先合契约；T1 用本地假来源插件即可验证可逆，不必等 MCP。T3 另开实现 Issue，抄走 Source / `DisposeSource` / 前缀配置。
4. **Skills**：装载器 **另开设计文**（T4 = 新 Issue + `docs/design/skills-v1-design.md`）。本篇只保留定位与边界；Accepted 后不在本文追加装载器规格，避免双事实源。`allowed-tools` 当不透明声明，不解析。

## 11. 参考

- 现实现：`loop/tools.go`、`loop/events.go`、`loop/loop.go`；`examples/internal/demoapp/hitl.go`、`bridge.go`
- 路线图原文：`docs/design/plugin-kernel-v2.md` §P1（待本设计 Accepted 同批修订）
- Local 事件：`docs/design/kernel-local-events.md`
- 观测边界：`docs/design/observability-v1-design.md`
- 评审：PR [#39](https://github.com/Luo-root/pulse/pull/39) 评论（2026-08-28）
- Agent Skills：https://agentskills.io/home 、https://agentskills.io/specification
- Anthropic skills 说明：https://github.com/anthropics/skills
