# Pulse
The Streaming LLM Agent Engine for Go

Pulse 是一个基于 Go 语言开发的流式大语言模型（LLM）智能体引擎。它提供了工作流编排、流式数据处理、多节点并行执行等核心能力，帮助开发者快速构建复杂的 AI 应用。

## ✨ 核心特性

- 🚀 **流式处理**：原生支持 LLM 流式输出，实现实时打字机效果
- 🔗 **工作流编排**：基于有向图的节点化工作流引擎
- ⚡ **并发安全**：内置线程安全的上下文和数据槽机制
- 🎯 **数据驱动**：自动依赖解析，节点按需触发执行
- 🔌 **可扩展架构**：支持切面编程（AOP），易于扩展和定制
- 📦 **多播支持**：一个流式输出可同时分发给多个消费者

## 📦 安装

```
bash
go get github.com/Luo-root/pulse
```
## 🏗️ 架构概览

Pulse 由以下核心组件构成：

### 1. Workflow（工作流引擎）

工作流引擎负责管理和调度所有节点的执行。它采用异步并发模式，自动处理节点间的依赖关系。

**核心功能：**
- 节点注册与管理
- 全局切面支持
- 自动依赖等待与触发
- 并发安全的数据传递

### 2. Node（节点系统）

节点是工作流的基本执行单元，每个节点可以：
- 声明输入依赖（Inputs）
- 定义输出结果（Outputs）
- 执行业务逻辑（Run）
- 附加切面逻辑（Aspects）

**内置节点类型：**

#### SimpleNode - 通用节点
最基础的节点类型，通过 `NewNode` 函数创建：

```
go
node := NewNode(
"my_node",                    // 节点ID
[]string{"input_key"},        // 输入依赖
[]string{"output_key"},       // 输出键
func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
// 业务逻辑
return map[string]any{"output_key": "result"}, nil
},
)
```
#### LLMStreamNode - 流式LLM节点
专门用于处理 LLM 流式输出的节点，支持多播分发：

```
go
llmNode := NewLLMStreamNode(
"llm_stream",      // 节点ID
"prompt",          // 从上下文获取提示词的key
"stream_readers",  // 输出StreamReader数组的key
model,             // BaseModel实例
1,                 // 多播副本数量
)
```
#### ConditionNode - 条件判断节点
根据条件函数的返回值进行分支：

```
go
conditionNode := NewConditionNode(
"condition",
"input_value",
func(value any) bool {
return value.(int) > 10
},
"true_branch",   // 条件为真时输出的key
"false_branch",  // 条件为假时输出的key
)
```
#### LoopNode - 循环节点
当条件为真时持续执行循环体：

```
go
loopNode := NewLoopNode(
"loop",
"trigger",           // 循环控制key
func(ctx *schema.FlowContext) bool {
// 返回true继续循环，false退出
return counter < 5
},
func(ctx *schema.FlowContext) {
// 循环体逻辑
},
"result",            // 循环结束输出key
)
```
#### ParallelNode - 并行汇聚节点
等待所有输入就绪后触发：

```
go
parallelNode := NewParallelNode(
"merge",
[]string{"task1", "task2", "task3"},  // 等待的所有key
"all_complete",                        // 全部完成后输出
)
```
### 3. FlowContext（流程上下文）

FlowContext 是工作流中数据传递的核心载体，提供：
- **线程安全**的数据存储与读取
- **自动等待**机制：节点调用 `Wait()` 时会阻塞直到数据就绪
- **广播唤醒**：数据设置后自动唤醒所有等待者

```
go
// 设置数据
ctx.Set("key", value)

// 等待单个数据
value := ctx.Wait("key")

// 等待多个数据
values := ctx.WaitAll("key1", "key2", "key3")
```
### 4. DataSlot（数据槽）

DataSlot 是实现自动等待机制的底层数据结构：
- 基于 `sync.Cond` 实现的阻塞等待
- 支持多次读取（值一旦设置不可更改）
- 多协程安全

### 5. StreamReader（流式读取器）

StreamReader 用于处理 LLM 的流式输出：

**核心方法：**
- `Recv()`: 接收下一条消息，符合 Go 标准流式读取风格
- `Multicast(n)`: 将一个流复制成 N 个独立的流
- `Close()`: 安全关闭流

**使用示例：**
```
go
for {
msg, err := streamReader.Recv()
if errors.Is(err, io.EOF) {
break
}
fmt.Print(msg.Content)
}
```
**多播特性：**
```
go
// 创建一个源流的3个独立副本
readers := streamReader.Multicast(3)
// 每个reader都可以独立消费完整的数据流
```
### 6. Aspect（切面系统）

支持 AOP 编程模式，可在节点执行前后插入横切关注点：

**切面类型：**
- `BeforeAspect`: 仅在节点执行前触发
- `AfterAspect`: 仅在节点执行后触发
- `AroundAspect`: 在节点执行前后都触发

**使用方式：**
```
go
// 全局切面（对所有节点生效）
workflow.AddAspect(&node.AroundAspect{
BeforeFn: func(ctx *schema.FlowContext, n node.Node) {
fmt.Printf("节点 %s 开始执行\n", n.ID())
},
AfterFn: func(ctx *schema.FlowContext, n node.Node, err error) {
fmt.Printf("节点 %s 执行完成\n", n.ID())
},
})

// 节点级切面
myNode.AddAspect(&node.BeforeAspect{
Fn: func(ctx *schema.FlowContext, n node.Node) {
// 前置逻辑
},
})
```
### 7. ChatModel（聊天模型接口）

定义了统一的 LLM 交互接口：

```
go
type BaseModel interface {
Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error)
Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error)
}
```
**OpenAI 兼容实现：**
Pulse 提供了 OpenAI 兼容的客户端实现，支持任何兼容 OpenAI API 的服务商（如 Moonshot、DeepSeek 等）。

```
go
model, err := openai.NewChatModel(
context.Background(),
&openai.ChatModelConfig{
BaseUrl: "https://api.moonshot.cn/v1/chat/completions",
Model:   "kimi-k2-0905-preview",
APIKey:  "sk-xxxxxxxx",
},
)
```
## 🎯 完整示例

下面是一个完整的流式 LLM 工作流示例，展示如何组合使用各个组件：

```
go
package main

import (
"context"
"errors"
"fmt"
"io"

	"Pulse/compoents/chatmodel/openai"
	"Pulse/compoents/flowchart"
	"Pulse/compoents/flowchart/node"
	"Pulse/compoents/schema"
)

func main() {
// 1. 初始化 LLM 模型
llm, err := openai.NewChatModel(
context.Background(),
&openai.ChatModelConfig{
BaseUrl: "https://api.moonshot.cn/v1/chat/completions",
Model:   "kimi-k2-0905-preview",
APIKey:  "sk-xxxxxxxx",
},
)
if err != nil {
fmt.Println("模型创建失败:", err)
return
}

	// 2. 创建工作流
	wf := flowchart.NewWorkflow()

	// 3. 创建输入节点
	// 作用：生成用户输入的 prompt
	inputNode := node.NewNode(
		"user_input",
		[]string{"trigger"},     // 依赖：需要一个 trigger 信号
		[]string{"prompt"},      // 输出：生成 prompt
		func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
			fmt.Println("[InputNode] 生成 prompt")
			return map[string]any{"prompt": "写一段Go语言介绍"}, nil
		},
	)

	// 4. 创建流式 LLM 节点
	// 作用：接收 prompt，调用 LLM 流式接口，输出 StreamReader 数组
	llmNode := node.NewLLMStreamNode(
		"llm_stream",
		"prompt",         // 从上下文获取 prompt
		"stream_readers", // 输出 StreamReader 数组
		llm,
		1,                // 创建 1 个副本
	)

	// 5. 创建打印节点
	// 作用：实时接收流式数据并打印（模拟前端打字机效果）
	printNode := node.NewNode(
		"stream_printer",
		[]string{"stream_readers"},  // 依赖：需要 stream_readers
		[]string{"llm_full_result"}, // 输出：标记完成
		func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
			fmt.Println("[PrintNode] 开始等待流式数据...")
			
			// 获取第一个 StreamReader
			stream := inputs["stream_readers"].([]*schema.StreamReader)[0]
			
			// 逐块接收并打印
			for {
				msg, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					fmt.Println("接收错误:", err)
					break
				}
				fmt.Print(msg.Content)
			}
			
			return map[string]any{"llm_full_result": "完成"}, nil
		},
	)

	// 6. 创建结束节点
	endNode := node.NewNode(
		"end",
		[]string{"llm_full_result"}, // 依赖：等待打印完成
		[]string{},                   // 无输出
		func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
			fmt.Println("\n[结束] LLM流式输出完成！")
			return nil, nil
		},
	)

	// 7. 添加所有节点到工作流
	wf.AddNode(inputNode)
	wf.AddNode(llmNode)
	wf.AddNode(printNode)
	wf.AddNode(endNode)

	// 8. （可选）添加全局切面，监控所有节点执行
	wf.AddAspect(&node.AroundAspect{
		BeforeFn: func(ctx *schema.FlowContext, n node.Node) {
			fmt.Printf(">>> 节点 [%s] 开始执行\n", n.ID())
		},
		AfterFn: func(ctx *schema.FlowContext, n node.Node, err error) {
			if err != nil {
				fmt.Printf("!!! 节点 [%s] 执行出错: %v\n", n.ID(), err)
			} else {
				fmt.Printf("<<< 节点 [%s] 执行完成\n", n.ID())
			}
		},
	})

	// 9. 注入初始数据，触发工作流
	wf.Input("trigger", "start")
	
	// 10. 启动工作流（所有节点异步启动，自动等待依赖）
	wf.Start()

	// 11. 保持主协程运行，等待所有节点完成
	select {}
}
```
### 示例执行流程

```

1. wf.Input("trigger", "start")
   → 设置 trigger = "start"

2. wf.Start()
   → 所有节点异步启动

3. inputNode 检测到 "trigger" 已就绪
   → 执行 Run 函数
   → 输出 prompt = "写一段Go语言介绍"

4. llmNode 检测到 "prompt" 已就绪
   → 调用 LLM Stream API
   → 输出 stream_readers = [StreamReader]

5. printNode 检测到 "stream_readers" 已就绪
   → 循环调用 Recv() 接收流式数据
   → 实时打印内容（打字机效果）
   → 输出 llm_full_result = "完成"

6. endNode 检测到 "llm_full_result" 已就绪
   → 打印结束信息
   → 工作流完成
```
## 🔧 高级用法

### 并行任务示例

```
go
wf := flowchart.NewWorkflow()

// 并行执行三个任务
task1 := node.NewNode("task1", []string{"trigger"}, []string{"result1"}, ...)
task2 := node.NewNode("task2", []string{"trigger"}, []string{"result2"}, ...)
task3 := node.NewNode("task3", []string{"trigger"}, []string{"result3"}, ...)

// 等待所有任务完成
merge := node.NewParallelNode(
"merge",
[]string{"result1", "result2", "result3"},
"all_done",
)

wf.AddNode(task1, task2, task3, merge)
wf.Input("trigger", "start")
wf.Start()
```
### 条件分支示例

```
go
// 根据用户输入决定走哪条路径
condition := node.NewConditionNode(
"router",
"user_input",
func(val any) bool {
return strings.Contains(val.(string), "help")
},
"help_path",
"normal_path",
)

helpHandler := node.NewNode("help", []string{"help_path"}, ...)
normalHandler := node.NewNode("normal", []string{"normal_path"}, ...)
```
### 循环节点示例

```
go
counter := 0
loop := node.NewLoopNode(
"retry_loop",
"start_retry",
func(ctx *schema.FlowContext) bool {
return counter < 3 && !success
},
func(ctx *schema.FlowContext) {
counter++
// 执行重试逻辑
success = doSomething()
},
"loop_result",
)
```
## 📊 数据流原理

Pulse 的核心是**数据驱动的自动调度**：

1. **节点声明依赖**：每个节点通过 `Inputs()` 声明需要的数据 key
2. **自动阻塞等待**：节点执行时调用 `ctx.WaitAll(inputs...)`，自动阻塞直到所有依赖就绪
3. **广播唤醒**：当某个节点 `Set(key, value)` 时，所有等待该 key 的节点被唤醒
4. **并发执行**：所有节点以 goroutine 形式启动，无需手动管理顺序

这种设计使得工作流具有天然的并发性和灵活性。

## 🛠️ 扩展开发

### 自定义节点

```
go
type MyCustomNode struct {
*node.SimpleNode
}

func NewMyCustomNode() *MyCustomNode {
return &MyCustomNode{
SimpleNode: node.NewNode(
"custom",
[]string{"input"},
[]string{"output"},
func(ctx *schema.FlowContext, inputs map[string]any) (map[string]any, error) {
// 自定义逻辑
return map[string]any{"output": "done"}, nil
},
),
}
}
```
### 自定义模型适配器

实现 `chatmodel.BaseModel` 接口即可接入任意 LLM 服务：

```
go
type MyModel struct {
// ...
}

func (m *MyModel) Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
// 实现同步调用
}

func (m *MyModel) Stream(ctx context.Context, input []*schema.Message) (*schema.StreamReader, error) {
// 实现流式调用
}
```
## 📝 License

Apache License

## 🤝 Contributing

欢迎提交 Issue 和 Pull Request！

---

**Pulse** - 让流式 AI 应用开发更简单 ⚡