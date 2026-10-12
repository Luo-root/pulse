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

## 2. 现状核实（读码定位，2026-10-12）

| 今天的形状 | 住哪（给**符号**，不写行号——行号会漂） | 与新模型的关系 |
|---|---|---|
| `Graph` 同时持定义与一次运行的全部字段：`ctx/cancel/slots/slotsMu/states/err/started/claimed/done/wg/finishOnce` | `graph.go` 的 `Graph` | **拆**：定义留 `Graph`，运行搬 `Execution` |
| `Graph.sem` 是**死字段**（全仓无读无写；名额在 `limiter.sem`） | `graph.go` 的 `Graph`；`limiter.go` 的 `limiter` | 迁移时删掉（`g.lim.sem` 才是活的，用例读的是它） |
| `New(ctx, graphID, description, opts…)`：ctx 属于**定义** | `graph.go` 的 `New` | ctx 移到 `NewExecution`；`New` 只收定义信息 |
| `Seed` / `SkipSeed` / `SeedByName` / `SkipSeedByName`：既是「声明外部来源」又是「写这一轮的值」 | `graph.go` 的 `Seed` 一族、`coerce.go` | **拆**成两件事：静态端口声明 + 本次执行输入。**声明那一半阶段 1 已落地**（`DeclareInput` / `DeclareOutput` + `SourceInput`）；「值」那一半仍归 `Seed`，随阶段 2 搬进 `Execution` 的输入 |
| 旧的动态形态 `SubFunc(g, id, desc, binds, build)`：子图在**运行期**由 `build(*SubCtx)` 造，**绑定校验也在运行期**（`runSub` 建完子图才比对键存在与类型） | **已删除**（原 `sub.go` 的 `SubCtx` / `SubFunc` / `runSub` / `subStartedErr`） | 由 `Sub` 组合现成 `*Graph` 取代；绑定校验进装配期与 `Validate`。**阶段 1 已落地**：整套动态面**直接删掉**，不是改名留着——改名只是把旧模型带进新模型；端口 / 类型 / 环都在装配期与 `Validate` 报 |
| `claimSub` + `adopt`：认领一次性 + **改写子图字段**（`observer` / `lim` / `aspects`） | `graph.go` 的 `claimSub`、`sub.go` 的 `adopt` | 由「创建子 Execution 时算有效配置」取代，定义不再被写。**阶段 1 已做一半**：`path` 已经完全不上定义（那个字段删掉了，路径按挂载位置现推）；有效配置（观察者 / 名额 / 图级切面）仍在运行期写进共享定义，阶段 2 收进执行树 |
| `Start` / `Wait` / `Run` / `Err` 都在 `Graph` 上；`Start` 只能成功一次 | `graph.go` 的运行入口 | 搬到 `Execution`；`Graph` 只留同步便捷入口 |
| `slot` 是「到达即发布、幂等首写」，没有 attempt 层 | `slot.go` | 每个 attempt 一份写暂存，成功才提交（第 5 节） |
| `Retry` 的文档明说「**重试安全的前提**：失败前没有写过任何 Provide」 | `aspect.go` 的 `Retry` | 这条前提改由引擎接管——本票硬要求 |
| `Topology()` 只覆盖**一张图** | `topology.go` 的 `Topology` | 递归全树。**阶段 1 已落地**：静态挂载的子图在 `Nodes[i].Sub` 里、路径按挂载位置推导（`GraphShape.Path` 是**这一次视图里**的位置，不需要运行、也不上定义） |
| `Graph.Snapshot()` + `NodeState.Child *Graph` 建在「Graph = 一次运行」之上 | `snapshot.go`；PR #310 | 形状要跟着模型走（第 6 节） |
| `Validate()` 只看本图；`Problems` 一次给全、`Start` 报第一条 | `topology.go` 的 `Topology`、`problems.go` | 组合**整棵树**校验；错误仍在最早可判定处报。**阶段 1 已落地**：`Validate` 沿静态挂载边递归、**按引用点**各报一条并点明挂载位置；组合环在 `Sub` 装配期拒 |
| YAML 的 `Load` 返回 `(*Graph, *SeedPlan)`，子图也在运行期建 | `yaml/load.go`、`yaml/subgraph.go` | 结构语义与 Go 组合对齐；`SeedPlan` 只负责**本次执行**的输入。**阶段 1 已落地**：子图在 `Load` 期按挂载位置装一次（一处一张实例、装完即可递归看全树）；`retry:` 落在 `graph:` 节点上当场拒（静态子图只跑一次）；`SeedPlan` 仍担「声明 + 值」两职，值那一半随阶段 2 拆 |
| 限流 / 切面 / 观察者靠 `adopt` **继承**（写进子图字段） | `sub.go` 的 `adopt`、`limiter.go` | 改成「执行树创建时算有效配置」，不动共享定义（阶段 2，现状不变） |

## 3. 职责与生命周期

| 对象 | 持有 | **不**持有 | 生命周期 | 谁创建 |
|---|---|---|---|---|
| **Graph**（定义） | 节点表、来源表、Key 声明表、**端口**（开放输入 / 输出）、静态子图挂载、描述、静态策略声明 | ctx / cancel、槽位、节点状态、首错、waitgroup、调度计数、观察者身份、路径 | 装配 → 封口 → 进程结束；封口后结构不变 | 宿主（`New` + `Add` / `DeclareInput` / `Sub`） |
| **Execution**（运行） | ctx / cancel、输入值、slot map、每节点状态、首错、done / wg、有效限额（引用或自有）、有效切面、观察者（含执行身份与 path）、运行时子执行句柄 | 节点定义本体（读 `Graph` 的） | `NewExecution` → `Start` → `Wait` → 回收；一次性 | 宿主，或父 Execution（为子图节点） |
| **Node**（定义） | `id` / `desc` / `requires` / `provides` / `run` / 节点级切面 | 任何运行状态（同一 Node 可被多图、多次执行复用） | 与定义同寿 | 宿主 |
| **静态 Child** | `Topology().Nodes[i].Sub`：这一步是一张子图的**定义**（递归） | 运行状态 | 与定义同寿 | 由 `Sub` 装配时挂上 |
| **运行时 Child** | `Snapshot().Nodes[i].Child`：本次挂载位置的**子 Execution** | 定义本体 | 随父 Execution | 父 Execution 在跑到那一步时创建 |

### 3.1 输入 / 输出接口（三层，别再混）

**端口是唯一的接线面**：一张图对外只暴露它**声明过**的端口，内部节点的 `Provides` 默认是**私有**的。输入与输出对称。

1. **端口声明（静态）**：`g.DeclareInput(ports…)` / `g.DeclareOutput(ports…)` 声明「这张图接受 / 产出这几条 Key」。它们参与拓扑与静态校验：
   - 声明过的**输入端口**不再算「无来源」——`Validate` 因此分得清「你忘了接线」与「这本来就是外部输入」；
   - `In(父, 子)` 只能接到子图**声明过的输入端口**，`Out(子, 父)` 只能接子图**声明过的输出端口**。接到内部中间 Key 是**静态错**（报文点明「它不是这张图的端口，要么声明它、要么改接到端口上」）——想导出中间 Key 就把它声明出来，意图明确；不声明就等于「这是我的实现细节」。
   - 这条**收掉一类静默断链**：子图把内部实现改了（中间 Key 改名 / 合并），如果没有端口，父图绑的就是实现细节；有了端口，父子之间只剩下声明过的那几条。
2. **组合接线（静态）**：`Sub(g, id, desc, binds, child)` 把父侧的 Key 接到子图的端口上（沿用 `In(父, 子)` / `Out(子, 父)` 的方向语义）。父图装配完就能递归校验整棵树。
3. **本次输入（运行）**：`Execution` 的入参提供实际值 / 显式跳过。根图这一层是宿主给的；子图这一层由父 Execution 按绑定喂进去。YAML 的 `SeedPlan` 属于这一层——它**不**再往定义里写东西。

**来源类型也要跟着分开**：静态拓扑里「端口」的来源是**新增的 `SourceInput`**（`pulse.SourceInput`，值与 `seed` / `node` 并列），不再是 `SourceSeed`。理由：`Seed` 是**运行期**的动作，让静态结构里出现一个只在该次运行成立的来源名，等于把「定义」和「这一次」又混回去了；`SourceSeed` 在迁移期仍表示今天的 `Seed/SkipSeed` 那条外部来源，两者可判可分。

校验分工：`Validate()` 只查静态结构与组合约束（不要求运行值存在）；`NewExecution` 查「这次运行需要的输入齐不齐」（这是启动条件，不是静态毛病）。

### 3.2 运行配置属于执行树，不写回定义

ctx、路径、观察者、切面、限额都是**一次嵌套执行环境**的属性。创建子 Execution 时按「父执行环境 + 子图自己的声明」算出有效值（默认继承、`WithoutAspects()` 切断切面、限额共享同一棵执行树的计数、`Sub` 那一步本身不吃名额）。定义保持只读，因此「同一份定义被两处挂载」不会再互相踩。

### 3.3 身份与观测归因

| 记号 | 含义 | 今天 |
|---|---|---|
| 图 id | **定义**身份（`New` 给的） | `pulse.graph` |
| 执行 id | **本次运行**身份（根执行由宿主给或自动生成，子执行由父执行派生） | 无 |
| path | 挂载位置（根 `""`，一层 `step`，再深 `step/inner`） | `pulse.path` |

要求：同一张 Graph 的多次执行在观测里**分得开**（否则并发执行的记录会合并成同一份，归因直接错）。**执行 id 对每一个执行实例唯一**，不做「父 id + path」的纯确定性派生：同一父执行、同一挂载点、`Retry` 第 n 次 attempt 重建出来的子执行也是**不同的实例**，实例序号要进 id——否则同一个挂载点重试几次的记录会合并成一份（评审 #62）。

### 3.4 两种环要分开（评审 #83）

- **依赖环（Key 环）**：节点之间 `Requires` / `Provides` 成环。今天就在 `Validate` / `Start` 报一条具体见证（`pulse: dependency cycle: A -> B -> A (A requires "y", B requires "x")`，`problems.go`），这条**不动**。
- **组合环（子图环）**：一张图出现在**自己的子树**里——自己挂自己，或 A 挂 B 之后 B 又挂 A。静态 `Topology()` / `Validate()` 会无限展开，所以**必须在装配期拒**：`Sub` 在挂载那一刻沿子图子树查一遍「父图是不是已经在它的子树里」，是就报一条点明环路径的错（`pulse: subgraph cycle: A -> B -> A`）。
- **判据不能用全局 visited**：同一张子图在**不同挂载位置**重复出现是**合法**的（这正是本模型要支持的核心用法），判定只能用**当前递归路径上的祖先集合**；兄弟分支共用一个子图不是环。
- **YAML 那条检查要与它统一**：`pulse/yaml` 已有的 graph-reference 环检查是对 **spec 名**做的，引擎这条管 **对象图**（同一个 `*Graph` 出现在自己的子树里）——两者判据不同（一份声明 vs 一个实例，同一份 spec 可以装出多张图），但**报文形状要一致**，且 YAML 装图期要把引擎这条也走一遍，不能只查 spec 名就放过。

## 4. 目标 API 形状（草案，待冻）

```go
// —— 定义侧（可复用、可组合、封口后不可变）——
func New(graphID, description string, opts ...Option) (*Graph, error)
func (g *Graph) Add(n *Node) error
func (g *Graph) DeclareInput(ports ...InputPort) error   // 静态输入端口（名字 + 类型）
func (g *Graph) DeclareOutput(ports ...OutputPort) error // 对外可绑定的产出端口
func (g *Graph) Topology() *Topology                     // 静态全树（递归，含端口与绑定）
func (g *Graph) Validate() error                         // 静态结构 + 组合约束

// —— 组合：挂一张现成的子图（唯一入口）——
func Sub(g *Graph, id, description string, binds []SubBind, child *Graph, opts ...SubOption) error

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

- **封口时机（评审 #157 定案）**：**`Validate()` 通过才封口**——校验失败时图保持**可变**，调用方补上 `Add` / `DeclareInput` 之后再校验（这条与 `Start` 现有行为同一条线性化规则：校验没过就说明「图还没装配完」，不是「装配完了但坏了」）。**不设独立的 `Seal()`**：它和「成功即封口」的 `Validate()` 职责重叠，多一个入口只会多一种「到底封没封」的问法；要显式冻结就调一次 `Validate()`。**封的是整棵子树**（阶段 1 落地）：挂上去的子图是这张定义的一部分，只封自己那句话就是半句空——子图还能接着 `Add`，刚校验过的那棵树会变；失败则一层都不封。
- `Run` 留在 `Graph` 上，但**不**在 `Graph` 上留 `Start` / `Wait` / `Snapshot`——避免「隐式 Execution」这种半吊子状态。
- `Inputs` 是**值**（builder 风格：`in.Set(k, v)` / `in.Skip(k)`），不是变参 Options：一次运行的输入是一份数据，不是一串装饰。
- 端口构造器收 `Key[T]`（`Port[T](k Key[T]) InputPort`），名字与类型只写一遍——写字符串名字最容易歪。
- `NodeState.Child` 从 `*Graph` 变成 `*Execution`；`Topology().Nodes[i].Sub *Topology` 承载静态子树，`Binds` 承载父子接线。
- **不**另造 `GraphTemplate` / `GraphPlan`：`Graph` 本身就是那个可复用定义。

## 5. 事务性 Retry（本票硬要求）

今天的形态：槽位「到达即发布、幂等首写」，`Retry` 重跑时前一次 attempt 已经 `Set` 过的槽位**已经发布**——下游被唤醒，第二次 attempt 的写还会被静默忽略（`aspect.go:67-75` 明说这条前提由调用方守）。新模型把它收进引擎：

1. **attempt 写集**：`Set` / `Skip` 先落到**本 attempt 的写集**，不直接进执行槽位；
2. **读自己写的**：attempt 内 `Get` 自己已写的 Provide 读到的是**本写集**的值（自洽）；
3. **丢弃**：attempt 失败且还要重试 → 整个写集丢掉，**下游从未见过**；
4. **提交**：attempt 成功（或在拿到 `Retry` 之后明确以跳过收尾）才把写集写进执行槽位；不同 attempt 的输出不会混写；
5. **跳过 ≠ 可重试失败**：`NoValue` / `*SkipError` 不触发重试（今天的判据照旧：`isSkipped`）。
6. **「不继续重试」的判据要说准**（评审 #114）——看**进入 `Retry` 这一层时拿到的那个 ctx**（含它外层派生出来的）是不是已经取消 / 到期：
   - **Execution / 父 ctx** 取消或到期 → **不重试**，错误原样带出（再试也不会变好，而且外层已经不再等它）；
   - `Retry` **外层**的 `Timeout` 触发（`Timeout(Retry(f))`）→ 就是上一条：`Retry` 拿到的正是那层派生 ctx，已到期 → 不重试；
   - `Retry` **内层**包的超时（`Retry(Timeout(f))`）→ 那只是**这一次 attempt 的失败**，照常重试（`Retry` 自己那一层的 ctx 并没有到期）——这是「这次没在时限内做完，再试一次」的正常用法。
   今天的 `Retry` 已经是这个判据（`aspect.go:90-92` 判 `rc.ctx.Err()`；`Timeout` 用 `Fork()` 把派生 ctx 交给内层，`aspect.go:45-65`）。新契约把它**写下来并测**，实现照旧——它必须先有专门用例（四种嵌套：`Retry(Timeout)`、`Timeout(Retry)`、父取消、attempt 自己返回 ctx 错误）才允许被改。
7. **最终失败**：写集丢弃 + 未写输出按既有语义自动跳过 + 错误原样冒泡（槽位终态与节点终态一致，可测）；
8. **切面顺序决定事务边界**：`Retry` 包住的那一段（`next` 之内、含更内层切面）属于 attempt；`Retry` **外层**切面里的写入**不**属于任何 attempt、不参与回滚——文档必须点名这条，不能把外层的写默默说成事务内的；
9. **多 Key 的可见性保证**：硬保证是「**没有任何失败 attempt 的输出被下游观察到**」。**已定案（2026-10-11）**：第一版**不承诺**同一节点多个输出严格原子可见——提交在 Execution 的锁下**逐条**写。窗口的**准确**形状（评审 #117）：槽位是一次性的、开始全是 `pending`，失败 attempt 的写集整批丢弃，所以窗口里**看不到任何旧 attempt 的值**；可能看到的是「**一部分输出已经 `ready`、另一部分仍是 `pending`**，等这些输出的下游可能被先到的那一条先唤醒」。验收要专门覆盖这个窗口（一条输出已到、另一条尚未提交）；要严格原子得另设 commit/read 同步协议，留作后续票。

### 5.1 事务边界必须诚实

引擎能回滚的只有**它自己管理的槽位暂存**。用户 `Run` 里已经发生的任意外部副作用（数据库提交、HTTP 请求、邮件、写文件）**不可能**被自动撤销——第一版就这么写进文档，不允许用「内存里暂存过」暗示外部世界能回滚。要接事务的调用方有两条正路：把副作用挪到最后一次成功 attempt（先算完再提交），或用幂等键 / outbox / 补偿；引擎只保证第 8 条那条边界**说清楚**。

## 6. 兼容与迁移

- **发布节奏**：这是 0.x 的破坏性改造，走 **minor**（每阶段一个小版本，配迁移说明）；不在 patch 里做。
- **`Sub` 换了含义（最要紧的一条，迁移说明放最前）**：`pulse.Sub(g, id, desc, binds, build)`（运行期现造子图）**被删除**，今天 `Sub` 是「挂一张**现成的**子图」（第 4 节）。跟着一起没的还有 `SubCtx`（`Context()` / `GraphID()` / `Path()` / `Observer()`）与运行期的绑定校验。替代写法逐条：① 子图在**装配期**建好，并 `DeclareInput` / `DeclareOutput` 声明端口——接线只能接端口，接子图内部的中间 Key 是静态错；② 子图的 ctx 用它建图时给的那个（取消由 `Sub` 在运行期转发进子图，不需要自己派生）；③ 图 id 自己起，建议沿用 `<父图 id>/<节点 id>` 这套写法；④ `pulse.path` 在**按层建出口**时填（声明式装配走 `LoadOptions.ObserverFor`）。YAML 侧同时收紧：`graph:` 节点上的 `retry:` 从「能重试」变成 **`Load` 期拒**（静态子图只跑一次）——把 `retry:` 放到子图里那个真会失败的节点上。
- **冻结面怎么动**：七条哨兵**不新增、不改名**，但 `ErrGraphStarted` / `ErrGraphNotStarted` 的**触发点**要在迁移说明里写准——新模型下「已经启动过」有两个可能含义：定义已封口（`Add` / `DeclareInput` 被拒）与这个 Execution 已经启动过（二次 `Start` 被拒）。倾向：两者共用这两个哨兵，语义在 godoc 与 release notes 里分开写清（待拍板 (e)）。
- **端口是新的一层，子图要跟着声明**：`DeclareInput` / `DeclareOutput` 落地后，**被当作子图组合的图必须声明端口**——旧形态那种「绑定指向子图里任意声明过的 Key」不再成立（接到非端口是静态错）。迁移说明要逐条给出「原来绑内部 Key → 现在补一句 `DeclareOutput`」的改法。
- **来源类型多一个值**：`pulse.SourceInput` 与 `seed` / `node` 并列。迁移期 `SourceSeed` 仍表示今天的 `Seed` / `SkipSeed` 那条外部来源；静态端口一律显示 `SourceInput`。`SourceKind` 不在冻结面里（冻结面只有槽位三态、`Aspect`、七条哨兵、Observer 次数、六条编码原语 + `LineRenderer`），所以这是**新增**而不是破坏。
- **组合环是新拒的一类**：今天 YAML 只查 graph-reference 名环；引擎侧「同一张图挂进自己的子树」从阶段 1 起在 `Sub` 装配期拒（见第 3.4 节）。这是**新增的静态错**，迁移说明里点名。
- **不保留旧入口**：`Seed` / `SkipSeed` 这类「往定义里写值」的入口不留在 `Graph` 上（留着就会让「装配完即可见完整结构」这个核心性质在旧入口上不成立，两套语义并存=含糊）。`Add` / `Validate` / `Topology` 这类不冲突的入口保持。
- **PR #310 的处置**（#311 点名要评审）：**已定案——替换**（见第 8 节）。`Graph.Snapshot()` / `Child *Graph` 这个形状不进 main；它的成果转成阶段 2 的素材，面向 `Execution` 重做。
- **`g.slots` 锁统一**：今天这张表装配期写点持 `g.mu`、运行期读点持 `slotsMu`（#310 的新读入口把它暴露成 `fatal error: concurrent map read and map write`）。它跟着阶段 2 一起走（单独拆出来无法用公开 API 复现）。
- **文档怎么跟**：`pulse.md` 顶部标「描述现状（迁移中）」；本文与 [`philosophy.md`](philosophy.md) 描述目标；阶段 5 把示例、README、站点全部迁完，不留只在旧模型下成立的示例。

## 7. 阶段路线（票的顺序与验收）

| 阶段 | 产出 | 验收要点（摘 #311，票面写全） |
|---|---|---|
| **0（本文）** | 对象模型与契约冻结 + 理念文档 | 本文定案；#303 / #309 / #310 关系写清 |
| **1 静态定义与组合** | `Sub` 组合现成子图是**唯一**入口，旧的动态形态（`SubFunc` / `SubCtx`）**整体删除**；`DeclareInput` / `DeclareOutput` 端口（**出口同样有静态判据**：声明了却没人产出 → 装配期报）；接线形状的判据共用一份 `checkBinds`（进口只许绑一次、进口在子图里已有来源就判死、`In` + `Out` 打同一端口 = 透传）；组合环在装配期拒（「查一遍子树 + 挂上」在同一个全局装配临界区里，递归另带挂载链兜底判环）；递归 `Topology` / `Validate`（**每层自己取锁**，`Validate` 通过 / `Start` 成功时**整棵子树一起封**，且「判 + 封」与改定义共用装配锁）；运行 ctx 由父节点给 | A/B/C 各自构建、D 组合后**无运行**即可递归看全树并报全部静态错；同一子图多处挂载不改定义；预建子图上的取消传得下去 —— **已落地**（PR #314，`refactor/sub-to-subfunc`：`Sub` / 端口 + 整棵子树封口 / 递归拓扑与校验 / yaml 在 `Load` 期装一次 / `retry:` 落在 `graph:` 节点上拒（yaml `Load` 期、Go 进 `Run` 第一件事）/ 未引用的 spec 不查来源） |
| **2 执行运行时** | `Execution`；槽位 / 状态 / 错误 / 取消域隔离；子 Graph → 每个挂载位置一个子 Execution | 同 Graph 顺序两次结果隔离；并发两次 `-race` 干净；取消只影响本次 |
| **3 事务性 Retry** | attempt 写集、提交 / 丢弃；aspect 边界 | 「第一次写 K=a 后失败、第二次写 K=b 成功 → 下游只见到 b」；多 Key 一个都不可见；变异对照：去掉暂存必红 |
| **4 快照 / 观测 / YAML 对齐** | `Execution.Snapshot()`；Observer 身份；YAML 与 Go 同语义；Mermaid 静态全貌（可叠加一次执行） | 快照与结构同序同 id；子 Child 指向子 Execution；YAML `Analyze` 与 Go `Topology` 语义一致 |
| **5 迁移与发布** | 文档 / 示例 / release notes 全部迁移；不兼容项逐条列 | `go build` / `vet` / `-race` / `gofmt` 全绿；零外部依赖；旧模型示例清零 |

每阶段一条铁律：**先开票、评审过再开工**；每条语义要有用例，行为类结论要有变异对照。

## 8. 已定案与遗留

**已定案（2026-10-11，含 PR #312 评审后的补充）**：

- **(a) 输入的形状**：`Inputs` 值 + builder（`in.Set(k, v)` / `in.Skip(k)`）——一次运行的输入是一份数据，不是一串装饰。
- **(b) 端口声明**：`g.DeclareInput(ports…)` + **对称的 `g.DeclareOutput(ports…)`**。端口是**唯一的接线面**：`In` 只能接输入端口、`Out` 只能接输出端口，接到内部中间 Key 是静态错；来源类型用**新增的 `SourceInput`**（第 3.1 节）。
- **(c) 封口时机（评审 #157 补充定案）**：`Validate()` **通过才封口**，校验失败保持可变（补 `Add` / `DeclareInput` 再校验）；**不设 `Seal()`**。阶段 1 加固时补了两条同源的边界，写进这里是免得阶段 2 重开：① **`Start()` 成功同样封整棵子树**（删掉这一步，持有子图指针的调用方能在「父图已启动、子图还没轮到」之间接着 `Add` / `DeclareInput`——实测都进得去）；② **改定义的四个入口（`Add` / `DeclareInput` / `DeclareOutput` / `Sub`）与 `Validate` / `Start` 的「判 + 封」共用一把装配锁**，所以「判完整棵树」与「封上它」之间没有窗口——不加锁的话 `Validate()` 会返回 nil 却封住一份没校验过的定义（实测第 0 轮即中）。两个冻结点同时成立时，报文按「已经启动」出（那是最贴近用户刚做过的动作的原因）。
- **(d) 多 Key 提交可见性**：只保证「失败 attempt 不可见」；成功提交逐条写，窗口的准确形状是「一部分输出已 `ready`、其余仍 `pending`」（见第 5 节第 9 条）。
- **(f) PR #310**：**替换**——`Graph.Snapshot()` + `Child *Graph` 的形态**不进 main**；#310 里与模型无关的成果（状态词表对齐、`waiting` 的两种读法、反射断言不带值、并发用例、`g.slots` 锁统一）转成阶段 2 的素材，面向 `Execution` 重做；#309 随之标 superseded。
- **组合环**：与依赖环分开；`Sub` 装配期拒，判据用**当前递归路径上的祖先集合**（多处挂载合法）；**「查一遍子树」与「真的挂上」在同一个全局装配临界区里**（阶段 1 落地：组合环是两张图各自动作的合成结果，单看任何一张图的锁都拦不住对向挂载），递归另带一条挂载链做兜底判环（环的代价是爆栈或互等锁，不是一句错话）；YAML 那条 spec 名检查与它报文形状统一（第 3.4 节）。**同一把锁（`assemblyMu`）在阶段 1 加固时扩了半个身位**：改定义的入口与 `Validate` / `Start` 共用它，于是「判完整棵树再封」这件事也成了全局不变式——见 (c)。
- **落在挂载子图那一步上的 `Retry`**：阶段 1 **不支持，且不是静默接受**——静态挂载的子图只跑一次，第二次 attempt 起不来，还会用「子图已经在用」把第一次失败的真正原因盖掉。装配期认不出一个 `Aspect` 是不是 `Retry`（裸函数类型，闭包没有可查标识——实测 `reflect` 代码指针每次实例化都不同），所以判据放在 `Retry` 进 `Run` 的第一件事（子图还没被认领）；用户自己写的重试切面由这一步的重入判据兜住。`pulse/yaml` 有 spec 可读，`Load` 期就拒同一个形状。**「每次 attempt 一张新子图」要等阶段 2 的 `Execution` + 阶段 3 的事务性 `Retry`**（第 5 节）。
- **执行 id**：对每个执行实例唯一，**不**做「父 id + path」的纯确定性派生——同一挂载点的第 n 次 attempt 重建的子执行也是不同实例（第 3.3 节）。
- **Retry 与取消 / 超时**：判据是「进入 `Retry` 那层拿到的 ctx 是否已到期」；内层 `Timeout` 可重试、外层 `Timeout` 与父取消不重试（第 5 节第 6 条）。
- **发布切分**：**每阶段落地就发一个小版本**（阶段 1 = 下一个 minor，附迁移说明），不攒到最后一并砸下来。

**暂定（评审时确认或推翻）**：

- **(e) 哨兵语义**：复用 `ErrGraphStarted` / `ErrGraphNotStarted`（不改名、不新增），在 godoc 与迁移说明里把两种触发点写清——定义已封口（`Add` / `DeclareInput` 被拒，这正是 (c) 里「校验通过才封口」后 `Add` 会被拒的那个时刻）与这个 Execution 已经启动过（二次 `Start` 被拒）。**阶段 1 已按这条落地了「写清」那一半**：`ErrGraphStarted` 的 godoc 现在写明两个触发点、并点出 `Seed` 不受封口影响（`ErrGraphNotStarted` 的语义随阶段 2 落地）。待确认的只剩「复用不改名」这条决定本身。
- **(g) 观测身份**：新增 `pulse.execution`（本次执行 id，承载上面的唯一性规则），`pulse.graph` 继续是定义 id——否则同一张图在**同一位置**跑两次的记录会合并。

## 9. 不做（本阶段就写清，免得回头重开）

- 不为静态图另造一套平行的模板 / 计划模型（`Graph` 本身是定义）；
- 不清空旧槽位来复用 `Execution`——每次执行新建独立运行状态；
- 不承诺自动撤销用户外部副作用（第 5.1 节）；
- 不为迁就 `Graph.Snapshot()` / `Child *Graph` 的旧形状而保留旧对象模型——公开面服从职责边界；
- 不引入 attempt 级 Observer 生命周期事件、隐式补偿或任何不透明的事务魔法（要加就得有单独契约 + 测试）。
