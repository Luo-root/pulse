# Pulse 示例：从入门到精通（8 课）

8 课渐进式教程，每课**独立可跑**（`go run ./examples/<课>`）、配详解 README。课程按依赖递进：前 4 课是框架基础（kernel → 词汇表 → ReAct → 审批），5-6 课是记忆层（会话 → 长期记忆），第 7 课是声明式编排与生产集成。

> 学习路径建议按编号顺序；每课 README 开头列「本课依赖」，可跳读。

| 课 | 目录 | 主题 | 核心包 | 依赖课 |
|---|---|---|---|---|
| 00 | [`00-hello-kernel`](00-hello-kernel/) | 内核生命周期与第一次模型调用：`New/Use/Dispose`、可逆 Effect、观测横幅 | `kernel` `llm` `observability` | — |
| 01 | [`01-chat`](01-chat/) | 完整装配链与消息词汇表：Registry、多模态 Part、REPL | `kernel` `llm` | 00 |
| 02 | [`02-react`](02-react/) | ReAct 循环与工具调用：`toolset.Registry`、流式输出、`AsToolSet` | `loop` `toolset` | 01 |
| 03 | [`03-hitl`](03-hitl/) | 人机协同审批：`before_tool_call`、deny/allow、session trust | `loop`（HITL 事件） | 02 |
| 04 | [`04-flow`](04-flow/) | 声明式与命令式编排：flow 节点图、槽位三态、YAML 装图 | `kernel/flow` `flow/yaml` | 02 |
| 05 | [`05-memory-session`](05-memory-session/) | 会话真相与压缩：事件日志、fold 投影、JSONL 持久化、compaction | `memory/session` `memory/compaction` | 01 |
| 06 | [`06-memory-agent`](06-memory-agent/) | 长期记忆全链路：store → index 召回 → candidate 提炼 → 审批 → assemble 注入 | `memory/*` 8 包 | 05 |
| 07 | [`07-production`](07-production/) | 生产形态集成：MCP/Skills 多来源、观测桥、反思与指标面 | 全部 | 03 04 06 |

## 运行

启动时自动读取仓库根 `.env`（已被 gitignore）。

```powershell
go run ./examples/00-hello-kernel
go run ./examples/01-chat
# ... 以此类推

# 全部示例测试（含离线单测，无真实 API）
go test ./examples/...
```

每课缺 API Key 时自动走 `ScriptedModel`（脚本响应），课程演示不依赖真实凭据；配置真实 Key 后同一份代码即走真实模型。变量说明见各课 README。

## 架构总览（示例如何拼出完整框架）

```text
00  kernel.New ──Use(observability)──Use(llm.Plugin)──Generate        ← 地基
01      + Registry 命名实例 / 词汇表 Part / REPL                       ← 会说话
02      + toolset.Registry + loop.Agent(ReAct) + RunStream             ← 会做事
03      + before_tool_call 审批 / 信任模式                              ← 可控
04      + flow 节点图 / YAML 装图（编排两种形态）                        ← 可编排
05      + session 事件日志 / fold / JSONL / compaction                 ← 记得住
06      + store + index + candidate + assemble（长期记忆闭环）          ← 会积累
07      + MCP / Skills 多来源 + 观测桥 + reflection 指标                ← 可上线
```

## 观测日志

- **装配期**（各课共用）：`observability.Bootstrap` → `MemorySink` / `SlogSink`，字段见 [`observability/README_zh.md`](../observability/README_zh.md)。
- **运行期**：观测桥把 llm/loop/flow 事实折进同一 Sink（`source=bridge`，必填 `trace_id`）——02 课手写 `reqBridge` 教学，03 课起复用 `demoapp.Bridge` 封装版。
- 隐私边界：记录**元数据**（次数、字节长度、耗时、状态），不记录 prompt 内容、附件内容、密钥和思维链。

## 当前不做

- 示例刻意不接 memory/ 的课程：05/06 之前不持久化（history 只活在进程内）——这是课程边界，见 [`memory/README_zh.md`](../memory/README_zh.md)。
- 真实向量库 / embedding（06 用内存版 index 与关键词召回演示语义一致的结构）。

## 设计说明

- `examples/internal/demoapp` 是课程私有的装配层（REPL 壳、`.env` 加载、封装版 Bridge/HITL），**库包本身无 internal**——此处不违反「库无 internal」约定。
- 教学节奏：01–03 关键实现（装配链 / Bridge / HITL）在课内**手写展开**，demoapp 封装版是它们的对照原型；04 起复用封装版，每课 `main.go` 刻意保持「能读一遍」的长度，复杂度沉淀进 demoapp 或对应包。
- 遇到 API 疑问：每个正式包的 `README_zh.md` 是事实源，`doc.go` 是 godoc 入口。
