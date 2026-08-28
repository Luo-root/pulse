# flow v2：数据就绪驱动的节点编排

> 状态：Accepted（设计拍板 2026-08-26）
> 实现状态：**核心契约已落地**（`kernel/flow/` + 包 README + 测试 + examples/03）；**E1 已落地**（Issue [#25](https://github.com/Luo-root/pulse/issues/25) / PR #28）；**E2 已落地**（Issue [#32](https://github.com/Luo-root/pulse/issues/32) / PR #33：`Registry` + `kernel/flow/yaml`）
> 包位置：`kernel/flow/`
> 公开 API 权威摘要：[`kernel/flow/README_zh.md`](../../kernel/flow/README_zh.md)（本篇保留理念、钉死契约与演进；API 段为与实现同形的摘要，避免双维护完整导出表）
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

## 已实现 API（摘要）

完整导出表与用法见包 README。此处只钉与契约相关的形态，避免与 README 双维护：

```go
package flow // import "github.com/Luo-root/pulse/kernel/flow"

var Summary = flow.NewKey[string]("summary")
var Docs    = flow.NewKey[[]Doc]("docs")

g := flow.New(ctx,
    flow.WithMaxRunning(0),       // <=0 = 无限（默认）
    flow.WithAspects(/* 全局 */), // 可选
)
_ = g.Add(flow.NewNode("summarize",
    flow.Requires(Docs),
    flow.Provides(Summary),
    func(rc *flow.RunCtx) error {
        docs, err := flow.Get(rc, Docs) // 类型安全；跳过 → ErrSkipped / *SkipError
        if err != nil {
            return err
        }
        return flow.Set(rc, Summary, summarize(docs))
    },
))
_ = flow.Seed(g, Docs, input) // 包级泛型函数，不是 g.Seed
err := g.Run()
```

- 异型 Requires/Provides 合并用 `Deps(...)`；节点级切面是 `NewNode` 末尾 variadic `Aspect`。
- 外部输入：`Seed` / `SkipSeed`（包函数）。图执行：`Run` 或 `Start`+`Wait`；`Err()` 读首错。
- 读 API（`Get` / `TryGet` / `WaitAll`）校验的是节点 `allowed`（Requires∪Provides）；**写**（`Set`/`Skip`）仍只允许声明过的 Provides。
- 契约错误常量：`ErrSkipped`（及带 Keys 的 `*SkipError`）、`ErrConflict`、`ErrUndeclared`、`ErrDuplicateSource`、`ErrNextCalledTwice`、`ErrGraphStarted`、`ErrGraphNotStarted`。

### Key

- `Key[T]`：name + 类型。同名不同类型在 `NewKey` / 注册期拒绝（对齐 `ServiceKey`）。
- 读（`Get` / `TryGet` / `WaitAll`）校验节点 `allowed`（Requires∪Provides）；写（`Set` / `Skip`）只允许 Provides。碰未声明的 Key → 显式错误，不是静默写到黑板上。
- **惯例**：不要 `Get` 自己尚未写出的 Provide——会阻塞到自己（或清理路径）写入，通常是逻辑错误；读 Provides 主要给切面/诊断，不是鼓励自读未写输出。

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
3. **`Run` 返回 error** → 记录首错 + 取消整次运行。工作流语义上不把失败翻译成 `ErrSkipped`；但框架仍可对未写 Provides 做**取消清理 Skip**（见「语义钉死」），以免等待者死等。
4. **节点自己对某条 Provide 调用 `Skip`** → 只跳过那一条，其余 Provides 仍按 1 处理。这是分支的一等表达：分类节点对未选中的边 `Skip`，对选中的边 `Set`。

工作流结束条件：所有节点进入终止态（跑完 / 被跳过 / 因首错取消）。没有人永远等。

`ErrSkipped` 不是工作流失败。`g.Err()` / `g.Run()` 只反映首个节点 error 或取消；跳过是正常终止路径。

### 调度

- 全量提交 + 阻塞等数据。去掉 ants 池、`GetStats`、`ResizePool`。
- `WithMaxRunning(n)`：`n<=0` 无限（默认）；`n>0` 为进入 `Run` 前的信号量。等数据不占名额，拿到全部输入、真正执行时才 Acquire。
- `Graph` API：`New` / `Add` / `Run` / `Start`+`Wait` / `Err`；外部输入是包函数 `Seed` / `SkipSeed`。
- `Add` 拒绝：空 id、重复 id、同一节点重复 Require/Provide、同一节点既 Require 又 Provide 某 Key、两个节点 Provide 同一 Key。
- 每个 Key 至多一种来源身份：外部 `Seed` / `SkipSeed`，或一个节点的 `Provides`，二者不可并存。外部来源可重复声明：重复 `Seed` 按槽位首写处理，重复 `SkipSeed` 幂等，`Seed` 与 `SkipSeed` 的值/跳过冲突返回 `ErrConflict`；不同来源身份冲突返回 `ErrDuplicateSource`。

### 切面

```go
type Aspect interface {
    Around(rc *flow.RunCtx, next func(*flow.RunCtx) error) error
}
```

- `rc.Fork()` 只派生取消上下文，不复制写入记录和声明权限；core 用切面级 ctx 做 `Get`/`WaitAll`，超时能打断等数据。
- 全局切面 + 节点切面，先全局后节点，外层先跑。每次 `Around` 内 **禁止并发/重叠** 调用 `next`（返回 `ErrNextCalledTwice`）；**允许顺序多次**（`Retry` 依赖此语义）。E1 生命周期事件靠每节点门闩保证 Waiting/Running 仍至多一次。
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
- 漏写的 Provides 在 Run **成功返回**后自动 Skip。
- 输入被跳过 → 不跑 Run，输出全跳过。
- **节点 error ≠ 跳过（工作流语义）**：error 记录为首错并取消整图；`Run()` / `Err()` 返回该 error，**不会**把失败伪装成 `ErrSkipped`。
- **取消清理 Skip（实现契约，2026-08-27 拍板）**：节点因 error / 输入 Skip 结束时，框架仍可对**未写入的 Provides** 执行 Skip，目的只是解开仍阻塞在这些槽位上的等待者，避免图在取消路径上死等。这不等于「失败翻译成跳过」——下游若因清理 Skip 而不跑，是解阻塞的后果；调用方判断失败仍只看 `Run()` 的 error。
- `Run()` 的 error 不含单纯的 `ErrSkipped`（跳过是正常终止路径）。
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
6. 节点 error → Run() 非 nil（不是 ErrSkipped）；未跑完的节点因取消结束；未写 Provide 可被清理 Skip 解阻塞。
7. Timeout 切面取消能打断 Wait；Recovery 把 panic 收成首错。
8. `WithMaxRunning(1)` 时两个无依赖节点串行进入 Run。
9. `go test -race ./kernel/flow/` 全绿。

## 演进路线

> 状态：方向共识（2026-08-27），按依赖关系排序；每一步动工前单独开 Issue 钉边界，不在此处预先实现。

### E1 观测 seam：节点生命周期事件

> 跟踪：[Issue #25](https://github.com/Luo-root/pulse/issues/25)
> 实现状态：**已落地**（`WithObserver` + `runNode` 埋点 + demoapp 桥两条 Record）；规格拍板见下。
> 拍板补充（2026-08-27/28）：派发载体 = **flow 自有 typed observer**；与正式 `observability/` 的关系见下。

**当时缺口（已解决）**：E1 开工前，切面只有一个 `Around(rc, next)`，包住「等待 + 执行」整段，桥只能记 total（旧事件名 `flow.node_finished`），并拒绝伪造 wait/run。现已由 `WithObserver` + 桥两条 Record（`flow.node_wait_finished` / `flow.node_run_finished`，`FiberName`=nodeID）落地；下文保留分层与契约，不再用现在时描述旧缺口。

#### 与 observability 的分层（钉死）

```text
kernel/flow
  │  NodeWaiting / NodeRunning / NodeFinished
  │  flow 自有 typed observer（默认 no-op）
  ▼
装配层桥（demoapp.Bridge / 未来宿主）
  │  订阅 flow observer；折成 **两条** Record（source=bridge）
  │  flow.node_wait_finished / flow.node_run_finished
  │  各用现有 Duration；填 HostID / TraceID / FiberName=nodeID → Sink.Write
  ▼
observability/
  │  只提供 Record / Sink / Bootstrap
  │  只依赖 kernel（fiber_state / loader_action）
  ✗  不 import flow，不订阅 Node* 事件
  ✗  官方 Record **不扩** WaitMs/RunMs 字段
```

| 层 | 认识什么 | 不认识什么 |
|---|---|---|
| flow observer | 节点生命周期事实 | Sink、HostID、TraceID、slog |
| 装配层桥 | flow 事件 + 请求 TraceID + Sink | 不进正式观测包源码树 |
| `observability/` | kernel 装配事件 + 通用信封/出口 | llm / loop / **flow** |

一句话：**observer 是 flow 的扩展 seam；observability 仍是通用信封；桥是唯一把二者接上的地方。** 对齐 [observability-v1-design.md](observability-v1-design.md) D1。

#### 派发载体（钉死）

- flow **自有** typed observer / 钩子，默认 no-op；`WithObserver(...)`（名称实现时可微调）挂到 Graph。
- **禁止** E1 直接 `kernel.Emit`：flow 必须保持「只吃 `context.Context`、可独立使用」，不强迫 import kernel 事件总线。
- flow **禁止** 直接 `observability.Sink.Write` 或 import 正式观测包。
- Aspect（Timeout/Retry）继续做控制流；**wait/run 必须来自新事件分段**，禁止再用 `Around` 整段耗时冒充分段。

#### 桥如何把 wait/run 写进信封（钉死，2026-08-28）

官方 `Record` 只有一个 `Duration`，且 observability-v1 **明确官方 Record 不扩字段**（token 走 slog 附加键）。因此桥侧采用**两条记录**，各用现有 `Duration`：

| 桥事件名 | Duration 含义 | 何时写 | 节点身份 |
|---|---|---|---|
| `flow.node_wait_finished` | wait 段墙钟 | 离开 Waiting（进入 Running，或因 Skip/超时直接 Finished） | `FiberName` = nodeID（借诊断名槽位，不扩信封） |
| `flow.node_run_finished` | run 段墙钟 | 仅当发过 Running 时，在 Finished 时写 | 同上 |

不写第三条「total」冒充分段；需要 total 时由消费方把两段相加。验收「分别断言 wait/run」对这两条 Record 按 `FiberName` 区分节点。

#### 三个只读事件

| 事件 | 触发点（对照 `runNode`） | 备注 |
|---|---|---|
| NodeWaiting | 进入 WaitAll 之前 | 开始阻塞等输入 |
| NodeRunning | WaitAll 成功且 `acquire` 之后、用户 `Run` 之前 | Skip / 超时打断 Wait → **不发** Running，直接 Finished |
| NodeFinished | 节点终止态已确定之后 | **在 skip 清理之后**发，带状态原因（completed / skipped / failed / canceled） |

**每节点次数（钉死）**：Waiting ≤ 1、Running ≤ 1、Finished = 1。`Retry` 的 `next()` 包住整段 core；多次 attempt **不得**重复打 Waiting/Running。Retry 的多次 Run（含 delay）计入同一次 Running→Finished 的 run 墙钟。

Timeout 现返回 `fmt.Errorf("flow: node … timeout…")`，不是 `context.Canceled`：Finished 原因归 `failed` 即可；`canceled` 留给图/ctx 取消。

验收证据：装配层桥能对线性链节点分别断言两条 Duration；Skip 级联与 Timeout 打断 Wait 的路径有 Finished、无 Running、无 `flow.node_run_finished`。

#### 实现约束

- observer 从 `runNode` 的每节点 goroutine 调用 → **必须并发安全**，或由 Graph 串行回调
- observer panic / error **不得**变成节点失败（只读 seam）
- 埋点位置在 innermost core 时，须用「每节点已发过 Waiting/Running」门闩，防止 Retry 双打点

#### 非目标

- 指标聚合、导出器、采样配置（observability / 宿主的事）
- 正式观测包增加 `OnFlowNode*` API，或给 Record 加 `WaitMs`/`RunMs`
- 改 AND / Skip / 失败显式 / 取消清理 Skip 等已钉死契约
- E2 JSON/YAML（另项）

### E2 结构化编排：JSON/YAML 流程定义（更先进形态）

> 跟踪：规格 [#29](https://github.com/Luo-root/pulse/issues/29)；实现 [#32](https://github.com/Luo-root/pulse/issues/32) / PR #33
> 实现状态：**已落地**（`Registry` + `kernel/flow/yaml.Load` / `SeedPlan`）；规格补钉（2026-08-28）下列条款钉死。

**目标（降调）**：用 JSON/YAML 声明流程图——节点列表、Requires/Provides、Seed 引用、可选内建 Timeout/Retry——由运行时装成 Graph。诚实目标是**配置与代码分离、可审查、可被工具生成**。节点实现仍是 Go 工厂时，YAML **不能**跨语言执行；「跨语言工具消费」不是本阶段目标。

**前置依赖**：

| # | 前置 | 状态 |
|---|---|---|
| 1 | 节点注册语义稳定（具名工厂，不进 Loader） | **已落地**（`NewRegistry` / `Register` / `RegisterKey`） |
| 2 | Seed 外部输入引用 Schema | **已落地**（`SeedPlan` + `literal|env|file|context`；IO 在宿主） |
| 3 | E1 落地且桥能消费 wait/run | **已满足**（#25 / #28） |

#### 边界立场（钉死）

- 声明式是图的**另一种书写方式**，不是另一种执行模型。
- 不引入 OR / WaitAny / 竞速；不新增表达式引擎做条件路由——条件仍是分类节点 Set/Skip。
- 声明式**只**覆盖内建 `Timeout` / `Retry`（duration、attempts 是数据）。自定义 `Aspect` **不能**进 YAML，仍走代码 `NewNode(..., aspect)`。
- 不改 AND / 三态槽位 / 失败显式 / 取消清理 Skip / E1 Observer。

#### 节点注册（钉死：YAML 拥有拓扑；Factory 只给 Run）

「类似 Loader」只指**具名工厂表**的形状，**禁止**把节点理解成 `kernel.Plugin`（P3 已废弃）。

**拓扑归属 A（2026-08-28 拍板）**：声明式既是另一种书写方式，**id / requires / provides 以 YAML 为准**。Go 侧工厂**只提供** `Run`（及可选的「代码侧切面」挂载点），不返回完整 `*Node`——否则 YAML 再写一遍边会变成废话或双源冲突。

```go
// 草图：名称实现时可微调；语义钉死。
type RunFunc func(*RunCtx) error
type NodeFactory = RunFunc // 只给 Run；不进 kernel.Loader；不返回 *Node

type Registry struct { /* 实例，非包级全局 */ }
func NewRegistry() *Registry
func (r *Registry) Register(name string, f NodeFactory) error
func (r *Registry) MustRegister(name string, f NodeFactory)
```

- 用 **`NewRegistry()` 实例**，不用包级全局 `flow.Register`（避免测试互相污染；装配期对象，不属于「一次运行一个世界」）。
- YAML **必填** `id` / `uses` / `requires` / `provides`；`uses` 解析为 `reg` 上的工厂名。
- 装图：`NewNode(id, requires, provides, reg.Lookup(uses), timeout?, retry?)` → `g.Add`——**Add 期全部校验原样适用**。
- 禁止 B 形态：Factory 返回完整 `*Node` 且 YAML 再声明边（除非实现 Issue 明确改拍板）。

#### Key 序列化（钉死）

`Key[T]` 在 YAML 里不能只写名字（会丢掉类型校验）。装图期编码：

```yaml
# name = Key 的稳定名；type = 注册期约定的类型记号（与 Go 侧 NewKey[T] 绑定）
requires:
  - { name: demo.user_input, type: "*llm.Message" }
provides:
  - { name: demo.query_text, type: string }
```

- 装图前 Registry / 类型表须能把 `(name, type)` 解析回已登记的 `Key[T]`；同名异型在装图期拒绝（对齐 `NewKey`）。
- 具体 `type` 字符串规范（包路径？短名？）实现 Issue 再定，但**必须带类型维度**。

#### Seed 引用枚举（钉死方向）

多模态字节不进 YAML 正文。Seed 只写引用：

| kind | 含义 | 例 |
|---|---|---|
| `literal` | 小标量 / JSON 字面量 | `value: "hello"` |
| `env` | 环境变量 | `env: PULSE_DEMO_QUERY` |
| `file` | 工作区内文件路径 | `path: ./input.json`（由宿主读入再 Seed） |
| `context` | 宿主注入的请求上下文键 | `key: user_message`（与 memory/session 对齐时遵守 model-visible） |

`SkipSeed` 对应 `skip: true` 或显式 skip 条目。装图 API 只生成「要 Seed / SkipSeed 哪些 Key」的计划；**实际读文件/环境由宿主执行**，flow 不偷偷 IO。

#### 最小 YAML 例子（3 节点 + Seed）

```yaml
# 示意：与 examples/03 同构的线性链；不是最终 schema 冻结稿
version: 1
seeds:
  - key: { name: demo.user_input, type: "*llm.Message" }
    from: { kind: context, key: user_message }
nodes:
  - id: extract_text
    uses: demo.extract_text          # Registry 工厂名
    requires: [{ name: demo.user_input, type: "*llm.Message" }]
    provides: [{ name: demo.query_text, type: string }]
  - id: retrieve
    uses: demo.retrieve
    requires: [{ name: demo.query_text, type: string }]
    provides: [{ name: demo.context_docs, type: "[]Document" }]
    retry: { attempts: 2, delay: 100ms }   # 仅内建 Retry
  - id: answer
    uses: demo.answer
    requires:
      - { name: demo.user_input, type: "*llm.Message" }
      - { name: demo.query_text, type: string }
      - { name: demo.context_docs, type: "[]Document" }
    provides: [{ name: demo.final_text, type: string }]
    timeout: 30s                           # 仅内建 Timeout
observer: host                             # 文档提示位：Load **忽略**；观察者走 LoadOptions.Graph / WithObserver
```

`version`：缺省或 `1` 接受；其它值拒绝。`observer` 字段**不是开关**——解码保留以免未知键报错，装图不解释。

装图伪代码：

```text
Parse(YAML)
  → 对每个 node：
       run := reg.Lookup(uses)                 // 只拿到 RunFunc
       n   := NewNode(id, requires, provides, run,
                      Timeout?, Retry?)      // 拓扑来自 YAML
       // Timeout 在外、Retry 在内（默认洋葱顺序，与「外层先跑」一致）
       g.Add(n)
  → 返回 Graph + SeedPlan
宿主执行 SeedPlan（读 env/file/context）后再 g.Run()
```

#### 实现约束 / 非目标

- YAML 解析放到 **`kernel/flow/yaml`**（推荐），避免把 yaml 依赖塞进 flow 核心；核心仍不得 import observability / llm（Key 类型记号是字符串）。
- `(name, type) → keyRef` 需要登记表（宿主或 flow 侧类型表在装图前注入），**不要**在装图包里 `import llm` 只为拿类型。
- 同一节点同时写 `timeout` + `retry` 时默认：**Timeout 在外、Retry 在内**。
- 不做：自定义 Aspect 序列化、表达式条件、跨语言运行时、把节点注册进 `kernel.Loader`、Factory 返回完整 `*Node` 与 YAML 双写拓扑。

### 与其他组件的关系

- **E1 → 装配层桥**：flow 出 typed 事实；桥折成两条 Record（`flow.node_wait_finished` / `flow.node_run_finished`，各用 `Duration`，`FiberName`=nodeID）写 Sink。正式 observability 包不 import flow，只提供信封与出口；
- **E2 → memory 层**：`context` 类 Seed 若涉及会话/上下文来源，遵循 memory 设计稿中「model-visible 投影不可破坏」的不变式；
- 全部演进不破坏本篇已钉死的契约：三态槽位、AND 汇聚、来源唯一、失败显式、E1 Observer。
