# Skills v1 设计：业界对齐的规程包装载（渐进披露）

> 状态：**Draft → 评审修订中**（PR [#47](https://github.com/Luo-root/pulse/pull/47) 评论已接受 S1–S8；激活缝与 §9 已按推荐口径写入，待标 Accepted）
> 包位置（拟）：公开 Go 包名 **`skills`**（**不**进 `loop/`，**不**做成 Source）。仓库根现有 gitignore 的 `skills/` 示例材料实现票迁到 `examples/skills/`（或改路径），避免与公开包撞名。
> 前置：toolset T1–T3 + 官方 MCP Client 已落地；toolset-v1 Accepted 仅钉边界
> 对齐：[Agent Skills Specification](https://agentskills.io/specification)；用户拍板「跟业界一致、不要特例独行」
> 与 toolset：Skill **指导**使用已注册工具；**不**自动 `Register` ToolDef
> Issue：[#46](https://github.com/Luo-root/pulse/issues/46)

## 0. 一句话定位

给 Pulse 一套与 agentskills.io 同构的 **Skill 发现 / 激活 / 披露** 能力：Skill 是文件夹里的规程说明书（+ 可选脚本与资料），经渐进披露进入 **Messages 上下文**；真正执行仍走 `pulse.tools` → `loop.ToolSet`。不做第二套工具调用协议，不做 Pulse 私有 frontmatter 规范。

## 1. 决策记录

| # | 决策 | 被否掉的替代方案 | 理由 |
|---|---|---|---|
| S1 | Skill = **规程包**（说明书 + 可选资源），不是可执行 Tool | Skill 实现 `loop.ToolSet` / 伪 `Execute` | 业界标准与用户拍板；Tool 走 `Tools` 字段 + `tool_call`，Skill 走 Messages |
| S2 | Skill **不是** MCP 式 Source 插件 | Skill 往 Registry `Register` 一批 ToolDef | Source = 工具目录来源；Skill = 上下文/规程（T3 讨论已钉） |
| S3 | 格式事实源 = agentskills.io | 以仓库现有 `skills/` 私有字段为规范 | 私有 `category`/`parameters`/`timeout`/`env_vars`/`tags` **不升格** |
| S4 | 渐进披露三阶段：Discovery → Activation → Resources | 启动时把全部 SKILL.md 正文塞进 system | 省上下文；与业界一致 |
| S5 | `allowed-tools` = **不透明声明** | 解析 `Bash(git:*)` 并自动预批准/注册 | 方言不稳定；Pulse v1 不解析、不据此 Register |
| S6 | 脚本若要跑，必须映射到**已注册工具** | 独立 Sandbox / Skill 专用执行面 | toolset-v1 已禁止空 Sandbox；避免第二执行总线 |
| S7 | 装载器实现与本文设计拆票 | 在 toolset-v1 文末追加装载器规格 | 避免双事实源（toolset-v1 §10 已拍板） |
| S8 | 与 loop 的集成点 = **装配层注入 / 只读宿主工具**，不改 `ToolSet` 接口 | 在 `before_generate` 里改 Tools；假装 Run 中途可插 system | Skill 不是 Tool；现有 `RunStream` 回合内只能追加 tool result |

评审（PR #47）：S1–S8 **Accepted**。

## 2. 与 Tool / MCP 的通道对照

```text
GenerateRequest
├── Messages[]     ← Skill 披露进这里（name/desc 或全文/引用）
└── Tools[]        ← 只有 llm.ToolDef（local / MCP Source → Registry）
         │
         ▼
   tool_call → before_tool_call → ToolSet.Execute → tool result
```

| | Tool / MCP Source | Skill |
|---|---|---|
| API 通道 | `Tools` + `tool_calls` | `Messages`（文本） |
| 宿主执行 | `ToolSet.Execute` | 无对等 Execute；最多再读文件进上下文 |
| 注册中心 | `pulse.tools` | 独立 Skill 目录/索引（公开包 `skills`） |
| 掉线语义 | `DisposeSource` | 卸载装载器 / 从发现列表移除（实现票定） |

**纠偏**：两者最终都是 token，但 Tool 还有结构化调用与宿主执行回路；Skill 没有标准 `skill_call` 线格式。

## 3. 格式契约（只承认业界字段）

### 3.1 目录

```text
skill-name/
├── SKILL.md           # 必选：YAML frontmatter + Markdown 正文
├── scripts/           # 可选
├── references/        # 可选
├── assets/            # 可选
└── ...
```

`name` 必须与父目录名一致（agentskills.io 约束）。

### 3.2 Frontmatter

| 字段 | 必填 | Pulse v1 行为 |
|---|---|---|
| `name` | 是 | 校验字符集与长度；作发现主键 |
| `description` | 是 | 发现阶段唯一详细提示 |
| `license` | 否 | 原样保留，不解释 |
| `compatibility` | 否 | **只存储**；v1 不强制跳过（见 §9） |
| `metadata` | 否 | `map[string]string`；宿主可读，不进模型 Tools |
| `allowed-tools` | 否 | **不透明字符串**；不解析、不 Register、不预批准 |

### 3.3 明确不升格的仓库私有字段

现有 gitignore 示例中出现的下列字段，**不是** Pulse Skills 规范的一部分：

`category`、`language`、`timeout`、`tags`、`parameters`、`env_vars`

未知/私有 YAML 键：**忽略**，不报错（可 debug 日志）；**不要**自动塞进 `metadata`。规范化示例时应删掉或显式挪进 `metadata:` 下的自定义键。

### 3.4 正文与引用

- 正文 = 激活后整份读入的指令（建议不超过 500 行 / 约 5000 tokens；此为 spec 建议，v1 **不硬拒绝**）
- 相对路径引用 `scripts/`、`references/`、`assets/`；保持一层深
- 装载器按需读引用文件进上下文，不在 Discovery 阶段预读

## 4. 运行时语义

### 4.1 与 `loop.RunStream` 的关系（定案）

`loop.Agent` 一次 `Run` 里，工作缓冲只在开头拼好（system + history + input）；步与步之间**只有 tool result 能追加 Messages**。没有 `skill_call`，也没有回合中途改 system 的 API。

因此激活只有两条合法路径：

```text
Discovery：A（system / 开发者消息里的 Skills 短表）——起步必有
回合前激活：装配层 Loader.Load → 写入本轮 input / 额外 system（I2）
回合内激活：只读宿主工具 load_skill（I3）
            → Execute 返回 SKILL.md 正文
            → 作为 tool result 进入 Messages
            → 该工具是 Tool（Registry + HITL），Skill 本身仍不是 Source

禁止：在 before_generate 里改 Tools 塞 Skill
禁止：假装 loop 能在回合中途插入 system
```

没有 I3，同一 `Run` 内模型无法按 spec「自己」把 SKILL.md 拉进来；故 I3 是**回合内激活的必要通道**，不是可永远不做的装饰。

### 4.2 三阶段

1. **Discovery**  
   扫描配置的技能根目录，解析 frontmatter，得到 `{name, description, path}` 列表。  
   **短表（A）是 Discovery 的底**：写入 system / 开发者消息。  
   B（`list_skills` / `load_skill`）不能替代短表——否则模型不知道有哪些 skill 可 load。

2. **Activation**  
   - **回合前**：宿主点名 → `Load` → 注入本轮 Messages（I2）  
   - **回合内**：模型 `tool_call` → `load_skill` → tool result 带正文（I3）

3. **Resources**  
   模型/规程要求时，再读 `references/*`、`scripts/*` 等（可经 `ReadFile` 或只读宿主工具）。  
   脚本执行必须通过已存在的 Tool；**本设计不发明执行面**。

### 4.3 与 HITL / toolset

- Skill 激活**不**绕过 `before_tool_call`（I3 的 `load_skill` 也走 HITL；建议 `RiskReadonly`）
- Skill 正文里点名的工具名，若未在 Registry，调用仍走 loop「未知工具」错误回传
- `allowed-tools` 可供**未来**HITL 策略参考，但 v1 **不解析**；策略若要用，另开票定义解析器

### 4.4 生命周期（拟）

```text
Host 配置 skills 根路径
  → skills.Loader 扫描（可挂 kernel Effect，卸载清空索引）
  → 每请求：Discovery 短表进 system
  → 回合前可选：Load 注入
  → 回合内可选：load_skill tool result
  → 请求结束：不卸载磁盘技能；仅丢弃本请求注入的正文
```

技能文件变更：默认「下次扫描生效」，不做 HMR。

## 5. API 草案（示意，非冻结实现）

实现票可调整方法签名细节；此处只定职责。**不预留** `Execute` / `Activate` 空接口。

```go
// 发现条目（进上下文的最小集）
type Meta struct {
    Name        string
    Description string
    Dir         string // 绝对或根相对路径
}

// Loader 负责扫描与按名加载正文/资源。不执行脚本。
type Loader interface {
    List(ctx context.Context) ([]Meta, error)
    // Load 返回该 skill 的 Markdown 指令正文（实现票决定是否剥 frontmatter）。
    // 不含任何「执行」语义。
    Load(ctx context.Context, name string) (body string, err error)
    // ReadFile 读取该 skill 目录内相对路径（scripts/references/assets/...）。
    // rel 必须落在该 skill 目录内；拒绝 ".." 与绝对路径。
    ReadFile(ctx context.Context, name, rel string) ([]byte, error)
}
```

- 装配层：`List` → 短表；回合前 `Load` → 注入 Messages  
- I3：`load_skill` Tool 内部调 `Loader.Load`，**不**让 `Loader` 实现 `loop.ToolSet`  
- `compatibility`：只存在 Meta/解析结果里；过滤若需要，实现票可加**可选** `func(Meta) bool` 回调，默认不过滤

## 6. 明确不做

1. Skill ≠ Tool ≠ Source  
2. 不解析 `allowed-tools` 方言  
3. 不把私有 frontmatter 升格为 API；未知键不自动并入 `metadata`  
4. 无独立脚本 Sandbox / 第二执行事件总线  
5. 不在 Discovery 把全文塞进上下文  
6. 不改 `llm.ToolDef` / `loop.ToolSet` 契约；禁止 `before_generate` 改 Tools 塞 Skill  
7. 本设计 PR 不写实现、不改 02-react 主路径  
8. 不夹带记忆层设计  
9. 不内置 `compatibility` 字符串强制跳过（假实现）

## 7. 实现分期（设计接受后）

| 阶段 | 交付 | 验收 |
|---|---|---|
| **D0** | 本文 Accepted + Issue #46 设计项关闭 | S1–S8 Accepted；§9 已拍板；激活缝入库 |
| **I1** | 包 `skills`：扫描 + frontmatter 校验 + List/Load/ReadFile；示例材料迁出公开包路径 | 单测：合法/非法 name、目录穿越拒绝、私有字段忽略 |
| **I2** | Discovery 短表 + **回合前**点名 `Load` 注入；测试 + 包 README（**不改 02-react**） | 可选独立小 example / 笔记，勿绑 02 |
| **I3** | 只读宿主工具 `list_skills` / `load_skill`（`RiskReadonly`）——**回合内激活必要通道** | 工具走 Registry；Load 结果为 tool result；Skill 仍不是 Source |

I2 与 I3 可同 PR 或紧随；声称「符合渐进披露的回合内自激活」则 I3 不可缺。

记忆层仍独立于 Skills；Skills 可被记忆层引用，但本设计不定义存储。

## 8. 测试计划摘要（实现票用）

- frontmatter：缺 name/description 失败；非法 name 失败；目录名不一致失败  
- 忽略未知/私有键，不报错（或仅 debug 日志）  
- `ReadFile`：`../` 与绝对路径拒绝  
- Discovery 列表稳定排序（按 name）  
- 负面：Load 不得导致 Registry 出现新 ToolDef  
- 负面：不得出现 `Skill.Execute` 或第二套 before_execute  
- I3：`load_skill` 经 Agent 后 Messages 含正文 tool result；HITL 可拦  

## 9. 已拍板（原评审四问）

> PR #47 审核推荐口径，写入正文后不再保留开放问题。

1. **Discovery / 激活**：先 **A（短表）**；回合内激活必须走 **B（只读宿主工具）**。完整形态是 A+B；B 不能替代短表。  
2. **公开包名：`skills`**。不要 `skillloader`。实现票将根目录示例材料与公开包路径错开（如迁到 `examples/skills/`）。  
3. **I2 不改 02-react**。用测试 + 包 README；人眼演示用独立小 example（可选）。  
4. **`compatibility`：v1 只存储，不强制跳过**。可选过滤回调留给实现票，默认不过滤；无内置字符串规则。

## 10. 参考

- https://agentskills.io/home  
- https://agentskills.io/specification  
- `docs/design/toolset-v1-design.md`（D4、§3.4、T4、§10）  
- `loop/loop.go`：`Tools` 来自 `Definitions()`；回合内仅 tool result 追加 Messages  
- 评审：PR [#47](https://github.com/Luo-root/pulse/pull/47) 评论（2026-08-28）  
- 仓库根 `skills/`（gitignore）：仅试验材料；实现票迁路径，字段勿当规范  
