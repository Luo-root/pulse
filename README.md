# Pulse - Go 语言 AI Agent 框架

> 一个模块化、可扩展的 Go 语言 AI Agent 开发框架，支持多模型对话、工具调用、记忆管理、流式输出、工作流编排和技能系统。

## 📋 目录

- [项目概述](#项目概述)
- [架构设计](#架构设计)
- [核心组件](#核心组件)
  - [Schema（核心数据结构）](#1-schema核心数据结构)
  - [ChatModel（模型层）](#2-chatmodel模型层)
  - [Agent（智能体）](#3-agent智能体)
  - [Tool（工具系统）](#4-tool工具系统)
  - [Memory（记忆系统）](#5-memory记忆系统)
  - [Flowchart（工作流引擎）](#6-flowchart工作流引擎)
  - [Skill（技能系统）](#7-skill技能系统)
- [快速开始](#快速开始)
- [使用样例](#使用样例)
- [API 参考](#api-参考)
- [许可证](#许可证)

---

## 项目概述

**Pulse** 是一个面向 Go 开发者的 AI Agent 框架，旨在简化大语言模型（LLM）应用的开发流程。框架采用模块化设计，核心特性包括：

- 🧠 **多模型支持**：OpenAI 兼容接口（已完成）、Claude（已完成 Generate + Stream）
- 🤖 **智能体循环**：自动工具调用循环，支持流式/非流式两种模式
- 🔧 **工具系统**：文件操作、命令执行、联网搜索、用户配置等内置工具，支持动态扩展
- 💾 **记忆管理**：短期滑动窗口 + 长期 SQLite 存储 + 向量语义搜索 + 云端记忆
- 🌊 **流式输出**：完整的 SSE 流式接收与多播机制
- 🔄 **工作流编排**：基于 DAG 的异步工作流引擎，支持 ReAct 规划模式
- 🎯 **技能系统**：基于 Markdown 的 Skill 规范，支持 Go 代码动态编译和纯指令型两种
- 🛡️ **安全约束**：工作目录限制、危险命令拦截、路径安全检查
- 📊 **用量追踪**：内置 Token 使用量统计、成本计算和预算控制
- 🔌 **AOP 切面**：重试、超时、熔断、兜底四种拦截器，洋葱模型可组合

---

## 架构设计

```
pulse/
├── components/
│   ├── schema/          # 核心数据结构（Message、ToolRegistry、FlowContext、StreamReader 等）
│   ├── chatmodel/       # 模型层（OpenAI 兼容 + Claude，统一 BaseModel 接口）
│   │   ├── openai/      # OpenAI 兼容实现（支持 DeepSeek/Moonshot/Kimi 等）
│   │   └── claude/      # Anthropic Claude 实现（支持 Generate + Stream）
│   ├── agent/           # 智能体（对话循环、工具调用调度、用量追踪）
│   ├── tool/            # 工具系统（文件/命令/搜索/配置 + 动态加载 + Prompt 加载器）
│   ├── memory/          # 记忆系统（Controller + 短期窗口 + 长期 SQLite + 向量搜索 + 云端记忆）
│   ├── flowchart/       # 工作流引擎（DAG + ReAct 规划 + 条件/循环/并行/拓扑节点）
│   │   └── node/        # 节点实现（含 AOP 切面与拦截器）
│   └── skill/           # 技能系统（Markdown 解析 + Go 动态编译 + 指令型）
├── pulse.go             # 顶层包声明
├── go.mod
└── README.md
```

### 数据流

```
用户输入 → Agent（对话循环）
              ├─ BuildContext → Memory.Controller（SystemPrompt + 长期记忆召回 + 短期窗口截断）
              ├─ Generate/Stream → ChatModel（调用 LLM）
              ├─ 有 ToolCalls → ToolRegistry.ExecuteBatch（并发执行工具）
              └─ 无 ToolCalls → 返回最终回答
              
Flowchart（高级编排）：
  ReActPlanner → Plan → TopologicalNode（分层并发执行） → ScheduleLoop（失败重规划）
```

---

## 核心组件

### 1. Schema（核心数据结构）

Schema 层定义了框架中所有核心数据结构，是整个框架的基石。

#### Message 消息结构

```go
type Message struct {
    Role             RoleType     `json:"role"`               // system/user/assistant/tool
    Content          string       `json:"content"`            // 消息内容
    ReasoningContent string       `json:"reasoning_content"`  // 推理内容（Kimi k2.6 等支持）
    Name             string       `json:"name"`               // 发送者名称
    Partial          bool         `json:"partial"`            // 是否为未完成消息
    ToolCalls        []ToolCall   `json:"tool_calls"`         // 工具调用请求
    ToolResults      []ToolResult `json:"tool_results"`       // 工具执行结果
    Usage            *Usage       `json:"-"`                  // Token 使用量
}

type ToolCall struct {
    ID       string       `json:"id"`
    Type     string       `json:"type"`
    Index    int          `json:"index,omitempty"`
    Function FunctionCall `json:"function"`
}

type ToolResult struct {
    CallID  string `json:"call_id"`  // 对应 ToolCall.ID
    Content string `json:"content"`  // 结果内容
    IsError bool   `json:"is_error"` // 是否错误
}
```

**便捷构造函数：**

```go
msg := schema.SystemMessage("系统提示")
msg := schema.UserMessage("用户输入")
msg := schema.AssistantMessage("助手回复", "推理内容")
msgs := schema.ToolResultsMessage(results)  // 返回 []*Message，每个 ToolResult 一条
```

#### ToolRegistry 工具注册中心

`ToolRegistry` 是一个功能完整的动态工具注册中心，支持：

- 动态注册/注销/热更新工具
- 启用/禁用工具
- 按分类、权限、标签查询工具
- 批量并行执行（多 tool_call 并发）
- 生命周期钩子（BeforeExecute / AfterExecute / OnRegister / OnUnregister）
- 使用统计（UseCount）

```go
// 创建注册中心
registry := schema.NewToolRegistry()

// 注册工具
registry.MustRegister(schema.ToolMetadata{
    Name:        "my_tool",
    Description: "我的自定义工具",
    Parameters: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "param1": map[string]any{"type": "string"},
        },
        "required": []string{"param1"},
    },
    Permission: schema.PermReadOnly,
    Category:   "custom",
    Version:    "1.0.0",
    Tags:       []string{"safe"},
    Timeout:    30 * time.Second,
}, func(ctx context.Context, args map[string]any) (any, error) {
    return fmt.Sprintf("收到: %v", args["param1"]), nil
})

// 获取启用的工具（发给模型）
tools := registry.GetEnabledTools()

// 按维度查询
tools := registry.GetByCategory("file")
tools := registry.GetByPermission(schema.PermDangerous)
tools := registry.GetByTag("safe")

// 执行工具
result := registry.Execute(ctx, toolCall)

// 批量并发执行
results := registry.ExecuteBatch(ctx, toolCalls)

// 添加生命周期钩子（如审计日志）
registry.AddBeforeExecuteHook(func(ctx context.Context, name string, args map[string]any) error {
    log.Printf("[AUDIT] executing tool: %s", name)
    return nil
})
```

**权限分级：**

| 常量 | 说明 |
|------|------|
| `PermReadOnly` | 只读权限（安全，无副作用） |
| `PermReadWrite` | 读写权限（可能修改状态） |
| `PermDangerous` | 危险权限（可能破坏数据/系统） |

#### StreamReader 流式读取器

```go
// 创建流读取器
reader := schema.NewStreamReader()

// 接收流式消息（Go 标准流式风格）
for {
    msg, err := reader.Recv()
    if errors.Is(err, io.EOF) {
        break
    }
    print(msg.Content)
}

// 多播：1 个源流 → N 个独立流（背压隔离）
mc := schema.NewMulticastController(streamReader, 16)
readers := mc.Fork(3)  // 分成 3 个独立流，慢消费者不影响其他
```

#### FlowContext 工作流上下文

支持数据驱动、多协程等待、级联取消和首错传播：

```go
ctx := schema.NewFlowContext(context.Background())

ctx.Set("key", value)           // 首次设置（幂等）
ctx.SetOrUpdate("key", value)   // 覆盖更新

val, err := ctx.Wait("key")     // 阻塞等待
vals, err := ctx.WaitAll("k1", "k2") // 等待多个
val, ok := ctx.TryGet("key")    // 非阻塞获取

// 级联取消：任意节点失败 → 所有等待中的节点立即退出
ctx.Cancel(err)
```

---

### 2. ChatModel（模型层）

#### BaseModel 接口

```go
type BaseModel interface {
    Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error)
    Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error)
}
```

#### OpenAI 兼容模型

支持所有 OpenAI 兼容 API（DeepSeek / Moonshot / Kimi / OpenAI 等）：

```go
import "github.com/Luo-root/pulse/components/chatmodel/openai"

model, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
    BaseUrl: "https://api.moonshot.cn/v1/chat/completions",
    APIKey:  "your-api-key",
    Model:   "kimi-k2-0905-preview",
    Tools:   registry.GetEnabledTools(),

    // 可选参数
    MaxCompletionTokens: 4096,
    Temperature:         0.6,
    TopP:                1.0,
    Thinking: openai.Thinking{
        Type: openai.Disabled,  // 控制思考模式（enabled/disabled）
        Keep: openai.All,       // 保留历史 reasoning_content
    },
})

// 非流式
resp, err := model.Generate(ctx, messages)

// 流式
reader, err := model.Stream(ctx, messages)
```

**支持的模型提供商：**

| 提供商 | 状态 | 包路径 |
|--------|------|--------|
| OpenAI 兼容 | ✅ 已完成 | `components/chatmodel/openai` |
| Claude | ✅ 已完成 | `components/chatmodel/claude` |

---

### 3. Agent（智能体）

Agent 是对话循环的封装，自动处理工具调用循环、记忆管理和用量追踪。位于 `components/agent/` 包下。

```go
import "github.com/Luo-root/pulse/components/agent"

// 创建 Agent（自动注入系统提示 + 窗口管理器）
ag := agent.NewAgent(model, registry)

// 可选配置
ag := agent.NewAgent(model, registry,
    agent.WithMemoryController(mc),        // 自定义记忆控制器
    agent.WithUsageTracker(tracker),       // 用量追踪
)

// 非流式发送（自动处理工具调用循环）
resp, err := ag.Send(ctx, "帮我列出当前目录的文件")
fmt.Println("AI:", resp.Content)

// 流式发送（带实时回调，支持用户中断）
resp, err := ag.SendStream(ctx, "帮我列出当前目录的文件",
    func(msg *schema.Message, isToolCall bool) bool {
        if isToolCall {
            for _, tc := range msg.ToolCalls {
                fmt.Printf("\n🔧 %s\n", tc.Function.Name)
            }
        } else {
            print(msg.Content) // 实时打字机效果
        }
        return true // false=中断
    },
)

// 对话管理
ag.AddSystemMessage("你是一个编程助手")
ag.ClearAgentHistory(ctx)         // 清空历史（保留 system prompt）
history, _ := ag.GetHistory(ctx)  // 获取完整历史
rawMsgs := ag.GetRawMessages()    // 获取含 ToolCalls/ToolResults 的消息
```

**Agent 内置系统提示：**

- 工作目录约束（所有文件操作限制在当前工作目录）
- 工具调用规则（不确定时必须调用工具验证、禁止凭空回答）
- 行为约束（严格执行工具调用循环）

#### UsageTracker 用量追踪

```go
tracker := agent.NewUsageTracker()

// 配置预算
tracker.SetBudget(5.0)  // 5 美元预算

// 检查
if tracker.IsOverBudget() {
    fmt.Println("预算已用完")
}
remaining := tracker.GetRemainingBudget()

// 统计
stats := tracker.GetStats()
fmt.Printf("调用次数: %d, 总Token: %d, 费用: $%.4f\n",
    stats.TotalCalls, stats.TotalTokens, stats.TotalCost)

// 导出
data, _ := tracker.ExportJSON()
```

内置价格表：`deepseek-v4-flash`、`deepseek-v4-pro`、`kimi-k2.6`（可自定义）。

---

### 4. Tool（工具系统）

#### 内置工具

| 工具名 | 功能 | 参数 | 权限 |
|--------|------|------|------|
| `file_read` | 读取文件（最大 10MB） | `path` | 只读 |
| `file_write` | 写入文件（自动创建父目录） | `path`, `content` | 读写 |
| `file_list` | 列出目录内容 | `path`（可选） | 只读 |
| `command_exec` | 执行系统命令（带安全检查） | `command`, `timeout`, `cwd` | 危险 |
| `web_search` | 联网搜索（基于 Serper.dev） | `query`, `count`, `gl`, `hl` | 只读 |
| `user_config` | 用户配置管理 | `action`, `key`, `value` | 读写 |
| `get_work_dir` | 获取当前工作目录和 OS 信息 | 无 | 只读 |

#### 注册所有工具

```go
import tools "github.com/Luo-root/pulse/components/tools"

registry := schema.NewToolRegistry()
tools.RegisterAll(registry)
```

#### 动态工具加载（选项模式）

```go
loader := tools.NewDynamicToolLoader(registry)

loader.Load(
    "calculator",
    "执行数学计算",
    calculatorHandler,
    map[string]any{
        "type": "object",
        "properties": map[string]any{
            "expression": map[string]any{"type": "string"},
        },
    },
    tools.WithCategory("math"),
    tools.WithTimeout(5*time.Second),
    tools.WithTags("safe", "math"),
)
```

#### PromptLoader（外部 Prompt 加载）

```go
loader := tools.NewPromptLoader("./prompt")

// 加载运行规则/rules.md
rules, _ := loader.LoadRules()

// 加载系统提示词/system_prompt.md
prompt, _ := loader.LoadSystemPrompt()

// 加载安全策略/safety.md
safety, _ := loader.LoadSafetyRules()

// 一次性加载所有并替换变量
all, _ := loader.LoadAllDefaultPrompt()
```

#### 安全特性

- **路径限制**：所有文件操作必须在当前工作目录内
- **命令拦截**：禁止 `rm -rf`、`mkfs`、`dd`、`;`、`&&`、`||` 等危险操作
- **超时控制**：命令执行默认 30 秒超时
- **跨平台**：支持 Windows/Linux/macOS

---

### 5. Memory（记忆系统）

记忆系统采用三层架构：**SystemPrompt（永久） + 短期滑动窗口 + 长期持久化存储**，由 `Controller` 统一协调。

```
Controller
├── SystemPrompt     ← 永久保留在上下文最前面
├── ShortMemory      ← 短期窗口管理（滑动窗口 / 窗口+摘要）
│   ├── SimpleWindowMemory      纯滑动窗口（无摘要）
│   └── WindowShortMemory       窗口+摘要（超出部分自动摘要）
│       └── WindowManager       Token/数量双重限制 + 孤ToolResult自动清理
└── LongStore        ← 长期持久化（可选）
    ├── GormStore               SQLite + HNSW 向量索引 + 混合召回
    └── MemNetAIStore           云端记忆（立体认知重构）
```

#### Controller 记忆控制器

```go
// 使用默认配置（仅短期窗口）
mc := memory.NewController(systemPrompt, memory.NewSimpleWindowMemory(wm), nil)

// 或接入长期存储
mc := memory.NewController(systemPrompt, shortMemory, gormStore)

// 保存一轮对话
mc.SaveTurn(ctx, sessionID, msgs)

// 构建上下文（SystemPrompt + 长期记忆召回 + 短期窗口截断）
contextMsgs, _ := mc.BuildContext(ctx, sessionID, currentQuery)

// 清空会话
mc.Clear(ctx, sessionID)
```

#### WindowManager 窗口管理器

```go
wm := memory.NewWindowManager(memory.WindowConfig{
    MaxHistoryMessages: 200,  // 最大保留消息数
    MaxHistoryTokens:   0,    // Token 限制（0=不限制）
    ReserveTokens:      8000, // 自动模式：为输出预留 Token
}, model, nil)

// 截断消息（保留所有 System 消息，丢弃过旧消息）
truncated := wm.Truncate(messages)
```

#### GormStore（SQLite + 向量搜索）

```go
cfg := memory.DefaultGormStoreConfig()
cfg.DBPath = "./chat.db"
cfg.EmbeddingDimension = 768
cfg.RecallMode = memory.RecallModeCombined  // 向量+关键词+时间组合权重

// 嵌入函数（Ollama）
embed := memory.NewOllamaEmbedder("http://localhost:11434", "nomic-embed-text")

// 或 OpenAI 嵌入
embed := memory.NewOpenAIEmbedder("your-key", "https://api.openai.com", "text-embedding-3-small")

store, _ := memory.NewGormStore(cfg, func(ctx context.Context, text string) ([]float32, error) {
    return embed.Embed(ctx, text)
})

// 保存 / 召回 / 获取
store.Save(ctx, sessionID, messages)
memories, _ := store.Recall(ctx, sessionID, "天气", 3)
history, _ := store.GetSession(ctx, sessionID)
store.ClearSession(ctx, sessionID)
```

**召回模式：**

| 模式 | 说明 |
|------|------|
| `RecallModeAuto` | 自动：优先向量，失败回退混合 |
| `RecallModeVector` | 仅向量语义搜索 |
| `RecallModeHybrid` | 仅关键词 + 时间衰减 |
| `RecallModeCombined` | 向量 + 关键词 + 时间组合权重 |

---

### 6. Flowchart（工作流引擎）

基于 DAG 的异步工作流引擎，支持自动依赖等待、分层并发执行、AOP 切面和拦截器。

#### 基础工作流

```go
import (
    "github.com/Luo-root/pulse/components/flowchart"
    "github.com/Luo-root/pulse/components/flowchart/node"
)

// 创建工作流（最大 10 个并发协程）
wf, _ := flowchart.NewWorkflow(ctx, 10)

// 定义节点
inputNode := node.NewNode(
    "user_input",
    nil,                    // 无输入依赖
    []string{"prompt"},     // 输出 key
    func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
        return map[string]any{"prompt": "写一段Go语言介绍"}, nil
    },
)

llmNode := node.NewLLMStreamNode(
    "llm_stream",
    "prompt",               // 输入 key
    "stream_readers",       // 输出 key
    model,
    1,                      // StreamReader 多播份数
)

// 添加节点并运行
wf.AddNode(inputNode)
wf.AddNode(llmNode)
wf.Run(map[string]any{"user_goal": "目标描述"})
```

#### 节点类型

| 节点类型 | 函数 | 说明 |
|----------|------|------|
| 通用节点 | `node.NewNode()` | 自定义执行逻辑 |
| 条件节点 | `node.NewConditionNode()` | 条件分支（if-then-else） |
| 循环节点 | `node.NewLoopNode()` | while 循环，支持超时和最大迭代次数 |
| 并行节点 | `node.NewParallelNode()` | 等待多个输入就绪 |
| 流式 LLM | `node.NewLLMStreamNode()` | 流式模型调用 + 多播 |
| ReAct 规划 | `node.NewReActPlannerNode()` | AI 自主任务规划 |
| 任务节点 | `node.NewTaskNode()` | 执行规划任务（含状态追踪） |
| 拓扑节点 | `node.NewTopologicalNode()` | 按拓扑序分层并发执行 |
| 调度循环 | `node.ScheduleLoopNode()` | 监听任务失败 → 自动重规划 |

#### AOP 切面与拦截器（洋葱模型）

```go
// 传统切面
type AroundAspect struct {
    BeforeFn func(ctx *schema.FlowContext, node node.Node)
    AfterFn  func(ctx *schema.FlowContext, node node.Node, err error)
}

// 四种拦截器
retryInterceptor := node.NewRetryInterceptor(3, 1*time.Second)
timeoutInterceptor := node.NewTimeoutInterceptor(30 * time.Second)
cbInterceptor := node.NewCircuitBreakerInterceptor(5, 30*time.Second)
recoveryInterceptor := node.NewRecoveryInterceptor(panicFallback)

// 洋葱模型：recovery → cb → timeout → retry → 实际执行
// 反向包裹：数组后面的包在最外层
plannerNode.AddAspect(retryInterceptor)
plannerNode.AddAspect(timeoutInterceptor)
plannerNode.AddAspect(cbInterceptor)
plannerNode.AddAspect(recoveryInterceptor)
```

**拦截器说明：**

| 拦截器 | 行为 |
|--------|------|
| `RetryInterceptor` | 失败自动重试 N 次，可配置延迟和重试条件 |
| `TimeoutInterceptor` | 超时控制，超时后立即返回错误 |
| `CircuitBreakerInterceptor` | 连续失败 N 次 → 熔断 → 定时尝试恢复 |
| `RecoveryInterceptor` | 捕获 panic，执行兜底逻辑，防止单个节点拖垮工作流 |

#### ReAct 规划模式

```go
// 1. 创建规划节点
plannerNode := node.NewReActPlannerNode("react_planner", agent)

// 2. 运行规划
plannerWF, _ := flowchart.NewWorkflow(ctx, 10)
plannerWF.AddNode(plannerNode)
plannerWF.Run(map[string]any{"user_goal": "创建一个前端页面"})

// 3. 获取规划
plan, _ := plannerWF.Get("react_planner_plan")

// 4. 创建拓扑执行节点（自动解析依赖、分层并发）
topoNode, _ := node.BatchNewTaskNodeWithTopo(
    "react_planner", plan.(*node.Plan), agent, []string{"final_answer"},
)

// 5. 创建调度循环（失败自动重规划）
loopNode := node.ScheduleLoopNode("react_planner", agent)

// 6. 运行
tasksWF, _ := flowchart.NewWorkflow(ctx, 10)
tasksWF.AddNode(loopNode)
tasksWF.AddNode(topoNode)
tasksWF.Run(map[string]any{"react_planner_plan": plan})

// 7. 获取结果
result, _ := tasksWF.Get("final_answer")
```

**Plan 状态机：** `TaskPending → TaskRunning → TaskSuccess / TaskFailed → (重规划)`

---

### 7. Skill（技能系统）

基于 [Agent Skills Open Specification](https://agentskills.io/) 的 Skill 系统，支持 Markdown 格式管理和两种执行模式。

```yaml
# SKILL.md 示例
---
name: "frontend-design"
description: "Create distinctive, production-grade frontend interfaces"
category: "design"
tags: ["frontend", "web"]
parameters:
  type: "object"
  properties: {}
---

This skill guides creation of distinctive frontend interfaces...
```

```go
import (
    "github.com/Luo-root/pulse/components/skill"
    "github.com/Luo-root/pulse/components/schema"
)

// 1. 创建注册表
sr := skill.NewSkillRegistry()
tr := schema.NewToolRegistry()
loader := skill.NewSkillLoader(sr, tr)

// 2. 从文件加载
loader.LoadFromFile("./skills/frontend-design/SKILL.md")

// 3. 从目录批量加载
loader.LoadFromDir("./skills")

// 4. 从字符串加载
loader.LoadFromString(skillMarkdown)

// 5. Skill 自动注册为 Tool → 可被 Agent 调用
tool, ok := tr.Get("frontend-design")
```

**两种 Skill 类型：**

| 类型 | 说明 | 执行方式 |
|------|------|---------|
| `SkillTypeInstruction` | 纯指令型 | 调用时返回 Markdown 正文给 LLM 作为指导 |
| `SkillTypeCode` | 可执行代码型 | Go 代码块通过 yaegi 解释器动态编译执行 |

---

## 快速开始

### 安装

```bash
go get github.com/Luo-root/pulse
```

### 依赖

```
go 1.24.2

github.com/glebarez/sqlite v1.11.0    // SQLite 驱动（纯 Go，无需 CGO）
github.com/panjf2000/ants/v2 v2.12.0  // 协程池
github.com/coder/hnsw v0.1.0          // HNSW 向量索引
github.com/traefik/yaegi v0.16.1      // Go 解释器（Skill 动态编译）
github.com/google/uuid v1.6.0         // UUID 生成
gopkg.in/yaml.v3 v3.0.1               // YAML 解析（Skill frontmatter）
gorm.io/gorm v1.31.1                  // ORM（记忆存储）
```

### 最小示例

```go
package main

import (
    "context"
    "fmt"

    "github.com/Luo-root/pulse/components/agent"
    "github.com/Luo-root/pulse/components/chatmodel/openai"
    "github.com/Luo-root/pulse/components/schema"
    tools "github.com/Luo-root/pulse/components/tools"
)

func main() {
    ctx := context.Background()

    // 1. 注册工具
    registry := schema.NewToolRegistry()
    tools.RegisterAll(registry)

    // 2. 创建模型
    model, _ := openai.NewChatModel(ctx, &openai.ChatModelConfig{
        BaseUrl: "https://api.deepseek.com",
        APIKey:  "your-api-key",
        Model:   "deepseek-v4-pro",
        Tools:   registry.GetEnabledTools(),
    })

    // 3. 创建 Agent
    ag := agent.NewAgent(model, registry)

    // 4. 对话
    resp, err := ag.Send(ctx, "列出当前目录的文件")
    if err != nil {
        panic(err)
    }
    fmt.Println("AI:", resp.Content)
}
```

---

## 使用样例

### 样例 1：基础对话（非流式）

```go
func basicChat() {
    ctx := context.Background()

    registry := schema.NewToolRegistry()
    tools.RegisterAll(registry)

    model, _ := openai.NewChatModel(ctx, &openai.ChatModelConfig{
        BaseUrl: "https://api.deepseek.com",
        APIKey:  "your-api-key",
        Model:   "deepseek-v4-pro",
        Tools:   registry.GetEnabledTools(),
    })

    ag := agent.NewAgent(model, registry)
    resp, err := ag.Send(ctx, "创建一个 hello.txt 文件，内容为 Hello World")
    if err != nil {
        panic(err)
    }
    fmt.Println("AI:", resp.Content)
}
```

### 样例 2：流式对话（打字机效果）

```go
func streamChat() {
    ctx := context.Background()

    registry := schema.NewToolRegistry()
    tools.RegisterAll(registry)

    model, _ := openai.NewChatModel(ctx, &openai.ChatModelConfig{
        BaseUrl: "https://api.deepseek.com",
        APIKey:  "your-api-key",
        Model:   "deepseek-v4-pro",
        Tools:   registry.GetEnabledTools(),
    })

    ag := agent.NewAgent(model, registry)

    resp, err := ag.SendStream(ctx, "介绍Go语言",
        func(msg *schema.Message, isToolCall bool) bool {
            if isToolCall {
                for _, tc := range msg.ToolCalls {
                    fmt.Printf("\n🔧 %s\n", tc.Function.Name)
                }
            } else {
                print(msg.Content) // 实时打字机效果
            }
            return true
        },
    )
    if err != nil {
        panic(err)
    }
    fmt.Printf("\n\n最终回答:\n%s\n", resp.Content)
}
```

### 样例 3：带长期记忆的多轮对话

```go
func memoryChat() {
    ctx := context.Background()

    // 初始化记忆存储
    store, _ := memory.NewGormStore(memory.DefaultGormStoreConfig(), nil)
    defer store.Close()

    registry := schema.NewToolRegistry()
    tools.RegisterAll(registry)

    model, _ := openai.NewChatModel(ctx, &openai.ChatModelConfig{
        BaseUrl: "https://api.deepseek.com",
        APIKey:  "your-api-key",
        Model:   "deepseek-v4-pro",
        Tools:   registry.GetEnabledTools(),
    })

    // 自定义记忆控制器
    mc := memory.NewController(
        []*schema.Message{schema.SystemMessage("你是一个友好的助手")},
        memory.NewSimpleWindowMemory(memory.NewWindowManager(
            memory.WindowConfig{MaxHistoryMessages: 40}, nil, nil,
        )),
        store,
    )

    ag := agent.NewAgent(model, registry, agent.WithMemoryController(mc))

    // 多轮对话
    resp1, _ := ag.Send(ctx, "我叫张三，喜欢Go语言")
    fmt.Println("AI:", resp1.Content)

    resp2, _ := ag.Send(ctx, "我叫什么名字？")
    fmt.Println("AI:", resp2.Content) // 能记住"张三"
}
```

### 样例 4：手动处理工具调用循环

```go
func manualToolLoop() {
    ctx := context.Background()

    registry := schema.NewToolRegistry()
    tools.RegisterAll(registry)

    model, _ := openai.NewChatModel(ctx, &openai.ChatModelConfig{
        BaseUrl: "https://api.deepseek.com",
        APIKey:  "your-api-key",
        Model:   "deepseek-v4-pro",
        Tools:   registry.GetEnabledTools(),
    })

    messages := []*schema.Message{schema.UserMessage("列出当前目录文件")}

    for {
        resp, err := model.Generate(ctx, messages)
        if err != nil {
            panic(err)
        }

        if len(resp.ToolCalls) == 0 {
            fmt.Println("AI:", resp.Content)
            break
        }

        // 执行工具调用
        results := registry.ExecuteBatch(ctx, resp.ToolCalls)

        // 追加到历史
        messages = append(messages, &schema.Message{
            Role:      schema.AssistantRole,
            Content:   resp.Content,
            ToolCalls: resp.ToolCalls,
        })
        messages = append(messages, schema.ToolResultsMessage(results)...)
    }
}
```

### 样例 5：ReAct 工作流编排

```go
func reactWorkflow() {
    ctx := context.Background()

    registry := schema.NewToolRegistry()
    tools.RegisterAll(registry)

    model, _ := openai.NewChatModel(ctx, &openai.ChatModelConfig{
        BaseUrl: "https://api.deepseek.com",
        APIKey:  "your-api-key",
        Model:   "deepseek-v4-pro",
        Tools:   registry.GetEnabledTools(),
    })

    ag := agent.NewAgent(model, registry)

    // 阶段 1：AI 自主规划
    plannerNode := node.NewReActPlannerNode("react_planner", ag)
    plannerNode.AddAspect(node.NewRetryInterceptor(2, 2*time.Second))

    plannerWF, _ := flowchart.NewWorkflow(ctx, 10)
    plannerWF.AddNode(plannerNode)
    plannerWF.Run(map[string]any{"user_goal": "创建一个待办事项Web应用"})

    plan, _ := plannerWF.Get("react_planner_plan")

    // 阶段 2：拓扑排序 + 分层并发执行 + 失败重规划
    topoNode, _ := node.BatchNewTaskNodeWithTopo(
        "react_planner", plan.(*node.Plan), ag, []string{"final_answer"},
    )
    loopNode := node.ScheduleLoopNode("react_planner", ag)

    tasksWF, _ := flowchart.NewWorkflow(ctx, 10)
    tasksWF.AddNode(loopNode)
    tasksWF.AddNode(topoNode)
    tasksWF.Run(map[string]any{"react_planner_plan": plan})

    result, _ := tasksWF.Get("final_answer")
    fmt.Println(result)
}
```

### 样例 6：Skill 动态加载

```go
func skillExample() {
    ctx := context.Background()

    sr := skill.NewSkillRegistry()
    tr := schema.NewToolRegistry()
    loader := skill.NewSkillLoader(sr, tr)

    // 加载 Skill
    loader.LoadFromFile("./skills/frontend-design/SKILL.md")

    // 同时注册内置工具
    tools.RegisterAll(tr)

    model, _ := openai.NewChatModel(ctx, &openai.ChatModelConfig{
        BaseUrl: "https://api.deepseek.com",
        APIKey:  "your-api-key",
        Model:   "deepseek-v4-pro",
        Tools:   tr.GetEnabledTools(),
    })

    ag := agent.NewAgent(model, tr)

    // Skill 已作为 tool 注册，可被 Agent 自动调用
    resp, _ := ag.Send(ctx, "用 frontend-design 创建一个登录页面")
    fmt.Println("AI:", resp.Content)
}
```

---

## API 参考

### schema 包

| 类型/函数 | 说明 |
|-----------|------|
| `Message` | 消息结构体 |
| `SystemMessage(content)` | 创建系统消息 |
| `UserMessage(content)` | 创建用户消息 |
| `AssistantMessage(content, reasoning)` | 创建助手消息 |
| `ToolResultsMessage(results)` | 创建工具结果消息（返回 `[]*Message`） |
| `NewToolResult(callID, content, isError)` | 创建工具结果条目 |
| `Tool` | 工具定义 |
| `ToolCall` | 工具调用请求 |
| `ToolResult` | 工具执行结果 |
| `ToolRegistry` | 工具注册中心 |
| `NewToolRegistry()` | 创建注册中心 |
| `ToolMetadata` | 工具元数据（声明式配置） |
| `ToolPermission` | 工具权限级别 |
| `StreamReader` | 流式读取器 |
| `NewStreamReader()` | 创建流读取器 |
| `MulticastController` | 流多播控制器（1→N） |
| `FlowContext` | 工作流上下文（数据驱动 + 级联取消） |
| `NewFlowContext(ctx)` | 创建上下文 |
| `DataSlot` | 数据槽（支持多协程等待） |
| `FormatMessages(msgs)` | 格式化消息为可读字符串 |
| `PrintMessages(msgs)` | 打印格式化消息 |
| `StreamWriter` | 流式消息写入器 |
| `PipeStreamReader()` | 创建配对的 StreamReader + StreamWriter |

### agent 包

| 类型/函数 | 说明 |
|-----------|------|
| `Agent` | 智能体（对话循环封装） |
| `NewAgent(model, registry, opts...)` | 创建 Agent |
| `AgentInterface` | Agent 统一接口 |
| `WithMemoryController(mc)` | 配置记忆控制器 |
| `WithUsageTracker(tracker)` | 配置用量追踪器 |
| `Agent.Send(ctx, content)` | 非流式发送 |
| `Agent.SendStream(ctx, content, onChunk)` | 流式发送（带回调） |
| `Agent.AddSystemMessage(content)` | 追加系统消息 |
| `Agent.ClearAgentHistory(ctx)` | 清空历史（保留 system prompt） |
| `Agent.GetHistory(ctx)` | 获取完整历史 |
| `Agent.GetRawMessages()` | 获取含 ToolCalls/ToolResults 的消息 |
| `UsageTracker` | 用量追踪器 |
| `NewUsageTracker()` | 创建追踪器 |
| `UsageRecord` | 单次调用记录 |
| `UsageStats` | 累计统计 |

### chatmodel 包

| 类型/函数 | 说明 |
|-----------|------|
| `BaseModel` | 模型接口（Generate + Stream） |
| `MockModel` | 模拟模型（测试用） |

### openai 包

| 类型/函数 | 说明 |
|-----------|------|
| `ChatModelConfig` | 模型配置 |
| `NewChatModel(ctx, config)` | 创建模型 |
| `ChatModel.Generate(ctx, msgs)` | 非流式生成 |
| `ChatModel.Stream(ctx, msgs)` | 流式生成 |
| `ChatModel.GetModelName()` | 获取模型名称 |
| `Thinking` | 思考模式配置 |

### tool 包

| 类型/函数 | 说明 |
|-----------|------|
| `RegisterAll(registry)` | 注册所有内置工具 |
| `RegisterFileTools(registry)` | 注册文件工具 |
| `RegisterCommandTools(registry)` | 注册命令工具 |
| `RegisterEnvTools(registry)` | 注册环境工具 |
| `RegisterWebTools(registry)` | 注册搜索工具 |
| `RegisterUserConfigTools(registry)` | 注册用户配置工具 |
| `GetWorkDir()` | 获取工作目录 |
| `DynamicToolLoader` | 动态工具加载器 |
| `NewDynamicToolLoader(registry)` | 创建加载器 |
| `PromptLoader` | Prompt 文件加载器 |
| `NewPromptLoader(configDir)` | 创建 Prompt 加载器 |

### memory 包

| 类型/函数 | 说明 |
|-----------|------|
| `Controller` | 记忆控制器（三层协调） |
| `NewController(prompt, short, long)` | 创建控制器 |
| `ShortMemoryManager` | 短期记忆接口 |
| `SimpleWindowMemory` | 纯滑动窗口实现 |
| `WindowShortMemory` | 窗口+摘要实现 |
| `WindowManager` | 窗口管理器（Token/数量双重限制） |
| `NewWindowManager(config, model, est)` | 创建窗口管理器 |
| `LongTermStore` | 长期记忆接口 |
| `GormStore` | SQLite + HNSW 向量存储 |
| `NewGormStore(config, embedFunc)` | 创建 GORM 存储 |
| `MemNetAIStore` | 云端记忆存储 |
| `OllamaEmbedder` | Ollama 嵌入器 |
| `OpenAIEmbedder` | OpenAI 嵌入器 |
| `Embedder` | 文本转向量接口 |
| `EmbedderFunc(e)` | 将 Embedder 适配为 EmbeddingFunc |

### flowchart 包

| 类型/函数 | 说明 |
|-----------|------|
| `Workflow` | 工作流引擎 |
| `NewWorkflow(ctx, maxWorkers)` | 创建工作流 |
| `Workflow.AddNode(node)` | 添加节点 |
| `Workflow.AddAspect(aspect)` | 添加全局切面 |
| `Workflow.Run(inputs)` | 运行工作流（阻塞） |
| `Workflow.Start()` | 异步启动 |
| `Workflow.Get(key)` | 获取结果 |

### node 包

| 类型/函数 | 说明 |
|-----------|------|
| `Node` | 节点接口 |
| `NewNode(id, inputs, outputs, run)` | 创建通用节点 |
| `NewConditionNode(...)` | 创建条件节点 |
| `NewLoopNode(...)` | 创建循环节点 |
| `NewParallelNode(...)` | 创建并行节点 |
| `NewLLMStreamNode(...)` | 创建流式 LLM 节点 |
| `NewReActPlannerNode(id, agent)` | 创建 ReAct 规划节点 |
| `ScheduleLoopNode(plannerID, agent)` | 创建调度循环节点 |
| `NewTaskNode(plannerID, task, agent)` | 创建任务节点 |
| `BatchNewTaskNode(plannerID, plan, agent)` | 批量创建任务节点 |
| `BatchNewTaskNodeWithTopo(...)` | 批量创建+拓扑排序 |
| `TopologicalNode` | 拓扑排序执行节点 |
| `Aspect` | 切面接口 |
| `AroundAspect` | 环绕切面 |
| `BeforeAspect` | 前置切面（仅 Before） |
| `AfterAspect` | 后置切面（仅 After） |
| `Interceptor` | 拦截器接口 |
| `RetryInterceptor` | 重试拦截器 |
| `TimeoutInterceptor` | 超时拦截器 |
| `CircuitBreakerInterceptor` | 熔断拦截器 |
| `RecoveryInterceptor` | 兜底拦截器 |

### skill 包

| 类型/函数 | 说明 |
|-----------|------|
| `Skill` | 技能定义 |
| `SkillRegistry` | 技能注册表 |
| `NewSkillRegistry()` | 创建注册表 |
| `SkillLoader` | 技能加载器 |
| `NewSkillLoader(registry, toolReg)` | 创建加载器 |
| `LoadFromFile(path)` | 从 Markdown 文件加载 |
| `LoadFromDir(dir)` | 从目录批量加载 |
| `ParseSkillMarkdown(content)` | 解析 Skill Markdown |

---

## 许可证

本项目采用 [Apache License 2.0](LICENSE) 开源许可证。

```
Copyright 2025 Pulse Contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
```

---

> **注意**：使用本框架时请妥善保管 API Key，避免在代码中硬编码敏感信息。建议通过环境变量或配置文件管理密钥。
