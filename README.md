# Pulse

[![Go Version](https://img.shields.io/badge/Go-1.25.0-blue.svg)](https://go.dev/)
[![License](https://img.shields.io/badge/License-Apache%202.0-green.svg)](LICENSE)

**Pulse** 是一个用 Go 语言编写的 AI Agent 框架，提供从模型接入、记忆管理、工具调用、MCP 协议通信、工作流编排、代码沙箱到流式处理的全栈能力，帮助开发者快速构建可扩展的智能 Agent 应用。

---

## 目录

- [架构概览](#架构概览)
- [核心特性](#核心特性)
- [模块说明](#模块说明)
- [安装与依赖](#安装与依赖)
- [快速开始](#快速开始)
- [项目结构](#项目结构)
- [许可证](#许可证)

---

## 架构概览

```
┌─────────────────────────────────────────────────────────┐
│                        Agent                             │
│  ┌─────────┐  ┌──────────┐  ┌────────────┐             │
│  │ Memory   │  │ ChatModel│  │ ToolRegistry│            │
│  │Controller│  │ Interface│  │             │            │
│  └────┬─────┘  └────┬─────┘  └──────┬──────┘            │
│       │              │               │                   │
│  ┌────▼─────┐  ┌────▼─────┐  ┌──────▼──────┐           │
│  │ShortTerm │  │  OpenAI   │  │ File / Cmd   │           │
│  │LongTerm  │  │ Anthropic │  │ Web / Config │           │
│  └──────────┘  └──────────┘  └──────────────┘           │
├─────────────────────────────────────────────────────────┤
│  Flowchart (Workflow) ─ DAG + 切面 + ReAct/PlanExecute   │
├─────────────────────────────────────────────────────────┤
│  MCP Client ─ JSON-RPC 2.0 over stdio                    │
├─────────────────────────────────────────────────────────┤
│  Sandbox ─ 多语言代码安全执行                              │
├─────────────────────────────────────────────────────────┤
│  Skill ─ Markdown 驱动的技能定义与动态加载                  │
└─────────────────────────────────────────────────────────┘
```

---

## 核心特性

1. **全栈 Agent 能力**：从模型接入到流式处理的完整解决方案
2. **模块化设计**：各组件职责清晰，易于扩展和维护
3. **安全机制**：危险命令拦截、代码沙箱隔离
4. **可扩展性**：支持动态工具注册、技能加载、工作流编排
5. **多模型支持**：支持 OpenAI 和 Anthropic 等主流模型接口（兼容 Ollama 等 OpenAI 兼容端点）
6. **多模态支持**：图片、音频、视频、文件的输入输出，工具结果多模态返回
7. **高性能**：基于协程池的工作流引擎，支持并行节点执行
8. **统一 Aspect 切面系统**：重试、超时、熔断、panic 捕获，每层独立可取消 context

---

## 模块说明

### agent —— Agent 核心

**位置**: `components/agent/`

Agent 是所有能力的调度中心，负责多轮对话循环、工具调用编排、Token 用量追踪和记忆管理。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Agent` | Agent 主体，封装 `BaseModel` + `ToolRegistry` + `MemoryController` |
| `Interface` | 统一接口：`SendMessage()` 非流式 / `SendMessageStream()` 流式 |
| `UsageTracker` | Token 使用追踪器，支持多模型定价、预算控制、JSON 导出 |

```go
ag := agent.NewAgent(model, registry,
    agent.WithSessionID("session-001"),
    agent.WithUsageTracker(tracker),
    agent.WithMaxToolRounds(5),
    agent.WithMaxRetries(3),              // API 瞬态错误重试次数
    agent.WithRetryBackoffBase(time.Second), // 指数退避基数
)
resp, err := ag.SendMessage(ctx, schema.UserMessage("你好"))
```

---

### chatmodel —— 模型抽象

**位置**: `components/chatmodel/`

定义模型统一接口并提供 OpenAI 和 Anthropic 实现。

**核心接口**:

```go
type BaseModel interface {
    Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error)
    Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error)
}
```

| 实现 | 位置 | 说明 |
|------|------|------|
| `openai.ChatModel` | `components/chatmodel/openai/` | OpenAI 兼容 API（支持 Ollama、vLLM 等兼容端点） |
| `anthropic.ChatModel` | `components/chatmodel/anthropic/` | Anthropic Messages API |
| `MockModel` | `components/chatmodel/mock/` | 可编程响应的测试模拟模型 |

---

### schema —— 消息与工具定义

**位置**: `components/schema/`

定义框架中通行的消息、工具调用、流式读取数据结构。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Message` | 统一消息结构，支持多模态 ContentParts、OutputImages、OutputAudio |
| `ContentPart` | 多模态内容片段（text/image_url/input_audio/video_url/file_url/inline_data） |
| `ToolCall` / `FunctionCall` | 工具调用结构体 |
| `ToolResult` | 工具执行结果，支持多模态 ContentParts 返回 |
| `ToolResultContent` | 工具返回多模态结果时使用的结构体 |
| `StreamReader` | 流式消息读取器 |
| `Tool` | 工具定义：`Name` + `Description` + `Parameters`（JSON Schema） |

---

### tools —— 工具注册与执行

**位置**: `components/tools/`

提供动态工具注册中心和丰富的内置工具，支持生命周期钩子和权限分级。

**ToolRegistry（工具注册中心）**:

- 动态注册/注销/更新工具
- `RegisterSimple()` 简化注册，`Register()` 完整注册
- 批量并行执行（`ExecuteBatch`）
- 4 阶段生命周期钩子（beforeExecute / afterExecute / onRegister / onUnregister）

**权限分级**:

| 级别 | 说明 |
|------|------|
| `PermReadOnly` | 无副作用（file_read、file_list） |
| `PermReadWrite` | 可能修改状态（file_write、file_edit） |
| `PermDangerous` | 可能破坏系统（command_exec） |

**内置工具**:

| 工具 | 说明 |
|------|------|
| `file_read` | 读取文件内容（≤10MB） |
| `file_write` | 写入文件，自动创建父目录 |
| `file_list` | 列出目录内容 |
| `file_edit` | 精确字符串替换（增量修改文件） |
| `file_search` | 文件名 glob 匹配 + 内容关键词搜索 |
| `command_exec` | 执行 shell 命令，危险命令自动拦截 |
| `web_fetch` | HTTP 抓取网页内容 |
| `web_browse` | chromium 浏览器自动化（截图、点击、输入） |
| `user_config` | 用户配置管理（SQLite） |
| `get_work_dir` | 获取当前工作目录 |

**一键注册**:

```go
registry := tools.NewToolRegistry()
registered := tools.RegisterAll(registry)
// 自动检测 Chrome 环境，不可用时跳过 web_browse
```

`RegisterAll()` 自动检测环境：
- 文件/命令/环境工具：始终注册
- `web_browse`：检查系统是否有 Chrome/Chromium，没有则跳过
- `RegisterAllWithBrowser()`：强制注册所有工具（Chrome 首次使用时自动下载）

---

### memory —— 记忆管理

**位置**: `components/memory/`

提供三层记忆架构：系统提示词（永久）、短期记忆（滑动窗口）、长期记忆（持久化 + 向量检索）。

**架构设计**:

长期记忆采用 **Store + Retriever 分离架构**，通过 `CompositeLongTermStore` 组合：

```
┌─────────────────────────────────────────────────────┐
│              CompositeLongTermStore                  │
│  ┌──────────┐    hooks    ┌──────────────┐          │
│  │  Store    │ ──────────→│  Retriever    │          │
│  │(存储层)    │  onSave    │(检索层)       │          │
│  └──────────┘  onClear   └──────────────┘          │
│       │                         │                   │
│       └───── Indexer ───────────┘                   │
│            (索引管理)                                │
└─────────────────────────────────────────────────────┘
```

**Store 实现**（数据持久化）：

| 实现 | 位置 | 说明 |
|------|------|------|
| `gorm.GORMStore` | `memory/gorm/` | SQLite 持久化，支持 embedding 生成 + 分块 |
| `redis.Store` | `memory/redis/` | Redis Hash 存储 |
| `milvus.Store` | `memory/milvus/` | Milvus collection 存储，支持认证（Username/Password/APIKey/TLS） |

**Retriever 实现**（向量/关键词检索）：

| 实现 | 位置 | 说明 |
|------|------|------|
| `gorm.HNSWRetriever` | `memory/gorm/` | 内存 HNSW 向量索引，4 种召回模式（Auto/Vector/Hybrid/Combined） |
| `redis.Retriever` | `memory/redis/` | RediSearch 向量 + 关键词检索（需 Redis Stack） |
| `milvus.Retriever` | `memory/milvus/` | Milvus 原生向量检索，支持认证 |

**Embedder**（文本转向量）：

| 实现 | 位置 | 说明 |
|------|------|------|
| `embedder.NewOpenAI` | `memory/embedder/` | OpenAI/vLLM/LocalAI 兼容 |
| `embedder.NewOllama` | `memory/embedder/` | Ollama 原生 /api/embeddings |

**组合示例**:

```go
// GORM + HNSW（本地开发，全内存向量索引）
store, _ := gorm.NewGORMStore(gormCfg, embeddingFunc)
retriever := gorm.NewHNSWRetriever(store.GetDB(), embeddingFunc, gormCfg)
composite := memory.NewCompositeLongTermStore(store, retriever)
composite.AttachIndexer(retriever)

// Redis + RediSearch（分布式一体化）
store, _ := redis.NewStore(redisCfg)
retriever, _ := redis.NewRetrieverFromStore(store, retCfg, embeddingFunc)
composite := memory.NewCompositeLongTermStore(store, retriever)

// GORM + Milvus（SQLite 存储 + Milvus 检索）
store, _ := gorm.NewGORMStore(gormCfg, embeddingFunc)
retriever, _ := milvus.NewRetriever(milvusCfg, embeddingFunc)
composite := memory.NewCompositeLongTermStore(store, retriever)

// Milvus 一体化
store, _ := milvus.NewStore(milvusCfg)
retriever, _ := milvus.NewRetrieverFromStore(store, embeddingFunc)
composite := memory.NewCompositeLongTermStore(store, retriever)

controller := memory.NewController(systemPrompts, shortMemory,
    memory.WithLongStore(composite),
    memory.WithTopK(3),
)
```

**Hook 通知机制**:

Store 的 Save/Clear 操作自动触发 Hook，通知 Retriever 更新索引：

```go
composite.AddHook(func(event memory.StoreEvent) {
    switch event.Type {
    case memory.StoreEventSave:
        // 新消息保存，更新向量索引
    case memory.StoreEventClear:
        // 会话清空，移除索引
    }
})
```

**短期记忆 - 窗口策略**:

- `window.Manager`: 按消息数量 / Token 数双重限制截断
- `window.CalibratedEstimator`: 基于模型实际返回的 prompt_tokens 动态校准估算比例
- `window.Config.ReserveTokens`: 设置预留 Token 数，自动计算 `MaxHistoryTokens`

**集成测试**:

```bash
# 启动 Docker 容器
docker compose -f components/memory/docker-compose.yml up -d

# 运行全部记忆测试（含集成测试）
go test ./components/memory/... -tags integration -v

# 测试矩阵覆盖：
# GORM+HNSW | GORM+RediSearch | Redis+RediSearch
# GORM+Milvus | Milvus+Milvus | Redis+Milvus
```

---

### mcp —— MCP 协议客户端

**位置**: `components/mcp/`

实现 MCP (Model Context Protocol) 客户端，通过 JSON-RPC 2.0 over stdio 与外部 MCP 服务器通信。

| 组件 | 说明 |
|------|------|
| `Client` | MCP 客户端，实现 initialize → tools/list → tools/call 协议流程 |
| `Manager` | 多 MCP 服务器管理器，自动注册/注销工具到 ToolRegistry |
| `ConfigFile` | 配置文件加载（支持 `${VAR}` 环境变量替换） |

---

### sandbox —— 代码执行沙箱

**位置**: `components/sandbox/`

纯子进程执行器：临时目录隔离 + 超时 + 输出限制 + 环境变量过滤。语言配置由调用方决定。

**核心接口**:

```go
type Sandbox interface {
    Execute(ctx context.Context, req ExecRequest) (*ExecResult, error)
    Close() error
}
```

**ExecRequest**:

| 字段 | 说明 |
|------|------|
| `Command` | 要执行的命令（如 `python3`、`node`） |
| `Args` | 命令参数 |
| `Timeout` | 超时（默认 30s） |
| `Files` | 执行前注入的文件 |
| `WorkDir` | 工作目录（空 = 临时目录） |
| `Env` | 额外环境变量 |

**语言配置示例**:

```go
// 调用方决定支持哪些语言
langs := map[string]sandbox.LangDef{
    "python": {Command: "python3", Ext: ".py"},
    "node":   {Command: "node", Ext: ".js"},
    "go":     {Command: "go", Args: []string{"run"}, Ext: ".go"},
    "shell":  {Command: "sh", Args: []string{"-c"}},
}

sb := sandbox.NewProcessSandbox(sandbox.ProcessConfig{})
sandbox.RegisterSandboxTools(registry, sb, langs)
```

**安全特性**:

- 环境变量三种模式：黑名单（默认）/ 白名单 / 透传
- 输入文件路径穿越校验
- 超时控制 + 输出截断

---

### flowchart —— 工作流编排与 Agent 规划

**位置**: `components/flowchart/`

基于声明式依赖的工作流引擎，支持并行节点执行、AOP 切面和 Agent 规划。

**核心组件**:

| 包 | 说明 |
|------|------|
| `flowchart/` | Workflow 引擎、工作流级错误 |
| `flowchart/flow/` | FlowContext、DataSlot、SafeMap — 数据传递基础设施 |
| `flowchart/node/` | Node 接口、功能节点、Aspect 切面系统、Loop 类型 |
| `flowchart/agent/` | AgentLoop（ReAct 循环）、PlanExecutor（规划-执行策略） |

**Node 接口**:

```go
type Node interface {
    ID() string
    Inputs() []string
    Outputs() []string
    Run(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error)
    Aspects() []Aspect
}
```

**内置功能节点**:

| 节点 | 说明 |
|------|------|
| `NewConditionNode` | 条件判断节点 |
| `NewLoopNode` | 循环节点（while 模式），支持超时/取消/最大迭代 |
| `NewParallelNode` | 并行汇聚节点 |
| `NewLLMStreamNode` | 流式 LLM 节点（Fork 出多个 StreamReader） |

**统一 Aspect 切面系统**:

所有切面实现统一接口，per-layer 独立可取消 context：

```go
type Aspect interface {
    Around(ctx *AspectContext, node Node, next func() (map[string]any, error)) (map[string]any, error)
}
```

| 切面 | 说明 |
|------|------|
| `RetryAspect` | 自动重试 |
| `TimeoutAspect` | 超时控制（创建带超时的子 context，超时自动取消 WaitAll） |
| `CircuitBreakerAspect` | 熔断降级（Closed → Open → HalfOpen） |
| `RecoveryAspect` | panic 捕获与兜底 |
| `ErrorSwallowAspect` | 错误吞没，防止传播到工作流层 |

便捷构造器：`BeforeFunc(fn)` / `AfterFunc(fn)` / `AroundFunc(fn)`

**Agent 规划**（`flowchart/agent/`）:

| 引擎 | 说明 |
|------|------|
| `AgentLoop` | ReAct 循环：思考 → 工具调用 → 观察 → 循环 |
| `PlanExecutor` | Plan-and-Execute：LLM 规划 → 验证依赖 → 逐步执行 → 重规划 |

```go
// ReAct 循环
loop := agent.NewAgentLoop(model, registry, agent.WithMaxRounds(10))
result, _ := loop.Run(ctx, "帮我分析这个数据集")

// Plan-and-Execute
executor := agent.NewPlanExecutor(model, registry)
result, _ := executor.Run(ctx, "生成一份销售报告")
```

**工作流执行流程**:

1. 用户添加节点 → 节点通过 `Inputs()/Outputs()` 声明依赖
2. 所有节点提交到协程池，每个节点启动后 `WaitAll(inputs...)` 阻塞等待
3. 数据就绪 → 构建切面链 → 执行节点 → 输出写入 FlowContext
4. 下游节点自动被唤醒

```go
wf, _ := flowchart.NewWorkflow(ctx, 10)
wf.AddNode(node.NewNode("step1", []string{"input"}, []string{"mid"}, runFunc))
wf.AddNode(node.NewNode("step2", []string{"mid"}, []string{"output"}, runFunc))
wf.Run(map[string]any{"input": "hello"})
```

---

### skill —— Agent Skill 管理

**位置**: `components/skill/`

基于 Markdown frontmatter 的技能定义系统，支持 Skill 作为 Agent 可调用工具的自动注册与动态加载。

```go
loader := skill.NewSkillLoader(registry, toolRegistry)
loader.LoadFromDir("./skills")
```

---

### stream —— 流式消息多播

**位置**: `components/stream/`

提供流式消息的一对多广播能力。

- `Fork(n)` 创建 N 个独立缓冲的子流
- 慢消费者不影响其他消费者（独立缓冲区）
- 广播超时保护（默认 5s）

---

## 安装与依赖

**Go 版本**: 1.25.0+

```bash
go get github.com/Luo-root/pulse
```

**核心依赖**:

| 依赖 | 用途 |
|------|------|
| `gorm.io/gorm` + `github.com/glebarez/sqlite` | GORM 长期记忆持久化 |
| `github.com/coder/hnsw` | HNSW 向量索引 |
| `github.com/redis/go-redis/v9` | Redis 存储 + RediSearch 检索 |
| `github.com/milvus-io/milvus-sdk-go/v2` | Milvus 向量数据库 |
| `github.com/panjf2000/ants/v2` | 工作流协程池 |
| `github.com/google/uuid` | 消息 ID 生成 |
| `gopkg.in/yaml.v3` | Skill YAML 解析 |
| `github.com/chromedp/chromedp` | 浏览器自动化工具 |
| `golang.org/x/net` | HTML 解析 |

---

## 快速开始

### 1. 创建 Agent

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
```

### 2. 使用 ReAct 循环

```go
loop := flowagent.NewAgentLoop(model, registry,
    flowagent.WithMaxRounds(10),
    flowagent.WithSystemPrompt("你是一个编程助手"),
)
result, _ := loop.Run(ctx, "帮我写一个 HTTP 服务器")
fmt.Println(result.Answer)
```

### 3. 使用工作流

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

---

## 项目结构

```
pulse/
├── pulse.go                          # 包入口
├── go.mod / go.sum
├── AGENTS.md                         # Agent 指令文件
├── LICENSE                           # Apache 2.0
└── components/
    ├── agent/                        # Agent 核心
    │   ├── agent.go                  # Agent 主体 + 工具调用循环
    │   └── usage_tracker.go          # Token 使用追踪
    ├── chatmodel/                    # 模型抽象层
    │   ├── base_model.go             # BaseModel 接口
    │   ├── mock/                     # 测试用 Mock 模型
    │   ├── openai/                   # OpenAI 兼容实现
    │   └── anthropic/                # Anthropic 实现
    ├── schema/                       # 数据结构定义
    │   ├── message.go                # Message / ContentPart / StreamReader
    │   └── tool.go                   # Tool 定义
    ├── tools/                        # 工具系统
    │   ├── registry.go               # ToolRegistry + 生命周期钩子
    │   ├── file_ops.go               # file_read / file_write / file_list
    │   ├── file_edit.go              # file_edit / file_search
    │   ├── command.go                # command_exec + 危险命令拦截
    │   ├── web.go                    # web_fetch / web_browse
    │   ├── user_config.go            # 用户配置管理
    │   ├── env.go                    # get_work_dir
    │   ├── chromium.go               # ChromiumManager
    │   ├── html_parser.go            # HTML 解析
    │   ├── prompt_loader.go          # AGENTS.md / CLAUDE.md 加载器
    │   └── options.go                # ToolOption 函数式选项
    ├── memory/                       # 记忆管理
    │   ├── controller.go             # Controller + functional options
    │   ├── short_memory_manager.go   # 短期记忆接口
    │   ├── long_term_store.go        # Store + Retriever + Indexer + CompositeLongTermStore
    │   ├── embedder/                 # Embedder 接口 + OpenAI/Ollama 实现
    │   ├── window/                   # 窗口管理器 + CalibratedEstimator
    │   ├── gorm/                     # GORMStore + HNSWRetriever
    │   ├── redis/                    # RedisStore + RediSearchRetriever
    │   ├── milvus/                   # MilvusStore + MilvusRetriever
    │   ├── memnetai/                 # MemNetAI 长期记忆
    │   └── integration/              # 跨后端组合集成测试
    ├── mcp/                          # MCP 协议客户端
    │   ├── client.go                 # JSON-RPC 2.0 客户端
    │   ├── transport.go              # Stdio 传输层
    │   ├── manager.go                # 多服务器管理器
    │   └── config.go                 # 配置文件加载
    ├── sandbox/                      # 代码沙箱
    │   ├── sandbox.go                # Sandbox 接口 + ExecRequest + 配置
    │   ├── tools.go                  # 沙箱工具注册 + 语言配置（LangDef）
    │   ├── process_unix.go           # Unix 进程管理
    │   └── process_windows.go        # Windows 进程管理
    ├── flowchart/                    # 工作流引擎
    │   ├── workflow.go               # Workflow 引擎（协程池 + 切面链）
    │   ├── error.go                  # 工作流级错误
    │   ├── flow/                     # FlowContext / DataSlot / SafeMap
    │   ├── node/                     # Node 接口 + 功能节点 + Aspect
    │   └── agent/                    # AgentLoop (ReAct) + PlanExecutor
    ├── skill/                        # Skill 管理
    │   ├── skill.go                  # Skill 定义
    │   ├── loader.go                 # 加载器
    │   └── registry.go               # Skill 注册表
    └── stream/                       # 流式处理
        ├── stream_writer.go          # StreamWriter
        └── stream_multicast.go       # MulticastController
```

---

## 实践指南

### 工作流节点间流式传递数据

FlowContext 的 `Set` 方法接受 `any` 类型，可以直接传递 `*schema.StreamReader` 实现节点间流式数据传输：

```go
// 生产者节点：创建流，写入 FlowContext
wf.AddNode(node.NewNode("llm_call", nil, []string{"llm_stream"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
    reader, _ := model.Stream(ctx.GetContext(), messages)
    ctx.Set("llm_stream", reader)
    return nil, nil
}))

// 消费者节点：从 FlowContext 获取流，逐 chunk 处理
wf.AddNode(node.NewNode("process", []string{"llm_stream"}, []string{"result"}, func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
    reader := inputs["llm_stream"].(*schema.StreamReader)
    var fullText string
    for {
        chunk, err := reader.Recv()
        if err == io.EOF { break }
        fullText += chunk.Content
    }
    return map[string]any{"result": fullText}, nil
}))
```

一个流可以被多个消费者读取 — 配合 `stream.MulticastController.Fork(n)` 实现一对多广播。

### 工作流切面与超时控制

切面按注册顺序从外到内形成洋葱链，每层拥有独立的可取消 context：

```go
wf.AddAspect(node.NewTimeoutAspect(30 * time.Second))  // 外层：整体超时
wf.AddAspect(node.NewRetryAspect(3, time.Second))       // 内层：重试

// 或用函数式切面
wf.AddAspect(node.AroundFunc(func(ctx *node.AspectContext, n node.Node, next func() (map[string]any, error)) (map[string]any, error) {
    log.Printf("开始执行: %s", n.ID())
    result, err := next()
    log.Printf("执行完成: %s, err=%v", n.ID(), err)
    return result, err
}))
```

TimeoutAspect 超时时会取消本层 context，`FlowContext.WaitAllWithContext` 能立即感知到取消信号，节点的阻塞等待会被打断。

### Agent ReAct 循环

单 Agent 自主推理-执行模式，每步思考后决定调用工具还是给出最终回答：

```go
loop := flowagent.NewAgentLoop(model, registry,
    flowagent.WithMaxRounds(10),
    flowagent.WithSystemPrompt("你是一个编程助手"),
    flowagent.WithStepCallback(func(step *flowagent.Step) {
        log.Printf("Round %d: %d tool calls", step.Round, len(step.ToolCalls))
    }),
)

result, _ := loop.Run(ctx, "帮我分析 sales.csv 的趋势")
fmt.Println(result.Answer)
fmt.Printf("共 %d 轮，消耗 %d tokens\n", result.Rounds, result.Usage.TotalTokens)
```

### Plan-and-Execute 规划执行

先让 LLM 拆解任务为步骤，再逐步执行（同批次无依赖的步骤并行执行）：

```go
executor := flowagent.NewPlanExecutor(model, registry,
    flowagent.WithPlanMaxSteps(8),
    flowagent.WithPlanMaxReplans(2),
    flowagent.WithPlanMaxPlanRounds(5),      // 规划阶段最大轮数
    flowagent.WithPlanMaxRoundsPerStep(10),  // 每步执行最大轮数
    flowagent.WithPlanPersistPath("./plan.gob"), // 断点持久化
    flowagent.WithPlanCallback(func(steps []flowagent.PlanStep) {
        for _, s := range steps {
            log.Printf("计划步骤: %s - %s", s.ID, s.Description)
        }
    }),
    flowagent.WithPlanStepCallback(func(step *flowagent.ExecStep) {
        log.Printf("步骤 %s: %s", step.PlanStep.ID, step.Status)
    }),
)

result, _ := executor.Run(ctx, "分析销售数据并生成季度报告")
```

自动处理：依赖排序 → 验证无环 → 按批次并行执行 → 失败重规划 → 断点持久化（可选）。

### 工具返回多模态结果

工具可以通过返回 `*schema.ToolResultContent` 向 Agent 传递图片、音频等非文本数据：

```go
registry.Register(tools.ToolMetadata{
    Name: "screenshot",
    // ...
}, func(ctx context.Context, args map[string]any) (any, error) {
    imageData := captureScreenshot()
    return &schema.ToolResultContent{
        Content: "截图完成",
        ContentParts: []schema.ContentPart{
            schema.TextPart("截图完成"),
            schema.ImagePartBase64("image/png", base64.StdEncoding.EncodeToString(imageData)),
        },
    }, nil
})
```

Agent 的 `web_browse` 工具已内置此能力 — 截图自动以 base64 ContentPart 返回给模型。

### 校准 Token 估算

窗口管理器的 Token 估算默认基于 `1 token ≈ 1.8 runes` 的固定比例，可以通过 `CalibratedEstimator` 用模型实际返回的 prompt_tokens 动态校准：

```go
ce := window.NewCalibratedEstimator()
wm := window.NewManager(config, model, ce)

// Agent 每次模型调用后喂入校准数据
// 估算 3 轮后自动启用校准，ratio 限制在 0.3~3.0
ce.Feed(estimatedTokens, actualPromptTokens)
```

### 自定义指令文件加载

`PromptLoader` 支持加载业界标准的 Agent 指令文件：

```go
loader := tools.NewPromptLoader(".")

// 加载所有可用指令文件
content, loaded, _ := loader.LoadWithDefaults()
// loaded = ["AGENTS.md", "CLAUDE.md", ".cursor/rules/"]

// 或单独加载
agentsContent, _ := loader.LoadAGENTS()
```

---

## 许可证

本项目采用 [Apache License 2.0](LICENSE) 许可证。
