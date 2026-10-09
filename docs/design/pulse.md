# pulse 设计：图编排 + 图观测

> 状态：Accepted
> 包位置：根包 `pulse`（图引擎）+ `pulse/observe`（图观测）+ `pulse/yaml`（声明式装图）
> 本篇是 pulse **唯一**的设计文档，只写两件事：**编排**与**观测**。

## 0. 定位

pulse 是一个**一次性运行的图引擎**，外加一层**图观测**：

> 依赖声明即拓扑，数据到达即调度；失败显式，取消能打断等数据。
> 观测经 Observer seam 折叠成结构化记录，引擎不认识任何观测包。

只有三样东西，边界由此决定：

| 包 | 是什么 | 依赖 |
|---|---|---|
| `pulse`（根） | 图引擎 | **零**（只用标准库） |
| `pulse/observe` | 图观测：把引擎的 Observer 回调折成 `Record` 写 `Sink` | `pulse` |
| `pulse/yaml` | 声明式装图：YAML → 图 | `pulse` + `yaml.v3` |

**依赖箭头单向且不许反向**：引擎不 import 观测，观测实现引擎的 seam。这让引擎可以被单独取用——一个不需要观测的宿主只 import 根包。

---

# 第一部分 · 编排

## 1. 图模型

### 1.1 Key：类型化的数据槽

```go
var Summary = pulse.NewKey[string]("summary")
var Docs    = pulse.NewKey[[]Doc]("docs")
```

`Key[T]` 把「槽位的名字」与「值的类型」绑在一起。名字用于诊断与 YAML 对账，类型给两件事：

- **编译期安全**：`Get(rc, Summary)` 直接得到 `string`，不需要断言；
- **装配期的同名跨型拒绝**：同名 Key 必须以同一个 `T` 注册（`keyRegistry.register` / `Registry.RegisterKey`），不做「同名换类型」的静默覆盖。

内部把 `Key[T]` 擦成 `keyRef{name, reflect.Type}`（未导出），运行期以类型指纹判等。

### 1.2 节点：声明读什么、写什么

节点**不声明下一个节点是谁**，只声明读哪些 Key、写哪些 Key：

```go
pulse.NewNode("summarize",
    pulse.Requires(Docs),     // AND 前置：全部输入到达（就绪或跳过）才判门
    pulse.Provides(Summary),  // 本节点会写出的槽位
    func(rc *pulse.RunCtx) error {
        docs, err := pulse.Get(rc, Docs)
        if err != nil {
            return err
        }
        return pulse.Set(rc, Summary, join(docs))
    })
```

依赖由 Key 的生产与消费**隐式形成**：没有边对象、没有拓扑排序、没有调度循环。`Deps(...)` 用来把一个节点的多组 Requires / Provides 拼成一条声明。

`RunCtx` 是一次运行里节点能看到的世界：**只有它声明过的槽位**加上本层可取消的 context。拿不到整个黑板——`must()` 会拦下未声明的读写（`ErrUndeclared`）。

### 1.3 槽位三态

```
未就绪(pending) | 已就绪(ready, 值) | 已跳过(skipped)
```

**就绪和跳过都是「到达」。** 等待者被唤醒后区分这两种到达，而不是把「永远不到」伪装成一个假值。这是依赖驱动的图引擎最容易做错的一处：把 skip 当失败会让「分支」这个基本操作无处安放。

由此推出三条规则：

- **到几个收几个**：`Requires` 等**全部**输入到达（就绪或跳过）后才判门——只要有一条输入真的到了值，节点就带着到了的那些进入 `Run`；
- 一条值都没到（全部输入都以跳过到达）→ **不执行 `Run`**，全部输出跳过（级联）——纯分支的下游仍按跳过收尾；
- `Run` 成功返回后**漏写的 Provides 自动跳过**（否则下游永远等不到到达）。

判门看的是「**有没有值**」，不是「有没有跳过」：上游某一路没有值，不该让手里还有数据的下游跟着停，也不该让已经到达的值作废。缺值的那几路在 `Run` 里读回来是 `*SkipError`（`errors.Is(err, ErrSkipped)` 成立），逐条问用 `TryGet`；反过来，想「缺一条就别跑我」的节点把 `WaitAll` 的返回值**直接返回**即可——那是显式的 fan-in 策略声明：引擎让本节点以「跳过」收尾，**尚未发布的输出被跳过、已经发布的槽位不回滚**（与 §2 的一次性槽位契约一致），且不是失败。

`error` 则相反：取消整图，`Run` / `Err` 返回原错误，**不会伪装成 `ErrSkipped`**。跳过是到达，失败是失败，两者不复用同一个出口。

### 1.4 分支怎么写

没有 `if` 原语，分支 = **对未选中的 Provide 调用 `Skip`**：

```go
if cond {
    if err := pulse.Set(rc, OutA, v); err != nil {
        return err
    }
    return pulse.Skip(rc, OutB)
}
if err := pulse.Set(rc, OutB, w); err != nil {
    return err
}
return pulse.Skip(rc, OutA)
```

**两边都要表态。** 只 `Skip` 未选中的那条不够：`Run` 成功返回后漏写的 Provides 会被自动跳过（§1.3 第二条级联规则），于是「没选 A」变成「A、B 都没到」——两条下游都不跑。

这比引入条件节点更小：跳过语义已经在槽位里了，分支只是它的一个用法。

## 2. 一次运行一个世界

> **`Graph` 是模板的一次实例，不是可重跑的容器。**

数据槽、取消、首错随 `Run` 而生、随结束而灭。**模板复用 ≠ 实例复用**——同一张图要跑第二次，正确做法是 `New` 第二次。类比 CI/CD：workflow 定义被 run 无数遍，每遍一个独立 run 实例。

这条是**对外契约**，不是现状描述：

- `Start()` 第二次调用返回 `ErrGraphStarted`；
- 槽位一旦到达即 `close(done)`，不会重开；重复 `Set` 幂等忽略，`Set` 与 `Skip` 冲突报 `ErrConflict`；
- 图启动后 `Seed` 同样被拒。

**为什么不做「多次运行」**：引擎的槽位是**流水线缓冲**，不是存储。判据一句话——

> pulse 持有「这一轮正在流动的数据」，不持有历史。

一旦为了跑第二轮而要保存上轮的值，引擎就得回答「什么该留、什么该清」——那是存储语义，是**调用方**的事。三类真实需求都不需要它：

| 需求 | 正确表达 |
|---|---|
| 同一张图跑 N 个独立请求 | 每次 `New` |
| 一次请求内跑 3 个候选 | 一张图内 fan-out（多 Provides） |
| 长会话多轮 | 每轮一张图，拓扑来自 YAML |

跨运行的状态（历史、缓存、会话）不属于这个世界。

## 3. 调度与失败

`Graph.Start()` 把**全部节点一次性提交**，每个节点一个 goroutine，阻塞在自己的输入槽位上。`Run()` = `Start()` + `Wait()`。

- **启动前静态校验两条**：① 每个 `Requires` 都必须有来源（外部 `Seed`/`SkipSeed`，或某个节点的 `Provides`）——没有来源的槽永远不会被写入；② 依赖关系**无环**——有来源不等于能满足，环里每条 `Requires` 都有生产者，但没有任何节点能先进入 `Run`（门要等全部输入到达），所有槽永远停在 `pending`。两条都描述**不可能跑完**的图，也都是启动那一刻就能判定的，所以 `Start()` 直接拒绝并指出节点与 Key，而不是留到运行时挂死（有 deadline 时是一句看不出病因的超时，没有时进程会被 runtime 判为 `fatal deadlock`）。环报成一条具体路径：`pulse: dependency cycle: A -> B -> A (A requires "y", B requires "x")`（Seed 的 Key 不构成边，只按节点的生产者建图）。校验不过时图仍未启动（`started` 保持 false）：补上来源可以重新 `Start`，含环的图则要**重新装一张**（引擎没有 `Remove`）；
- **首错即取消**：任一节点返回非跳过错误 → 记录首错 + `cancel()` 整图，所有等待者被唤醒；**取消优先于到达**——ctx 已取消时等待一律返回 `ctx.Err()`（「到达与取消同时就绪」也以取消为准），所以因首错而没跑的下游稳定报 `canceled`，不随调度在 `skipped` / `canceled` 之间抖。失败节点未写的 Provide 仍会补一条跳过，那只是解开阻塞，不是下游的终态；
- **`WithMaxRunning(n)`** 限制同时进入 `Run` 的节点数，**等数据不占名额**（否则限流会退化成死锁）；**排队等名额也能被取消打断**（排队节点以 `canceled` 收尾，不进入 `Run`；名额空出与取消同时就绪时以取消为准——拿到名额后再复查一次 ctx）；
- **四个终态的判据**（`NodeFinishReason`）：`completed` = Run 正常返回（返回后未写的 Provide 被自动跳过不算失败）；`skipped` = 一条输入值都没到而没进入 `Run`（全部 `Requires` 以跳过到达），或自己 `Skip` 了输出、把 `WaitAll` 的跳过返回了出去；`failed` = 节点真实错误，**含 panic 与 `Timeout` 切面的节点超时**；`canceled` = 这一轮被从外面拆了——首错取消、**父 ctx 被取消或截止时间到期**、排队等名额期间被取消。宿主按 reason 分流，所以「父 ctx 到期」不能报成 `failed`：那会把它显示成一个并不存在的节点缺陷；
- **panic 不穿透**：节点 panic 被转成节点错误，走同一套失败路径；
- **`Err()` 不含单纯的跳过**：全图都跳过是合法结果；`Wait()` 返回后图自己的 ctx 会被取消（它是 `New` 从父 ctx 派生的子 ctx，不释放就会一直挂在长生命周期的父 ctx 上），这次收尾的取消**不算运行结果**。

## 4. 切面

```go
type Aspect func(rc *RunCtx, next func(*RunCtx) error) error
```

切面包住节点的「**等输入 + 执行**」整段——这样 `Timeout` 才能打断还在等数据的节点，而不只是打断执行。

- `Timeout(d)`：超时取消本层 ctx，**并等内层返回**——切面与父层共享写入记录，提前返回会让收尾路径和还在跑的 `Run` 并发碰同一批槽（数据竞态），也让 `Run` 的「阻塞到全部终止」失真；内层不看 ctx 时只能等它自己结束（协作式，不是硬看门狗）。**已发布的槽不撤回**：超时是失败，不是回滚；
- `Retry(attempts, delay)`：对内层执行错误重试；**等待阶段的取消不重试，以跳过收尾的也不重试**（一条值都没到而没执行、或自己把 `WaitAll` 的跳过返回出去）——跳过是到达，不是失败。**重试安全的前提**：失败前没写过任何 Provide、也没有不可重入的副作用——槽位「到达即发布、幂等首写」，前一次 attempt 一旦写成功，下游已被唤醒，后续 attempt 的写被静默忽略且不回滚（回滚等于重开槽位，与 §2 一次性契约冲突）；
- 全局切面（`WithAspects`）先于节点切面，外层先跑。

**门闩约束**：单节点的 `Run` **不得并发进入**（两个 goroutine 同时跑同一节点会抢同一批槽位，语义上必然错），顺序重入**合法**——`Retry` 正依赖它（1→0→1）。所以判据是「重叠」而不是「多次」，违反返回 `ErrNextCalledTwice`。

`RunCtx.Fork()` 只派生可取消 context，**共享**声明权限与写入记录；它不是独立写入事务。

## 5. 装配

两条路，都产出 `*Graph`：

**命令式**（`Registry` 登记具名 Run 工厂，Go 侧装图）：

```go
reg := pulse.NewRegistry()
reg.MustRegister("summarize", summarizeRun)
pulse.MustRegisterKey(reg, Docs)
```

**声明式**（`pulse/yaml`，拓扑归 YAML）：

```yaml
version: 1
seeds:
  - key: {name: docs, type: "[]Doc"}
    from: {kind: file, path: docs.json}
nodes:
  - id: summarize
    uses: summarize          # → Registry 上的具名 Run 工厂
    requires: [{name: docs, type: "[]Doc"}]
    provides: [{name: summary, type: string}]
    timeout: 30s
    retry: {attempts: 3, delay: 100ms}
```

分工是硬的：**YAML 拥有拓扑，工厂只给 Run**。工厂不返回 `*Node`，因为它不该决定自己接在图的哪里。

`Load` 返回 `*Graph` + `SeedPlan`：`Seed` 里的 `from.kind` 除 `literal` 外需要宿主提供 `resolve` 回调——**引擎不做 IO**，读文件、读环境变量、从请求取 context 都是宿主的事。

`literal` 的取值由 YAML 解码器给出（泛型容器 `[]any` / `map[string]any`），`SeedByName` 会按登记类型做**形状对齐**：容器递归、标量同族（数值之间、命名类型、字符串之间）。**不做** map→struct 的字段猜测，也不做字符串↔数字互转、浮点截断成整数——这三类直接报错，由 `resolve` 给出类型正确的值。判据一句话：**形状可以机器对齐，语义只有调用方知道。**

时间字段用 Go 的 `ParseDuration` 形式（`30s` / `100ms`），不写裸数字。

## 6. 并发与读语义

- 节点各自一个 goroutine；`RunCtx` 的 context 是唯一取消通道；
- `Graph` 的 `Add` / `Seed` / `Start` 由内部互斥量保护，启动后拒绝变更；
- 槽位读写由各自的互斥量与 `done` channel 保护；
- 观测回调**在节点 goroutine 上同步执行**（见第二部分）——所以观察者必须并发安全且不得长时间阻塞。

---

# 第二部分 · 观测

## 7. 分层

```
pulse（引擎）          不认识观测
   ↑ 实现 Observer seam
pulse/observe          Record / Sink / 折叠适配
```

引擎只暴露一个 **seam**：

```go
type Observer interface {
    OnGraphStarted(graphID string)
    OnGraphFinished(graphID string, reason NodeFinishReason, err error)
    OnNodeWaiting(graphID, nodeID string)
    OnNodeRunning(graphID, nodeID string)
    OnNodeFinished(graphID, nodeID string, reason NodeFinishReason, err error)
}
```

图级不超过两条（`GraphStarted ≤ 1`、`GraphFinished ≤ 1`），节点不超过三次回调（`Waiting ≤ 1`、`Running ≤ 1`、`Finished = 1`）；`Retry` 的多次 attempt **不重复打点**。时序是**夹住**：`GraphStarted` 在提交任何节点 goroutine 之前发出（启动校验失败的图没有启动，不发），`GraphFinished` 在全部节点终止之后、`Wait` 返回之前发出——只 `Start` 不 `Wait` 的宿主拿不到它，重复 `Wait` 不重复发。图级终态只会是 `completed` / `failed` / `canceled`（跳过是节点级的事实，升不到这一层）。图默认 no-op，`WithObserver` 挂载，需要多个时用 `MultiObserver` 组合。

节点回调在**节点自己的 goroutine** 上执行，图级两条在 **`Start` / `Wait` 的调用方 goroutine** 上执行——两边都同步。

**观察者的 panic 与 error 不得升格为节点失败**——这是只读 seam：观测坏了不该让业务图挂掉。

## 8. Record：观测信封

```go
type Record struct {
    Time     time.Time
    HostID   string
    TraceID  string
    Source   Source
    Event    string
    Duration time.Duration
    Status   string
    Err      error
    Attrs    Attrs   // 开放段
}
```

设计约束：**业务维度一律经 `Attrs` 进入，不扩具名字段**。具名字段只服务于所有记录共有的事实。「同一出口 ≠ Record 变万能袋」——一旦为某个域加具名字段，Record 就会长成所有包观测字段的汇聚点。

`Attrs` 是**插入序小切片**（不是 map）：顺序 = 产生方的语义序，出口无需排序即得稳定输出；读取为线性扫描，条数少时快于哈希。

## 9. Sink：出口

```go
type Sink interface{ Write(r Record) }
```

契约三条：

1. **并发安全**，且不得长时间阻塞调用方（`Write` 在节点 goroutine 的回调路径上）；
2. **无 `context.Context`**——回调不带 ctx；需要截止时间的导出器自行持有内部队列，不把阻塞回传到回调路径；
3. **引用语义**：产出方构造独立 `Attrs`、`Write` 返回后不再修改；Sink 只读消费；异步导出器必须自行拷贝。

内置出口：

| 出口 | 形态 | 适用 |
|---|---|---|
| `LineSink` | **默认**。一行一条的人读文本，自带 32 KiB 缓冲、**零分配**、不经 slog | 终端、日志文件 |
| `SlogSink` | `log/slog`（Text / JSON） | 接宿主既有 logger、要 JSON 喂采集器 |
| `MemorySink` | 内存收集 | 测试断言 |
| `MultiSink` | 扇出 | 同时给多个出口 |
| `AsyncSink` | 异步包装：有界队列 + 单后台协程 | 手慢的出口（文件/网络） |

`AsyncSink` 的注意点：**异步不提高吞吐上限**——持续速率超过出口能力时，有界队列会被填满并把生产者回压到出口速率；这正是「不丢记录」的代价。要丢不堵就用 `DropOnFull()`。对已经很快的出口是负优化，别默认套。

## 10. 图适配：`NewRecordObserver`

`observe` 实现引擎的 seam，把**一次运行折成两条运行级记录**，把**每个节点折成两条分段计时记录**：

| 事件 | 何时 | 内容 |
|---|---|---|
| `pulse.graph_started` | 提交任何节点之前 | `Duration = 0`；`Status = "running"` |
| `pulse.graph_finished` | 全部节点终止之后（`Wait` 返回前） | `Duration` = 整轮耗时；`Status` = 运行终态 |
| `pulse.node_wait_finished` | 等待完成（进入执行，或以 skip/失败终结） | `Duration` = 等待段；进入执行时 `Status = "running"`，否则为 finish reason |
| `pulse.node_run_finished` | 执行完成 | `Duration` = 执行段；`Status` = finish reason |

运行级两条把本轮的节点记录**夹在中间**：宿主因此不必再靠 `pulse.graph` 这个 Attr 把节点记录自行拼回一轮——一轮在观测里是一个有头有尾的实体。运行级终态只会是 `completed` / `failed` / `canceled`：**一轮里全部节点都跳过，整轮仍是 `completed`**（跳过是节点级的事实，不升格为失败）。

跳过节点**只有一条 skipped 等待记录，无运行记录**——这与「跳过是到达」一致：它确实到达了，只是没执行。

归因维度走 Attrs：`pulse.graph`（图 ID）+ `pulse.node`（节点 ID）。**key 契约由引擎定义**（事实归属包），`observe` 只消费不定义。节点记录两个都带，运行级两条只带 `pulse.graph`。

**耗时的口径**：四段都是墙钟，且**执行段包含「等待段那条记录写出口」的耗时**——回调在节点 goroutine 上同步执行，`Running` 里先给 `runStart` 打点、再落等待段记录，于是慢出口把自己的写开销记进了执行段。实测同一张图、同一个空节点（`noop`）：`MemorySink` 四段都是 `0s`；出口换成每条 `Write` 睡 20ms 的假出口时，执行段 `20.57ms`（就是那次写出的代价）；**整轮那条 `62.24ms`**——运行级的窗口覆盖「提交节点 → 全部终止」，这一轮里三次写出（运行级 started、节点等待段、节点执行段）都在窗口内，所以它天然比宿主的掐表少一点（不含装图、`Seed` 与 `Flush`），又天然比节点段之和大一些。两条推论：**耗时列偏大先怀疑出口**（慢出口套 `AsyncSink`）；段耗时为 `0`（内置版式渲染 `-`）表示该段耗时被计时精度取整为 0，而不是「没有这一段」。

```go
obs, err := observe.NewRecordObserver(observe.ObserveConfig{
    Sink: sink, HostID: "host-1", TraceID: observe.NewTraceID(),
})
g, _ := pulse.New(ctx, "demo", pulse.WithObserver(obs))
```

单实例可复用于多图并发：内部按 `(graphID, nodeID)` 记账，同名节点跨图不串扰。

## 11. 宿主自带出口

列式版式是**默认**的，不是唯一的。宿主想让域事实进列时用 `WithRenderer` 换掉行体渲染器，用导出的编码原语拼自己的列——**出口仍然不认识任何业务语义**，域语义留在宿主手里。

六条编码原语（`AppendDuration` / `AppendTextValue` / `AppendAttrs` / `AppendAttrsExcept` / `AppendPadding` / `DisplayWidth`）与内置版式**同形**：同一条记录在两种版式下，耗时、属性组、列补齐逐字节一致。一致性由基座保证，宿主只决定「我这个域有哪些列」。

渲染器的契约只有四条：只产行体（行首标识与结尾换行由 sink 加）；拿到的 `color` 是 sink 解析好的结论；不缓冲、不写 writer；**在 sink 的内部锁内被调用**——别在渲染器里回调 sink 的 `Write`/`Flush`/`Err`（`sync.Mutex` 不可重入，会安静挂住）。

## 12. TraceID

由宿主**单一生成源**注入：宿主每次运行调用一次 `NewTraceID()` 即构成单一生成源，也可以完全自带方案（如 hostID 前缀 + 自增序号）。返回值无契约语义，消费方不要解析其结构。同一次运行的全部记录共享同一个 TraceID——这是运行级关联的全部机制。

接 W3C trace context / OTel 的宿主**注入自己的 trace id**（OTel 的 `span.SpanContext().TraceID().String()`，或按规范自行生成 32 位小写 hex），不要指望 `NewTraceID()` 的形状——它是「时间戳-随机段-序号」，不是 W3C 格式。两条边界：① 只有 trace-id 发不出一条合法 `traceparent`，`parent-id` 与 `trace-flags` 是 span 语义（本层只有「一次运行」，没有 span），归宿主；② 入站续接 = 把收到的 trace-id 填进 `ObserveConfig.TraceID`，非法 `traceparent` 整条忽略由宿主负责。

## 13. 隐私边界

`Record` 没有 `map[string]any` 逃生舱：`Attrs` 的写入面只有泛型 `Set`（标量约束 `~string|~int64|~float64|~bool`），prompt、附件字节、密钥、思维链**在类型上就无法进入**。

边界的残余部分要说清：`Err` 是调用方传入的 `error`——所以**适配层不得把上游原始错误体直接塞进 `Err`**，应传已分类的摘要。

---

# 附 · 冻结面与不提供的东西

**冻结契约**（破坏要 minor 版本 + release notes）：

- 槽位三态与「跳过是到达」语义；
- `Aspect` 的 `func(rc, next)` 形态与门闩约束（重叠拒、顺序允）；
- 哨兵错误的判据：`ErrUndeclared` / `ErrConflict` / `ErrGraphStarted` /
  `ErrGraphNotStarted` / `ErrDuplicateSource` / `ErrSkipped` / `ErrNextCalledTwice`
  （同一份清单也在 `AGENTS.md` 的 Freeze contract 一节）；
- `Observer` 的回调次数契约（图级两条 + 节点三条）与「panic 不升格」；
- 六条编码原语与 `LineRenderer` 的字节级同形承诺。

**刻意不提供**：

| 不提供 | 理由 |
|---|---|
| 多次运行 / 重置 | 见 §2——那是存储语义，归调用方 |
| 熔断 / 错误吞没切面 | 降级是节点显式写出的数据，不是切面偷偷吞掉的错误 |
| 图内 IO（读文件 / 读 env / 网络） | 引擎不做 IO；`SeedPlan` 把取值交给宿主 |
| 具名观测字段的扩展 | 见 §8——业务维度走 `Attrs` |
| `map[string]any` 请求逃生舱 | 类型系统是隐私边界的第一道防线 |
| 节点自动命名 | 节点 ID 是观测归因键，自动名会在重构后漂移 |
