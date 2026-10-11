# 对象模型与契约（#311 阶段零）

> **本文状态：阶段零已定案（2026-10-11）。** 它是 [#311](https://github.com/Luo-root/pulse/issues/311)「阶段零」的产物：
> 先把 Graph / Execution 的职责、边界与契约定下来，**再**动代码。第 8 节列出**已定案**与
> **暂定（评审时确认或推翻）** 两批；定案部分与 `pulse.md` 的冻结面那一节同步。
>
> 读法：本文描述**目标模型**；今天的实现（一次性 `Graph`）在 [`pulse.md`](pulse.md)，
> 「现状 vs 目标」的逐条差距在下面第 2 节。设计**理念**（为什么这样取舍）在
> [`philosophy.md`](philosophy.md)。

## 1. 一句话

**`Graph` 是可复用的静态定义，`Execution` 是一运行实例。**

- `Graph`：节点、依赖（由 Key 推出）、Key 类型声明、子图组合、输入 / 输出接口、描述、静态策略声明（默认切面 / 默认限额）。
- `Execution`：这次运行的 ctx / cancel、输入值、槽位、每节点状态、首错、调度与限流计数、观察者身份、运行时子执行句柄。

同一张 `Graph` 应当能被：顺序执行多次（每次全新且隔离）、并发执行多次（在用户回调自身的并发安全约定之下）、在不同父图里作为子图被挂载多次（每次挂载位置各有自己的 `Execution`）。一次 `Execution` 的取消 / 失败 / 完成**不**污染定义，也不污染别的 `Execution`。

## 2. 现状核实（读码定位，2026-10-11）

| 今天的形状 | 住哪 | 与新模型的关系 |
|---|---|---|
| `Graph` 同时持定义与一次运行的全部字段：`ctx/cancel/slots/slotsMu/states/err/started/claimed/done/wg/finishOnce` | `graph.go:16-70` | **拆**：定义留 `Graph`，运行搬 `Execution` |
| `Graph.sem` 是**死字段**（全仓无读无写；名额在 `limiter.sem`） | `graph.go:62`；`limiter.go:14` | 迁移时删掉（`g.lim.sem` 才是活的，`graph_test.go:1119` 读的是它） |
| `New(ctx, graphID, description, opts…)`：ctx 属于**定义** | `graph.go:151` | ctx 移到 `NewExecution`；`New` 只收定义信息 |
| `Seed` / `SkipSeed` / `SeedByName` / `SkipSeedByName`：既是「声明外部来源」又是「写这一轮的值」 | `graph.go:298`+、`coerce.go` | **拆**成两件事：静态端口声明 + 本次执行输入 |
| `Sub(g, id, desc, binds, build)`：子图在**运行期**由 `build(*SubCtx)` 造，**绑定校验也在运行期**（`runSub` 建完子图才比对键存在与类型） | `sub.go:173`、`sub.go:229`（`runSub`，绑定校验在 `sub.go:284-294`）；`SubCtx` 在 `sub.go:11-49` | 主路径改成**组合现成 `*Graph`**；动态 builder 降为显式能力；绑定校验进装配期与 `Validate` |
| `claimSub` + `adopt`：认领一次性 + **改写子图字段**（`path` / `observer` / `lim` / `aspects`） | `sub.go:269`、`sub.go:313`、`sub.go:388`+ | 由「创建子 Execution 时算有效配置」取代，定义不再被写 |
| `Start` / `Wait` / `Run` / `Err` 都在 `Graph` 上；`Start` 只能成功一次 | `graph.go:384-540` | 搬到 `Execution`；`Graph` 只留同步便捷入口 |
| `slot` 是「到达即发布、幂等首写」，没有 attempt 层 | `slot.go:42-73` | 每个 attempt 一份写暂存，成功才提交（第 5 节） |
| `Retry` 的文档明说「**重试安全的前提**：失败前没有写过任何 Provide」 | `aspect.go:67-75` | 这条前提改由引擎接管——本票硬要求 |
| `Topology()` 只覆盖**一张图**，且 `GraphShape.Path` 由 `Sub` 在运行期盖章 | `topology.go:107-177`；`sub.go:313` | Topology 递归全树；path 由**挂载位置**算，不改定义 |
| `Graph.Snapshot()` + `NodeState.Child *Graph` 建在「Graph = 一次运行」之上 | `snapshot.go`；PR #310 | 形状要跟着模型走（第 6 节） |
| `Validate()` 只看本图；`Problems` 一次给全、`Start` 报第一条 | `topology.go:122-130`、`problems.go` | 组合**整棵树**校验；错误仍在最早可判定处报 |
| YAML 的 `Load` 返回 `(*Graph, *SeedPlan)`，子图也在运行期建 | `yaml/load.go`、`yaml/subgraph.go` | 结构语义与 Go 组合对齐；`SeedPlan` 只负责**本次执行**的输入 |
| 限流 / 切面 / 观察者靠 `adopt` **继承**（写进子图字段） | `sub.go:313`、`limiter.go:5-13` | 改成「执行树创建时计算有效值」，不动共享定义 |

## 3. 职责与生命周期

| 对象 | 持有 | **不**持有 | 生命周期 | 谁创建 |
|---|---|---|---|---|
| **Graph**（定义） | 节点表、来源表、Key 声明表、**端口**（开放输入 / 输出）、静态子图挂载、描述、静态策略声明 | ctx / cancel、槽位、节点状态、首错、waitgroup、调度计数、观察者身份、路径 | 装配 → 封口 → 进程结束；封口后结构不变 | 宿主（`New` + `Add` / `DeclareInput` / `Sub`） |
| **Execution**（运行） | ctx / cancel、输入值、slot map、每节点状态、首错、done / wg、有效限额（引用或自有）、有效切面、观察者（含执行身份与 path）、运行时子执行句柄 | 节点定义本体（读 `Graph` 的） | `NewExecution` → `Start` → `Wait` → 回收；一次性 | 宿主，或父 Execution（为子图节点） |
| **Node**（定义） | `id` / `desc` / `requires` / `provides` / `run` / 节点级切面 | 任何运行状态（同一 Node 可被多图、多次执行复用） | 与定义同寿 | 宿主 |
| **静态 Child** | `Topology().Nodes[i].Sub`：这一步是一张子图的**定义**（递归） | 运行状态 | 与定义同寿 | 由 `Sub` 装配时挂上 |
| **运行时 Child** | `Snapshot().Nodes[i].Child`：本次挂载位置的**子 Execution** | 定义本体 | 随父 Execution | 父 Execution 在跑到那一步时创建 |

### 3.1 输入 / 输出接口（三层，别再混）

1. **端口声明（静态）**：`g.DeclareInput(...)` 声明「这张图接受外部喂这几条 Key」。它参与拓扑与静态校验——声明过的 `Requires` **不再**算「无来源」。
2. **组合接线（静态）**：`Sub(g, id, desc, binds, child)` 把父侧的 Key 接到子图的端口上（沿用 `In(父, 子)` / `Out(子, 父)` 的方向语义）。父图装配完就能递归校验整棵树。
3. **本次输入（运行）**：`Execution` 的入参提供实际值 / 显式跳过。根图这一层是宿主给的；子图这一层由父 Execution 按绑定喂进去。YAML 的 `SeedPlan` 属于这一层——它**不**再往定义里写东西。

校验分工：`Validate()` 只查静态结构与组合约束（不要求运行值存在）；`NewExecution` 查「这次运行需要的输入齐不齐」（这是启动条件，不是静态毛病）。

### 3.2 运行配置属于执行树，不写回定义

ctx、路径、观察者、切面、限额都是**一次嵌套执行环境**的属性。创建子 Execution 时按「父执行环境 + 子图自己的声明」算出有效值（默认继承、`WithoutAspects()` 切断切面、限额共享同一棵执行树的计数、`Sub` 那一步本身不吃名额）。定义保持只读，因此「同一份定义被两处挂载」不会再互相踩。

### 3.3 身份与观测归因

| 记号 | 含义 | 今天 |
|---|---|---|
| 图 id | **定义**身份（`New` 给的） | `pulse.graph` |
| 执行 id | **本次运行**身份（根执行由宿主给或自动生成，子执行由父执行派生） | 无 |
| path | 挂载位置（根 `""`，一层 `step`，再深 `step/inner`） | `pulse.path` |

要求：同一张 Graph 的多次执行在观测里**分得开**（否则并发执行的记录会合并成同一份，归因直接错）。

## 4. 目标 API 形状（草案，待冻）

```go
// —— 定义侧（可复用、可组合、封口后不可变）——
func New(graphID, description string, opts ...Option) (*Graph, error)
func (g *Graph) Add(n *Node) error
func (g *Graph) DeclareInput(ports ...InputPort) error   // 静态端口（名字 + 类型）
func (g *Graph) Seal() error                             // 可选：显式封口（装配期立刻冻结）
func (g *Graph) Topology() *Topology                     // 静态全树（递归，含端口与绑定）
func (g *Graph) Validate() error                         // 静态结构 + 组合约束

// —— 组合：现成子图优先；动态 builder 是显式能力 ——
func Sub(g *Graph, id, description string, binds []SubBind, child *Graph, opts ...SubOption) error
func SubFunc(g *Graph, id, description string, binds []SubBind,
	build func(*ExecCtx) (*Graph, error), opts ...SubOption) error // 运行期才决定结构

// —— 运行侧 ——
func (g *Graph) NewExecution(ctx context.Context, in Inputs, opts ...ExecOption) (*Execution, error)
func (e *Execution) Start() error
func (e *Execution) Wait() error
func (e *Execution) Err() error
func (e *Execution) Snapshot() *Snapshot
func (e *Execution) ID() string

// —— 同步便捷入口（每次调用新建一份 Execution）——
func (g *Graph) Run(ctx context.Context, in Inputs, opts ...ExecOption) error
```

形状上的取舍（写下来免得回头重开）：

- `Run` 留在 `Graph` 上，但**不**在 `Graph` 上留 `Start` / `Wait` / `Snapshot`——避免「隐式 Execution」这种半吊子状态。
- `Inputs` 是**值**（builder 风格：`in.Set(k, v)` / `in.Skip(k)`），不是变参 Options：一次运行的输入是一份数据，不是一串装饰。
- `NodeState.Child` 从 `*Graph` 变成 `*Execution`；`Topology().Nodes[i].Sub *Topology` 承载静态子树，`Binds` 承载父子接线。
- **不**另造 `GraphTemplate` / `GraphPlan`：`Graph` 本身就是那个可复用定义。

## 5. 事务性 Retry（本票硬要求）

今天的形态：槽位「到达即发布、幂等首写」，`Retry` 重跑时前一次 attempt 已经 `Set` 过的槽位**已经发布**——下游被唤醒，第二次 attempt 的写还会被静默忽略（`aspect.go:67-75` 明说这条前提由调用方守）。新模型把它收进引擎：

1. **attempt 写集**：`Set` / `Skip` 先落到**本 attempt 的写集**，不直接进执行槽位；
2. **读自己写的**：attempt 内 `Get` 自己已写的 Provide 读到的是**本写集**的值（自洽）；
3. **丢弃**：attempt 失败且还要重试 → 整个写集丢掉，**下游从未见过**；
4. **提交**：attempt 成功（或在拿到 `Retry` 之后明确以跳过收尾）才把写集写进执行槽位；不同 attempt 的输出不会混写；
5. **跳过 ≠ 可重试失败**：`NoValue` / `*SkipError` 不触发重试（今天的判据照旧：`isSkipped`）；取消 / 截止时间到达不继续重试；
6. **最终失败**：写集丢弃 + 未写输出按既有语义自动跳过 + 错误原样冒泡（槽位终态与节点终态一致，可测）；
7. **切面顺序决定事务边界**：`Retry` 包住的那一段（`next` 之内、含更内层切面）属于 attempt；`Retry` **外层**切面里的写入**不**属于任何 attempt、不参与回滚——文档必须点名这条，不能把外层的写默默说成事务内的；
8. **多 Key 的可见性保证**：至少保证「**没有任何失败 attempt 的输出被下游观察到**」。**已定案（2026-10-11）**：第一版**不承诺**同一节点多个输出严格原子可见——提交在 Execution 的锁下逐条写，下游理论上存在「读到一半新一半旧」的窗口，这条写进文档与 godoc；要严格原子得另设 commit/read 同步协议，留作后续票。

### 5.1 事务边界必须诚实

引擎能回滚的只有**它自己管理的槽位暂存**。用户 `Run` 里已经发生的任意外部副作用（数据库提交、HTTP 请求、邮件、写文件）**不可能**被自动撤销——第一版就这么写进文档，不允许用「内存里暂存过」暗示外部世界能回滚。要接事务的调用方有两条正路：把副作用挪到最后一次成功 attempt（先算完再提交），或用幂等键 / outbox / 补偿；引擎只保证第 7 条那条边界**说清楚**。

## 6. 兼容与迁移

- **发布节奏**：这是 0.x 的破坏性改造，走 **minor**（每阶段一个小版本，配迁移说明）；不在 patch 里做。
- **冻结面怎么动**：七条哨兵**不新增、不改名**，但 `ErrGraphStarted` / `ErrGraphNotStarted` 的**触发点**要在迁移说明里写准——新模型下「已经启动过」有两个可能含义：定义已封口（`Add` / `DeclareInput` 被拒）与这个 Execution 已经启动过（二次 `Start` 被拒）。倾向：两者共用这两个哨兵，语义在 godoc 与 release notes 里分开写清（待拍板 (e)）。
- **不保留旧入口**：`Seed` / `SkipSeed` 这类「往定义里写值」的入口不留在 `Graph` 上（留着就会让「装配完即可见完整结构」这个核心性质在旧入口上不成立，两套语义并存=含糊）。`Add` / `Validate` / `Topology` 这类不冲突的入口保持。
- **PR #310 的处置**（#311 点名要评审）：**已定案——替换**（见第 8 节）。`Graph.Snapshot()` / `Child *Graph` 这个形状不进 main；它的成果转成阶段 2 的素材，面向 `Execution` 重做。
- **`g.slots` 锁统一**：今天这张表装配期写点持 `g.mu`、运行期读点持 `slotsMu`（#310 的新读入口把它暴露成 `fatal error: concurrent map read and map write`）。它跟着阶段 2 一起走（单独拆出来无法用公开 API 复现）。
- **文档怎么跟**：`pulse.md` 顶部标「描述现状（迁移中）」；本文与 [`philosophy.md`](philosophy.md) 描述目标；阶段 5 把示例、README、站点全部迁完，不留只在旧模型下成立的示例。

## 7. 阶段路线（票的顺序与验收）

| 阶段 | 产出 | 验收要点（摘 #311，票面写全） |
|---|---|---|
| **0（本文）** | 对象模型与契约冻结 + 理念文档 | 本文定案；#303 / #309 / #310 关系写清 |
| **1 静态定义与组合** | `Graph` 只持定义；`Sub` 组合现成子图；递归 `Topology` / `Validate` | A/B/C 各自构建、D 组合后**无运行**即可递归看全树并报全部静态错；同一子图多处挂载不改定义 |
| **2 执行运行时** | `Execution`；槽位 / 状态 / 错误 / 取消域隔离；子 Graph → 每个挂载位置一个子 Execution | 同 Graph 顺序两次结果隔离；并发两次 `-race` 干净；取消只影响本次 |
| **3 事务性 Retry** | attempt 写集、提交 / 丢弃；aspect 边界 | 「第一次写 K=a 后失败、第二次写 K=b 成功 → 下游只见到 b」；多 Key 一个都不可见；变异对照：去掉暂存必红 |
| **4 快照 / 观测 / YAML 对齐** | `Execution.Snapshot()`；Observer 身份；YAML 与 Go 同语义；Mermaid 静态全貌（可叠加一次执行） | 快照与结构同序同 id；子 Child 指向子 Execution；YAML `Analyze` 与 Go `Topology` 语义一致 |
| **5 迁移与发布** | 文档 / 示例 / release notes 全部迁移；不兼容项逐条列 | `go build` / `vet` / `-race` / `gofmt` 全绿；零外部依赖；旧模型示例清零 |

每阶段一条铁律：**先开票、评审过再开工**；每条语义要有用例，行为类结论要有变异对照。

## 8. 已定案与遗留

**已定案（2026-10-11）**：

- **(a) 输入的形状**：`Inputs` 值 + builder（`in.Set(k, v)` / `in.Skip(k)`）——一次运行的输入是一份数据，不是一串装饰。
- **(b) 端口声明**：`g.DeclareInput(ports...)` 方法。声明过的 `Requires` 不再算「无来源」，`Validate` 因此能分清「你忘了接线」与「这本来就是外部输入」。
- **(d) 多 Key 提交可见性**：只保证「失败 attempt 不可见」，成功提交逐条写（见第 5 节第 8 条）。
- **(f) PR #310**：**替换**——`Graph.Snapshot()` + `Child *Graph` 的形态**不进 main**；#310 里与模型无关的成果（状态词表对齐、`waiting` 的两种读法、反射断言不带值、并发用例、`g.slots` 锁统一）转成阶段 2 的素材，面向 `Execution` 重做；#309 随之标 superseded。
- **发布切分**：**每阶段落地就发一个小版本**（阶段 1 = 下一个 minor，附迁移说明），不攒到最后一并砸下来。

**暂定（评审时确认或推翻）**：

- **(c) 封口时机**：第一次 `NewExecution` / `Validate` 时**隐式**封口，另留可选的 `Seal()` 供「装配完立刻冻结并在装配期报错」。
- **(e) 哨兵语义**：复用 `ErrGraphStarted` / `ErrGraphNotStarted`（不改名、不新增），在 godoc 与迁移说明里把两种触发点写清——定义已封口（`Add` / `DeclareInput` 被拒）与这个 Execution 已经启动过（二次 `Start` 被拒）。
- **(g) 观测身份**：新增 `pulse.execution`（本次执行 id），`pulse.graph` 继续是定义 id——否则同一张图在**同一位置**跑两次的记录会合并。

## 9. 不做（本阶段就写清，免得回头重开）

- 不为静态图另造一套平行的模板 / 计划模型（`Graph` 本身是定义）；
- 不清空旧槽位来复用 `Execution`——每次执行新建独立运行状态；
- 不承诺自动撤销用户外部副作用（第 5.1 节）；
- 不为迁就 `Graph.Snapshot()` / `Child *Graph` 的旧形状而保留旧对象模型——公开面服从职责边界；
- 不引入 attempt 级 Observer 生命周期事件、隐式补偿或任何不透明的事务魔法（要加就得有单独契约 + 测试）。
