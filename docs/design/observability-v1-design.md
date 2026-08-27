# Observability v1 设计：kernel typed 旁路事件 + 快照横幅 + Sink

> 状态：Accepted（方案 A，评审定案 2026-08-27）
> 包位置：`observability/`（与 Issue [#16](https://github.com/Luo-root/pulse/issues/16) 同步实现）
> 前置：examples/internal/observability 原型已验证运行期桥可行（PR #15）；本篇为其正式化收缩版设计
> 依赖：只 import `kernel`；**禁止** import llm/loop/flow

## 0. 一句话定位

给 Pulse 宿主 SpringBoot 式的装配可观测体验：启动时能看到每个插件「inactive → loading → active」的完整轨迹与分类横幅；Dispose 后零残留。v1 刻意不认识任何业务组件。

## 1. 决策记录（为什么是现在这个形状）

| # | 决策 | 被否掉的替代方案 | 理由 |
|---|---|---|---|
| D1 | 正式包零业务依赖（方案 A） | 观测包 import llm/loop/flow 整体迁原型（方案 B） | 方案 B 使「观测包不认识业务」变成空话；分层一旦打开就关不上 |
| D2 | kernel 发 typed 结构体事件，观测侧 `On` 订阅 | `Collector.Emit(string, map[string]any)` 字符串总线 | 第二套事件总线 + 无类型逃生舱：拼错静默丢、隐私边界靠约定不靠编译器；与词汇表侧否决 `map[string]any` 的既有立场同构 |
| D3 | trace 拆 host_id / trace_id 两层 | 全部共用一个 trace_id | 装配期没有用户请求；强行共用制造假关联 |
| D4 | 横幅 = 订阅后状态快照 | 靠事件流累积出横幅 | kernel 事件不回放；后装 Bootstrap 会错过历史。快照保证任意时刻正确 |
| D5 | 观察只用 `On`/`Emit` | 在 before_generate 等 Waterfall 上挂计量 | 观测是旁路观察者，进 around 链就有短路真实流程的风险（原型 bug，正式版修正） |
| D6 | Fiber 对外只出值类型快照 | `Fibers() []*Fiber` 活指针枚举 | 外部可调 Close、可触内部锁；settleLoop 并发改 state 下是指针竞态 |

## 2. 分层与归属

```text
┌─────────────────────────────────────────────────────┐
│ 装配层（demoapp / 未来宿主）                          │
│   bootstrap.go 桥：llm/loop 事件 → 桥自己的记录类型    │
│   trace_id 生成注入（四层贯通在这里实现）              │
│   FlowAspect（node_total_ms / alive_nodes_peak）      │
└───────────────┬─────────────────────────────────────┘
                │ 只依赖 observability.Sink
┌───────────────▼─────────────────────────────────────┐
│ observability/（本包）                                │
│   Record 信封 / Sink 三实现 / Bootstrap                │
│   On(pulse.kernel.fiber_state|loader_action) → Record │
│   SnapshotBanner(hostID)                              │
└───────────────┬─────────────────────────────────────┘
                │ kernel.On / kernel.Emit（typed）
┌───────────────▼─────────────────────────────────────┐
│ kernel/：发装配期事实（锁外派发，不 import 本包）      │
└─────────────────────────────────────────────────────┘
```

划线：**正式包** = Fiber 状态机、Loader 动作、启动清单、Dispose 后不再写。**桥** = token 计数、HITL 结果、flow 节点耗时——同一 stderr 出口、不同记录形状。

「同一出口」的实现语义：桥把运行期事实**折进 Record 信封**（Time/HostID/TraceID/Source/Event/Duration/Status）再 `Sink.Write`；token 数等无法装进信封的业务指标走 SlogSink 的附加键输出。同一出口 ≠ 官方 Record 变成万能袋，也不允许桥绕过 Sink 另开一路输出。

## 3. 数据契约

### 3.1 Record 信封

通用可空字段 + 装配专用具名段。没有 `Attributes map`（隐私边界由编译器保证，不靠约定）：

```text
通用观测信封：Time, HostID, TraceID, Source, Event, Duration, Status, Err
装配专用段：  FiberName, From, To, LoaderKind, EntryID, PluginName
```

- 装配记录：TraceID/Duration 为零值
- 桥记录：填 TraceID/Duration/Status；token 数等业务指标用**桥自己的类型**或 slog 附加键输出——官方 Record 不扩

### 3.2 Sink

```go
type Sink interface {
    Write(context.Context, Record)
}
```

内置 SlogSink（stderr）/ MemorySink（测试断言）/ MultiSink（扇出）。导出器（otel/prometheus）将来以「新增 Sink 实现」方式接入，不动包结构。

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

| # | 触发 | plugin.go 行号 | From → To | 备注 |
|---|---|---|---|---|
| T1 | settleSync | :171 | inactive→loading | 装配期首次评估 |
| T2 | doLoad 开始 | :249 | failed→loading | 仅 Failed 重试出现 |
| T3 | doLoad 宿主销毁 | :258 | loading→inactive | Apply 中宿主没了 |
| T4 | doLoad 失败 | :285 | loading→failed | Err=apply err |
| T5 | doLoad 成功 | :290 | loading→active | |
| T6 | doUnload | :298/:308 | active→unloading→inactive | 依赖消失驱动 |
| T7 | forceUnload（树级联销毁） | :320 | current→inactive | 无单独 unloading 过渡 |
| T8a | Close（Active 回收） | :344/:348 | active→unloading→inactive | 手动 Close |
| T8b | Close 打断 Loading | :277 | loading→inactive | doLoad 内 closed 竞态 |

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

三路径必须全部满足：手动 `Host.Close`（T8a）、Close 打断 Loading（T8b）、host 树销毁（T7）。验收手段：MemorySink 增量断言。

### T7 可达性约束（实现者必读）

dispose 顺序为 events.clear → forceUnload → 递归 children。因此：

1. Bootstrap 只在自己的 Apply(c) 私有子 ctx 上 On（全树收集使子总线听得到 T7）；
2. 不把观测监听登记到 host root（root 总线先 clear，会错过 T7）;
3. T7 在 forceUnload 锁外 Emit，且先于 children dispose。

## 6. examples 改造

- 删除 `examples/internal/observability/`
- demoapp 新增 `bridge.go`：llm/loop 事件 → 桥记录类型写同一 Sink；FlowAspect 迁入；trace_id 由 bridge 生成并注入 llm/loop/tool/flow 四层记录
- `demoapp.Open` 装配顺序改为 **Bootstrap 最先 Use**

## 7. 明确不做

Collector 概念本身 · otel/prometheus 导出器 · 采样与动态级别 · Web UI · 正式包内业务事件订阅 · flow Waiting/Running 占位接口（等 E1 有事实再定形）· Diagnostic 接口与组件发现（v2）· Waterfall 旁路 · Record map 逃生舱 · prompt/附件/密钥/思维链内容记录。

每一条「不做」都对应一轮评审的具体反对意见；重开时需先推翻对应决策记录。

## 8. 测试计划摘要

1. 迁移矩阵 T1–T8b 全覆盖（重点：T7 锁外可达、T8b 竞态、from==to 抑制）
2. 快照横幅两种场景：后装 Boot 只保横幅；最先装则全轨迹
3. host_id 隔离与 Dispose 后三路径零增量
4. Reconcile 四动作一致；noop 静默
5. Record 表面测试：无 map、装配段在桥记录中零值
6. examples 回归 + trace_id 桥内四层贯通
7. race 下并发收敛不丢事件
