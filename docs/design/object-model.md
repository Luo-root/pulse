# 对象模型与契约（#311 阶段零）

> **本文状态：阶段零已定案（2026-10-11），阶段 1 / 2 / 3 已落地；阶段 4 已实现（#319），静态端口、执行快照、Mermaid 与 YAML Analyze 均已具备。** 它是 [#311](https://github.com/Luo-root/pulse/issues/311)「阶段零」的产物：
> 先把 Graph / Execution 的职责、边界与契约定下来，**再**动代码。第 8 节列出**已定案**内容与
> **原暂定项的实施结果**；定案部分与 `pulse.md` 的冻结面那一节同步。第 7 节的阶段表逐行
> 标着落地情况。
>
> 读法：本文描述**目标模型**；今天的实现（`Graph` 定义 + 一份 `Execution` 一次运行）在 [`pulse.md`](pulse.md)，
> 「现状 vs 目标」的逐条差距在下面第 2 节。设计**理念**（为什么这样取舍）在
> [`philosophy.md`](philosophy.md)。

## 1. 一句话

**`Graph` 是可复用的静态定义，`Execution` 是一运行实例。**

- `Graph`：节点、依赖（由 Key 推出）、Key 类型声明、子图组合、输入 / 输出接口、描述、静态策略声明（默认切面 / 默认限额）。
- `Execution`：这次运行的 ctx / cancel、输入值、槽位、每节点状态、首错、调度与限流计数、观察者身份、运行时子执行句柄。

同一张 `Graph` 应当能被：顺序执行多次（每次全新且隔离）、并发执行多次（在用户回调自身的并发安全约定之下）、在不同父图里作为子图被挂载多次（每次挂载位置各有自己的 `Execution`）。一次 `Execution` 的取消 / 失败 / 完成**不**污染定义，也不污染别的 `Execution`。

## 2. 旧模型与迁移结果（读码定位，2026-10-12）

| 迁移前的形状 | 住哪（给**符号**，不写行号——行号会漂） | 与新模型的关系 |
|---|---|---|
| `Graph` 同时持定义与一次运行的全部字段：`ctx/cancel/slots/slotsMu/states/err/started/claimed/done/wg/finishOnce` | `graph.go` 的 `Graph` | **拆**：定义留 `Graph`，运行搬 `Execution`。**阶段 2 已落地**：那些字段全在 `execution.go`，`Graph` 只剩 id / 描述 / Key / 来源表 / 端口 / 节点 / 声明式默认配置 + `sealed` |
| `Graph.sem` 是**死字段**（全仓无读无写；名额在 `limiter.sem`） | 迁移前 `graph.go` 的 `Graph`；当前 `execution_run.go` 的 `limiter` | 迁移时已删掉（旧模型活的是 `g.lim.sem`，不是 `Graph.sem`）；当前名额由 Execution 的 limiter 持有。**阶段 2 已落地** |
| `New(ctx, graphID, description, opts…)`：ctx 属于**定义** | `graph.go` 的 `New` | ctx 移到 `NewExecution`；`New` 只收定义信息。**阶段 2 已落地**：`New(graphID, description, opts…)`，一次运行的取消域由 `Run(ctx, in)` / `NewExecution(ctx, in)` 给 |
| `Seed` / `SkipSeed` / `SeedByName` / `SkipSeedByName`：既是「声明外部来源」又是「写这一轮的值」 | 迁移前 `graph.go` 的 `Seed` 一族；当前 `inputs.go` 的输入与形状对齐 | **拆**成两件事：静态端口声明 + 本次执行输入。**声明的另一半阶段 1 已落地**（`DeclareInput` / `DeclareOutput` + `SourceInput`）；**值的这一半阶段 2 已落地**：`pulse.Seed(&in, k, v)` 收的是这一次运行的 `*Inputs`（零值可用、重复写当场报），同一家族的 `Fix` / `FixSkip` 把「属于某一处挂载」的固定值也收进绑定；定义层的 `SourceSeed` 随之删除 |
| 旧的动态形态 `SubFunc(g, id, desc, binds, build)`：子图在**运行期**由 `build(*SubCtx)` 造，**绑定校验也在运行期**（`runSub` 建完子图才比对键存在与类型） | **已删除**（原 `sub.go` 的 `SubCtx` / `SubFunc` / `runSub` / `subStartedErr`） | 由 `Sub` 组合现成 `*Graph` 取代；绑定校验进装配期与 `Validate`。**阶段 1 已落地**：整套动态面**直接删掉**，不是改名留着——改名只是把旧模型带进新模型；端口 / 类型 / 环都在装配期与 `Validate` 报 |
| `claimSub` + `adopt`：认领一次性 + **改写子图字段**（`observer` / `lim` / `aspects`） | `graph.go` 的 `claimSub`、`sub.go` 的 `adopt` | 由「创建子 Execution 时算有效配置」取代，定义不再被写。**阶段 1 做了一半**（`path` 已经不上定义，按挂载位置现推）；**阶段 2 已收口**：两个东西整体删除——不再有认领（定义可以跑很多次），有效配置（观察者 / 名额 / 图级切面）在**建子执行**时按「父执行环境 + 子图自己的声明」算出来 |
| `Start` / `Wait` / `Run` / `Err` 都在 `Graph` 上；`Start` 只能成功一次 | `graph.go` 的运行入口 | 搬到 `Execution`；`Graph` 只留同步便捷入口。**阶段 2 已落地**：`Start` / `Wait` / `Err` / `Cancel` / `Site` / `ID` 以及槽位与每节点状态都在 `Execution`；`Graph` 留 `Run(ctx, in)`，内部 `NewExecution` + `Start` + `Wait`。`ErrGraphStarted` 的第二层含义（定义已封口的那条）不变 |
| `slot` 是「到达即发布、幂等首写」，没有 attempt 层 | `slot.go` | 每个 attempt 一份写暂存，成功才提交（第 5 节）。**阶段 3 已落地**：写路径按「有没有尝试边界」分流——有边界就落写暂存（`slot.go`），没有就直写执行槽位；暂存与槽位共用同一个 `slot` 类型，所以「幂等首写 / 就绪与跳过互斥 / 阻塞等待」几条语义逐条一致 |
| `Retry` 的文档明说「**重试安全的前提**：失败前没有写过任何 Provide」 | `aspect.go` 的 `Retry` | 这条前提改由引擎接管——本票硬要求。**阶段 3 已落地**：失败的尝试整条暂存丢掉，那一次的输出版本下游从未见过（不是「被幂等首写静默吞掉」，而是根本没写进去） |
| `Topology()` 只覆盖**一张图** | `topology.go` 的 `Topology` | 递归全树。**阶段 1 已落地**：静态挂载的子图在 `Nodes[i].Sub` 里、路径按挂载位置推导（`GraphShape.Path` 是**这一次视图里**的位置，不需要运行、也不上定义） |
| `Graph.Snapshot()` + `NodeState.Child *Graph` 建在「Graph = 一次运行」之上 | PR #310 的旧草案 | **阶段 4 已实现（#319），静态端口、执行快照、Mermaid 与 YAML Analyze 均已具备**：`Execution.Snapshot()` 替换旧形状，保留本次 Execution 身份、状态和来源；Child 是最新已创建尝试的活子执行。Topology 补完整声明端口，Mermaid 从结构派生，YAML Analyze 无需注册表 |
| `Validate()` 只看本图；`Problems` 一次给全、`Start` 报第一条 | `topology.go` 的 `Topology`、`problems.go` | 组合**整棵树**校验；错误仍在最早可判定处报。**阶段 1 已落地**：`Validate` 沿静态挂载边递归、**按引用点**各报一条并点明挂载位置；组合环在 `Sub` 装配期拒 |
| YAML 的 `Load` 返回 `(*Graph, *SeedPlan)`，子图也在运行期建 | `yaml/load.go`、`yaml/subgraph.go` | 结构语义与 Go 组合对齐；`SeedPlan` 只负责**本次执行**的输入。**阶段 1 已落地**：子图在 `Load` 期按挂载位置装一次（一处一张实例、装完即可递归看全树）。**阶段 3 已落地**：`retry:` 落在 `graph:` 节点上照装照跑（每次尝试各建一份新子执行，第一次那轮的桥回随它自己那条写暂存一起作废）。**阶段 2 已落地**：`SeedPlan.Apply` 换成 `SeedPlan.Inputs(resolve)`——它产的是**一次运行**的 `pulse.Inputs`，装配器不再碰任何 ctx（`LoadOptions.Context` 随之删除） |
| 限流 / 切面 / 观察者靠 `adopt` **继承**（写进子图字段） | 原 `sub.go` 的 `adopt`；当前 `execution.go` 的有效配置与 `execution_run.go` 的 limiter | 改成「执行树创建时算有效配置」，不动共享定义。**阶段 2 已落地**：三个有效值（出口 / 名额 / 图级切面）在建子执行时按「父执行环境 + 子图自己的声明」算定，此后不写；`WithExecObserver` / `WithExecMaxRunning` / `WithExecAspects` 覆盖**这一次运行**，优先级 = 本次运行 > 定义声明 > 父执行环境 |

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

**来源类型也要跟着分开**：静态拓扑里「端口」的来源是**新增的 `SourceInput`**（`pulse.SourceInput`），不再是 `SourceSeed`。理由：`Seed` 是**运行期**的动作，让静态结构里出现一个只在该次运行成立的来源名，等于把「定义」和「这一次」又混回去了。**阶段 2 之后 `SourceSeed` 已经删除**：值属于一次运行的 `Inputs`，定义层的「外部来源」只剩 `SourceInput` 这一种写法（`SourceKind` 因此只有 `SourceNone` / `SourceInput` / `SourceNode` 三个值）。

校验分工：`Validate()` 只查静态结构与组合约束（不要求运行值存在）；`NewExecution` 查「这次运行需要的输入齐不齐」（这是启动条件，不是静态毛病）。

### 3.2 运行配置属于执行树，不写回定义

ctx、路径、观察者、切面、限额都是**一次嵌套执行环境**的属性。创建子 Execution 时按「父执行环境 + 子图自己的声明」算出有效值（默认继承、`WithoutAspects()` 切断切面、限额共享同一棵执行树的计数、`Sub` 那一步本身不吃名额）。定义保持只读，因此「同一份定义被两处挂载」不会再互相踩。

### 3.3 身份与观测归因

| 记号 | 含义 | 今天 |
|---|---|---|
| 图 id | **定义**身份（`New` 给的） | `pulse.graph` |
| 执行 id | **本次运行**身份（引擎生成进程内唯一序号，子执行还携带父执行与挂载位置） | `pulse.execution`，`Site.Execution`，`Snapshot.Graph.Execution` |
| path | 挂载位置（根 `""`，一层 `step`，再深 `step/inner`） | `pulse.path` |

要求：同一张 Graph 的多次执行在观测里**分得开**（否则并发执行的记录会合并成同一份，归因直接错）。**执行 id 对每一个执行实例唯一**，不做「父 id + path」的纯确定性派生：同一父执行、同一挂载点、`Retry` 第 n 次 attempt 重建出来的子执行也是**不同的实例**，实例序号要进 id——否则同一个挂载点重试几次的记录会合并成一份（评审 #62）。

### 3.4 两种环要分开（评审 #83）

- **依赖环（Key 环）**：节点之间 `Requires` / `Provides` 成环。今天就在 `Validate` / `Start` 报一条具体见证（`pulse: dependency cycle: A -> B -> A (A requires "y", B requires "x")`，`problems.go`），这条**不动**。
- **组合环（子图环）**：一张图出现在**自己的子树**里——自己挂自己，或 A 挂 B 之后 B 又挂 A。静态 `Topology()` / `Validate()` 会无限展开，所以**必须在装配期拒**：`Sub` 在挂载那一刻沿子图子树查一遍「父图是不是已经在它的子树里」，是就报一条点明环路径的错（`pulse: subgraph cycle: A -> B -> A`）。
- **判据不能用全局 visited**：同一张子图在**不同挂载位置**重复出现是**合法**的（这正是本模型要支持的核心用法），判定只能用**当前递归路径上的祖先集合**；兄弟分支共用一个子图不是环。
- **YAML 那条检查要与它统一**：`pulse/yaml` 已有的 graph-reference 环检查是对 **spec 名**做的，引擎这条管 **对象图**（同一个 `*Graph` 出现在自己的子树里）——两者判据不同（一份声明 vs 一个实例，同一份 spec 可以装出多张图），但**报文形状要一致**，且 YAML 装图期要把引擎这条也走一遍，不能只查 spec 名就放过。

## 4. API 形状（已实现的定义、执行与视图）

```go
// —— 定义侧（可复用、可组合、封口后不可变）——
func New(graphID, description string, opts ...Option) (*Graph, error)
func (g *Graph) Add(n *Node) error
func (g *Graph) DeclareInput(ports ...Port) error        // 静态输入端口（NewPort(k)）
func (g *Graph) DeclareOutput(ports ...Port) error       // 对外可绑定的产出端口
func (g *Graph) Topology() *Topology                     // 静态全树（递归，含端口与绑定）
func (g *Graph) Validate() error                         // 静态结构 + 组合约束

// —— 组合：挂一张现成的子图（唯一入口）——
func Sub(g *Graph, id, description string, binds []SubBind, child *Graph, aspects ...Aspect) error

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
- `Inputs` 是**值**（`pulse.Seed(&in, k, v)` / `pulse.SkipSeed(&in, k)`），不是变参 Options：一次运行的输入是一份数据，不是一串装饰。
- 端口构造器 `NewPort(k Key[T]) Port` 收类型化的键，名字与类型只写一遍——写字符串名字最容易歪。
- `NodeState.Child` 从 `*Graph` 变成 `*Execution`；`Topology().Nodes[i].Sub *Topology` 承载静态子树，`Binds` 承载父子接线。
- **不**另造 `GraphTemplate` / `GraphPlan`：`Graph` 本身就是那个可复用定义。

### 4.1 阶段 4 的视图边界

快照挂在 `Execution` 上，`GraphState.Execution` 与 `Site.Execution` 一致；节点声明序与 Topology 对齐，运行态按定义节点指针存在本次执行的 map 中，启动前新增节点仍可见为 pending。回调前先写状态；Retry 不重复 waiting/running，也不让状态倒退。图级终态在 Wait 收尾时确定，Child 保留最新已创建尝试的活执行，失败后不丢句柄。

`GraphShape.InPorts` / `OutPorts` 是首次成功声明序的完整端口，包括 unused 和透传；它们使 GraphShape 不再可整体比较，按 minor 发布并给迁移说明。Snapshot 没有值字段，也没有带值开关；节点与槽位分别采样，不承诺原子切面。运行中的节点已有 ready / skipped 输出是合法事实。

Mermaid 归根包：`(*Topology).Mermaid(opts ...MermaidOption) string`，用 `WithMermaidSnapshot(s)` 叠状态。结构一律取 Topology，状态按图 ID + Path + Node ID 匹配，子执行再单独采样，不显示固定值与错误正文。YAML 的 `Analyze(data []byte, opts LoadOptions) (*pulse.Topology, error)` 无需注册表，仍需 GraphID 与静态选项，确保和 Load 的身份与判据对齐。Type 是文档标签，可沿绑定传播；无已知声明时为空串，不能证明真实 Go 类型或 literal 赋值性。

## 5. 事务性 Retry（本票硬要求）

**已落地（阶段 3，票 #317，分支 `refactor/txn-retry`）**：下面 9 条是阶段零定下的硬要求，实现按它们收；逐条对应的用例与变异对照见票面。落地后的对外口径写在 [`pulse.md`](pulse.md) §4「`Retry` 的尝试边界」。

当时的形态：槽位「到达即发布、幂等首写」，`Retry` 重跑时前一次 attempt 已经 `Set` 过的槽位**已经发布**——下游被唤醒，第二次 attempt 的写还会被静默忽略（`aspect.go` 的 `Retry` 明说这条前提由调用方守）。新模型把它收进引擎：

1. **attempt 写集**：`Set` / `Skip` 先落到**本 attempt 的写集**，不直接进执行槽位；
2. **读自己写的**：attempt 内 `Get` 自己已写的 Provide 读到的是**本写集**的值（自洽）；
3. **丢弃**：attempt 失败且还要重试 → 整个写集丢掉，**下游从未见过**；
4. **提交**：attempt 成功（或在拿到 `Retry` 之后明确以跳过收尾）才把写集写进执行槽位；不同 attempt 的输出不会混写；
5. **跳过 ≠ 可重试失败**：`NoValue` / `*SkipError` 不触发重试（今天的判据照旧：`isSkipped`）。
6. **「不继续重试」的判据要说准**（评审 #114）——看**进入 `Retry` 这一层时拿到的那个 ctx**（含它外层派生出来的）是不是已经取消 / 到期：
   - **Execution / 父 ctx** 取消或到期 → **不重试**，错误原样带出（再试也不会变好，而且外层已经不再等它）；
   - `Retry` **外层**的 `Timeout` 触发（`Timeout(Retry(f))`）→ 就是上一条：`Retry` 拿到的正是那层派生 ctx，已到期 → 不重试；
   - `Retry` **内层**包的超时（`Retry(Timeout(f))`）→ 那只是**这一次 attempt 的失败**，照常重试（`Retry` 自己那一层的 ctx 并没有到期）——这是「这次没在时限内做完，再试一次」的正常用法。
   今天的 `Retry` 已经是这个判据（`Retry` 判 `rc.ctx.Err()`；`Timeout` 用 `Fork()` 把派生 ctx 交给内层）。新契约把它**写下来并测**，实现照旧——它必须先有专门用例（四种嵌套：`Retry(Timeout)`、`Timeout(Retry)`、父取消、attempt 自己返回 ctx 错误）才允许被改。**阶段 3 已落地这四个用例**（`TestRetryCtxJudgement`）。
7. **最终失败**：写集丢弃 + 未写输出按既有语义自动跳过 + 错误原样冒泡（槽位终态与节点终态一致，可测）；
8. **切面顺序决定事务边界**：`Retry` 包住的那一段（`next` 之内、含更内层切面）属于 attempt；`Retry` **外层**切面里的写入**不**属于任何 attempt、也不参与作废——文档必须点名这条，不能把外层的写默默说成事务内的；
9. **多 Key 的可见性保证**：硬保证是「**没有任何失败 attempt 的输出被下游观察到**」。**已定案（2026-10-11；2026-10-12 评审后补一条）**：第一版**不承诺**同一节点多个输出严格原子可见——提交**逐条**写（每条走槽位自己的幂等首写与广播，没有执行级大锁；按 key 名排序，map 的迭代顺序不进任何可观察行为）。**补的那一条**：逐条写之前先**整批判过**（判据 = 对面是不是已经以另一个状态到达，值写进已跳过的槽 / 跳过写进已就绪的槽都是 `ErrConflict`），判不过**一条都不发布**——「提交失败留下半批」（前面几条已经落地、下游可能已被唤醒）没有可解释的语义，不是这条窗口允许的东西（PR #318 评审点名，实现与用例见 `slot.go` 的 `commit` / `checkCommitTarget` 与 `TestRetryCommitConflictPublishesNothing`）。窗口的**准确**形状（评审 #117）：槽位是一次性的、开始全是 `pending`，失败 attempt 的写集整批丢弃，所以窗口里**看不到任何旧 attempt 的值**；可能看到的是「**一部分输出已经 `ready`、另一部分仍是 `pending`**，等这些输出的下游可能被先到的那一条先唤醒」。验收要专门覆盖这个窗口（一条输出已到、另一条尚未提交）；要严格原子得另设 commit/read 同步协议，留作后续票。

**嵌套 `Retry` 的边界**（同一轮评审点名）：内层提交的目标是**外层那一次尝试的暂存**（`writeSet.outer`），所以内层的成功**不等于**对外发布——外层那一次尝试失败时，内层的写随外层的暂存一起作废（用例 `TestNestedRetryInnerResultBelongsToOuterAttempt`）。「内层直接落执行槽位」是一条能跑出错误结果的实现（外层失败那批会先发出去，而成功那批被幂等首写静默吞掉），已进变异对照。

另有一条**阶段 3 实施时新增的边界**（不在原 9 条内）：**流生产端（`Produce` / `Tee`）不能套在写入暂存切面下**——它们「先 `Set` 一条 channel、再往里发值」，有尝试边界时那条 channel 要等提交之后才发布，而消费端在拿到它之前只能等着，双方互等、整轮挂到 ctx 到期（实测 3s 到期时 `produced=1, consumed_sum=0`）。判据看「有没有写暂存」并在运行期当场报错（`Aspect` 是裸函数类型，装配期认不出切面的身份），见 `pulse.md` §5 流式那一节。这是阶段 2 之后的一处**静默回归**（那份图在阶段 2 是「能跑」的），所以补了明确的拒装 + 用例 + 两条变异对照。

### 5.1 事务边界必须诚实

引擎能作废的只有**它自己管理的槽位暂存**。用户 `Run` 里已经发生的任意外部副作用（数据库提交、HTTP 请求、邮件、写文件）**不可能**被自动撤销——**阶段 3 落地时这段原文照进了 `Retry` 的 godoc 与 [`pulse.md`](pulse.md) §4**，不允许用「内存里暂存过」暗示外部世界能回滚。要接事务的调用方有两条正路：把副作用挪到最后一次成功 attempt（先算完再提交），或用幂等键 / outbox / 补偿；引擎只保证第 8 条那条边界**说清楚**。

## 6. 兼容与迁移

- **发布节奏（2026-10-18 更新）**：这是 0.x 的破坏性改造，走 **minor**，配迁移说明；不在 patch 里做。阶段完成与发版分开：Pulse 完成约定能力、文档、候选审核与 Durable 桥接联调后，准备 `v0.5.0`。两仓不再共用发布闸门；`pulse-durable` 保持未发布，待维护的数据库 Store [#40](https://github.com/Luo-root/pulse-durable/issues/40) 完成并验收后，再考虑首发 `v0.1.0`，仅供评测的 SQLite 适配器不算这项交付。Core tag 可下载后，Durable 可以对齐依赖并复核，不因此触发发版。发版由维护者决定；首期代码已具备完整节点链检查点桥接、Store/Codec、Run/Resume、方案 A 子图恢复与本机文件后端，持久等待和分布式运行管理仍属后续方向。之后继续 0.x 迭代，直到项目目标达到、确认后续不再有系统性重大变更，再完善详细文档体系并发布两仓各自的 `v1.0.0`，基本固定 API 与实现。首期必要的 godoc、示例与迁移说明仍随实现同步，不留到 `v1.0.0` 才补。完整发布闸门见 [`workflow-foundation.md` §8](workflow-foundation.md#8-演进顺序与可交付产物)。
- **`Sub` 换了含义（最要紧的一条，迁移说明放最前）**：`pulse.Sub(g, id, desc, binds, build)`（运行期现造子图）**被删除**，今天 `Sub` 是「挂一张**现成的**子图」（第 4 节）。跟着一起没的还有 `SubCtx`（`Context()` / `GraphID()` / `Path()` / `Observer()`）与运行期的绑定校验。替代写法逐条：① 子图在**装配期**建好，并 `DeclareInput` / `DeclareOutput` 声明端口——接线只能接端口，接子图内部的中间 Key 是静态错；② 子图的取消域**不用自己管**：每一次跑到那一步，引擎从**那一步的 ctx** 派生子执行的 ctx（阶段 2 之后没有转发 goroutine 这回事）；③ 图 id 自己起，建议沿用 `<父图 id>/<节点 id>` 这套写法；④ 挂载位置随每条回调由引擎发出来（`Site.Path`），出口直接读它就能带 `pulse.path`——不必再按层各建一个出口。`Fix` / `FixSkip` 补上「某一处挂载的固定值」这一格。YAML 侧**阶段 1 收紧过的那条已经撤回**（阶段 3）：`graph:` 节点上的 `retry:` 照装照跑——每一次尝试各建一份**新的子执行**，第一次那轮的桥回随它自己那条写暂存一起作废，父侧只拿得到成功那一次的输出版本（Go 侧 `Sub(..., pulse.Retry(...))` 同理）。
- **冻结面怎么动**：七条哨兵**不新增、不改名**，但触发点要在 godoc、迁移说明与 release notes 里写准（第 8 节 (e)）：`ErrGraphStarted` 有两处——定义已封口后，`Add` / `DeclareInput` / `DeclareOutput` / `Sub` 改定义被拒；同一 `Execution` 二次 `Start` 被拒。`ErrGraphNotStarted` 仅由尚未成功启动的 `(*Execution).Wait` 返回；`Err` 不要求先启动；收尾前读取首错或当前 ctx 错误，收尾后与并发/重复 Wait 复用最终结论（会合可靠工作后，已锁存的树级 CheckpointError 优先并归为 failed，否则保持普通首错或真实取消），Wait 正常收尾的资源释放不会变成取消结果。
- **端口是新的一层，子图要跟着声明**：`DeclareInput` / `DeclareOutput` 落地后，**被当作子图组合的图必须声明端口**——旧形态那种「绑定指向子图里任意声明过的 Key」不再成立（接到非端口是静态错）。迁移说明要逐条给出「原来绑内部 Key → 现在补一句 `DeclareOutput`」的改法。
- **来源类型多一个值**：`pulse.SourceInput` 与 `node` 并列（`SourceSeed` 已随阶段 2 删除，见第 6 节）。`SourceKind` 不在冻结面里（冻结面只有槽位三态、`Aspect`、七条哨兵、Observer 次数、六条编码原语 + `LineRenderer`），所以这是**新增**而不是破坏。
- **组合环是新拒的一类**：今天 YAML 只查 graph-reference 名环；引擎侧「同一张图挂进自己的子树」从阶段 1 起在 `Sub` 装配期拒（见第 3.4 节）。这是**新增的静态错**，迁移说明里点名。
- **不保留旧入口**：`Seed` / `SkipSeed` 这类「往定义里写值」的入口不留在 `Graph` 上（留着就会让「装配完即可见完整结构」这个核心性质在旧入口上不成立，两套语义并存=含糊）。`Add` / `Validate` / `Topology` 这类不冲突的入口保持。
- **PR #310 的处置**（#311 点名要评审）：**已定案——替换**（见第 8 节）。`Graph.Snapshot()` / `Child *Graph` 这个形状不进 main；它的成果面向 `Execution` 重做，**归阶段 4**（阶段 2 只把运行态搬到位，没有引入快照）。
- **`g.slots` 锁统一**：阶段 1 时这张表装配期写点持 `g.mu`、运行期读点持 `slotsMu`（#310 的新读入口把它暴露成 `fatal error: concurrent map read and map write`）。**阶段 2 已从形状上消除**：槽位表整体搬进 `Execution`（各自的 `slots` + `slotsMu`），共享的那张表不再存在。
- **文档怎么跟**：`pulse.md` 顶部标状态与**已落地的阶段**；本文与 [`philosophy.md`](philosophy.md) 描述目标；**每个阶段落地时把示例、README、站点一起迁完**（阶段 1 与阶段 2 都是这么收的，阶段 2 顺带把 `TestDocsExamplesUseCurrentSignatures` 扩到本阶段改动的入口——否则同一类漂移会再发生一次）。

## 7. 阶段路线（票的顺序与验收）

| 阶段 | 产出 | 验收要点（摘 #311，票面写全） |
|---|---|---|
| **0（本文）** | 对象模型与契约冻结 + 理念文档 | 本文定案；#303 / #309 / #310 关系写清 |
| **1 静态定义与组合** | `Sub` 组合现成子图是**唯一**入口，旧的动态形态（`SubFunc` / `SubCtx`）**整体删除**；`DeclareInput` / `DeclareOutput` 端口（**出口同样有静态判据**：声明了却没人产出 → 装配期报）；接线形状的判据共用一份 `checkBinds`（进口只许绑一次、进口在子图里已有来源就判死、`In` + `Out` 打同一端口 = 透传）；组合环在装配期拒（「查一遍子树 + 挂上」在同一个全局装配临界区里，递归另带挂载链兜底判环）；递归 `Topology` / `Validate`（**每层自己取锁**，`Validate` 通过 / `Start` 成功时**整棵子树一起封**，且「判 + 封」与改定义共用装配锁）；运行 ctx 由父节点给 | A/B/C 各自构建、D 组合后**无运行**即可递归看全树并报全部静态错；同一子图多处挂载不改定义；预建子图上的取消传得下去 —— **已落地**（PR #314，`refactor/sub-to-subfunc`：`Sub` / 端口 + 整棵子树封口 / 递归拓扑与校验 / yaml 在 `Load` 期装一次 / 未引用的 spec 不查来源）。这一阶段顺手加的那条临时限制——`retry:` 落在 `graph:` 节点上拒（yaml `Load` 期、Go 进 `Run` 第一件事）——**已由阶段 3 撤回**：每次尝试一份新子执行之后，它不再是问题 |
| **2 执行运行时** | `Execution`；槽位 / 状态 / 错误 / 取消域隔离；子 Graph → 每个挂载位置一个子 Execution | 同 Graph 顺序两次结果隔离；并发两次 `-race` 干净；取消只影响本次 —— **已落地**（票 #315，`refactor/sub-to-subfunc`：`Execution` / `Inputs` + `Seed(&in,…)` / `Site{Graph,Execution,Path}`（观测身份一次到位）/ 每挂载点一份子执行 / 启动条件在建执行那一刻判 / `ExecOption` 三件 / 定义侧只剩 `Validate` + `Run` 便捷入口 / `New` 去 ctx / `SourceSeed` 删除 / YAML 的 `Load` 不碰 ctx） |
| **3 事务性 Retry** | attempt 写集、提交 / 丢弃；aspect 边界 | 「第一次写 K=a 后失败、第二次写 K=b 成功 → 下游只见到 b」；多 Key 一个都不可见；变异对照：去掉暂存必红 —— **已落地**（票 #317，`refactor/txn-retry`：`slot.go` 的写暂存帧链（提交目标是父写入目标）/ 写路径与读路径分开（读不建条目）/ 推进闸统一到最内层 core（判据 = 有没有新尝试边界）/ 收尾按「落没落地」判 / `Retry` 装上子图步骤与 yaml `graph:` 节点 / 流生产端拒写暂存 / 五条既有契约用例翻面 + 8 条新验收用例 + 8 条变异对照） |
| **4 快照 / 观测 / YAML 对齐** | `Execution.Snapshot()`；Observer 身份；YAML 与 Go 同语义；Mermaid 静态全貌（可叠加一次执行） | 快照与结构同序同 id；子 Child 指向子 Execution；YAML `Analyze` 与 Go `Topology` 结构语义一致 —— **已实现（#319），静态端口、执行快照、Mermaid 与 YAML Analyze 均已具备**：完整有序端口 / 无值执行快照 / 活 Child / 根包 Mermaid 与快照选项 / registry-free Analyze(data, LoadOptions) / 文档签名闸门扩容 |
| **5 迁移与发布** | 文档 / 示例 / release notes 全部迁移；不兼容项逐条列 | `go build` / `vet` / `-race` / `gofmt` 全绿；零外部依赖；旧模型示例清零 |

每阶段一条铁律：**先开票、评审过再开工**；每条语义要有用例，行为类结论要有变异对照。

## 8. 已定案与遗留

**已定案（2026-10-11，含 PR #312 评审后的补充）**：

- **(a) 输入的形状**：`Inputs` 值 + `pulse.Seed(&in, k, v)` / `pulse.SkipSeed(&in, k)`——一次运行的输入是一份数据，不是一串装饰。
- **(b) 端口声明**：`g.DeclareInput(ports…)` + **对称的 `g.DeclareOutput(ports…)`**。端口是**唯一的接线面**：`In` 只能接输入端口、`Out` 只能接输出端口，接到内部中间 Key 是静态错；来源类型用**新增的 `SourceInput`**（第 3.1 节）。
- **(c) 封口时机（评审 #157 补充定案）**：`Validate()` **通过才封口**，校验失败保持可变（补 `Add` / `DeclareInput` 再校验）；**不设 `Seal()`**。阶段 1 加固时补了两条同源的边界，写进这里是免得后面重开：① **`Start()` 成功同样封整棵子树**（阶段 2 之后这个 `Start()` 是 `(*Execution).Start()`；删掉这一步，持有子图指针的调用方能在「父图已启动、子图还没轮到」之间接着 `Add` / `DeclareInput`——实测都进得去）；② **改定义的四个入口（`Add` / `DeclareInput` / `DeclareOutput` / `Sub`）与 `Validate` / `Start` 的「判 + 封」共用一把装配锁**，所以「判完整棵树」与「封上它」之间没有窗口——不加锁的话 `Validate()` 会返回 nil 却封住一份没校验过的定义（实测第 0 轮即中）。`Graph.sealed` 与 `Execution.started` 是两个对象上的独立状态，不存在字段优先级：四个改定义入口按定义是否 sealed 拒绝；同一份 `Execution` 二次 `Start` 按该执行是否已启动拒绝。两种触发共用 `ErrGraphStarted`，封口后的定义仍能创建新的执行。
- **(d) 多 Key 提交可见性**：只保证「失败 attempt 不可见」；成功提交逐条写，窗口的准确形状是「一部分输出已 `ready`、其余仍 `pending`」（见第 5 节第 9 条）。
- **(f) PR #310**：**替换**——`Graph.Snapshot()` + `Child *Graph` 的形态**不进 main**；#310 里与模型无关的成果（状态词表对齐、`waiting` 的两种读法、反射断言不带值、并发用例、`g.slots` 锁统一）转成阶段 2 的素材，面向 `Execution` 重做；#309 随之标 superseded。
- **组合环**：与依赖环分开；`Sub` 装配期拒，判据用**当前递归路径上的祖先集合**（多处挂载合法）；**「查一遍子树」与「真的挂上」在同一个全局装配临界区里**（阶段 1 落地：组合环是两张图各自动作的合成结果，单看任何一张图的锁都拦不住对向挂载），递归另带一条挂载链做兜底判环（环的代价是爆栈或互等锁，不是一句错话）；YAML 那条 spec 名检查与它报文形状统一（第 3.4 节）。**同一把锁（`assemblyMu`）在阶段 1 加固时扩了半个身位**：改定义的入口与 `Validate` / `Start` 共用它，于是「判完整棵树再封」这件事也成了全局不变式——见 (c)。
- **落在挂载子图那一步上的 `Retry`**：**阶段 1 / 2 期间不支持，阶段 3 起支持**。当时拒的理由值得留档：这一步的桥回（写进父侧槽位）是在第一次 attempt 里做完的，而父侧槽位是幂等首写——凡是第一次已经桥过去的键，第二次 attempt 的写会被静默丢掉，父图手里就是「一半第一次、一半第二次」的混装；同时 `Retry` 返回的是**最后一次** attempt 的错误，第一次失败的真正原因也被盖掉。当时的判据放在 `Retry` 进 `Run` 的第一件事（装配期认不出一个 `Aspect` 是不是 `Retry`——裸函数类型，闭包没有可查标识，实测 `reflect` 代码指针每次实例化都不同），`pulse/yaml` 的 `Load` 期拒同一个形状。**阶段 3 把缺的那一半补上之后，这个形状整体解禁**：每次尝试各建一份**新的子执行**，第一次的桥回随它自己那条 attempt 写集一起作废，父侧只拿得到成功那一次的输出版本；`retry:` 与 `Sub(..., Retry(...))` 都照装照跑。**重入仍然管住**：无新边界的重入（用户自己写的重试切面、或用户自写的重试用例）当场 `ErrNextCalledTwice`，并把第一次的失败原因一起带出来。
- **执行 id**：对每个执行实例唯一，**不**做「父 id + path」的纯确定性派生——同一挂载点的第 n 次 attempt 重建的子执行也是不同实例（第 3.3 节）。**阶段 3 落地后这条真的生效了**：`Retry` 装在子图步骤上时，每次尝试各是一份新子执行、各有自己的执行 id（用例 `TestRetryOnSubgraphStepRunsEveryAttemptOnAFreshChildExecution` 按 `Site.Path == "step"` 数出两个执行 id）。
- **Retry 与取消 / 超时**：判据是「进入 `Retry` 那层拿到的 ctx 是否已到期」；内层 `Timeout` 可重试、外层 `Timeout` 与父取消不重试（第 5 节第 6 条）。
- **发布切分（历史记录与当前口径）**：早期阶段规划曾拟「每阶段落地就发一个小版本」；现行规则是阶段完成与发版分开。Pulse 完成约定能力、文档、候选审核与 Durable 桥接联调后，准备 `v0.5.0`。两仓不再共用发布闸门；`pulse-durable` 保持未发布，待维护的数据库 Store [#40](https://github.com/Luo-root/pulse-durable/issues/40) 完成并验收后，再考虑首发 `v0.1.0`，仅供评测的 SQLite 适配器不算这项交付。Core tag 可下载后，Durable 可以对齐依赖并复核，不因此触发发版。发版由维护者决定。首期代码已具备完整节点链检查点桥接、Store/Codec、Run/Resume、方案 A 子图恢复与本机文件后端，持久等待和分布式运行管理仍属后续方向；之后继续 0.x 迭代，直到项目目标达到、确认后续不再有系统性重大变更，再完善详细文档体系并发布两仓各自的 `v1.0.0`，基本固定 API 与实现。首期必要的 godoc、示例与迁移说明仍随实现同步。

**原暂定项的实施结果**：

- **(e) 哨兵语义**：复用 `ErrGraphStarted` / `ErrGraphNotStarted`（不改名、不新增），触发点已落地并在 godoc 与迁移口径中写清：`ErrGraphStarted` 用于定义已封口后改定义被拒，以及同一 `Execution` 二次 `Start` 被拒；`ErrGraphNotStarted` 仅由尚未成功启动的 `(*Execution).Wait` 返回；`Err` 不要求先启动；收尾前读取首错或当前 ctx 错误，收尾后与并发/重复 Wait 复用最终结论（会合可靠工作后，已锁存的树级 CheckpointError 优先并归为 failed，否则保持普通首错或真实取消）。
- **(g) 观测身份**：`pulse.execution`（本次执行 id）、`pulse.graph` 继续是定义 id——否则同一张图在**同一位置**跑两次的记录会合并。**阶段 2 已落地**：引擎的 `Observer` 回调带 `Site{Graph, Execution, Path}`，`observe` 把 `AttrExecution` 写进**每一条**记录（内部记账也按 `(本次运行, 节点 id)` 分桶）。

## 9. 不做（本阶段就写清，免得回头重开）

- 不为静态图另造一套平行的模板 / 计划模型（`Graph` 本身是定义）；
- 不清空旧槽位来复用 `Execution`——每次执行新建独立运行状态；
- 不承诺自动撤销用户外部副作用（第 5.1 节）；
- 不为迁就 `Graph.Snapshot()` / `Child *Graph` 的旧形状而保留旧对象模型——公开面服从职责边界；
- 不因节点重试而重复生命周期回调：`Retry` 重入不重复 `Waiting` / `Running` / `Finished`，节点三回调的次数契约不变。逐次尝试、重试、退避与超时到期事实由现有可选 `AspectObserver` 另行提供，见[观测指南](../../site/guide/observability.md)（[English](../../site/en/guide/observability.md)）。子图步骤每次尝试是一份新的**子执行**，它自己那对图级事件照发，归属各自的子执行；
- 不做隐式补偿，也不做任何不透明的事务魔法（要加就得有单独契约 + 测试）。
