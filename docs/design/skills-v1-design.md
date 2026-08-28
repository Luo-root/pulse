# Skills v1 设计：业界对齐的规程包装载（渐进披露）

> 状态：**Draft**（待 Issue [#46](https://github.com/Luo-root/pulse/issues/46) 评审定案）
> 包位置（拟）：`skills/` 或 `toolset` 旁独立公开包（实现票再定名；**不**进 `loop/`，**不**做成 Source）
> 前置：toolset T1–T3 + 官方 MCP Client 已落地；toolset-v1 Accepted 仅钉边界
> 对齐：[Agent Skills Specification](https://agentskills.io/specification)；用户拍板「跟业界一致、不要特例独行」
> 与 toolset：Skill **指导**使用已注册工具；**不**自动 `Register` ToolDef

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
| S8 | 与 loop 的集成点 = **装配层注入上下文**，不改 `ToolSet` 接口 | 在 `before_generate` 里偷偷改 Tools 列表塞 Skill | Skill 不是 Tool；改 Tools 会污染模型工具目录 |

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
| 注册中心 | `pulse.tools` | 独立 Skill 目录/索引（拟） |
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
| `compatibility` | 否 | 原样保留；可不强制执行 |
| `metadata` | 否 | `map[string]string`；宿主可读，不进模型 Tools |
| `allowed-tools` | 否 | **不透明字符串**；不解析、不 Register、不预批准 |

### 3.3 明确不升格的仓库私有字段

现有 gitignore 的 `skills/` 示例中出现的下列字段，**不是** Pulse Skills 规范的一部分：

`category`、`language`、`timeout`、`tags`、`parameters`、`env_vars`

规范化示例时应删掉或挪进 `metadata:` 下的自定义键（若仍需要宿主侧提示）。

### 3.4 正文与引用

- 正文 = 激活后整份读入的指令（建议不超过 500 行 / 约 5000 tokens）
- 相对路径引用 `scripts/`、`references/`、`assets/`；保持一层深
- 装载器按需读引用文件进上下文，不在 Discovery 阶段预读

## 4. 运行时语义（设计层）

### 4.1 三阶段

1. **Discovery**  
   扫描配置的技能根目录，解析 frontmatter，得到 `{name, description, path}` 列表。  
   向模型暴露的方式（实现票二选一或组合，评审拍板）：  
   - **A.** 装配层写入 system / 开发者消息中的「可用 Skills 目录」短表  
   - **B.** 提供只读「列举/激活」类**宿主工具**（注意：那是 Tool，不是 Skill 本身）  

2. **Activation**  
   当任务与某 Skill 的 description 匹配（由模型决定，或由宿主策略点名），装载器读取该 `SKILL.md` **全文**并注入 Messages（或等价上下文槽）。

3. **Resources**  
   模型/规程要求时，再读 `references/*`、`scripts/*` 等；脚本执行必须通过已存在的 Tool（例如 `run_script` 类工具若已注册），**本设计不发明执行面**。

### 4.2 与 HITL / toolset

- Skill 激活**不**绕过 `before_tool_call`
- Skill 正文里点名的工具名，若未在 Registry，调用仍走 loop「未知工具」错误回传
- `allowed-tools` 可供**未来**HITL 策略参考，但 v1 **不解析**；策略若要用，另开票定义解析器

### 4.3 生命周期（拟）

```text
Host 配置 skills 根路径
  → Skills.Loader 扫描（可挂 kernel Effect，卸载清空索引）
  → 每请求：Discovery 摘要进上下文
  → 激活：读 SKILL.md 进本请求 Messages
  → 请求结束：不要求卸载磁盘技能；仅丢弃本请求注入的正文
```

技能文件变更是否热更新：实现票定；设计默认「下次扫描生效」，不做 HMR。

## 5. API 草案（示意，非冻结实现）

实现票可调整命名；此处只定职责，避免空接口占位。

```go
// 发现条目（进上下文的最小集）
type Meta struct {
    Name        string
    Description string
    Dir         string // 绝对或根相对路径
}

// Loader 负责扫描与按名加载正文/资源。
type Loader interface {
    List(ctx context.Context) ([]Meta, error)
    // Load 返回 SKILL.md 正文（含或含处理后的 Markdown）；不执行脚本。
    Load(ctx context.Context, name string) (body string, err error)
    // ReadFile 读取 skill 目录内相对路径资源（防目录穿越）。
    ReadFile(ctx context.Context, name, rel string) ([]byte, error)
}
```

装配层负责把 `List` 结果变成提示词，并在激活时调用 `Load` 注入 Messages。  
**不**要求 `Loader` 实现 `loop.ToolSet`。

## 6. 明确不做

1. Skill ≠ Tool ≠ Source  
2. 不解析 `allowed-tools` 方言  
3. 不把私有 frontmatter 升格为 API  
4. 无独立脚本 Sandbox / 第二执行事件总线  
5. 不在 Discovery 把全文塞进上下文  
6. 不改 `llm.ToolDef` / `loop.ToolSet` 契约  
7. 本设计 PR 不写实现、不改 examples 主路径  
8. 不夹带记忆层设计

## 7. 实现分期（设计接受后）

| 阶段 | 交付 | 验收 |
|---|---|---|
| **D0** | 本文 Accepted + Issue #46 关闭设计项 | 评审接受 S1–S8 |
| **I1** | Loader：扫描 + frontmatter 校验 + List/Load/ReadFile | 单测：合法/非法 name、目录穿越拒绝、私有字段忽略 |
| **I2** | 装配示例：Discovery 短表 + 点名激活注入 Messages | demo 或小 example；不强制改 02-react |
| **I3** | （可选）只读「list_skills / load_skill」宿主工具 | 工具本身走 Registry；Skill 仍不是 Source |

记忆层仍独立于 Skills；Skills 可被记忆层引用，但本设计不定义存储。

## 8. 测试计划摘要（实现票用）

- frontmatter：缺 name/description 失败；非法 name 失败；目录名不一致失败  
- 忽略未知/私有键，不报错（或仅 debug 日志）  
- `ReadFile`：`../` 拒绝  
- Discovery 列表稳定排序（按 name）  
- 负面：Load 不得导致 Registry 出现新 ToolDef  
- 负面：不得出现 `Skill.Execute` 或第二套 before_execute  

## 9. 评审时请拍板

1. Discovery 暴露方式：仅提示词短表（A），还是加只读宿主工具（B），或 A+B？  
2. 公开包名：`skills` vs `skillloader` vs 其他？  
3. I2 是否必须改某个 example，还是测试 + README 即可？  
4. `compatibility` 字段：只存储，还是装载期可配置强制跳过不兼容项？

## 10. 参考

- https://agentskills.io/home  
- https://agentskills.io/specification  
- `docs/design/toolset-v1-design.md`（D4、§3.4、T4、§10）  
- `loop/loop.go`：`Tools` 来自 `Definitions()`；与 Messages 分离  
- 仓库 `skills/`（gitignore）：仅试验材料，字段勿当规范  
