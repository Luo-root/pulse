# Pulse

[![Go Version](https://img.shields.io/badge/Go-1.25.0-blue.svg)](https://go.dev/)
[![License](https://img.shields.io/badge/License-Apache%202.0-green.svg)](LICENSE)

**Pulse** 是一个用 Go 语言编写的 AI Agent 框架。它提供模型接入、工具调用、记忆管理、工作流编排、MCP 协议、代码沙箱、HITL 人类确认、OpenTelemetry 可观测性等全栈能力，帮助你快速构建可扩展的智能 Agent 应用。

> **核心设计原则**：模块可插拔、接口可组合、零硬编码、一切可配置。

---

## 目录

- [快速上手](#快速上手)
- [架构概览](#架构概览)
- [模块总览](#模块总览)
- [最佳实践](#最佳实践)
- [安装与依赖](#安装与依赖)
- [项目结构](#项目结构)
- [许可证](#许可证)

---

## 快速上手

### 1. 最简 Agent（3 行代码）

```go
registry := tools.NewToolRegistry()
tools.RegisterAll(registry)

model, _ := openai.NewChatModel(&openai.ChatModelConfig{
    BaseURL: "https://api.openai.com/v1",
    APIKey:  os.Getenv("OPENAI_API_KEY"),
    Model:   "gpt-4o",
    Tools:   registry.GetEnabledTools(),
})

ag := agent.NewAgent(model, registry)
resp, _ := ag.SendMessage(ctx, schema.UserMessage("列出当前目录文件"))
fmt.Println(resp.Content)
```

### 2. ReAct 循环（自主推理 + 工具调用）

```go
loop := flowagent.NewAgentLoop(model, registry,
    flowagent.WithMaxRounds(10),
    flowagent.WithSystemPrompt("你是一个编程助手"),
)
result, _ := loop.Run(ctx, "帮我写一个 HTTP 服务器")
fmt.Println(result.Answer)
```

### 3. Plan-and-Execute（规划 + 执行）

```go
executor := flowagent.NewPlanExecutor(model, registry,
    flowagent.WithPlanMaxSteps(8),
    flowagent.WithPlanPersistPath("./plan.gob"), // 断点持久化
)
result, _ := executor.Run(ctx, "分析销售数据并生成季度报告")
```

### 4. 工作流编排

```go
wf, _ := flowchart.NewWorkflow(ctx, 10)
wf.AddAspect(node.NewTimeoutAspect(30 * time.Second))
wf.AddAspect(node.NewRetryAspect(3, time.Second))

n1 := node.NewNode("fetch", nil, []string{"data"}, fetchFunc)
n2 := node.NewNode("process", []string{"data"}, []string{"result"}, processFunc)
wf.AddNode(n1)
wf.AddNode(n2)
wf.Run(nil)
```

### 5. HITL 人类确认

```go
approver := hitl.NewChannelApprover(reqCh, respCh)

// 工具级确认（ReadWrite 以上权限）
hitl.RegisterWithConfirmation(registry, approver)

// 节点级确认
wf.AddAspect(hitl.NewNodeConfirmationAspect(approver, nil, 30*time.Second))
```

### 6. OpenTelemetry 可观测性

```go
tracer := telemetry.NewOTelTracer("my-app")
metrics, _ := telemetry.NewOTelMetrics("my-app")
logger := telemetry.NewOTelAgentLogger(tracer, metrics)

// 工作流切面
wf.AddAspect(telemetry.NewWorkflowAspect(tracer))

// Agent 日志
logger.LogToolCall(ctx, "file_read", args, duration, err)
logger.LogModelCall(ctx, "gpt-4", 1500, duration, nil)
```

---

## 架构概览

```
┌─────────────────────────────────────────────────────────────┐
│                        Agent                                │
│  ┌─────────┐  ┌──────────┐  ┌────────────┐  ┌───────────┐ │
│  │ Memory   │  │ ChatModel│  │ ToolRegistry│  │  HITL     │ │
│  │Controller│  │ Interface│  │             │  │ Approver  │ │
│  └────┬─────┘  └────┬─────┘  └──────┬──────┘  └─────┬─────┘ │
│       │              │               │               │       │
│  ┌────▼─────┐  ┌────▼─────┐  ┌──────▼──────┐  ┌────▼─────┐ │
│  │ShortTerm │  │  OpenAI   │  │ File / Cmd   │  │ Channel  │ │
│  │LongTerm  │  │ Anthropic │  │ Web / Config │  │ Auto     │ │
│  └──────────┘  └──────────┘  └──────────────┘  └──────────┘ │
├─────────────────────────────────────────────────────────────┤
│  Flowchart (Workflow) ─ DAG + Aspect + ReAct / PlanExecutor  │
├─────────────────────────────────────────────────────────────┤
│  MCP Client ─ JSON-RPC 2.0 over stdio / SSE                  │
├─────────────────────────────────────────────────────────────┤
│  Sandbox ─ 多语言代码安全执行                                  │
├─────────────────────────────────────────────────────────────┤
│  Telemetry ─ OTel Tracer + Metrics + Slog + WorkflowAspect   │
├─────────────────────────────────────────────────────────────┤
│  Skill ─ Markdown 驱动的技能定义与动态加载                      │
└─────────────────────────────────────────────────────────────┘
```

---

## 模块总览

| 模块 | 位置 | 职责 |
|------|------|------|
| **agent** | `components/agent/` | Agent 核心：多轮对话循环、工具调用编排、API 重试、Usage 追踪 |
| **chatmodel** | `components/chatmodel/` | 模型抽象：OpenAI / Anthropic / Mock |
| **schema** | `components/schema/` | 数据结构：Message、ToolCall、StreamReader、ContentPart（多模态） |
| **tools** | `components/tools/` | 工具系统：ToolRegistry + 10 个内置工具 + 生命周期钩子 |
| **memory** | `components/memory/` | 记忆系统：三层架构 + GORM/Redis/Milvus 后端 + 向量检索 |
| **flowchart** | `components/flowchart/` | 工作流：DAG 引擎 + Aspect 切面 + AgentLoop + PlanExecutor |
| **mcp** | `components/mcp/` | MCP 客户端：stdio / SSE 传输 + 多服务器管理 |
| **sandbox** | `components/sandbox/` | 代码沙箱：进程隔离 + 超时 + 输出限制 + 环境变量过滤 |
| **skill** | `components/skill/` | 技能系统：Markdown + YAML frontmatter 定义与加载 |
| **stream** | `components/stream/` | 流式多播：一对多广播 + 慢消费者隔离 |
| **telemetry** | `components/telemetry/` | 可观测性：OTel Tracer / Metrics + Slog + WorkflowAspect |
| **hitl** | `components/hitl/` | 人类确认：节点/工具/计划三级确认 + ChannelApprover |

---

### agent — Agent 核心

Agent 是所有能力的调度中心。它封装了 `BaseModel` + `ToolRegistry` + `MemoryController`，提供多轮对话、工具调用循环、API 重试和 Token 追踪。

```go
ag := agent.NewAgent(model, registry,
    agent.WithSessionID("session-001"),
    agent.WithUsageTracker(agent.NewUsageTracker()),
    agent.WithMaxToolRounds(20),
    agent.WithMaxRetries(3),
    agent.WithRetryBackoffBase(time.Second),
)

// 非流式
resp, _ := ag.SendMessage(ctx, schema.UserMessage("你好"))

// 流式
ag.SendMessageStream(ctx, msg, func(chunk *schema.Message, isToolCall bool) bool {
    fmt.Print(chunk.Content)
    return true
})
```

**特性**：多轮工具调用循环（默认 20 轮）、指数退避重试（429/500/502/503）、Usage 追踪与预算控制、运行时切换模型。

---

### chatmodel — 模型抽象

```go
type BaseModel interface {
    Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error)
    Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error)
}
```

| 实现 | 说明 |
|------|------|
| `openai.ChatModel` | OpenAI 兼容 API（支持 Ollama、vLLM 等端点） |
| `anthropic.ChatModel` | Anthropic Messages API |
| `mock.MockModel` | 可编程响应的测试模型（支持多模态、预设场景） |

---

### schema — 消息与工具定义

核心类型：`Message`、`ContentPart`（多模态）、`ToolCall`、`ToolResult`、`StreamReader`、`Tool`、`Document`。

```go
// 多模态消息
msg := schema.UserMultimodalMessage(
    schema.TextPart("描述这张图片"),
    schema.ImagePartBase64("image/png", base64Data),
)
```

---

### tools — 工具注册与执行

```go
registry := tools.NewToolRegistry()
tools.RegisterAll(registry) // 一键注册所有内置工具
```

**内置工具**：`file_read`、`file_write`、`file_list`、`file_edit`、`file_search`、`command_exec`、`web_fetch`、`web_browse`、`user_config`、`get_work_dir`

**自定义工具**：

```go
registry.Register(tools.ToolMetadata{
    Name:       "my_tool",
    Description: "我的工具",
    Permission: tools.PermReadWrite,
    Category:   "custom",
}, func(ctx context.Context, args map[string]any) (any, error) {
    return "result", nil
})
```

**生命周期钩子**：`AddBeforeExecuteHook` / `AddAfterExecuteHook` / `AddOnRegisterHook` / `AddOnUnregisterHook`

---

### memory — 记忆管理

三层架构：系统提示词（永久）→ 短期记忆（滑动窗口）→ 长期记忆（向量检索）。

**Store + Retriever 分离架构**：

```
┌─────────────────────────────────────────────────────┐
│              CompositeLongTermStore                  │
│  ┌──────────┐    hooks    ┌──────────────┐          │
│  │  Store    │ ──────────→│  Retriever    │          │
│  │(存储层)    │  onSave    │(检索层)       │          │
│  └──────────┘  onClear   └──────────────┘          │
│       │                         │                   │
│       └───── Indexer ───────────┘                   │
└─────────────────────────────────────────────────────┘
```

**可组合的后端**：

| Store | Retriever | 场景 |
|-------|-----------|------|
| GORM (SQLite) | HNSW (内存) | 本地开发，零依赖 |
| Redis | RediSearch | 分布式，Redis Stack |
| Milvus | Milvus | 大规模向量数据库 |
| GORM | Milvus | 本地存储 + 云端检索 |

```go
// 示例：GORM + HNSW
store, _ := gorm.NewGORMStore(gormCfg, embeddingFunc)
retriever := gorm.NewHNSWRetriever(store.GetDB(), embeddingFunc, gormCfg)
composite := memory.NewCompositeLongTermStore(store, retriever)
composite.AttachIndexer(retriever)
```

**Embedder**：`embedder.NewOpenAI`（OpenAI/vLLM）、`embedder.NewOllama`（Ollama 原生）

---

### flowchart — 工作流编排与 Agent 规划

基于声明式依赖的 DAG 工作流引擎，支持并行节点执行和 AOP 切面。

**Node 接口**：

```go
type Node interface {
    ID() string
    Inputs() []string
    Outputs() []string
    Run(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error)
    Aspects() []Aspect
}
```

**内置切面**：`RetryAspect`、`TimeoutAspect`、`CircuitBreakerAspect`、`RecoveryAspect`、`ErrorSwallowAspect`

**Agent 引擎**：

| 引擎 | 说明 |
|------|------|
| `AgentLoop` | ReAct 循环：思考 → 工具调用 → 观察 → 循环 |
| `PlanExecutor` | Plan-and-Execute：LLM 规划 → 验证依赖 → 并行执行 → 重规划 → 断点持久化 |

---

### mcp — MCP 协议客户端

支持 stdio 和 SSE 两种传输方式，可连接多个 MCP 服务器。

```json
{
  "mcpServers": [
    { "name": "local", "transport": { "type": "stdio", "command": "node", "args": ["server.js"] } },
    { "name": "remote", "transport": { "type": "sse", "url": "https://mcp.example.com/sse", "headers": { "Authorization": "Bearer ${TOKEN}" } } }
  ]
}
```

---

### sandbox — 代码执行沙箱

```go
langs := map[string]sandbox.LangDef{
    "python": {Command: "python3", Ext: ".py"},
    "node":   {Command: "node", Ext: ".js"},
    "go":     {Command: "go", Args: []string{"run"}, Ext: ".go"},
    "shell":  {Command: "sh", Args: []string{"-c"}},
}
sb := sandbox.NewProcessSandbox(sandbox.ProcessConfig{})
sandbox.RegisterSandboxTools(registry, sb, langs)
```

安全特性：环境变量黑名单（默认）/ 白名单 / 透传、路径穿越校验、超时控制。

---

### telemetry — 可观测性

```go
// OTel 追踪
tracer := telemetry.NewOTelTracer("my-app")
wf.AddAspect(telemetry.NewWorkflowAspect(tracer))

// OTel 指标
metrics, _ := telemetry.NewOTelMetrics("my-app")

// 统一日志器（slog + OTel metrics）
logger := telemetry.NewOTelAgentLogger(tracer, metrics)
logger.LogToolCall(ctx, "file_read", args, duration, err)
logger.LogModelCall(ctx, "gpt-4", 1500, duration, nil)
```

---

### hitl — 人类确认

三级确认：节点级、工具级、计划步骤级。

```go
approver := hitl.NewChannelApprover(reqCh, respCh)

// 工具级：ReadWrite 以上权限需要确认
hitl.RegisterWithConfirmation(registry, approver)

// 节点级：所有节点需要确认
wf.AddAspect(hitl.NewNodeConfirmationAspect(approver, nil, 30*time.Second))

// 计划步骤级
pc := hitl.NewPlanConfirmation(approver, nil, 30*time.Second)
executor := flowagent.NewPlanExecutor(model, registry,
    flowagent.WithPlanStepCallback(pc.StepCallback(ctx)),
)
```

`Approver` 接口可自定义实现：TUI 弹窗、Web 前端、Slack 消息等。

---

### skill — 技能系统

基于 Markdown + YAML frontmatter 的技能定义：

```markdown
---
name: code-review
description: 代码审查技能
timeout: 60s
---
你是一个代码审查专家...
```

```go
loader := skill.NewSkillLoader(registry, toolRegistry)
loader.LoadFromDir("./skills")
```

---

## 最佳实践

### Agent 设计

1. **优先使用 AgentLoop 而非 Agent**：`AgentLoop` 是无状态的 ReAct 引擎，适合单次任务；`Agent` 是有状态的会话管理器，适合多轮对话。
2. **合理设置 `maxToolRounds`**：默认 20 轮，复杂任务可调高，简单任务可调低以控制成本。
3. **使用 UsageTracker 监控成本**：设置预算上限，防止意外高消耗。

### 工作流设计

4. **切面顺序很重要**：切面按注册顺序从外到内形成洋葱链。`TimeoutAspect` 应在外层，`RetryAspect` 在内层。
5. **用 `WaitWithContext` 替代 `Wait`**：切面取消 context 时，`WaitWithContext` 能立即感知，`Wait` 会一直阻塞。
6. **PlanExecutor 适合复杂任务**：自动处理依赖排序、并行执行、失败重规划、断点持久化。

### 记忆系统

7. **根据场景选择后端**：本地开发用 GORM+HNSW（零依赖），分布式用 Redis+RediSearch，大规模用 Milvus。
8. **Store 和 Retriever 可自由组合**：GORM 存储 + Milvus 检索、Redis 存储 + RediSearch 检索等。
9. **使用 CalibratedEstimator**：喂入模型实际返回的 token 数，3 轮后自动校准估算精度。

### 安全

10. **工具权限分级**：`PermReadOnly` / `PermReadWrite` / `PermDangerous`，配合 HITL 确认。
11. **沙箱隔离**：代码执行务必通过 `ProcessSandbox`，不要直接 `exec.Command`。
12. **MCP 认证**：远程 MCP 服务器使用 SSE 传输 + `headers` 传递认证信息。

### 可观测性

13. **生产环境用 OTel**：`OTelTracer` + `OTelMetrics` 接入 Prometheus/Jaeger。
14. **开发环境用 Slog**：`SlogTracer` 零依赖，直接输出到 stdout。
15. **WorkflowAspect 必加**：每个工作流节点自动记录 span，排查问题一目了然。

---

## 安装与依赖

**Go 版本**: 1.25.0+

```bash
go get github.com/Luo-root/pulse
```

**核心依赖**:

| 依赖 | 用途 |
|------|------|
| `gorm.io/gorm` + `glebarez/sqlite` | GORM 长期记忆持久化 |
| `coder/hnsw` | HNSW 向量索引 |
| `redis/go-redis/v9` | Redis 存储 + RediSearch 检索 |
| `milvus-io/milvus-sdk-go/v2` | Milvus 向量数据库 |
| `panjf2000/ants/v2` | 工作流协程池 |
| `chromedp/chromedp` | 浏览器自动化 |
| `go.opentelemetry.io/otel` | OpenTelemetry 可观测性 |
| `gopkg.in/yaml.v3` | Skill YAML 解析 |

---

## 项目结构

```
pulse/
├── pulse.go                              # 包入口
├── go.mod / go.sum
├── AGENTS.md                             # Agent 指令文件
├── LICENSE                               # Apache 2.0
└── components/
    ├── agent/                            # Agent 核心（多轮对话、工具循环、重试、Usage）
    ├── chatmodel/                        # 模型抽象（OpenAI / Anthropic / Mock）
    ├── schema/                           # 数据结构（Message / ToolCall / StreamReader / Document）
    ├── tools/                            # 工具系统（Registry + 10 内置工具 + Hook）
    ├── memory/                           # 记忆系统
    │   ├── controller.go                 # 三层记忆控制器
    │   ├── embedder/                     # Embedder（OpenAI / Ollama）
    │   ├── window/                       # 短期记忆（滑动窗口 + 校准估算）
    │   ├── gorm/                         # GORM Store + HNSW Retriever
    │   ├── redis/                        # Redis Store + RediSearch Retriever
    │   ├── milvus/                       # Milvus Store + Retriever
    │   └── integration/                  # 跨后端组合集成测试
    ├── flowchart/                        # 工作流引擎
    │   ├── workflow.go                   # DAG 引擎（协程池 + 切面链）
    │   ├── flow/                         # FlowContext / DataSlot / SafeMap
    │   ├── node/                         # Node + 功能节点 + Aspect 切面
    │   └── agent/                        # AgentLoop (ReAct) + PlanExecutor
    ├── mcp/                              # MCP 客户端（stdio / SSE 传输）
    ├── sandbox/                          # 代码沙箱（进程隔离 + 多语言）
    ├── skill/                            # 技能系统（Markdown + YAML）
    ├── stream/                           # 流式多播
    ├── telemetry/                        # 可观测性（OTel + Slog + WorkflowAspect）
    ├── hitl/                             # 人类确认（节点/工具/计划确认）
    └── bufutil/                          # 缓冲区工具
```

---

## 许可证

本项目采用 [Apache License 2.0](LICENSE) 许可证。
