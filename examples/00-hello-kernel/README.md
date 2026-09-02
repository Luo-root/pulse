# 00-hello-kernel

Pulse 第一课：**内核生命周期与第一次模型调用**。本课只有三个包——`kernel`、`llm`、`observability`，刻意不经过示例装配层（`demoapp.Open`，那是 01 课起的封装）、不走 Registry、不需要任何 API Key。跑完本课你会看到 Pulse 的三块地基：**宿主作用域、provider 中立的模型调用、可观测的插件装载**。

```powershell
go run ./examples/00-hello-kernel
```

## 本课依赖

无。这是整条学习路径的起点。

## 主线拆解（对照 main.go 的 ①–⑥）

### ① 宿主作用域：`kernel.New()`

kernel 是 Pulse 的插件内核：所有能力（模型、工具、观测、记忆）都是**插件**，装载进一个宿主作用域（`*kernel.Context`）。作用域是资源边界——销毁它，里面的一切按登记的逆序还原。

### ② 观测最先装：`observability.Bootstrap`

`Bootstrap(hostID, sink)` 订阅 kernel 的 `fiber_state`（插件状态迁移）与 `loader_action`（装载/卸载动作）事件。**必须最先 `Use`**：kernel 事件不回放，后装的观测只能靠快照横幅兜底当前视图，装配历史轨迹不保证。

`MemorySink` 把每条事件存成 `Record`（`Source`/`Event`/`FiberName`/`From`/`To`/`PluginName`/`LoaderKind`），本课结尾直接打印它——这就是「插件装载全过程」的可观测呈现。

### ③ 模型从哪来：`llm.NewScripted`

`llm.ChatModel` 是 provider 中立接口（`Generate` + `Stream` 两个方法）。`NewScripted` 是官方测试实现：按给定脚本依次返回响应。**课程默认用它**——不依赖 Key、输出确定；01 课换成真实 provider 后，调用侧代码一行不用改。

### ④ 第一次调用：`Generate`

```go
res, err := model.Generate(ctx, &llm.GenerateRequest{
    Messages: []*llm.Message{llm.UserText("用一句话介绍你自己")},
})
```

请求与响应都是统一词汇表：进 `[]*llm.Message`，出 `*Response`（`Message` + `FinishReason` + `Usage`）。文本取 `res.Message.Text()`。无论底层是 OpenAI、Anthropic 还是脚本模型，**这个结构不变**——这就是「provider 中立」的含义。

### ⑤ 观测记录

`Sink.Snapshot()` 返回装配期记录：每条插件的 fiber 状态迁移（`initializing → ready`）、每个 loader 动作（`mount`）。把这些记录和源码里的 `Use` 调用对上，插件内核就不再抽象。

### ⑥ 卸载即还原：`Dispose`

`host.Dispose()` 按 LIFO 还原全部效应。`Use` 返回的 dispose、插件登记的清理函数都在这一刻执行。「卸载即还原」不靠肉眼观察（进程退出后 OS 回收一切），单测可断言——`demoapp_test.go` 的 `TestHostCloseReclaimsServices` 是现成示范：`Close` 后 Registry 服务消失、Sink 记录数不再增长。

## 下一课

[01-chat](../01-chat/)：引入 `llm.Registry`（命名模型实例）、完整装配链（`llm.Plugin` + adapter 注册）、多模态消息词汇表与 REPL。
