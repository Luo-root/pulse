# pulse v2：插件化内核（plugin-kernel-v2）设计文档

> 状态：Accepted · 分支 `feat/plugin-kernel-v2` · 起始日期 2026-08-25

## 1. 背景与动机

pulse v1 的出发点是"一切皆插件、所有实现和工具可装配"，但缺少支撑这个
出发点的底座：

- 组件之间靠具体类型硬引用接线（`agent` 直接持有 `*tools.ToolRegistry`、
  `*memory.Controller`），换实现要改源码；
- 注册了就收不回来：没有统一的效应跟踪，资源释放依赖调用方自觉；
- 三套通知机制并存（tools Hook / stream.Multicast / flowchart interceptor），
  没有统一的事件契约；
- 组件各自 New，没有生命周期语义：依赖消失时使用方无从感知；
- 装配逻辑散落在使用方代码里，配置格式分裂（MCP JSON / skill frontmatter /
  user_config 各一套）。

DeepSeek Harness（DSH）给出了这个底座的完整参考实现：其论文
《A Programming Paradigm for Spatiotemporal Composability》（cordiverse/paper，
北大 × DeepSeek-AI）把"一切皆插件"形式化为**时空可组合性**，落地为
Cordis 元框架；DSH 在其上证明了一个复杂 Agent 产品可以做到
**没有任何特权内核**——model adapter、tool registry、session log、
agent loop 本身全是插件。

v2 的目标：把 Cordis 的思想以 Go 的方式重新实现为 pulse 的内核，
并让模型适配层成为第一个基于内核构建的能力层。

## 2. 理论 → 实现映射

| 论文概念 | 含义 | kernel 对应 |
|---|---|---|
| revertible effect | 每个上下文变换携带显式逆元，运行时跟踪 | `Context.Effect(apply)` 返回 dispose，作用域销毁 LIFO unwind |
| reactive coeffect | 依赖声明规范，环境变化时响应式通知 | `Plugin.Inject() []Dependency` + 服务变更广播 + fiber 收敛 |
| context type Γ | 效应上下文与余效应上下文统一 | `Context`：服务仓库 + 效应栈 + 作用域树节点 |
| component {inject, apply} | 组件声明 | `Plugin` 接口 |
| fiber（惯性状态机） | 组件实例化与转换 | `Fiber`（Inactive/Loading/Active/Unloading/Failed） |
| loader entry + reconciliation | 声明式配置树 + 增量调和 | `Loader.Reconcile([]Entry)` |

## 3. 与 Cordis（TypeScript）的有意差异

这些不是偷懒，是基于 Go 语言特性和当前需求的明确取舍：

| # | 差异 | 原因 |
|---|---|---|
| 1 | 服务读取用泛型 `Get(ServiceKey[T])`，不做 Proxy 属性访问 | Go 无 Proxy；泛型键让依赖关系在定义处显式可见 |
| 2 | **服务仓库全局唯一**（挂在根作用域），作用域只管效应归属与事件传播 | 对齐 Cordis runtime-store 的实际行为；避免"谁提供谁可见"的作用域陷阱——插件在其私有作用域 Provide 的服务必须对系统可见 |
| 3 | 效应 = dispose 闭包；`Provide` 内部自动走 `Effect` 登记 | "set 即 effect"不变式的 Go 表达；插件丢弃闭包也不会泄漏 |
| 4 | 运行期状态收敛用"脏标记 + 单飞 goroutine"（`markDirty`/`settleLoop`），Use 首次装载同步 | Cordis 依靠 async/await 实现"卸载等待依赖方先行"，Go 同步调用无法安全重入；单飞协程从结构上消除卸载环死锁 |
| 5 | 不做 realm/isolate 多租户隔离 | 当前无场景；键名前缀约定已够用，留有扩展点 |
| 6 | 不做代码级 HMR | Go 无法卸载已加载代码。Loader 的"重载"是状态级的：dispose 旧 Fiber、同一工厂重建新实例 |
| 7 | waterfall 监听器契约只有注册顺序，不支持 prepend | 契约最小化；需要优先级时显式分层注册 |
| 8 | 事件三种派发模式（Emit/Waterfall/Parallel），不设独立 Serial | Cordis 的 serial 与 emit 差异在 await 与否；Go 同步调用天然串行累积（监听器经 `*P` 就地修改、对后续可见），两模式实现完全相同——保留两个入口只会让人误以为有行为差异 |

### 有意钉死的语义（有测试背书，不是漏测）

- **同名覆盖即撤旧，不还原前值**：后到的 `Provide` 覆盖旧绑定并广播变更；
  覆盖者卸载后服务消失、依赖方自动卸载——被覆盖方的旧 dispose 是空操作，
  不复活前值。两个插件抢同一服务名视为装配错误，行为可预期且可观测。
- **事件派发为全树广播**：任意作用域派发，整棵树（根到叶、层内注册顺序）
  上该事件的监听都会收到——兄弟作用域的策略插件因此能拦截彼此；
  监听本身是 Effect，随注册方作用域销毁自动摘除。
- **条目配置是实例私有的**：Loader 经由可选接口 `Configurable.Configure`
  把 `Entry.Config` 交给对应实例（对齐 Cordis 把 config 绑进 apply 参数），
  不经过全局服务仓库，多实例互不覆盖、卸载互不影响。

## 4. 内核 API 一览

```go
// 作用域与效应
root := kernel.New()
child := root.Derive()                    // 子作用域：父销毁级联，子销毁不伤父
d, err := ctx.Effect(func() (func(), error) {
    ln, _ := net.Listen(...)              // 装载动作
    return func() { ln.Close() }, nil     // 撤销动作（登记即跟踪）
})
ctx.Dispose()                             // LIFO unwind 一切效应

// 服务（类型安全键）
var Key = kernel.NewServiceKey[*Registry]("pulse.llm")
dispose, _ := kernel.Provide(ctx, Key, reg)   // 自动登记为效应；覆盖=撤旧装新
reg, ok := kernel.Get(ctx, Key)               // 全局仓库查找 + 类型断言

// 事件（三种派发模式）
var EvReq = kernel.NewEventKey[*Req]("x.req")
kernel.OnWaterfall(ctx, EvReq, func(r *Req, next func(*Req) *Req) *Req { ... }) // around 链
kernel.On(ctx, EvOther, func(p *P))           // Emit / Parallel 共用签名
out := kernel.Waterfall(ctx, EvReq, req)      // 可短路、可改写

// 插件（依赖响应式）
p := &MyPlugin{}                              // Inject() + Apply(c)
f, err := kernel.Use(ctx, p)                  // Use 返回即 Active / Failed / Inactive-挂起
// 依赖消失 => 自动 Unloading→Inactive（副作用全回收）
// 依赖恢复 => 自动重新装载
f.WaitState(time.Second, kernel.StateActive)

// 装配器（声明式条目 + 增量调和）
l := kernel.NewLoader(ctx)
l.MustRegister("echo", func() kernel.Plugin { return NewEcho() })
l.Reconcile([]kernel.Entry{
    {ID: "a", Name: "echo", Config: map[string]any{"tag": "v1"}},
})
// 条目增删改 => fiber 增删改；Config 变化 => 重建；Disabled => 卸载保留记录
```

## 5. LLM 适配层（llm 包）设计

定位对标 DSH 的 `ctx.llm`：**provider 中立的词汇表 + 适配器注册中心**。
消费方（未来的 agent-loop）只见 `llm.ChatModel`，不见任何 provider。

### 5.1 词汇表

- **消息 = content-block 模型**（对齐 Anthropic block 语义，adapter 负责转
  自家线格式）：`Message{Role, []Part}`，Part 六种——text / image /
  tool_call / tool_result / reasoning（思维链）/ custom（MIME 驱动的
  开放模态块，承载音频、视频、PDF 及未来一切未知类型）；
- **请求是完整的 `GenerateRequest`**：messages / tools / tool_choice /
  temperature / top_p / max_tokens / stop / response_format（JSON Schema
  结构化输出）/ audio（官方对话接口音频输出模态：voice + format；仅
  Chat Completions 线格式支持，Responses 变体显式 bad_request）/ metadata
  （审计透传）；零值字段一律交 provider 默认；
- **流式 = 统一事件流** `<-chan StreamEvent`：text_delta /
  reasoning_delta / tool_call_begin / tool_call_delta / error / done，
  done 携带聚合 Response（含 Usage、FinishReason）。**有意不做 audio
  增量事件**：音频分片对逐字 UI 无意义（无法边收边播的半帧），适配器
  在流内聚合解码、随 done 的 Message 以 custom 块交付；
- **错误带分类**：`Error{Kind, Provider, StatusCode}`——可重试性由
  Kind 唯一决定（单一真相），`KindOf` / `IsRetryable` 供上层退避与
  failover 决策，分类是接口契约的一部分，不是日志细节。

### 5.2 注册中心与拦截 seam

```
adapter 插件(openai/anthropic/deepseek…)
    │ RegisterProvider(scope, "openai", factory)  // scope = adapter 自己的 Apply ctx
    ▼
Registry —— kernel 服务 "pulse.llm"（生命周期随 llm.Plugin，卸载关全部实例）
    │ Declare("main", cfg) + Open("main")（实例缓存单例；observed 实现 io.Closer 转发）
    ▼
observed 包装的 ChatModel（消费方接口）
```

每次调用经过两个内核事件，能力挂载不需要包裹任何实例：

- `pulse.llm.before_generate`（waterfall，载荷 `*GenerateRequest`）：可就地
  改写请求——路由、默认参数注入、脱敏、限流检查都在监听器里做
  （监听器应 `req.Clone()` 后改写再 next 委托，避免污染调用方）；
- `pulse.llm.after_response`（emit，载荷值类型 `Response`）：计量、审计、
  缓存观察——观察者拿到只读快照，改不了调用方的结果。

### 5.3 有意为之的边界

- 不内置重试/主备 failover：那是策略，属于上层编排插件（用 before/
  after 事件 + 错误分类即可实现），不该焊死在适配层；
- Registry.Declare 替换同名声明时关闭旧实例；Open 双检防止并发重复建；
- `ScriptedModel` 是唯一的 mock：按脚本回放响应/工具调用/错误，供
  上层组件测试复用。

## 6. 迁移路线图（围绕核心的重构计划）

原则：**核心先站稳，能力逐个搬**。每个 P 级合入后 `go test ./...`
保持绿线；旧 components/* 在迁移完成前原样保留可运行。

### P0 agent-loop（已完成，PR #5）

- 新包 `loop/`：基于 `llm.ChatModel` 的 ReAct 循环；
- **Agent 是库对象而非插件**（有意取舍）：它没有可装载的副作用、
  没有资源要回收，硬套 Plugin/Loader 只会多一层无意义的壳。依赖
  经构造注入（ChatModel / ToolSet），事件派发作用域可选注入；
  "一切皆插件"约束的是内核里的能力服务，不要求把每个库对象都
  塞进 Loader。若未来出现 bundle 级消费者（CLI/server 装配），
  在对应 P 级再补装配入口；
- 每一步派发事件（step/start、tool/call、turn/end），拦截点即扩展点
  （对标 DSH turn flow）；before_tool_call 为 waterfall，是 HITL
  审批与权限策略的标准挂载点；
- usage 统计从 llm.Response.Usage 取，不再各处拼装；
- 交付验收：mock 模型跑通多轮工具循环；事件监听器能完整还原执行轨迹；
  压测（500 步长回合 / 32 并发 / 事件洪峰 / 协程泄漏）全绿。

### P1 工具体系

- 新包 `toolset/`：ToolRegistry 作为 kernel 服务（`pulse.tools`）；
- 每个工具一个插件（fs / command / web …），注册即可逆；
- 执行管线事件化：before_execute（waterfall，审批/沙箱挂载点）→
  execute → after_execute（emit，审计）；
- 权限分级（readonly/readwrite/dangerous）沿用 v1 设计，审批策略作为
  监听 before_execute 的独立插件；
- MCP client 改造为工具来源插件：MCP server 掉线 = 撤销其工具注册 =
  依赖它的能力自动降级。

### P2 记忆与会话

- `memory-window`（上下文窗口管理）与 `memory-store`（持久化）拆成
  两个 seam；窗口摘要依赖 ChatModel（Inject `pulse.llm`）而非反向；
- session log 引入 DSH 不变式的 Go 版：**进模型的内容必须可从日志重建**
  （model-visible means logged），为多轮回放/fork 打地基；
- gorm/sqlite 存储实现为存储 seam 的实现插件。

### P3 flowchart 重构（大改，单独立项）

- DAG 编排思路保留（拓扑并行执行 + 拦截器是对的）；
- 实现层重构方向：节点 = Plugin（依赖注入取代手工传参）、数据槽 =
  类型安全 DataSlot[T]、retry/timeout/circuit-breaker 拦截器改挂在
  内核事件管线上；
- 与 loop 的关系重新定义：workflow 是 loop 的上游规划者或下游批处理
  执行器，两者通过服务互相发现而不是 import。

### 持续约束

- kernel/llm 保持零业务依赖：kernel 只用标准库，llm 只加 kernel；
- 新能力一律先回答三个问题：Service Definition 是什么？Provider 有谁？
  Consumer 在哪？（capability seam 三角色缺一不算数）
- 禁止"v0.2 候选"分期尾巴：确定要做的直接进对应 P 级，不做的明说不做。

## 7. 参考资料

- 论文：<https://github.com/cordiverse/paper>（A Programming Paradigm for
  Spatiotemporal Composability）
- DSH 架构：<https://github.com/deepseek-ai/deepseek-harness>
  （docs/architecture.md、docs/cordis-primer.md、docs/capability-seams.md）
- 本地研究副本：`%TEMP%\dsh-research\`（paper.pdf + 关键 md）
