# Observability v1 设计：kernel typed 旁路事件 + 快照横幅 + Sink

> 状态：Accepted（方案 A，评审定案 2026-08-27；适配下沉修订 2026-09，Issue [#127](https://github.com/Luo-root/pulse/issues/127)——双基座模型：折叠适配从伴生 bridge 下沉至各事实归属包，bridge 包废除）
> 包位置：`observability/`（与 Issue [#16](https://github.com/Luo-root/pulse/issues/16) 同步实现）；观测适配面在 llm / loop / flow 各包（`Observe` / `NewRecordObserver`）
> 前置：examples/internal/observability 原型已验证运行期桥可行（PR #15）；本篇为其正式化收缩版设计
> 依赖：observability 本体只 import `kernel`；llm/loop/flow 可 import kernel + observability（双基座租户），全仓无逆向依赖

## 0. 一句话定位

给 Pulse 宿主 SpringBoot 式的装配可观测体验：启动时能看到每个插件「inactive → loading → active」的完整轨迹与分类横幅；Dispose 后零残留。v1 刻意不认识任何业务组件。

## 1. 决策记录（为什么是现在这个形状）

| # | 决策 | 被否掉的替代方案 | 理由 |
|---|---|---|---|
| D1 | 正式包零业务依赖（方案 A） | 观测包 import llm/loop/flow 整体迁原型（方案 B） | 方案 B 使「观测包不认识业务」变成空话；分层一旦打开就关不上 |
| D2 | kernel 发 typed 结构体事件，观测侧 `On` 订阅 | `Collector.Emit(string, map[string]any)` 字符串总线 | 第二套事件总线 + 无类型逃生舱：拼错静默丢、隐私边界靠约定不靠编译器；与词汇表侧否决 `map[string]any` 的既有立场同构 |
| D3 | trace 拆 host_id / trace_id 两层 | 全部共用一个 trace_id | 装配期没有用户请求；强行共用制造假关联 |
| D4 | 横幅 = 订阅后状态快照 | 靠事件流累积出横幅 | kernel 事件不回放；后装 Bootstrap 会错过历史。快照保证任意时刻正确 |
| D5 | observability 本体观察只用 `On`/`Emit` | 在 before_generate 等 Waterfall 上挂计量 | 观测是旁路观察者，进 around 链就有短路真实流程的风险（原型 bug，正式版修正）。**修订（#127）**：`after_response` 不携带耗时，Waterfall 回调是唯一可行的计时起点——恒 `next` 且不改参的观察者不改变 Waterfall 语义（短路风险来自改参/吞调用，不来自「进入链」本身）。计量起点例外主体 = `llm.Observe`（事实归属包自身）；observability 本体禁令不变 |
| D6 | Fiber 对外只出值类型快照 | `Fibers() []*Fiber` 活指针枚举 | 外部可调 Close、可触内部锁；settleLoop 并发改 state 下是指针竞态 |
| D7 | Record 加 `Attrs` 开放段：标量 kv，值域经泛型 `Set/Get` 锁死（~string/~int64/~float64/~bool） | ① 继续加运行期具名字段；② `map[string]any` 逃生舱 | ① 具名字段让信封随包增长，每加一类观测就改信封；② map 逃生舱静默吞未知键、与词汇表侧立场同构。Attrs 把「开放」与「任意对象」切开：Message 切片/附件/思维链在类型上进不来（隐私边界的类型部分），key 自述意图 + Sink 侧 redact 兜住蓄意标量注入 |
| D8 | 观测 key 契约由**事实归属包**定义（`llm.AttrModel`、`loop.AttrTool`、`flow.AttrNode`），桥执行折叠 | observability 聚合适配各包维度 | 字段语义的知识留在归属包——observability 只做信封与出口，不成为所有包观测字段的汇聚点；key 约定 `<组件>.<字段>` 点分，各包独立 key 空间 |
| D9 | 桥正式化为伴生包 `observability/bridge`（允许 import llm/loop/flow） | 留在 demoapp 手写（60 行 + 三个 hack） | 装配层桥是所有宿主的公共需求，不是示例私有物；demoapp 改为官方包消费者。分层边界不变：本体不 import 业务组件，「认识业务组件」的桥单独成包 |
| D10 | Collector 服务化：`bridge.CollectorKey` 进请求 scope，业务插件 `kernel.Get` 直写 | 业务观测走 kernel 事件总线 | 直写不带总线开销与类型面膨胀；HostID/TraceID 自动携带（#125 Web 地基入口形态：kernel+observability 以 Web 框架地基为方向） |

## 2. 分层与归属

```text
┌─────────────────────────────────────────────────────┐
│ 宿主（examples/internal/demoapp / 业务宿主）          │
│   每请求：ObserveConfig{Sink,HostID,TraceID} +        │
│   AttachCollector + llm/loop.Observe + flow 图挂      │
│   NewRecordObserver；宿主业务 Observer 经 MultiObserver│
│   组合；TraceID 宿主单一生成源注入（D3）               │
└───────────────┬─────────────────────────────────────┘
                │ 显式装配（可选注册：不调即零足迹）
┌───────────────▼─────────────────────────────────────┐
│ llm / loop / flow（基座上的租户）                     │
│   llm.Observe / loop.Observe：监听自身事件折叠写 Sink  │
│   flow.NewRecordObserver：节点分段计时                │
│   事件名常量与 key 契约归各包（D8）                    │
│   ✓ import kernel + observability                    │
└───────────────┬─────────────────────────────────────┘
                │ 只依赖 observability.Record / Attrs / Sink
┌───────────────▼─────────────────────────────────────┐
│ observability/（基座）                                │
│   Record 信封（具名段 + Attrs 开放段）/ Sink 三实现    │
│   ObserveConfig / Collector / AttachCollector         │
│   Bootstrap：On(fiber_state|loader_action) → Record   │
│   ✗ 只 import kernel，且无任何例外（bridge 已废除）    │
└───────────────┬─────────────────────────────────────┘
                │ kernel.On / kernel.Emit（typed）
┌───────────────▼─────────────────────────────────────┐
│ kernel/：发装配期事实（锁外派发，不 import 本包）      │
└─────────────────────────────────────────────────────┘
```

划线：**双基座** = kernel（生命周期/服务/事件）+ observability（信封/出口/直写服务），互不依赖业务包。**租户** = llm/loop/flow 依赖两个基座，各自适配自己的观测（折叠代码住事实归属包，D8/D9）。宿主是唯一同时认识所有包的装配层。E1 生命周期在 `kernel/flow.Observer` 定形，observability 本体**故意不认识**它；flow 包经 Observer seam 自适配（`NewRecordObserver`）。

「同一出口」的实现语义：各包折叠函数把运行期事实**折进 Record 信封**（Time/HostID/TraceID/Source/Event/Duration/Status + Attrs）再 `Sink.Write`；业务维度（模型名、token 数、工具名、节点 ID）走 Attrs，key 契约归事实归属包（D8）。同一出口 ≠ 官方 Record 变成万能袋，也不允许适配层绕过 Sink 另开一路输出。

## 3. 数据契约

### 3.1 Record 信封

通用可空字段 + 装配专用具名段 + Attrs 开放段。没有 `map[string]any` 逃生舱；Attrs 值域经泛型 `Set/Get` 锁死在标量：

```text
通用观测信封：Time, HostID, TraceID, Source, Event, Duration, Status, Err
装配专用段：  FiberName, From, To, LoaderKind, EntryID, PluginName
Attrs 开放段：标量 kv（~string/~int64/~float64/~bool），key 约定 <组件>.<字段>
```

- 装配记录：TraceID/Duration/Attrs 为零值
- 桥记录：填 TraceID/Duration/Status/Attrs；业务维度（`llm.AttrModel`、`llm.AttrTokensIn/Out/Cached`、`loop.AttrTool`、`loop.AttrSteps`、`flow.AttrNode`）经 Attrs 进入，**不再扩具名字段**（D7/D8）
- 隐私边界（类型部分）：Attrs 的写入面只有泛型 `Set[T AttrValue]`，`[]byte`、struct、slice、任意对象在类型上无法进入——prompt、消息切片、思维链内容不能以 kv 形式进记录；「把 payload 塞进一个标量值」属于蓄意行为，防线是 key 自述意图 + Sink 侧 redact 钩子（宿主 Sink 实现可拒绝敏感 key / 截断超长 / 限条数）
- 出口确定性：Attrs 内部 map 无序，SlogSink 按 key 字典序输出，`Attrs.MarshalJSON` 同序；导出实现应保持同一约定

### 3.2 Sink

```go
type Sink interface {
    Write(Record) // 无 ctx：kernel Emit 路径不带 context
}
```

内置 SlogSink（stderr）/ MemorySink（测试断言）/ MultiSink（扇出）。`Time` 为零时由内置 Sink 补 wall clock。导出器（otel/prometheus）将来以「新增 Sink 实现」方式接入，不动包结构。

### 3.3 kernel 侧新增公开面

typed 事件键 + 只读快照，共三类：

```go
// 事件载荷（struct，非 map）
type FiberStateChange struct {
    Name string; From, To FiberState; Err error
}
var EventFiberState = NewEventKey[FiberStateChange]("pulse.kernel.fiber_state")

type LoaderAction struct {
    Kind string /* mount|unmount|recreate|disable */; EntryID, Name string; Err error
}
var EventLoaderAction = NewEventKey[LoaderAction]("pulse.kernel.loader_action")

// 只读快照（横幅专用，不给活指针）
type FiberSnapshot struct {
    Name string; State FiberState; Err error; WaitingFor []string // 拷贝
}
func (c *Context) FiberSnapshots() []FiberSnapshot   // 从 root 扫整棵树
func (f *Fiber) Name() string   // Loader=Entry.ID；裸 Use=类型名#序号
```

不加实例方法 `WaitingFor()`：防内部切片泄漏；等待列表只在快照中输出。

**扫整棵树是横幅的 blocker**：Bootstrap 在自己的私有子 ctx 上 Apply，业务插件是兄弟子树——只 dump 本层 `c.fibers` 的话横幅永远看不见它们。实现为 root 起点全树遍历 + 锁内拷贝。

## 4. 状态迁移矩阵（唯一事实源）

派发规则：**改 state 的锁外 Emit；from==to 不发**（消除 settleSync/doLoad 双 loading；T2 仅剩 failed→loading 重试）。

**T7 不作为事件（评审定案 2026-08-27）**：`Context.dispose()` 的时序是 **先** `forceUnload`（静默，不发 `fiber_state`）与 children 级联，**再** `events.clear()`（见 `kernel/context.go`）。树销毁路径因此本来就不会发出逐 Fiber 迁移；T7 **不产生 fiber_state 记录**。宿主终止以 host 级语义呈现——Dispose 后 Sink 零残留 + 快照横幅终态。这与 SpringBoot 关机日志的惯例一致（context closing/stopped，而非逐 Bean 迁移）。

行号会漂；定位以符号 / 注释锚点为准（`plugin.go` 内 `// T*: …` 注释）。下表行号为 2026-08-27 main 快照，仅作跳转辅助：

| # | 触发 | 锚点（plugin.go） | From → To | 备注 |
|---|---|---|---|---|
| T1 | settleSync | `settleSync` / :175 | inactive→loading | 装配期首次评估 |
| T2 | doLoad 开始 | `doLoad` / :259 | failed→loading | 仅 Failed 重试出现；首次 from==to 不发 |
| T3 | doLoad 宿主销毁 | `doLoad` Derive 失败 / :266 | loading→inactive | Apply 前宿主没了 |
| T4 | doLoad 失败 | `doLoad` / :293 | loading→failed | Err=apply err |
| T5 | doLoad 成功 | `doLoad` / :299 | loading→active | |
| T6 | doUnload | `doUnload` / :305+:317 | active→unloading→inactive | 依赖消失驱动 |
| T8a | Close（Active 回收） | `Close` / :356+:363 | active→unloading→inactive | 手动 Close |
| T8b | Close 打断 Loading | `doLoad` closed 分支 / :286 | loading→inactive | Apply 中 Close 竞态 |

LoaderAction 对照 Reconcile 三阶段实际分支：removed→unmount、Name/Config 变→recreate、Disabled→disable、plans→mount；mount 失败逐条带 Err；无变化静默（无 noop）。

## 5. 生命周期语义

### 横幅（快照）

```text
[observability] host <id> ready: N active, M failed, K waiting
  ✔ pulse.llm        active
  ✔ greeter          active
  ◌ cache-warmer     inactive (waiting: redis.config)
```

- 内容来自 `FiberSnapshots()` 快照，不是事件累积
- **完整轨迹的前提是 Bootstrap 最先 Use**；后装只保证横幅正确，历史轨迹不保证（doc.go 写死）

### Dispose 后零残留

两路径必须全部满足：手动 `Host.Close`（T8a）与 Close 打断 Loading（T8b）；host 树销毁按 §4 T7 裁决不发事件，验收为 **Dispose 返回后 Sink 零增量**（MemorySink 断言）。

## 6. examples 改造

- 删除 `examples/internal/observability/`（原型，PR #15）
- demoapp `Host.NewObserve`：每请求装配 `AttachCollector` + `llm.Observe` + `loop.Observe`，返回 cfg 供 flow 图 `NewRecordObserver` 复用（同一请求共享 TraceID，D3 两层关联在 cfg 合流）；宿主/业务自定义事实经 Collector 直写
- flowpeak 等宿主业务 Observer 与官方 `NewRecordObserver` 经 `flow.MultiObserver` 组合挂图
- `demoapp.Open` 装配顺序改为 **Bootstrap 最先 Use**

## 7. 明确不做

otel/prometheus 导出器 · 采样与动态级别 · Web UI · 正式包内业务事件订阅 · 在 observability 本包订阅 flow NodeWaiting/Running/Finished（E1 归属 flow.Observer，本包不认识业务 seam）· Diagnostic 接口与组件发现（v2）· 本体侧 Waterfall 计量旁路（D5：本体观察只用 On/Emit；计量起点例外主体 = llm.Observe，见 §1 D5 修订与 §9）· prompt/附件/密钥/思维链内容记录。

**「Record map 逃生舱」条款的精确化决议（Issue #125，2026-09）**：本条「不做」指 D2 意义上的**无类型 any 逃生舱**——`map[string]any` 字符串分发，未知键静默吞、跨 provider/出口行为漂移。#125 引入的 `Attrs` 不是该意义上的逃生舱：值域被泛型标量约束锁死（`~string|~int64|~float64|~bool`），payload 结构（Message 切片/附件/思维链）在类型上进不来，开放面经 `Set/Get` 单一入口。本票即对该条款的重开与收窄记录；「无 map[string]any」四个字仍然成立。

原「Collector 概念本身」一条已由 #125 推翻并落地为 `observability.CollectorKey` 直写服务（D10）：被否的是「Collector.Emit 字符串总线」（D2），不是「宿主直写观测」这件事。原「bridge 伴生装配层」形态已由 #127 废除——折叠适配下沉至各事实归属包，全仓无逆向依赖。每一条「不做」都对应一轮评审的具体反对意见；重开时需先推翻对应决策记录。

## 8. 测试计划摘要

1. 迁移矩阵 T1–T6/T8a/T8b 全覆盖（重点：T8b 竞态、from==to 抑制）；树销毁不发事件（T7 裁决），以 Dispose 后零增量断言替代
2. 快照横幅两种场景：后装 Boot 只保横幅；最先装则全轨迹
3. host_id 隔离；Dispose 后**两路径**零增量（T8a 手动 Close、T8b Close 打断 Loading）+ 树销毁 T7 零增量（不发逐 Fiber 事件）
4. Reconcile 四动作一致；noop 静默
5. Record 表面测试：无 map、装配段在桥记录中零值
6. examples 回归 + trace_id 桥内四层贯通
7. race 下并发收敛不丢事件
8. Attrs 表面测试：Set/Get 四类标量往返、命名标量（~int64 等）兼容、类型不符 ok=false、MarshalJSON 按序、SlogSink attrs 段排序且位于具名字段之后
9. bridge 全链路（#125）：scripted agent 工具回合记录序列与 attrs 逐条断言；双 scope TraceID 隔离 + Dispose 摘除；HITL 中立（before_tool_call 监听恰一次、拒绝路径记 rejected）；Collector 直写自动携带标识；FlowObserver wait/run 分段（Duration>0）、skip 单条等待、双节点 nodeID 隔离

## 9. 观测适配下沉（Issue #127，2026-09）

bridge 伴生装配层形态废除；折叠适配下沉至各事实归属包。依赖模型：kernel ← observability（基座）← llm/loop/flow（租户）← 宿主。全仓无逆向依赖；宿主可选装配，不调即零观测足迹。

### 9.1 各包适配面

| 包 | 导出 | 折叠 |
|---|---|---|
| llm | `Observe(scope, cfg)` + `EventGenerateFinished` | before_generate 只计时（waterfall 透传，唯一计时起点，D5 修订）；after_response → `llm.generate_finished`（Status=finish_reason，Duration=本次生成，Attrs=model/tokens_in/tokens_out/tokens_cached） |
| loop | `Observe(scope, cfg)` + `EventToolFinished` / `EventTurnFinished` | after_tool_call → `loop.tool_finished`（三态判定 completed\|failed\|rejected 归本包，Duration/Err 透传，Attrs=tool）；turn_end → `loop.turn_finished`（Status=stopped_by，Attrs=steps；token 不重复记）；before_tool_call 不订阅（AfterToolCall 自带 Duration/Err，无观测增益，少一份与 HITL 审批链的顺序耦合） |
| flow | `NewRecordObserver(cfg) (Observer, error)` + `EventNodeWaitFinished/RunFinished` | 节点 wait/run 分段计时两条，nodeID 走 flow.AttrNode 不占具名字段；单次图运行一个实例（nodeID 记账，异常路径残留随实例丢弃） |
| observability | `ObserveConfig` / `Collector` / `CollectorKey` / `AttachCollector` / `NewTraceID` | 基座服务：业务插件 `kernel.Get(scope, CollectorKey)` 直写，HostID/TraceID 自动携带 |

事件名保持 `<组件>.<事实>` 点分；key 前缀 `llm./loop./flow./kernel./observability.` 为官方组件保留，业务插件用自己的包路径前缀（无注册机制，靠约定，与 OTel attribute 命名同理）。

### 9.2 装配语义

- `ObserveConfig{Sink, HostID, TraceID}` 生命周期 = 请求：同一请求多适配复用同一值（共享 TraceID 即 D3 请求级关联）；跨请求必须新建。TraceID 生成方案归宿主（单一生成源）；`observability.NewTraceID` 提供默认生成器（时间戳 + 随机段 + 进程内序号），demoapp 的 hostID 前缀格式是自带方案示例。
- scope 必须与 Agent 的 `llm.WithEventScope` 相同：EmitLocal/WaterfallLocal 只本 scope 可见；观测适配**不能做成 kernel.Plugin**（插件 Apply 私有子 scope 听不到），故各包导出 `Observe(scope, cfg)` 装配函数，与 Bootstrap 挂法同构。
- 各包重复调用 `Observe` = 双监听双记录，godoc 显式警告；nil scope / nil Sink 返回哨兵错误。
- 宿主自定义事实走 `c.Write / c.WriteAttrs`（状态型直写，不带 Duration/Err——运行期耗时与失败语义由各包 Observe 折叠产生）。

### 9.3 Attrs 引用语义契约

`Attrs` 是 Record 第一个引用类型字段，`MultiSink` 会把同一个 Attrs 递给多个 Sink。契约（已写入 `Sink` 接口 godoc）：

- 产出方每次构造独立 Attrs，`Write` 返回后不再修改该 Record；
- Sink 实现不得修改收到的 Record 及其 Attrs（只读消费）；
- 异步导出器（队列化后再落盘/上报）必须自行拷贝所需字段后再持有。

验收锚：`TestMultiSinkConcurrentAttrsFanout`——多 Sink 扇出同一带 Attrs 的 Record，多 goroutine 并发写 + 并发读，`-race` 下无竞争。

### 9.4 测试锚分布

- observability：Collector 注册/直写/撤除/覆盖（`collector_test.go`）+ Attrs/Sink/并发扇出。
- llm：折叠/透传/可选注册/隔离摘除/校验五锚（`observe_test.go`）。
- loop：折叠/失败/HITL 中立三重/可选注册/隔离摘除/校验六锚（`observe_test.go`）。
- flow：分段/skip/双节点/MultiObserver 组合/校验五锚（`observer_record_test.go`）。
- demoapp：全链路双 TraceID 隔离（`TestRequestObservesDoNotCrossTalk`，唯一同时装配三包的位置）。
