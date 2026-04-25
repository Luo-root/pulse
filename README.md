# Pulse - Go 语言 AI Agent 框架

> 一个轻量级、模块化、可扩展的 Go 语言 AI Agent 开发框架，支持多模型对话、工具调用、记忆管理、流式输出和工作流编排。

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
- [快速开始](#快速开始)
- [使用样例](#使用样例)
- [API 参考](#api-参考)
- [许可证](#许可证)

---

## 项目概述

**Pulse** 是一个面向 Go 开发者的 AI Agent 框架，旨在简化大语言模型（LLM）应用的开发流程。框架采用模块化设计，核心特性包括：

- 🧠 **多模型支持**：OpenAI 兼容接口（已完成）、Claude、Gemini（适配中）
- 🔧 **工具调用**：内置文件操作、命令执行、环境查询等工具，支持自定义扩展
- 💾 **记忆管理**：基于 SQLite 的本地记忆存储，支持对话历史召回
- 🌊 **流式输出**：完整的 SSE 流式接收与多播机制
- 🔄 **工作流编排**：基于 DAG 的异步工作流引擎，支持 ReAct 规划模式
- 🛡️ **安全约束**：工作目录限制、危险命令拦截、路径安全检查

---

## 架构设计

```
pulse/
├── components/
│   ├── schema/          # 核心数据结构（Message、Tool、FlowContext 等）
│   ├── chatmodel/       # 模型层（OpenAI/Claude/Gemini + Agent 封装）
│   ├── tool/            # 工具系统（文件/命令/环境）
│   ├── memory/          # 记忆系统（SQLite 存储 + 管理器）
│   └── flowchart/       # 工作流引擎（DAG + ReAct 规划）
├── go.mod
├── main_test.go         # 完整使用示例
└── README.md
```

### 数据流

```
用户输入 → Agent → 模型生成 → 工具调用 → 执行结果 → 模型再生成 → 最终输出
                ↓
           Memory（记忆注入/保存）
                ↓
           Flowchart（工作流编排）
```

---

## 核心组件

### 1. Schema（核心数据结构）

Schema 层定义了框架中所有核心数据结构，是整个框架的基石。

#### Message 消息结构

```go
type Message struct {
    Role             RoleType   `json:"role"`              // system/user/assistant/tool
    Content          string     `json:"content"`           // 消息内容
    ReasoningContent string     `json:"reasoning_content"` // 推理内容（Kimi k2.6）
    Name             string     `json:"name"`              // 发送者名称/ToolCallID
    Partial          bool       `json:"partial"`           // 是否为未完成消息
    ToolCalls        []ToolCall `json:"tool_calls"`        // 工具调用请求
    ToolResults      []ToolResult `json:"tool_results"`    // 工具执行结果
    Usage            *Usage     `json:"-"`                 // Token 使用量
}
```

**便捷构造函数：**

```go
msg := schema.SystemMessage("系统提示", "")
msg := schema.UserMessage("用户输入")
msg := schema.AssistantMessage("助手回复")
msg := schema.ToolMessage("工具结果")
```

#### Tool 工具定义

```go
type Tool struct {
    Name        string `json:"name"`        // 工具名称
    Description string `json:"description"` // 功能描述
    Parameters  any    `json:"parameters"`  // JSON Schema 参数定义
}
```

#### ToolExecutor 工具执行器

```go
// 创建执行器
executor := schema.NewToolExecutor()

// 注册工具
executor.MustRegister(schema.Tool{
    Name:        "get_weather",
    Description: "获取指定城市的天气",
    Parameters: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "city": map[string]any{"type": "string"},
        },
        "required": []string{"city"},
    },
}, func(ctx context.Context, args map[string]any) (any, error) {
    city := args["city"].(string)
    return map[string]string{
        "city": city,
        "temperature": "25°C",
    }, nil
})

// 获取工具 Schema（发给模型）
tools := executor.GetToolsSchema()

// 执行单个工具调用
result := executor.Execute(ctx, toolCall)

// 批量并发执行
results := executor.ExecuteBatch(ctx, toolCalls)

// 转换为 Message 列表（回传给模型）
msgs := executor.ToToolMessages(results)
```

#### StreamReader 流式读取器

```go
// 创建流读取器
reader := schema.NewStreamReader()

// 接收流式消息
for {
    msg, err := reader.Recv()
    if errors.Is(err, io.EOF) {
        break
    }
    if err != nil {
        panic(err)
    }
    print(msg.Content)
}

// 多播：1 个源流 → N 个独立流
readers := reader.Multicast(3)  // 分成 3 个独立流
```

#### FlowContext 工作流上下文

```go
// 创建上下文
ctx := schema.NewFlowContext(context.Background())

// 设置数据
ctx.Set("key", value)

// 等待数据（支持多节点同时等待）
val, err := ctx.Wait("key")

// 等待多个数据
vals, err := ctx.WaitAll("key1", "key2", "key3")
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

```go
import "github.com/Luo-root/pulse/components/chatmodel/openai"

// 创建模型
model, err := openai.NewChatModel(context.Background(), &openai.ChatModelConfig{
    BaseUrl: "https://api.moonshot.cn/v1/chat/completions",  // Moonshot/Kimi
    APIKey:  "your-api-key",
    Model:   "kimi-k2-0905-preview",
    Tools:   executor.GetToolsSchema(),  // 可选：绑定工具
    
    // 可选参数
    MaxCompletionTokens: 4096,
    Temperature:         0.6,
    TopP:                1.0,
    Stream:              false,
    Thinking: openai.Thinking{
        Type: openai.Disabled,  // 控制思考模式
    },
})

// 非流式生成
resp, err := model.Generate(ctx, messages)

// 流式生成
reader, err := model.Stream(ctx, messages)
```

**支持的模型提供商：**

| 提供商 | 状态 | 包路径 |
|--------|------|--------|
| OpenAI 兼容 | ✅ 已完成 | `components/chatmodel/openai` |
| Claude | 🚧 适配中 | `components/chatmodel/claude` |
| Gemini | 🚧 适配中 | `components/chatmodel/gemini` |

---

### 3. Agent（智能体）

Agent 是对话循环的封装，自动处理工具调用循环。

#### 基础 Agent

```go
import "github.com/Luo-root/pulse/components/chatmodel"

// 创建 Agent
agent := chatmodel.NewAgent(model, executor)

// 添加系统消息
agent.AddSystemMessage("你是一个天气助手", "")

// 添加用户消息
agent.AddUserMessage("北京天气怎么样")

// 非流式发送（自动处理工具调用循环）
resp, err := agent.Send(ctx, "北京天气怎么样")
fmt.Println("AI:", resp.Content)

// 流式发送（带实时回调）
resp, err := agent.SendStream(ctx, "北京天气怎么样", func(msg *schema.Message, isToolCall bool) bool {
    if isToolCall {
        if len(msg.ToolCalls) > 0 {
            fmt.Printf("\n[工具调用] %s\n", msg.ToolCalls[0].Function.Name)
        }
    } else {
        print(msg.Content)  // 实时打印文本
    }
    return true  // true=继续，false=中断
})
```

#### 带记忆的 Agent

```go
// 创建记忆存储
store, err := memory.NewLocalStore("./chat.db")
if err != nil {
    log.Fatal(err)
}
defer store.Close()

// 创建带记忆的 Agent
memAgent := chatmodel.NewMemoryAgent(model, executor, store, "session_001")

// 发送消息（自动注入历史记忆、保存对话）
resp, err := memAgent.Send(ctx, "北京天气怎么样")

// 流式发送
resp, err := memAgent.SendStream(ctx, "上海呢？", callback)

// 清空会话记忆
err = memAgent.Clear(ctx)

// 获取历史
history := memAgent.GetHistory()
```

**Agent 内置系统提示：**

Agent 自动注入工作目录约束和工具调用规则：
- 所有文件操作限制在当前工作目录
- 不确定时必须调用工具验证
- 禁止凭空回答，必须基于工具返回的真实数据

---

### 4. Tool（工具系统）

#### 内置工具

| 工具名 | 功能 | 参数 |
|--------|------|------|
| `file_read` | 读取文件（最大 10MB） | `path` |
| `file_write` | 写入文件（自动创建父目录） | `path`, `content` |
| `file_list` | 列出目录内容 | `path`（可选） |
| `command_exec` | 执行系统命令（带安全检查） | `command`, `timeout`, `cwd` |
| `get_work_dir` | 获取当前工作目录 | 无 |

#### 注册所有工具

```go
import tools "github.com/Luo-root/pulse/components/tool"

executor := schema.NewToolExecutor()
tools.RegisterAll(executor)  // 注册所有内置工具
```

#### 自定义工具

```go
executor.MustRegister(schema.Tool{
    Name:        "my_tool",
    Description: "我的自定义工具",
    Parameters: map[string]any{
        "type": "object",
        "properties": map[string]any{
            "param1": map[string]any{"type": "string"},
        },
        "required": []string{"param1"},
    },
}, func(ctx context.Context, args map[string]any) (any, error) {
    // 实现逻辑
    return result, nil
})
```

#### 安全特性

- **路径限制**：所有文件操作必须在当前工作目录内
- **命令拦截**：禁止 `rm -rf`, `mkfs`, `dd` 等危险命令
- **超时控制**：命令执行默认 30 秒超时
- **跨平台**：支持 Windows/Linux/macOS

---

### 5. Memory（记忆系统）

#### Store 接口

```go
type Store interface {
    Save(ctx context.Context, sessionID string, msgs []*schema.Message) error
    Recall(ctx context.Context, sessionID string, query string, topK int) ([]*schema.Message, error)
    GetSession(ctx context.Context, sessionID string) ([]*schema.Message, error)
    ClearSession(ctx context.Context, sessionID string) error
    Close() error
}
```

#### LocalStore（SQLite 实现）

```go
// 创建本地存储
store, err := memory.NewLocalStore("./chat.db")
if err != nil {
    log.Fatal(err)
}
defer store.Close()

// 保存消息
err = store.Save(ctx, "session_001", messages)

// 召回相关记忆（关键词匹配）
memories, err := store.Recall(ctx, "session_001", "天气", 3)

// 获取完整历史
history, err := store.GetSession(ctx, "session_001")

// 清空会话
err = store.ClearSession(ctx, "session_001")
```

#### Manager（记忆管理器）

```go
manager := memory.NewManager(store)

// 保存一轮对话
err = manager.SaveTurn(ctx, "session_001", userMsg, assistantMsg)

// 构建带记忆的上下文
contextMsgs, err := manager.BuildContext(ctx, "session_001", currentQuery, history)
```

---

### 6. Flowchart（工作流引擎）

基于 DAG 的异步工作流引擎，支持自动依赖等待、并发执行、AOP 切面。

#### 基础工作流

```go
import "github.com/Luo-root/pulse/components/flowchart"
import "github.com/Luo-root/pulse/components/flowchart/node"

// 创建工作流（最大 10 个并发协程）
wf, err := flowchart.NewWorkflow(ctx, 10)
if err != nil {
    panic(err)
}

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

// 添加节点
wf.AddNode(inputNode)
wf.AddNode(llmNode)

// 运行工作流
wf.Run(map[string]any{"user_goal": "目标描述"})

// 获取结果
result, err := wf.Get("final_answer")
```

#### 节点类型

| 节点类型 | 函数 | 说明 |
|----------|------|------|
| 通用节点 | `node.NewNode()` | 自定义执行逻辑 |
| 条件节点 | `node.NewConditionNode()` | 条件分支 |
| 循环节点 | `node.NewLoopNode()` | while 循环 |
| 并行节点 | `node.NewParallelNode()` | 等待多个输入 |
| 流式 LLM | `node.NewLLMStreamNode()` | 流式模型调用 |
| ReAct 规划 | `node.NewReActPlannerNode()` | AI 任务规划 |
| 任务节点 | `node.NewTaskNode()` | 执行规划任务 |

#### AOP 切面

```go
// 定义切面
aspect := &node.AroundAspect{
    BeforeFn: func(ctx *schema.FlowContext, node node.Node) {
        fmt.Println("节点开始:", node.ID())
    },
    AfterFn: func(ctx *schema.FlowContext, node node.Node, err error) {
        if err != nil {
            fmt.Printf("节点 %s 失败: %v\n", node.ID(), err)
        } else {
            fmt.Println("节点完成:", node.ID())
        }
    },
}

// 添加到节点
plannerNode.AddAspect(aspect)

// 或添加到工作流（全局生效）
wf.AddAspect(aspect)
```

#### ReAct 规划模式

```go
// 1. 创建规划节点
plannerNode := node.NewReActPlannerNode("react_planner", agent)

// 2. 运行规划
plannerWF, _ := flowchart.NewWorkflow(ctx, 10)
plannerWF.AddNode(plannerNode)
plannerWF.Run(map[string]any{"user_goal": "创建前端页面"})

// 3. 获取规划结果
plan, _ := plannerWF.Get("react_planner_plan")

// 4. 创建任务执行节点
taskNodes := node.BatchNewTaskNode("react_planner", plan.(*node.Plan), agent)

// 5. 创建调度循环
loopNode := node.ScheduleLoopNode("react_planner", agent)

// 6. 运行任务工作流
tasksWF, _ := flowchart.NewWorkflow(ctx, 10)
tasksWF.AddNode(loopNode)
for _, n := range taskNodes {
    tasksWF.AddNode(n)
}
tasksWF.Run(map[string]any{"react_planner_plan": plan})

// 7. 获取最终结果
result, _ := tasksWF.Get("final_answer")
```

---

## 快速开始

### 安装

```bash
go get github.com/Luo-root/pulse
```

### 依赖

```
go 1.25.0

require (
    github.com/glebarez/sqlite v1.11.0    // SQLite 驱动
    github.com/panjf2000/ants/v2 v2.12.0  // 协程池
)
```

### 最小示例

```go
package main

import (
    "context"
    "fmt"
    
    "github.com/Luo-root/pulse/components/chatmodel"
    "github.com/Luo-root/pulse/components/chatmodel/openai"
    "github.com/Luo-root/pulse/components/schema"
    tools "github.com/Luo-root/pulse/components/tool"
)

func main() {
    ctx := context.Background()
    
    // 1. 初始化工具
    executor := schema.NewToolExecutor()
    tools.RegisterAll(executor)
    
    // 2. 初始化模型
    model, _ := openai.NewChatModel(ctx, &openai.ChatModelConfig{
        BaseUrl: "https://api.moonshot.cn/v1/chat/completions",
        APIKey:  "your-api-key",
        Model:   "kimi-k2-0905-preview",
        Tools:   executor.GetToolsSchema(),
    })
    
    // 3. 创建 Agent
    agent := chatmodel.NewAgent(model, executor)
    
    // 4. 对话
    resp, err := agent.Send(ctx, "在当前目录创建一个 hello.txt 文件，内容为 Hello World")
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
    
    // 创建执行器并注册工具
    executor := schema.NewToolExecutor()
    executor.MustRegister(schema.Tool{
        Name:        "get_weather",
        Description: "获取指定城市的天气",
        Parameters: map[string]any{
            "type": "object",
            "properties": map[string]any{
                "city": map[string]any{"type": "string"},
            },
            "required": []string{"city"},
        },
    }, func(ctx context.Context, args map[string]any) (any, error) {
        city := args["city"].(string)
        return map[string]string{
            "city":        city,
            "temperature": "25°C",
            "weather":     "晴天",
        }, nil
    })
    
    // 创建模型
    model, _ := openai.NewChatModel(ctx, &openai.ChatModelConfig{
        BaseUrl: "https://api.moonshot.cn/v1/chat/completions",
        APIKey:  "your-api-key",
        Model:   "kimi-k2-0905-preview",
        Tools:   executor.GetToolsSchema(),
    })
    
    // 创建 Agent 并对话
    agent := chatmodel.NewAgent(model, executor)
    agent.AddSystemMessage("你是一个天气助手", "")
    
    resp, err := agent.Send(ctx, "北京天气怎么样")
    if err != nil {
        panic(err)
    }
    
    fmt.Println("AI:", resp.Content)
    // 输出：北京天气不错，25°C，晴天...
}
```

### 样例 2：流式对话

```go
func streamChat() {
    ctx := context.Background()
    
    executor := schema.NewToolExecutor()
    tools.RegisterAll(executor)
    
    model, _ := openai.NewChatModel(ctx, &openai.ChatModelConfig{
        BaseUrl: "https://api.moonshot.cn/v1/chat/completions",
        APIKey:  "your-api-key",
        Model:   "kimi-k2-0905-preview",
        Tools:   executor.GetToolsSchema(),
    })
    
    agent := chatmodel.NewAgent(model, executor)
    
    resp, err := agent.SendStream(ctx, "北京天气怎么样", func(msg *schema.Message, isToolCall bool) bool {
        if isToolCall {
            if len(msg.ToolCalls) > 0 {
                fmt.Printf("\n[工具调用] %s\n", msg.ToolCalls[0].Function.Name)
            }
        } else {
            print(msg.Content)  // 实时打印，打字机效果
        }
        return true
    })
    
    if err != nil {
        panic(err)
    }
    
    fmt.Printf("\n\n最终回答:\n%s\n", resp.Content)
}
```

### 样例 3：带记忆的多轮对话

```go
func memoryChat() {
    ctx := context.Background()
    
    // 1. 初始化记忆存储
    store, err := memory.NewLocalStore("./chat.db")
    if err != nil {
        log.Fatal(err)
    }
    defer store.Close()
    
    // 2. 初始化工具和模型
    executor := schema.NewToolExecutor()
    tools.RegisterAll(executor)
    
    model, _ := openai.NewChatModel(ctx, &openai.ChatModelConfig{
        BaseUrl: "https://api.moonshot.cn/v1/chat/completions",
        APIKey:  "your-api-key",
        Model:   "kimi-k2-0905-preview",
        Tools:   executor.GetToolsSchema(),
    })
    
    // 3. 创建带记忆的 Agent
    agent := chatmodel.NewMemoryAgent(model, executor, store, "user_123")
    
    // 4. 多轮对话
    resp1, _ := agent.Send(ctx, "我叫张三")
    fmt.Println("AI:", resp1.Content)
    
    resp2, _ := agent.Send(ctx, "我叫什么名字？")
    fmt.Println("AI:", resp2.Content)  // 能记住"张三"
}
```

### 样例 4：手动处理流式输出

```go
func manualStream() {
    ctx := context.Background()
    
    executor := schema.NewToolExecutor()
    tools.RegisterAll(executor)
    
    model, _ := openai.NewChatModel(ctx, &openai.ChatModelConfig{
        BaseUrl: "https://api.moonshot.cn/v1/chat/completions",
        APIKey:  "your-api-key",
        Model:   "kimi-k2-0905-preview",
        Tools:   executor.GetToolsSchema(),
    })
    
    msgs := []*schema.Message{schema.UserMessage("北京天气怎么样")}
    
    for {
        reader, err := model.Stream(ctx, msgs)
        if err != nil {
            panic(err)
        }
        
        var fullMsg schema.Message
        var contentParts []string
        
        // 流式读取
        for {
            msg, err := reader.Recv()
            if err == io.EOF {
                break
            }
            if err != nil {
                panic(err)
            }
            
            if msg.Content != "" {
                contentParts = append(contentParts, msg.Content)
                fullMsg.Content = strings.Join(contentParts, "")
                print(msg.Content)  // 实时输出
            }
            
            if len(msg.ToolCalls) > 0 {
                fullMsg.ToolCalls = msg.ToolCalls
            }
        }
        
        // 无工具调用，直接输出
        if len(fullMsg.ToolCalls) == 0 {
            fmt.Println("\nAI:", fullMsg.Content)
            break
        }
        
        // 有工具调用，执行并追加历史
        fmt.Println("\nAI 调用工具:", fullMsg.ToolCalls)
        results := executor.ExecuteBatch(ctx, fullMsg.ToolCalls)
        
        msgs = append(msgs, &schema.Message{
            Role:      schema.AssistantRole,
            Content:   fullMsg.Content,
            ToolCalls: fullMsg.ToolCalls,
        })
        msgs = append(msgs, executor.ToToolMessages(results)...)
    }
}
```

### 样例 5：工作流编排

```go
func workflowExample() {
    llm, _ := openai.NewChatModel(context.Background(), &openai.ChatModelConfig{
        BaseUrl: "https://api.moonshot.cn/v1/chat/completions",
        Model:   "kimi-k2-0905-preview",
        APIKey:  "your-api-key",
    })
    
    wf, _ := flowchart.NewWorkflow(context.Background(), 10)
    
    // 输入节点
    inputNode := node.NewNode(
        "user_input",
        nil,
        []string{"prompt"},
        func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
            return map[string]any{"prompt": "写一段Go语言介绍"}, nil
        },
    )
    
    // 流式 LLM 节点
    llmNode := node.NewLLMStreamNode(
        "llm_stream",
        "prompt",
        "stream_readers",
        llm,
        1,
    )
    
    // 实时打印节点
    printNode := node.NewNode(
        "stream_printer",
        []string{"stream_readers"},
        []string{"llm_full_result"},
        func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
            stream := inputs["stream_readers"].([]*schema.StreamReader)[0]
            for {
                msg, err := stream.Recv()
                if errors.Is(err, io.EOF) {
                    break
                }
                print(msg.Content)
            }
            return map[string]any{"llm_full_result": "完成"}, nil
        },
    )
    
    // 结束节点
    endNode := node.NewNode(
        "end",
        []string{"llm_full_result"},
        []string{},
        func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
            fmt.Println("\n[结束] LLM流式输出完成！")
            return nil, nil
        },
    )
    
    // 添加并运行
    wf.AddNode(inputNode)
    wf.AddNode(llmNode)
    wf.AddNode(printNode)
    wf.AddNode(endNode)
    
    wf.Run(nil)
}
```

### 样例 6：ReAct 规划模式

```go
func reactExample() {
    ctx := context.Background()
    
    executor := schema.NewToolExecutor()
    tools.RegisterAll(executor)
    
    model, _ := openai.NewChatModel(ctx, &openai.ChatModelConfig{
        BaseUrl: "https://api.moonshot.cn/v1/chat/completions",
        APIKey:  "your-api-key",
        Model:   "kimi-k2-0905-preview",
        Tools:   executor.GetToolsSchema(),
    })
    
    agent := chatmodel.NewAgent(model, executor)
    
    // 定义 AOP 切面
    aspect := node.AroundAspect{
        BeforeFn: func(ctx *schema.FlowContext, node node.Node) {
            fmt.Println("正在运行:", node.ID())
        },
        AfterFn: func(ctx *schema.FlowContext, node node.Node, err error) {
            if err != nil {
                fmt.Printf("%s 失败: %v\n", node.ID(), err)
            } else {
                fmt.Println(node.ID(), "完成")
            }
        },
    }
    
    // 阶段 1：规划
    plannerNode := node.NewReActPlannerNode("react_planner", agent)
    plannerWF, _ := flowchart.NewWorkflow(ctx, 10)
    plannerWF.AddNode(plannerNode)
    plannerNode.AddAspect(&aspect)
    plannerWF.Run(map[string]any{"user_goal": "创建前端页面"})
    
    plan, _ := plannerWF.Get("react_planner_plan")
    
    // 阶段 2：执行
    agent.ClearAgentHistory()
    
    loopNode := node.ScheduleLoopNode("react_planner", agent)
    taskNodes := node.BatchNewTaskNode("react_planner", plan.(*node.Plan), agent)
    
    tasksWF, _ := flowchart.NewWorkflow(ctx, 10)
    tasksWF.AddNode(loopNode)
    for _, n := range taskNodes {
        tasksWF.AddNode(n)
    }
    tasksWF.AddAspect(&aspect)
    tasksWF.Run(map[string]any{"react_planner_plan": plan})
    
    // 获取结果
    result, _ := tasksWF.Get("final_answer")
    fmt.Println(result)
}
```

---

## API 参考

### schema 包

| 类型/函数 | 说明 |
|-----------|------|
| `Message` | 消息结构体 |
| `SystemMessage()` | 创建系统消息 |
| `UserMessage()` | 创建用户消息 |
| `AssistantMessage()` | 创建助手消息 |
| `ToolMessage()` | 创建工具消息 |
| `Tool` | 工具定义结构体 |
| `ToolCall` | 工具调用请求 |
| `ToolResult` | 工具执行结果 |
| `ToolExecutor` | 工具执行器 |
| `NewToolExecutor()` | 创建执行器 |
| `StreamReader` | 流式读取器 |
| `NewStreamReader()` | 创建流读取器 |
| `FlowContext` | 工作流上下文 |
| `NewFlowContext()` | 创建上下文 |

### chatmodel 包

| 类型/函数 | 说明 |
|-----------|------|
| `BaseModel` | 模型接口 |
| `Agent` | 基础智能体 |
| `NewAgent()` | 创建 Agent |
| `MemoryAgent` | 带记忆的智能体 |
| `NewMemoryAgent()` | 创建记忆 Agent |
| `Agent.Send()` | 非流式发送 |
| `Agent.SendStream()` | 流式发送 |
| `Agent.AddSystemMessage()` | 添加系统消息 |
| `Agent.AddUserMessage()` | 添加用户消息 |
| `Agent.ClearAgentHistory()` | 清空历史 |
| `Agent.GetHistory()` | 获取历史 |

### openai 包

| 类型/函数 | 说明 |
|-----------|------|
| `ChatModelConfig` | 模型配置 |
| `NewChatModel()` | 创建模型 |
| `ChatModel.Generate()` | 非流式生成 |
| `ChatModel.Stream()` | 流式生成 |
| `Thinking` | 思考模式配置 |

### tool 包

| 类型/函数 | 说明 |
|-----------|------|
| `RegisterAll()` | 注册所有内置工具 |
| `RegisterFileTools()` | 注册文件工具 |
| `RegisterCommandExecTools()` | 注册命令工具 |
| `RegisterEnvTools()` | 注册环境工具 |
| `GetWorkDir()` | 获取工作目录 |

### memory 包

| 类型/函数 | 说明 |
|-----------|------|
| `Store` | 存储接口 |
| `LocalStore` | SQLite 实现 |
| `NewLocalStore()` | 创建本地存储 |
| `Manager` | 记忆管理器 |
| `NewManager()` | 创建管理器 |
| `Manager.SaveTurn()` | 保存一轮对话 |
| `Manager.BuildContext()` | 构建上下文 |

### flowchart 包

| 类型/函数 | 说明 |
|-----------|------|
| `Workflow` | 工作流引擎 |
| `NewWorkflow()` | 创建工作流 |
| `Workflow.AddNode()` | 添加节点 |
| `Workflow.AddAspect()` | 添加切面 |
| `Workflow.Run()` | 运行工作流 |
| `Workflow.Get()` | 获取结果 |
| `Node` | 节点接口 |
| `NewNode()` | 创建通用节点 |
| `NewConditionNode()` | 创建条件节点 |
| `NewLoopNode()` | 创建循环节点 |
| `NewParallelNode()` | 创建并行节点 |
| `NewLLMStreamNode()` | 创建流式 LLM 节点 |
| `NewReActPlannerNode()` | 创建 ReAct 规划节点 |
| `ScheduleLoopNode()` | 创建调度循环节点 |
| `BatchNewTaskNode()` | 批量创建任务节点 |
| `Aspect` | 切面接口 |
| `AroundAspect` | 环绕切面 |

---

## 许可证

本项目采用 [Apache License 2.0](LICENSE) 开源许可证。

```
Copyright [yyyy] [name of copyright owner]

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
```

---

> **注意**：使用本框架时请妥善保管 API Key，避免在代码中硬编码敏感信息。建议通过环境变量或配置文件管理密钥。
