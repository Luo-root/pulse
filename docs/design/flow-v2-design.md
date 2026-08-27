# flow v2：数据就绪驱动的节点编排

> 状态：Accepted（设计拍板 2026-08-26）
> 包位置：`kernel/flow/`
> 前置：v1 `components/flowchart` 已按 breaking 决策删除；本篇从源码提炼理念并给出 v2 契约。

## 理念（四条）

> flow 是 kernel 的响应式理念在「一次运行」这个时间尺度上的应用：
> 依赖声明即拓扑，数据到达即调度；失败显式，取消能打断等数据。

1. **数据驱动，不维护显式边。** 节点只声明读哪些 Key、写哪些 Key。依赖由 Key 的生产与消费隐式形成，没有边对象、没有拓扑排序、没有调度循环。`Requires` 就是 AND 前置：全部就绪才进入 Run。
2. **一次运行一个世界。** 数据槽、取消、首错随 `Run` 而生、随结束而灭。跨运行状态（熔断计数、会话、缓存）不属于这个世界。
3. **和 kernel 同构，不同层。** 洋葱链用同样的 `next` 签名；层级取消对齐作用域树。kernel 管服务装卸载，flow 管数据到达。
4. **失败是一等公民。** 节点返回 error → 记录首错 + 取消整次运行。降级是节点自己显式写出的数据，不是切面偷偷吞掉的错误。

槽位因此有**三种**到达形态，第一版一次设计完，避免以后为分支再改契约：

```
槽位 = 未就绪 | 已就绪(值) | 已跳过
```

就绪和跳过都是「到达」。等待者被唤醒后区分这两种到达，而不是把「永远不到」伪装成一个假值。

## 与 kernel 的同构

| 维度 | kernel | flow |
|---|---|---|
| 时间尺度 | 服务生命周期（稳定态） | 一次运行内的数据流（瞬态） |
| 依赖声明 | `Inject() []Dependency` | `Requires(...Key)` |
| 响应式触发 | 依赖满足 → Fiber 装载 | 槽位到达（值或跳过）→ 节点继续 |
| 拦截 | `Waterfall(p, next)` | `Aspect.Around(rc, next)` |
| 层级 | 作用域树 Dispose | RunCtx.Fork 可取消 |
| 共享状态 | ServiceKey 仓库 | 运行级槽位（不进服务仓库） |

flow 可独立使用（只吃 `context.Context`），也可把取消挂到 `kernel.Context` 上。运行数据**不进**服务仓库。

## API 草案

```go
package flow // import "github.com/Luo-root/pulse/kernel/flow"

var Summary = flow.NewKey[string]("summary")
var Docs    = flow.NewKey[[]Doc]("docs")

g := flow.New(ctx) // ctx 可以是 context.Background()
g.Add(flow.NewNode("summarize",
    flow.Requires(Docs),
    flow.Provides(Summary),
    func(rc *flow.RunCtx) error {
        docs, err := flow.Get(rc, Docs) // 类型安全，无断言
        if err != nil {                 // 含 ErrSkipped
            return err
        }
        return flow.Set(rc, Summary, summarize(docs))
    },
))
g.Seed(Docs, input)
err := g.Run()
```

### Key

- `Key[T]`：name + 类型。同名不同类型在 `NewKey` / 注册期拒绝（对齐 `ServiceKey`）。
- 节点只能 `Get` 声明过的 Requires、`Set` 声明过的 Provides。碰未声明的 Key → 显式错误，不是静默写到黑板上。

### 槽位写入

| 操作 | 语义 |
|---|---|
| `Set` | 幂等首写为「已就绪(值)」；二次调用静默忽略 |
| `Skip` | 标记「已跳过」并唤醒等待者；已就绪(值)的槽位不允许再 Skip |
| 互斥 | 同一槽位不能既有值又跳过。Set 与 Skip 冲突显式报错；二次 Set 静默忽略 |

### 槽位读取

| 操作 | 语义 |
|---|---|
| `Get` | 阻塞到到达；值为 `T`，跳过为 `ErrSkipped` |
| `WaitAll` | AND：全部到达。任一跳过 → 返回 `ErrSkipped`（带哪些 Key 被跳过）。框架在进入用户 Run 前自动调用 |
| `TryGet` | 非阻塞。返回 (值, ready, skipped)，不把跳过伪装成未就绪 |

没有 OR 调度。分支靠分类节点对未选中的 Provide 调用 `Skip`，下游 AND 节点因输入跳过而不执行。

### 跳过传播（第一版就钉死）

节点被提交后阻塞在 Requires 上。醒来时：

1. **全部 Requires 已就绪(值)** → 执行 `Run`。`Run` 返回 nil 且 Provides 都已 Set 或 Skip，节点正常结束。未写的 Provides 在 `Run` 返回后**自动 Skip**（节点有责任写完；漏写 = 这条输出被跳过，不是永远阻塞下游）。
2. **任一 Requires 为跳过** → **不执行 `Run`**，全部 Provides 标记跳过，级联。
3. **`Run` 返回 error** → 记录首错 + 取消整次运行。不把失败翻译成跳过。
4. **节点自己对某条 Provide 调用 `Skip`** → 只跳过那一条，其余 Provides 仍按 1 处理。这是分支的一等表达：分类节点对未选中的边 `Skip`，对选中的边 `Set`。

工作流结束条件：所有节点进入终止态（跑完 / 被跳过 / 因首错取消）。没有人永远等。

`ErrSkipped` 不是工作流失败。`g.Err()` / `g.Run()` 只反映首个节点 error 或取消；跳过是正常终止路径。

### 调度

- 全量提交 + 阻塞等数据。去掉 ants 池、`GetStats`、`ResizePool`。
- `WithMaxRunning(n)`：`n<=0` 无限（默认）；`n>0` 为进入 `Run` 前的信号量。等数据不占名额，拿到全部输入、真正执行时才 Acquire。
- `Graph` API：`New` / `Add` / `Seed` / `SkipSeed` / `Run` / `Start`+`Wait` / `Err`。
- `Add` 拒绝：空 id、重复 id、同一节点重复 Require/Provide、同一节点既 Require 又 Provide 某 Key、两个节点 Provide 同一 Key。
- 每个 Key 至多一种来源身份：外部 `Seed` / `SkipSeed`，或一个节点的 `Provides`，二者不可并存。外部来源可重复声明：重复 `Seed` 按槽位首写处理，重复 `SkipSeed` 幂等，`Seed` 与 `SkipSeed` 的值/跳过冲突返回 `ErrConflict`；不同来源身份冲突返回 `ErrDuplicateSource`。

### 切面

```go
type Aspect interface {
    Around(rc *flow.RunCtx, next func(*flow.RunCtx) error) error
}
```

- `rc.Fork()` 只派生取消上下文，不复制写入记录和声明权限；core 用切面级 ctx 做 `Get`/`WaitAll`，超时能打断等数据。
- 全局切面 + 节点切面，先全局后节点，外层先跑。每个切面每次 `Around` 调用只能调用 `next` 至多一次；重复（含并发）调用返回 `ErrNextCalledTwice`，节点不会重入。
- **内建、不可关**：Recovery（panic → 首错 + 取消）。
- **可选**：Timeout、Retry。
- **不移植**：
  - CircuitBreaker：状态跨运行存活，是服务治理，不属于「一次运行一个世界」。
  - ErrorSwallow：把节点失败变成工作流成功，与「失败显式」冲突。需要降级就在节点里 `Set` 降级值，或 `Skip` 未走的边。

### 与 loop

正交。节点函数里可以构造 `loop.Agent` 跑模型回合；`loop` 不感知 flow。

## 语义钉死

- 同名不同类型的 Key 拒绝。
- Set 二次静默忽略；Set 与 Skip 冲突显式报错。
- 每个 Key 至多一种来源身份：外部 Seed/SkipSeed，或一个节点输出；重复外部声明仍按槽位首写与值/Skip 冲突规则处理。
- 漏写的 Provides 在 Run 返回后自动 Skip。
- 输入被跳过 → 不跑 Run，输出全跳过。
- 节点 error ≠ 跳过。error 取消整图；跳过只影响依赖链。
- `Run()` 的 error 不含 `ErrSkipped`。
- 运行数据不进 kernel 服务仓库。

## 不做

持久化、断点续跑、分布式、服务治理（熔断）、默认吞错、字符串 key、拓扑排序器、固定 goroutine 池。

条件节点是建立在 Skip 级联上的语法糖（分类节点 Set 一边、Skip 另一边），不另开 OR 调度。不做 WaitAny / 竞速汇聚 / SetOrUpdate。

## 验收

1. 线性：A→B→C，类型安全 Get/Set，二次 Set 忽略。
2. 并行汇聚：C 的 Requires(Aout, Bout) 在 A、B 都完成后才跑。
3. 分支：分类节点 Set 一边、Skip 另一边；未选中下游不执行，工作流正常结束。
4. 级联跳过：A skip → B（依赖 A）不跑且输出 skip → C（依赖 B）不跑。
5. 多生产者 Provide 同一 Key → Add 拒绝。
6. 节点 error → Run() 非 nil，未跑完的节点因取消结束，不把失败写成 skip。
7. Timeout 切面取消能打断 Wait；Recovery 把 panic 收成首错。
8. `WithMaxRunning(1)` 时两个无依赖节点串行进入 Run。
9. `go test -race ./kernel/flow/` 全绿。

## 演进路线

> 状态：方向共识（2026-08-27），按依赖关系排序；每一步动工前单独开 Issue 钉边界，不在此处预先实现。

### E1 观测 seam：节点生命周期事件

**现状缺口**：切面只有一个 `Around(rc, next)`，包住「等待输入 + 执行」整段。观测方拿不到「等待输入何时开始/结束」，只能给出 node_total_ms；examples 的观测原型因此明确拒绝伪造 wait/run 拆分（见 examples/03-flow-agent/README「时间统计怎么读」）。

**第一消费者是装配层桥**（demoapp/bridge.go，已有 FlowAspect 原型）：flow 出 typed 事实，桥折成日志写 Sink——正式 observability 包不 import flow，不做消费者（见 [observability-v1-design.md](observability-v1-design.md) D1），避免从后门把业务依赖请回核心观测包。

**计划**：在框架内增加三个只读事件位——NodeWaiting（开始阻塞等输入，即进入 WaitAll）、NodeRunning（拿到全部输入、Acquire 之后、进入用户 Run 之前）、NodeFinished（终止态 + 状态原因）。粒度与 examples/03 的诚实边界对齐；注意 Skip 与超时路径没有 Running，直接 Finished。验收证据：装配层桥能输出单节点的 wait_ms / run_ms 分段。

**非目标**：不做指标聚合、导出器、采样配置——那些是 observability 包的事，flow 只暴露事实。

### E2 结构化编排：JSON/YAML 流程定义（更先进形态）

**方向**：用 JSON/YAML 等结构化语言声明流程图——节点列表、Requires/Provides 关系、Seed 输入、可选的 Timeout/Retry 参数——由运行时装配成 Graph。目标是让流程描述脱离 Go 源码，可被配置管理、跨语言工具消费。

**前置依赖**（缺一不动工）：

1. **节点注册语义稳定**：序列化的只是「ID + 声明 + 配置」，节点实现必须是具名可寻址的（类似 kernel Loader 的 Factory 注册表）。Add 期全部校验规则（来源唯一、自环拒绝等）原样适用于反序列化路径；
2. **可序列化的输入 Schema**：多模态消息、附件字节不适合直接放 YAML——需要定义外部输入的引用方式（如 Seed 引用文件/环境/上游服务），这可能与记忆层的 Session/Context 设计产生交集，需先对齐；
3. **E1 落地且装配层能消费 wait/run**：声明式图的问题定位比代码图更依赖节点分段耗时；kernel 装配日志解决不了这类排障，E1 是它的排障前提。

**明确的边界立场**：YAML 编排不引入 OR/竞速、不改 AND 汇聚语义、不新增表达式求值引擎做条件路由——条件分支仍然是「分类节点 Set/Skip」。声明式是图的**另一种书写方式**，不是另一种执行模型。

### 与其他组件的关系

- **E1 → 装配层桥**：flow 出 typed 事实；桥折成 wait_ms/run_ms 写 Sink。正式 observability 包不 import flow，只提供信封与出口；
- **E2 → memory 层**：外部输入引用若涉及会话/上下文来源，遵循 memory 设计稿中「model-visible 投影不可破坏」的不变式；
- 全部演进不破坏本篇已钉死的契约：三态槽位、AND 汇聚、来源唯一、失败显式。
