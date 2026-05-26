# Pulse

[![Go Version](https://img.shields.io/badge/Go-1.25.0-blue.svg)](https://go.dev/)
[![License](https://img.shields.io/badge/License-Apache%202.0-green.svg)](LICENSE)

**Pulse** 是一个用 Go 语言编写的 AI Agent 框架，提供从模型接入、记忆管理、工具调用、MCP 协议通信、工作流编排、代码沙箱到流式处理的全栈能力，帮助开发者快速构建可扩展的智能 Agent 应用。

---

## 目录

- [架构概览](#架构概览)
- [核心特性](#核心特性)
- [模块说明](#模块说明)
  - [agent —— Agent 核心](#agent--agent-核心)
  - [chatmodel —— 模型抽象](#chatmodel--模型抽象)
  - [schema —— 消息与工具定义](#schema--消息与工具定义)
  - [tools —— 工具注册与执行](#tools--工具注册与执行)
  - [memory —— 记忆管理](#memory--记忆管理)
  - [mcp —— MCP 协议客户端](#mcp--mcp-协议客户端)
  - [sandbox —— 代码执行沙箱](#sandbox--代码执行沙箱)
  - [flowchart —— 工作流编排](#flowchart--工作流编排)
  - [skill —— Agent Skill 管理](#skill--agent-skill-管理)
  - [stream —— 流式消息多播](#stream--流式消息多播)
  - [flow —— 流式数据上下文](#flow--流式数据上下文)
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
│  Flowchart (Workflow) ─ DAG + 拓扑并行                   │
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
3. **安全机制**：包含路径安全校验、危险命令拦截、代码沙箱隔离等
4. **可扩展性**：支持动态工具注册、技能加载、工作流编排
5. **多模型支持**：支持 OpenAI 和 Anthropic 等主流模型接口
6. **高性能**：基于协程池的工作流引擎，支持并行节点执行

---

## 模块说明

### agent —— Agent 核心

**位置**: `components/agent/`

Agent 是所有能力的调度中心，负责多轮对话循环、工具调用编排、Token 用量追踪和记忆管理。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Agent` | Agent 主体，封装 `BaseModel` + `ToolRegistry` + `MemoryController` |
| `AgentInterface` | 统一接口：`Send()` 非流式 / `SendStream()` 流式 |
| `UsageTracker` | Token 使用追踪器，支持多模型定价、预算控制、JSON 导出 |
| `UsageRecord` / `UsageStats` | Token 消耗记录与汇总统计 |

**关键机制**:

- **工具调用循环**: 每个 `Send` / `SendStream` 调用最多执行 `DefaultMaxToolRounds`（默认 10）轮工具调用
- **默认内存控制器**: 自动创建 200 条消息上限 + 8000 Token 预留的滑动窗口
- **系统提示词模板**: 自动注入工作目录约束和工具调用规则

```go
ag := agent.NewAgent(model, toolRegistry,
    agent.WithSessionID("session-001"),
    agent.WithUsageTracker(tracker),
    agent.WithMaxToolRounds(5),
)
resp, err := ag.Send(ctx, "你好")
```

---

### chatmodel —— 模型抽象

**位置**: `components/chatmodel/`

定义模型统一接口并提供 OpenAI 和 Anthropic 实现，附带测试用 Mock 模型。

**核心接口**:

```go
type BaseModel interface {
    Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error)
    Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error)
}
```

**实现**:

| 实现 | 位置 | 说明 |
|------|------|------|
| `openai.ChatModel` | `components/chatmodel/openai/` | OpenAI 兼容 API（支持任意兼容端点） |
| `anthropic.ChatModel` | `components/chatmodel/anthropic/` | Anthropic Messages API |
| `MockModel` | `components/chatmodel/mock_model.go` | 可编程响应的测试模拟模型 |

**MockModel 特性**:

- 预设响应队列（`MockResponse` 支持文本、推理、工具调用、延迟、错误）
- 循环/非循环模式、自定义 `generateFunc` / `streamFunc`
- 记录所有输入消息供断言验证
- 预置场景：天气助手、回声模型、流式模型

---

### schema —— 消息与工具定义

**位置**: `components/schema/`

定义框架中通行的消息、工具调用、流式读取数据结构。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Message` | 统一消息结构：`Role`、`Content`、`ReasoningContent`、`ToolCalls`、`ToolCallID`、`Usage` |
| `ToolCall` / `FunctionCall` | 工具调用结构体 |
| `ToolResult` | 工具执行结果（含错误标记） |
| `StreamReader` | 流式消息读取器（`Send` / `Recv` 模式） |
| `Tool` | 工具定义：`Name` + `Description` + `Parameters`（JSON Schema） |

**辅助能力**:

- `Clone()` 深拷贝消息
- `SystemMessage()` / `UserMessage()` / `AssistantMessage()` / `ToolResultsMessage()` 便捷构造
- `FormatMessages()` / `PrintMessages()` 可读化打印

---

### tools —— 工具注册与执行

**位置**: `components/tools/`

提供动态工具注册中心和丰富的内置工具，支持生命周期钩子和权限分级。

**ToolRegistry（工具注册中心）**:

```go
type ToolRegistry struct {
    tools map[string]*RegisteredTool
    // 生命周期钩子
    beforeExecuteFunc []func(...)
    afterExecuteFunc  []func(...)
    onRegisterFunc    []func(...)
    onUnregisterFunc  []func(...)
}
```

- 动态注册/注销/更新工具
- 批量并行执行（`ExecuteBatch`）
- 按分类/权限/标签筛选
- 启用/禁用控制
- 4 阶段生命周期钩子

**权限分级**:

| 级别 | 常量 | 说明 |
|------|------|------|
| 只读 | `PermReadOnly` | 无副作用（file_read、file_list、web_search） |
| 读写 | `PermReadWrite` | 可能修改状态（file_write、user_config） |
| 危险 | `PermDangerous` | 可能破坏系统（command_exec） |

**内置工具**:

| 工具 | 说明 |
|------|------|
| `file_read` | 读取文件内容（≤10MB），路径安全校验 |
| `file_write` | 写入文件，自动创建父目录，路径安全校验 |
| `file_list` | 列出目录内容，返回文件/目录元信息 |
| `command_exec` | 执行系统命令，危险命令自动拦截（rm -f /、mkfs、dd、format 等） |
| `web_search` | 联网搜索（基于 Serper.dev），支持地区和语言参数 |
| `user_config` | 用户配置管理：偏好设置、运行规则读取与持久化（SQLite） |
| `get_work_dir` | 获取当前工作目录和操作系统信息 |
| `chromium` | 基于 Chrome DevTools Protocol 的浏览器自动化工具 |
| `html_parser` | HTML 解析工具，支持提取文本、链接、图片等信息 |

**路径安全（file.go）**: `safePath()` 函数确保所有文件操作严格限定在工作目录内，防止路径穿越攻击。

**动态加载器（loader.go / options.go）**: 支持运行时动态注册新工具，通过 `ToolOption` 模式配置权限、分类、超时、标签等属性。

---

### memory —— 记忆管理

**位置**: `components/memory/`

提供三层记忆架构：系统提示词（永久）、短期记忆（滑动窗口）、长期记忆（持久化 + 向量检索）。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Controller` | 记忆控制器，协调三类记忆的存储与上下文构建 |
| `ShortMemoryManager` | 短期记忆接口 |
| `SimpleWindowMemory` | 纯滑动窗口实现 |
| `WindowManager` | 对话窗口管理器，支持数量限制 + Token 限制 |
| `WindowConfig` | 窗口配置（消息数上限、Token 上限、可自动计算） |
| `LongTermStore` | 长期记忆存储接口 |
| `GormStore` | 基于 GORM + SQLite 的高级记忆存储 |
| `Embedder` / `EmbeddingFunc` | 文本向量化接口 |

**短期记忆 - 窗口策略**:

- `WindowManager.Truncate()`: 始终保留 System 消息；按消息数量 / Token 数双重限制；自动修复孤立的 ToolResult
- 自动窗口计算：设置 `ReserveTokens` + 模型实现 `ModelContextWindow` → 自动计算 `MaxHistoryTokens`
- 默认 Token 估算器：`1 token ≈ 1.8 个 rune`（中英文混合保守估算）

**长期记忆 - GormStore**:

- 基于 SQLite 持久化存储
- HNSW 向量索引实现高效语义搜索
- 4 种召回模式：Auto（自适应）、Vector（向量语义）、Hybrid（关键词+时间衰减）、Combined（组合权重）
- 长文本自动分块（`SplitText`）+ 自然边界断句
- 异步向量索引重建、安全 panic 捕获

```go
store, _ := memory.NewGormStore(&memory.GormStoreConfig{
    DBPath:     "./chat.db",
    RecallMode: memory.RecallModeAuto,
}, embedFunc)

controller := memory.NewController(
    systemPrompts,
    memory.NewSimpleWindowMemory(windowMgr),
    store,
)
```

---

### mcp —— MCP 协议客户端

**位置**: `components/mcp/`

实现 MCP (Model Context Protocol) 客户端，通过 JSON-RPC 2.0 over stdio 与外部 MCP 服务器通信。

**核心组件**:

| 组件 | 说明 |
|------|------|
| `Client` | MCP 客户端，实现 initialize → tools/list → tools/call 协议流程 |
| `Transport` | 传输层接口（`Connect` / `Send` / `Recv` / `Close`） |
| `StdioTransport` | 基于子进程 stdin/stdout 的传输层实现 |
| `Manager` | 多 MCP 服务器管理器，自动注册/注销工具到 ToolRegistry |
| `ConfigFile` | 配置文件加载（支持 `${VAR}` 环境变量替换） |

**MCP 工具自动注册**:

1. 连接 MCP 服务器 → 自动 `tools/list` 发现工具
2. 按 `prefix/工具名` 格式注册到 `ToolRegistry`
3. 工具调用自动转发到 MCP 服务器
4. 断开连接自动注销

```go
manager := mcp.NewManager(toolRegistry)
err := manager.ConnectFromConfig(ctx, "mcp_config.json")
```

---

### sandbox —— 代码执行沙箱

**位置**: `components/sandbox/`

提供安全的代码执行环境，支持 Python、Node.js、Go、Shell 多语言。

**核心接口**:

```go
type Sandbox interface {
    Execute(ctx context.Context, req ExecRequest) (*ExecResult, error)
    CheckLang(lang string) error
    ListLangs() []string
    Close() error
}
```

**ProcessSandbox 特性**:

- 临时工作目录隔离（`os.MkdirTemp`）
- 超时控制（默认 30s）
- 输出截断（默认 1MB）
- Windows 进程树管理（`CREATE_NEW_PROCESS_GROUP` + `taskkill /T`）
- 自动创建 `go.mod` 等初始化文件
- 支持代码执行前注入文件

**工具注册**: 通过 `RegisterSandboxTools()` 注册 `sandbox-execute_code` 和 `sandbox-run_command` 两个工具。

---

### flowchart —— 工作流编排

**位置**: `components/flowchart/`

基于声明式依赖的工作流引擎，支持并行节点执行和 AOP 切面。

**核心类型**:

| 类型 | 说明 |
|------|------|
| `Workflow` | 工作流引擎：基于 `FlowContext` 的声明式依赖工作流引擎 |
| `Node` (interface) | 节点接口：`ID()` / `Inputs()` / `Outputs()` / `Run()` / `Aspects()` |
| `SimpleNode` | 通用节点实现（函数式） |
| `TopologicalNode` | 拓扑排序节点包装器，自动按依赖分层并行执行 |

**内置功能节点（functional_node.go）**:

| 节点 | 说明 |
|------|------|
| `NewConditionNode` | 条件判断节点 |
| `NewLoopNode` | 循环节点（while 模式），支持超时/取消/最大迭代 |
| `NewParallelNode` | 并行汇聚节点 |
| `NewLLMStreamNode` | 流式 LLM 节点（Fork 出多个 StreamReader） |
| `NewTaskNode` | 任务节点，由 Agent 驱动执行 |
| `NewReActPlannerNode` | ReAct 规划节点，自动拆解目标为任务序列 |
| `ScheduleLoopNode` | 调度循环节点，监听失败任务自动重规划 |

**AOP 切面（node/）**:

| 切面 | 说明 |
|------|------|
| `Aspect` | 切面接口：`Before` / `After` |
| `Interceptor` | 拦截器接口：`Around`（洋葱模型调用链） |
| `RetryInterceptor` | 自动重试（支持最大尝试次数和延迟） |
| `TimeoutInterceptor` | 超时控制（goroutine + timer 模式） |
| `CircuitBreakerInterceptor` | 熔断降级（Closed → Open → HalfOpen 三态转换） |
| `RecoveryInterceptor` | Panic 捕获与兜底逻辑 |
| `ErrorSwallowInterceptor` | 作为外层拦截器使用，避免Error触发工作流层的 `ctx.Cancel` |

**工作流执行流程**:

1. 用户添加节点 → `buildDAG()` 从 `Inputs()/Outputs()` 构建生产者映射
2. 拓扑排序（Kahn 算法）检测循环依赖
3. 按层分组 → 层内并行执行（`ants.Pool` 协程池）
4. 层间串行 → 前一层输出写入 `FlowContext`，后一层 `WaitAll` 等待

```go
wf, _ := flowchart.NewWorkflow(ctx, 10)
wf.AddNode(node.NewNode("step1", []string{"input"}, []string{"mid"},
    func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
        return map[string]any{"mid": "processed"}, nil
    },
))
wf.AddNode(node.NewNode("step2", []string{"mid"}, []string{"output"},
    func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
        return map[string]any{"output": inputs["mid"]}, nil
    },
))
err := wf.Run(map[string]any{"input": "hello"})
```

---

### skill —— Agent Skill 管理

**位置**: `components/skill/`

基于 Markdown frontmatter 的技能定义系统，支持 Skill 作为 Agent 可调用工具的自动注册与动态加载。

**Skill 结构**:

```yaml
---
name: my-skill
description: 这是一个示例 Skill
allowed-tools: file_read file_list
parameters:
  type: object
  properties:
    path:
      type: string
timeout: 30
---
# Skill 正文（返回给 Agent 的指令内容）
```

**核心组件**:

| 组件 | 说明 |
|------|------|
| `Skill` | Skill 定义结构体（YAML + Markdown） |
| `SkillRegistry` | Skill 注册表（内存索引） |
| `SkillLoader` | Skill 加载器：从文件/字符串/目录加载并注册为工具 |
| `ParseSkillMarkdown()` | Frontmatter 解析器 |

**AllowedTools 机制**: Skill 可声明 `allowed-tools` 白名单，通过 `beforeExecute` 钩子限制该 Skill 只能调用指定的工具。

---

### stream —— 流式消息多播

**位置**: `components/stream/`

提供流式消息的一对多广播能力。

**核心组件**:

| 组件 | 说明 |
|------|------|
| `StreamWriter` | 流式消息写入器（与 `StreamReader` 配对使用） |
| `MulticastController` | 多播控制器：源流 Fork 为多个子流，独立背压隔离 |

**多播特性**:

- `Fork(n)` 创建 N 个独立缓冲的子流
- 慢消费者不影响其他消费者（独立缓冲区）
- 广播超时保护（默认 5s）
- 优雅关闭和错误传播

---

### flow —— 流式数据上下文

**位置**: `components/flow/`

提供工作流节点间数据传递的基础设施。

**核心组件**:

| 组件 | 说明 |
|------|------|
| `FlowContext` | 工作流上下文：数据槽管理 + 取消信号 + 错误传播 |
| `DataSlot` | 数据槽：支持 `SetOnce`（幂等）/ `SetOrUpdate`（覆盖）、`Wait`（阻塞等待）、`TryGet`（非阻塞） |
| `SafeMap[K, V]` | 泛型并发安全 Map（基于 `sync.Map`） |
| `LoopResult` / `LoopStatus` | 循环执行结果与状态 |

**DataSlot 机制**:

- `SetOnce(value)`: 首次写入，后续忽略（幂等）
- `SetOrUpdate(value)`: 始终覆盖 + 广播唤醒等待者
- `Get(ctx)`: 阻塞等待数据就绪，支持 context 取消
- `WaitForAny(keys...)`: 使用 `reflect.Select` 多路复用等待任意一个就绪

---

## 安装与依赖

**Go 版本**: 1.25.0+

```bash
go get github.com/Luo-root/pulse
```

**核心依赖**:

| 依赖 | 用途 |
|------|------|
| `gorm.io/gorm` + `github.com/glebarez/sqlite` | 长期记忆持久化 + 用户配置存储 |
| `github.com/coder/hnsw` | HNSW 向量索引（语义搜索） |
| `github.com/panjf2000/ants/v2` | 工作流协程池 |
| `github.com/google/uuid` | 消息 ID 生成 |
| `gopkg.in/yaml.v3` | Skill YAML 解析 |
| `github.com/chromedp/chromedp` | 浏览器自动化工具 |

---

## 快速开始

### 1. 创建 Agent

```go
package main

import (
    "context"
    "github.com/Luo-root/pulse/components/agent"
    "github.com/Luo-root/pulse/components/chatmodel/openai"
    "github.com/Luo-root/pulse/components/tools"
)

func main() {
    // 1) 工具注册
    registry := tools.NewToolRegistry()
    // 注册内置工具（根据实际API调整）
    // registry.Register(...)
    
    // 2) 模型
    model, _ := openai.NewChatModel(&openai.ChatModelConfig{
			BaseURL: m.config.API.BaseURL,
			APIKey:  m.config.API.APIKey,
			Model:   m.config.Model.ModelID,
			Thinking: openai.Thinking{
				Type: thinkingType,
			},
        	Tools: registry.GetEnabledTools(),
		})

    // 3) Agent
    ag := agent.NewAgent(model, registry)
    resp, _ := ag.Send(context.Background(), "列出当前目录文件")
    println(resp.Content)
}
```

### 2. 添加记忆

```go
store, _ := memory.NewGormStore(nil, nil)
controller := memory.NewController(nil,
    memory.NewSimpleWindowMemory(memory.NewWindowManager(
        memory.WindowConfig{MaxHistoryMessages: 100, ReserveTokens: 4000},
        model, nil,
    )),
    store,
)

ag := agent.NewAgent(model, registry, agent.WithMemoryController(controller))
```

### 3. 使用工作流

```go
wf, _ := flowchart.NewWorkflow(ctx, 10)
wf.AddNode(node.NewConditionNode("check", "data",
    func(v any) bool { return v != nil },
    "valid", "invalid",
))

// 创建节点并添加重试拦截器
testNode := node.NewNode("test", []string{"input"}, []string{"output"},
    func(ctx *flow.FlowContext, inputs map[string]any) (map[string]any, error) {
        return map[string]any{"output": "test"}, nil
    },
)
testNode.AddAspect(node.NewRetryInterceptor(3, time.Second))
wf.AddNode(testNode)
wf.Run(map[string]any{"data": someValue})
```

### 4. 使用浏览器自动化工具（需要先注册chromium工具）

```go
// 使用 chromium 工具进行网页抓取（假设chromium工具已注册）
// 首先需要确保chromium工具已注册到工具注册中心
resp, err := ag.Send(ctx, "使用浏览器打开 https://example.com 并提取页面标题")
```

---

## 项目结构

```
pulse/
├── pulse.go                          # 包入口
├── go.mod / go.sum                   # Go 模块定义
├── LICENSE                           # Apache 2.0
├── README.md                         # 项目文档
├── components/
│   ├── agent/                        # Agent 核心调度
│   │   ├── agent.go                  # Agent 主体 + 工具调用循环
│   │   └── usage_tracker.go          # Token 使用追踪
│   ├── chatmodel/                    # 模型抽象层
│   │   ├── base_model.go             # BaseModel 接口
│   │   ├── mock_model.go             # 测试用 Mock 模型
│   │   ├── openai/                   # OpenAI 兼容实现
│   │   └── anthropic/                # Anthropic 实现
│   ├── schema/                       # 数据结构定义
│   │   ├── message.go                # Message / ToolCall / StreamReader
│   │   └── tool.go                   # Tool 定义
│   ├── tools/                        # 工具系统
│   │   ├── registry.go               # 动态工具注册中心
│   │   ├── file.go                   # 文件操作工具 + 安全路径
│   │   ├── command.go                # 系统命令工具 + 危险命令拦截
│   │   ├── web.go                    # 联网搜索工具
│   │   ├── user_config.go            # 用户配置管理工具
│   │   ├── env.go                    # 环境信息工具
│   │   ├── chromium.go               # 浏览器自动化工具
│   │   ├── html_parser.go            # HTML 解析工具
│   │   ├── loader.go / options.go    # 动态工具加载
│   │   └── tools_registry.go         # RegisterAll 入口
│   ├── memory/                       # 记忆管理
│   │   ├── controller.go             # 记忆控制器
│   │   ├── short_memory_manager.go   # 短期记忆接口
│   │   ├── simple_window_memory.go   # 滑动窗口实现
│   │   ├── window.go                 # 窗口管理器（数量/Token 限制）
│   │   ├── long_term_store.go        # 长期记忆接口
│   │   ├── gorm_store.go             # GORM + HNSW 存储
│   │   ├── embedder.go               # 向量化接口
│   │   └── ollama_embedder.go        # Ollama 向量化实现
│   ├── mcp/                          # MCP 客户端
│   │   ├── client.go                 # JSON-RPC 2.0 客户端
│   │   ├── transport.go              # Stdio 传输层
│   │   ├── manager.go                # 多服务器管理器
│   │   ├── types.go                  # 协议类型定义
│   │   └── config.go                 # 配置文件加载
│   ├── sandbox/                      # 代码沙箱
│   │   ├── sandbox.go                # Sandbox 接口 + 配置
│   │   ├── process.go                # 子进程沙箱实现
│   │   └── tools.go                  # 沙箱工具注册
│   ├── flowchart/                    # 工作流引擎
│   │   ├── workflow.go               # 工作流主引擎（DAG + 拓扑分层）
│   │   └── node/
│   │       ├── node.go               # Node 接口 + SimpleNode
│   │       ├── planner.go            # Plan / Task 定义
│   │       ├── react_planner.go      # ReAct 规划器 + 重规划
│   │       ├── task_node.go          # 任务执行节点
│   │       ├── topological_node.go   # 拓扑排序节点
│   │       ├── functional_node.go    # 内置功能节点
│   │       ├── aspect.go             # Aspect 接口 + 简易实现
│   │       └── interceptor.go        # 拦截器（重试/超时/熔断/兜底）
│   ├── skill/                        # Skill 管理
│   │   ├── skill.go                  # Skill 定义
│   │   ├── loader.go                 # 加载器（文件/字符串/目录）
│   │   └── registry.go               # Skill 注册表
│   └── stream/                       # 流式处理
│       ├── stream_writer.go          # 流式写入器
│       └── stream_multicast.go       # 多播控制器
├── prompt/                           # 系统提示词文件
└── skills/                           # Skill 定义文件
    ├── code-summarizer/              # 代码分析技能
    ├── data-transformer/             # 数据转换技能
    ├── frontend-design/              # 前端设计技能
    ├── git-log-analyzer/             # Git 日志分析技能
    ├── pptx/                         # PPT 生成技能
    ├── system-info/                  # 系统信息技能
    └── web-researcher/               # 网页研究技能
```

---

## 许可证

本项目采用 [Apache License 2.0](LICENSE) 许可证。
