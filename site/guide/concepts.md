# 核心概念

Pulse v2 的底座是一个**插件内核**：对环境的所有修改都注册为可逆 Effect，服务依赖变化驱动装载 / 卸载。理解五个概念就能读懂全部包的设计。

## kernel.Context 五件套

| 概念 | 一句话 | 关键性质 |
|---|---|---|
| **Effect** | 对环境的修改都登记为效应 | 卸载即还原——插件卸载时按登记逆序自动回滚 |
| **ServiceKey[T]** | 类型安全的服务句柄 | `kernel.Get(ctx, key)` 取值无类型断言；服务 = 惰性值 |
| **Event** | 四模式事件 | Emit / Waterfall / Parallel（全树广播）+ EmitLocal / WaterfallLocal（本 scope） |
| **Plugin** | 装载单元 | `kernel.Use(host, plugin)` 装载；Fiber = 依赖响应式的生命周期域 |
| **Loader** | 增量调和 | 依赖变化时自动装载 / 卸载受影响的插件子树 |

请求级隔离用 **scope**（`host.Derive()`）：请求 scope 上的 Effect / 事件不影响宿主，回合结束即回收——`loop.Agent` 的 `WithEventScope` 就挂在这里。

## 词汇表优先

`llm` 包是 v2 的另一个支柱：请求词汇表只收**跨 provider 有稳定语义**的字段。provider 线格式没有对应参数时，adapter 显式返回 `ErrBadRequest`——不静默吞参数，也不提供 `map[string]any` 逃生舱。这保证了同一份请求代码在 OpenAI / Anthropic 上的行为可预期。

```go
reg.Declare("main", llm.Config{
	Provider: openai.ProviderCompletions,
	Model:    "gpt-4o-mini",
	APIKey:   os.Getenv("OPENAI_API_KEY"),
})
model, err := reg.Open("main") // observed 包装：自动发 kernel 事件
```

## Agent 无状态

`loop.Agent` 只执行**一个回合**：模型推理 → 工具调用 → 结果回填 → 最终回答，中间暴露 HITL 决策事件。它不持有历史——会话存储、压缩、重试与 failover 由记忆层（`memory/session`、`memory/compaction`）或宿主承担。

## 拦截点

Kernel 事件是框架的扩展面：

- `before_generate` / `after_response`（Waterfall）——llm 层拦截 seam，observed 包装即由此驱动；
- `before_tool_call`（WaterfallLocal）——HITL 审批挂载点，工具调用前可拒绝 / 改写；
- `fiber_state` / `loader_action`——观测包订阅的生命周期事件。

详见 [observability 包文档](/packages/observability/) 与 [kernel 包文档](/packages/kernel/)。

## 设计边界

- **彻底 breaking**：v1 组件树已删除，不保留兼容层；
- **插件不是口号**：每个环境修改都可逆、可审计；
- **词汇表优先**：见上文；
- **Agent 无状态**：见上文。
