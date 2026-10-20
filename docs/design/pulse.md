# pulse 设计：图编排 + 图观测

> 状态：Accepted（**对象模型按 [#311](https://github.com/Luo-root/pulse/issues/311) 迁移中：阶段 1 静态组合、阶段 2 `Execution` 拆分、阶段 3 事务性 `Retry` 已落地**）
> 包位置：根包 `pulse`（图引擎）+ `pulse/observe`（图观测）+ `pulse/yaml`（声明式装图）
> 本篇写**当前实现**的编排与观测：类型、时序、冻结面。理念（为什么这样取舍）见
> [`philosophy.md`](philosophy.md)；**目标对象模型**（`Graph` 定义 / `Execution` 运行，含现状差距与
> 迁移策略）见 [`object-model.md`](object-model.md)——两者冲突时以 object-model 为准，**已经落地的阶段
> 本文已经跟着改过**；阶段 4 的完整静态端口、无值执行快照、观测身份、Mermaid 与 YAML Analyze 已实现（#319）；本文也描述声明出口读取与可靠检查点桥接。发版由维护者决定。

## 0. 定位

pulse 是一个**图引擎**（`Graph` 是可复用的静态定义，一次运行一份 `Execution`），外加一层**图观测**：

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
pulse.NewNode("summarize", "把 docs 折成一句摘要",
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

## 2. 定义可复用，一次运行一个世界

> **`Graph` 是可复用的静态定义，`Execution` 是一次运行。**

定义侧只有结构：id / 描述 / Key / 来源表 / 端口 / 节点 / 声明式默认配置（观察者 / 名额 / 图级切面），**没有生命周期**——所以 `New` 不收 ctx。数据槽、取消域、首错、每节点进度随 `Execution` 而生、随那一轮结束而灭：`g.Run(ctx, in)` 内部就是 `NewExecution` + `Start` + `Wait`，**每次调用一份新的执行**。要分步控制就自己建（`g.NewExecution(ctx, in)` 之后 `e.Start()` / `e.Wait()`）。

这条是**对外契约**：

- 同一份**执行** `Start()` 第二次返回 `ErrGraphStarted`；槽位一旦到达即关闭，不会重开；重复 `Set` 幂等忽略，`Set` 与 `Skip` 冲突报 `ErrConflict`；
- 同一张**定义**想跑几遍就建几份执行——顺序跑、并发跑、挂在不同父图里当子图都行；一轮的取消 / 失败 / 完成不污染定义，也不污染别的执行；
- **值属于这一次运行，声明属于定义**：值走 `Inputs`（`pulse.Seed(&in, k, v)` / `SkipSeed`），声明走 `DeclareInput`。两件事分成两个入口之后，`Validate` 才分得清「这本来就是外部输入」与「你忘了接线」；
- **启动条件判两次**：建执行那一刻判一次（早反馈），`Start` 在**封口之前**按同一份判据、同一句报文再判一次——中间定义还长得进去（`Add` 一条要新进口的节点就行），而 `Start` 紧接着封上的正是它判过的那张——「判过的 = 封上的 = 真跑的」必须是同一个版本。判据本体：每一条被节点 `Requires` 的进口，这一次都要有个交代（`Seed` 给了值 / `SkipSeed` 标了跳过 / 父图那一步 `In` 绑定了）。漏喂、喂了没声明过的键、喂了图内自己产出的键，都点名报出来——不留到运行期变成挂死。

**为什么不做「同一份执行重跑」**：引擎的槽位是**流水线缓冲**，不是存储。判据一句话——

> pulse 持有「这一轮正在流动的数据」，不持有历史。

一旦为了跑第二轮而要保存上轮的值，引擎就得回答「什么该留、什么该清」——那是存储语义，是**调用方**的事。三类真实需求都不需要它：

| 需求 | 正确表达 |
|---|---|
| 同一张图跑 N 个独立请求 | 每份 `Execution` 跑一次（`Run` 每次调用就是一份） |
| 一次请求内跑 3 个候选 | 一张图内 fan-out（多 Provides） |
| 长会话多轮 | 每轮一份执行，拓扑来自 YAML |

跨运行的状态（历史、缓存、会话）不属于这个世界。

## 3. 调度与失败

`(*Execution).Start()` 把**全部节点一次性提交**，每个节点一个 goroutine，阻塞在自己的输入槽位上。`g.Run(ctx, in)` = `NewExecution` + `Start` + `Wait`。

- **启动前静态校验三类**（判据本体在 `problems.go`，顺序固定：来源 → 出口 → 依赖环 → 流式 → 每处挂载的静态子树）：① 每个 `Requires` 都必须有来源（这一次运行喂进来的进口，或某个节点的 `Provides`，或声明过的入口）——没有来源的槽永远不会被写入；② **声明过的出口必须有人产出**（`DeclareOutput` 的一条键，既没节点 `Provides`、也不是同名入口透传）——它是这张图对外的承诺，没人产出时那句 `never arrived` 要等子图跑完才出现，而读一遍声明就能定；③ 依赖关系**无环**——有来源不等于能满足，环里每条 `Requires` 都有生产者，但没有任何节点能先进入 `Run`（门要等全部输入到达），所有槽永远停在 `pending`。三类都描述**不可能跑完**的图，也都是启动那一刻就能判定的，所以 `Start()` 直接拒绝并指出节点与 Key，而不是留到运行时挂死（有 deadline 时是一句看不出病因的超时，没有时进程会被 runtime 判为 `fatal deadlock`）。环报成一条具体路径：`pulse: dependency cycle: A -> B -> A (A requires "y", B requires "x")`（只按节点的生产者建图，进口不构成边——它们不来自某个节点）。校验不过时这次运行**仍未启动**（`started` 保持 false）：补上生产者可以重新 `Start`，含环的图则要**重新装一张**（引擎没有 `Remove`）；另外两类（进口这一次有没有交代）在**建执行那一刻**就判过了，见 §2；
- **同一套判据也有只读入口 `(*Graph).Validate()`**（0.5 新增）：跑的就是上面那几条（外加流式名额那条），它只检查定义，**没有某次 `Execution.started` 的判据，也不建 goroutine**；通过后仍可给独立 `Inputs` 种值，并为这张定义创建、启动新的执行。**通过即封口，而且封的是整棵子树**：那次之后 `Add` / `DeclareInput` / `DeclareOutput` / 挂子图在**这一层与它挂着的每一层子图**上都被拒（`ErrGraphStarted`——这是它的第二个触发点：不是「这一次运行已经启动」，而是「这张**定义**不再变化」）。为什么连子图一起封：挂上去的子图是这张定义的一部分，只封自己那句话就是半句空——子图还能接着 `Add`，刚校验过的那棵树会变。反过来不成立：子图自己 `Validate()` 通过只封它自己与它的子树；**已经封口的子图照样能被别处挂载**（挂载不改它的定义）。校验**失败一层都不封**（所以实现上是两趟：先判完整棵树，干净了再逐层封，`sealTreeLocked`），那只说明图还没装配完，补上缺的那块再校验正是要支持的用法。给**装配期**用：声明式装配（`pulse/yaml`）与静态组合（`Sub`）都拿它在装图那一刻把子图的静态毛病报出来（详见 §5 装配），而且沿静态挂载边**递归**——同一张子图挂两处时按引用点各报一条、报文点明挂载位置。**递归读子图时每往下一层都由那一层自己取锁**（`child.problemsWithStreamsAt` / `child.topologyAt`）：父图那把锁管不到子图，而子图在装配期一直躺在宿主手里（那头一边 `Add`、这头一边读就是一处竞态）；锁序恒为父 → 子，挂载边不成环所以不会互等，递归另带一条挂载链做兜底判环（见 §5 与 `mountChain`）。`problemsAtLocked` 先计算整棵树的流别名、额度池与生命周期诊断，再由 `problemsWithStreamsAt` 递归共享同一份 `streams`；不会在每层重建局部流模型。共同静态问题沿用相同判据与报文；定义检查仅对声明的 managed 根出口省略缺读者诊断，Start 另按本次真实订阅严格核验。已封口且不含 Sub 或流的普通定义可复用既有结构检查；含 Sub 或流时仍按有效静态约束检查，不因某份执行已经启动而直接通过；
- **首错即取消**：任一节点返回非跳过错误 → 记录首错 + `cancel()` 整图，所有等待者被唤醒；**取消优先于尚未完成的普通槽等待**——ctx 已取消时等待返回 `ctx.Err()`（「到达与取消同时就绪」也以取消为准），所以因首错而没跑的下游稳定报 `canceled`，不随调度在 `skipped` / `canceled` 之间抖。失败节点未写的 Provide 仍会补一条跳过，那只是解开阻塞，不是下游的终态；
- **`WithMaxRunning(n)`** 限制**一棵图树里**同时进入 `Run` 的节点数（`n<=0` 无限）：名额**整棵树共享一份**——子图没自己声明时继承父图那份，挂子图那一步（`Sub`）**不吃名额**（它整段都在等子图，占着等于把额度锁在「等」上；共享之后那就是死锁）；子图自己声明 `WithMaxRunning(m)` 就是显式覆盖，那一层及其后代换成它自己那份（`m<=0` = 这一子树不限）。**等数据不占名额**（否则限流会退化成死锁）；**排队等名额也能被取消打断**（尚未进入业务的排队节点以 `canceled` 收尾；首次取额后复查 ctx。已在受管 I/O 等待中让出的额度必须在业务继续、取消返回和 panic 展开前取回，完成的交付保留成功；纯受管 pipeline 额度 1 可推进，任意业务 I/O 不自动让额）；
- **四个终态的判据**（`NodeFinishReason`）：`completed` = Run 正常返回 nil（逐键 `Skip` 或返回后未写的 Provide 被自动跳过都不改变这一终态）；`skipped` = 一条输入值都没到而没进入 `Run`（全部 `Requires` 以跳过到达），或返回正常的 `NoValue()` / `*SkipError`（含 `WaitAll` 的跳过）；`failed` = 节点真实错误，**含 panic、`Timeout` 切面的节点超时，以及即使包装 `ErrSkipped` 也不能降级的可靠检查点故障**；`canceled` = 这一轮被从外面拆了——首错取消、**父 ctx 被取消或截止时间到期**、排队等名额期间被取消。宿主按 reason 分流，所以「父 ctx 到期」不能报成 `failed`：那会把它显示成一个并不存在的节点缺陷；
- **节点链的 panic 转为失败**：业务体以及引擎错误分类时自定义 Is/As/Unwrap 的 panic 转为节点错误，走同一套失败路径；分类方法 panic 后不再遍历该故障 error。这一保护不覆盖用户自建 goroutine；
- **`Err()` 不含单纯的跳过**：全图都跳过是合法结果；`Wait()` 返回后这一轮的 ctx 会被取消（它是 `NewExecution` 从宿主给的 ctx 派生的子 ctx，不释放就会一直挂在长生命周期的父 ctx 上），这次收尾的取消**不算运行结果**；
- **协作退出与终态取消分别判断**：还在等数据 / 等名额的节点会响应 ctx；已经进入 `Run` 的业务体需要自行检查 ctx 并返回，引擎不强杀它，`Wait()` 仍等全部节点退出。随后 `Wait()` 会合登记的可靠流工作，在标记结束前优先锁存此时已知的树级 CheckpointError 并归为 failed；否则保持普通首错，没有首错时锁存本次执行的真实取消。因此没有可靠故障时，父 ctx 在终态确定前被取消，即使业务体没有读取 ctx、最终返回 nil，节点可以是 `completed`，整轮仍是 `canceled`（`Run` / `Wait` 返回 context 错误）。终态确定后用于释放资源的内部 cancel 不算运行结果，重复或并发 Wait 复用已锁存结果（#327）。

## 4. 切面

```go
type Aspect func(rc *RunCtx, next func(*RunCtx) error) error
```

切面包住节点的「**等输入 + 执行**」整段——这样 `Timeout` 才能打断还在等数据的节点，而不只是打断执行。

- `Timeout(d)`：超时取消本层 ctx，**并等内层返回**——切面与父层共享写入目标与推进账，提前返回会让收尾路径和还在跑的 `Run` 并发碰同一批槽（数据竞态），也让 `Run` 的「阻塞到全部终止」失真；内层不看 ctx 时只能等它自己结束（协作式，不是硬看门狗）。**已发布的槽不撤回**：超时是失败，不是回滚（`Timeout` 在 `Retry` **里面**时另说：那只是这一次尝试失败，整条尝试写集作废，见下节）；
- `Retry(attempts, delay)`：对内层普通业务错误重试；**本层 ctx 已取消 / 到期、正常跳过或错误链包含 `CheckpointError` 时不继续重试**。跳过是到达，不是失败（一条值都没到而没执行、或自己把 `WaitAll` 的跳过返回出去）；可靠操作则可能已经完成却没有返回确认，即使本图未启用检查点也立即停止，不能当作普通业务错误重复执行。**每一次尝试各开一条独立的写暂存**（attempt 写集，见下）：那一次尝试里写出去的东西先落在暂存上，**失败且还要重试就整条丢掉**，下游从来没有见过——「先写一半再失败」不再需要调用方自己躲。总上限 `attempts` 包含首次调用，`attempts <= 0` 按一次处理；
- 全局切面（`WithAspects`）先于节点切面，外层先跑。**图级切面会继承给子图**：挂子图那一步跑子图时把这批**前置**到子图的切面链上，子图自己声明的那批排在其后（**叠加**，不是替换）；`WithoutAspects()` 声明这一层不继承（自己声明的照挂）。继承读的是父图的**有效**切面，所以它是传递的，且**不随嵌套深度重复叠加**；
- **图级切面慎用，尽量用节点级切面。** 它一次落在这一层**所有**节点上，包括挂子图那一步（那一步背后是一整张子图），还会继续往子图里的每个节点上叠——量级不同的东西被同一个参数管住，很容易跑出不在预期的行为。`Timeout` 最容易被咬到：一个图级 `Timeout(d)` 同时是「父图普通节点各自的时限」「挂子图那一步（= 整张子图）的时限」「子图里每个节点的时限」，而**子图天生比普通节点更容易超时**。要限时就写在该限时的那一个节点上（`NewNode(..., pulse.Timeout(...))`，整张子图限时用 `Sub` 的 `aspects`）；图级留给「全树都要一致施加」的那类（追踪 / 指标）。**切断只解开「继承到子图节点上」这一段**：父图那一步仍然被父图的图级切面包着——那是父图对自己节点做的事。另两条容易算错的账：**子图的「总时间」先被挂子图那一步上的 `d` 掐死**，不会变成「每个节点各 `d`、加起来 `N*d`」（继承来的 `d` 只是更外层那个 `d` 里面的一段）；名额紧时**超时可能发生在子图拿到名额之前**——那一步不吃名额，可它的 `d` 从进入 `Run` 就开始计时，而子图节点还在排队等额度。

**推进闸（门闩的准确形状）**：单节点的 `Run` **不得并发进入**（两个 goroutine 同时跑同一节点会抢同一批槽位，语义上必然错）；**顺序重入也只有在「开了新的尝试边界」时才被放行**——判据是「这一次推进有没有新边界」，不是「第几次」：同一个节点执行上下文里，第一次推进放行；第二次推进时写暂存**换了**（`Retry` 每次尝试都开一条新的）才放行；没换就当场 `ErrNextCalledTwice`，把**第一次失败的真正原因**一并带出来（用户自己写的重试切面正是这个形状——它的第二次推进没有隔离，第一次已经写出去的东西会被幂等首写静默吞掉，节点手里就成了两次推进的混装）。它限的是**这一次执行里这个节点**的推进，不是按节点 id 全局禁跑：另一次 `Execution`（另一次 `Run`）是全新的上下文，账从零开始。

`RunCtx.Fork()` 只派生可取消 context，**共享**声明权限、写入目标与推进账；它不是独立写入事务——想要一条独立的写边界，那是 `Retry` 的事（它换的是写入目标那条链）。

### `Retry` 的尝试边界（attempt 写集）

`Retry` 的每一次尝试都开一条**独立的写暂存**：那一次尝试里 `Set` / `Skip` 的东西先落在暂存上，**不碰执行槽位**。于是「失败尝试写出去的东西」在引擎管的这一层里有了一个干净的边界：

- **失败且还要重试 → 整条暂存丢掉**：失败那一次的输出版本下游从来没有见过——槽位连动都没动过，等它的下游也就从来没被唤醒。旧的写法要求调用方自己守「先算完再 `Set`」这条前提，从阶段 3 起由引擎接管；
- **成功（或以跳过收尾）→ 整条交出去**：外层还有一层暂存就交给外层（嵌套 `Retry` 的里层属于外层那一次尝试），否则逐条写进执行槽位。交出去之前**先整批判过**：判据是「对面有没有已经以另一个状态到达」（值写进已跳过的槽、跳过写进已就绪的槽都是 `ErrConflict`，同状态是幂等首写），判不过就**一条都不发布**——不留「一半发布了、一半没发」那个没有可解释语义的中间态。这条判据依赖一条**不变量：一条输出只有一个顺序写入者**（能写某个执行槽位的只有三类路径，都在同一条栈上顺序执行：建执行时种输入、这个节点自己的尝试链——体 / 切面 / 子图桥回、它的收尾路径；别的节点写不了它，因为一条键只有一个来源）。判过之后的逐条写**不承诺严格原子可见**：窗口里可能「一部分输出已就绪、其余仍 `pending`」，等这些输出的下游可能被先到的那一条先唤醒；窗口里**看不到任何旧尝试的值**（失败那批整批都丢了）；
- **收尾按「落没落地」判**：`Provides` 的键没有落地才被隐式跳过。判「这个节点写过谁」是不够的——失败的尝试写过它、而整条暂存被丢掉了，那条输出仍然没有落地，该以跳过收尾，否则等它的下游会一直等到 ctx 取消；
- **读自己写的**：尝试**内部** `Get` 自己刚 `Set` 过的 Provide 读到的是**本暂存**的值（自洽）。反过来有一条边界要说清：**跨尝试读不回被丢弃的写**——那条键在这一次尝试里从来没有过值，读它就是等一个没人会写的槽（与「一条键没人产出」同一个形状，直到本层 ctx 取消）。作废的写重新变得可读，正是这条边界要消灭的东西；
- **事务边界就是 `Retry` 套的那一层**：`Retry` **外面**（更外层切面）的写不属于任何尝试、也不参与作废——`Retry(Timeout(f))` 里 `Timeout` 只是**这一次尝试**的失败，照常重试；反过来 `Timeout(Retry(f))` 里那层 ctx 已到期，`Retry` 拿到的就是它，不重试。**外层切面写的东西不作废**这条要单独点名，不能把外层的写默默说成事务内的；
- **只作废引擎管的槽位**：用户 `Run` 里已经发生的外部副作用（数据库提交、HTTP 请求、邮件、写文件）撤不回来——把副作用挪到最后一次成功的尝试，或用幂等键 / outbox / 去重 / 补偿。「内存里暂存过」不等于外部世界能回滚，这条与「引擎不做隐含补偿」是同一条边界（见 [`philosophy.md`](philosophy.md)）。

**它也可以装在「挂载子图」那一步上**：每一次尝试各建一份**新的子执行**，第一次尝试桥回父侧的结果随它那条暂存一起作废——父侧只拿得到成功那一次的输出版本（`pulse/yaml` 里 `graph:` 节点上的 `retry:` 同理，`Load` 期照装）。

**流 I/O 有独立边界**：受管 OpenStream/Send/Receive 不能借普通 Retry 的写集撤回或倒带；Sub 的实际外接流输入/输出也在 I/O 前拒绝，确定 skip 无数据仍正常。原生 Produce/Tee 还存在延迟发布 channel 与消费者互等的问题，当场报错。消息级有限重试用 HandleMessage；逐条渐进发布按显式来源游标续读，见 §5 流式。

## 5. 装配

两条路，都产出 `*Graph`：

**命令式**（`Registry` 登记具名 Run 工厂，Go 侧装图）：

```go
reg := pulse.NewRegistry()
reg.MustRegister("summarize", summarizeRun)
pulse.MustRegisterKey(reg, Docs)
```

**声明式**（`pulse/yaml`，拓扑归 YAML）：下面假定 `Doc` 定义在 `main` 包，因此 `Docs` 的登记类型标签为 `[]main.Doc`。实际项目以 `reg.TypeTagOf("docs")` 返回值为准；命名类型的包名不能省略。

```yaml
version: 1
seeds:
  - key: {name: docs, type: "[]main.Doc"}
    from: {kind: file, path: docs.json}
nodes:
  - id: summarize
    uses: summarize          # → Registry 上的具名 Run 工厂
    requires: [{name: docs, type: "[]main.Doc"}]
    provides: [{name: summary, type: string}]
    timeout: 30s
    retry: {attempts: 3, delay: 100ms}
```

分工是硬的：**YAML 拥有拓扑，工厂只给 Run**。工厂不返回 `*Node`，因为它不该决定自己接在图的哪里。

`Load` 返回 `*Graph` + `SeedPlan`：`Seed` 里的 `from.kind` 除 `literal` 外需要宿主提供 `resolve` 回调——**引擎不做 IO**，读文件、读环境变量、从请求取 context 都是宿主的事。

`literal` 的取值由 YAML 解码器给出（泛型容器 `[]any` / `map[string]any`），`SeedByName` 会按登记类型做**形状对齐**：容器递归、标量同族（数值之间、命名类型、字符串之间）。**不做** map→struct 的字段猜测，也不做字符串↔数字互转、浮点截断成整数——这三类直接报错，由 `resolve` 给出类型正确的值。判据一句话：**形状可以机器对齐，语义只有调用方知道。**

时间字段用 Go 的 `ParseDuration` 形式（`30s` / `100ms`），不写裸数字。

### 不用注册表的静态分析：`Analyze`

```go
top, err := yaml.Analyze(data, yaml.LoadOptions{GraphID: "demo", MaxRunning: 4})
if err != nil {
    return err
}
text := top.Mermaid()
_ = text
```

Analyze 共用 Load 的解码、assembler、核心静态校验，返回同一种 `*pulse.Topology`。它不接收 Registry，但仍要求 GraphID，静态选项与 Load 相同。根层诊断保留在 Topology.Problems；子图与装配错误返回 error。不创建 Execution、不执行工厂或切面、不解析外部 seed、不调用 ObserverFor。

Type 是 YAML 标签，可沿 in/out 传播；显式 KeySpec 必须有 type，同名键及绑定两端的已知标签冲突会报错。纯接线中没有已知标签可推导的键，Type 为空串。任意标签原样保留，不能由此证明真实 Go 类型、宿主登记或 literal 可赋值性；这些仍由 Load / SeedPlan.Inputs 检查。不从 literal 猜类型，也不能推测普通工厂内部的 Go 流糖行为。标签预检查早于装配，多处同时出错时不承诺两个入口的首条诊断完全相同。

`SeedPlan.Inputs(resolve)` 每次提供一份独立运行输入，同一图可接不同输入创建多份执行；它复制输入表，不深拷贝宿主值里的指针 / map。

### YAML 里的子图：`graphs` / `graph`

声明式装配里「一步 = 一张图」不用写 Go 工厂：`graphs:` 声明可复用的子图，`nodes[].graph` 指向它。

```yaml
version: 1
graphs:
  enrich:
    nodes:
      - id: work
        uses: demo.enrich
        requires: [{name: sg.topic, type: string}]
        provides: [{name: sg.summary, type: string}]
nodes:
  - id: step1
    graph: enrich
    in:  {sg.topic: sg.input}      # 子键: 父键（父读 → 子 Seed）
    out: {sg.summary: sg.result}   # 子键: 父键（子产出 → 父 Set）
```

它展开成 **`pulse.Sub`**（静态组合，见下面「图即节点」）：子图在 `Load` 期按**挂载位置**装好（一处挂载一张实例），父侧那个节点的 `Requires` / `Provides` 由 `in` / `out` 推出来，引擎的静态校验照常生效。**YAML 只是把引擎已有的能力写出来**——引擎没有的能力它变不出来；这一层新增的是**装图期的对账**：静态可判定的问题全部在 `Load` 报，不留到运行期。装完之后父图**不运行**就能递归看完整棵树（`Topology().Nodes[i].Sub`，路径按挂载位置推导）。

| 检查 | 判据 |
|---|---|
| 引用 | `graph:` 指向的名字必须在同一份文档的 `graphs` 里；子图必须有节点 |
| 环 | 子图之间「图引用图」的环（含自引用）→ 报出一条**具体**路径 `a -> b -> a`；递归本身允许、深度不设限 |
| 类型 | 两端各按 `{name, type}` 查表，且用**同一个** type 记号——父侧那条键登记成别的类型就报 |
| 名字 | `in` / `out` 的子侧键必须真被那张图里的某个节点读 / 写，否则报 |
| 形状 | 工厂节点不能写 `in` / `out`、子图节点不能写 `requires` / `provides`——边界只有一处说法；写错那一侧就是**静默不生效**，所以在装图期吵 |
| 来源 | `in:` 喂的那条子侧键**不能又被子图自己的节点提供**——一条键恰好一个来源：声明那条进口的那一刻（`DeclareInput`）就撞 `ErrDuplicateSource`，不必等它跑 |
| seed | 子图的 `seeds` 里，**要种值**的那条只允许 `literal`（`env` / `file` / `context` 要靠宿主 IO，而 `SeedPlan` 是**父图**的产物；`skip: true` 的那条 `from` 没有语义，与顶层 `doc.Seeds` 一个口径）；键名 / 类型按登记表对账；**同一条键在同一张图里声明两次**也报（第二次会被幂等首写静默忽略，谁赢取决于声明顺序） |
| 跑得起来 | 每张装好的图再过一遍引擎的只读校验 `(*Graph).Validate()`——子图里「某条 `Requires` 没人提供、也没人 seed」「声明过的出口没人产出」与「依赖成环」也在 `Load` 报，**报文与 `Start` 那次逐字相同**（一份判据，两个入口） |

这些检查对**被引用的每一张 `graphs:` 声明**生效，并且按**引用点**各来一遍：同一张 spec 挂两处时两边的 `in:` 可以不一样，而「子图里这条键有没有来源」正是按引用点成立的——有一处没喂，那张图跑到就一定会挂，按并集校验等于说「另一处喂了 = 这一处也喂了」。

**谁也引用不到的 spec 是例外**：它没有挂载点、也就没有接口面——「这条键没人喂」在那里判不了（按闭合图去查会把它一律报成「名字写错了」，于是模板库里只要有一张暂时还没接上，整份文档就 `Load` 不过）。这类 spec 仍查工厂名 / 空 `nodes` / 重复 id / 已知来源冲突 / 依赖成环 / 流式关系。literal 或 skip seed 都是已明确的外部来源，会声明进临时校验图；同键还有节点 Provides 就包装 ErrDuplicateSource 拒绝。只有解析不到来源的 Requires 才按未来入口补声明（`declareSpecInputs`，直接用引擎的 `Topology().Requires[].From`，不在 yaml 里重写来源表）。真正引用时，接口面由那一处的 `in:` / `out:` 定，缺来源按引用点判断。

做法是 `Load` 期先装一棵**只装不跑**的校验树：**同一段装配代码**（子图一律按静态形态挂上去，`Sub` / `Add` 在装配期做的检查一个不少），每个引用点各是一张实例。每张实例按**那一处**的 `in:` / `out:` 把它自己的接口面声明成端口（`DeclareInput` / `DeclareOutput`）——于是「这本来就是外部输入」与「你忘了接线」分得开，也不必再往校验图上种「假来源」；父侧能接的也只有端口（接到子图内部的中间 Key 是静态错）。最后让引擎自己判一遍（`Validate`，判据本体是 `problems.go` 那一份）：所以**报文与 `Start` 那次逐字相同**，而且点明**挂载路径**（`mounted at "step1/toInner"`）。

**Load 构建的校验树和正式树都是静态定义，不接收宿主 context**。校验全部通过后才把正式 Graph 与 SeedPlan 交出去，失败返回 `nil, nil, err`。宿主在 `Run(ctx, in)` 或 `NewExecution(ctx, in)` 时建立本次取消域；每个子执行从挂载节点的 context 派生，定义本身没有要取消的东西。Wait 等业务链自行退出，再锁存终态并释放本次执行资源。

**另一条边界**：Load 不对根层执行完整的 `Validate`；根层声明的入口来自 `seeds:`，实际值由宿主在运行前通过 `SeedPlan.Inputs(resolve)` 提供。根层的完整来源 / 出口 / 依赖环 / 流式检查仍在 `Validate` 或 `Start` 时做；这一次需要的入口是否喂齐，则由 `NewExecution` 与 `Start` 核验。子图按各引用点的 `in:` / `out:` / 固定 seed 在 Load 期校验。

子图节点**不能**再写 `requires` / `provides`（反过来，工厂节点写了 `in` / `out` 也在装图期报）：边界只有一处说法。切面落在**父侧那个节点**上——`timeout: 30s` 就是给整张子图限时，`retry: {attempts: 3, delay: 100ms}` 就是整张子图最多跑三次：**每一次尝试各建一份新的子执行**，第一次那轮的桥回结果随它自己那条 attempt 写集一起作废，父侧只拿得到**成功那一次**的输出版本（「一半第一次、一半第二次」的混装不可能出现；写暂存的语义见 §4）。放进子图**里面**的 `retry:` 照旧只重跑那一个节点。**透传**（同一条子侧键同时挂在 `in:` 与 `out:`，内部没人产出它）与引擎那条「同名入口 + 出口是透传」同规则：两条「内部得有人读 / 有人产出」对它不适用，但内部有人产出它仍要拒（同一个槽两个来源）。

**观测默认继承，可按挂载点另选出口**：`LoadOptions.ObserverFor(path)` 用于不同子图挂载点需要不同观察者或记录目的地的场景。每处子图挂载在 `Load` 装配时调用一次；校验树**不调**它，返回 nil 则沿用父图观察者。每条回调已携带 `Site.Path`，同一个出口能继承多层并保留层级，通常不填 `ObserveConfig.Path`。同一张 spec 被引用两次会装成两个定义实例，每次运行再各建独立子 Execution；挂载位置靠 path 区分，运行靠 execution id 区分。

### 语法糖：`FanOut` / `Join`

手写装图要三次对齐同一个名字（`NewNode` 声明、`Get` 读、`Set` 写），三处都可能漂移。糖把名字收进**函数签名**：

```go
// fan-in：N 条同类型输入 → 一束
err := pulse.Join(g, "collect", "汇总多路结果", pulse.Keys(a, b, c), out,
    func(rc *pulse.RunCtx, m pulse.Batch[string]) (Report, error) {
        headline, err := m.Get(a) // 单路严格：缺 a 就让本节点跳过
        if err != nil {
            return Report{}, err
        }
        return report(headline, m.Values()), nil // Values()：到几个收几个
    })

// fan-out：一个输入 → N 个并行实例，各自一条输出槽（节点名 id-1 … id-N）
err = pulse.FanOut(g, "worker", "处理文档分片", docs, pulse.Keys(r1, r2, r3),
    func(rc *pulse.RunCtx, shard int, doc string) (Result, error) {
        if nothingFor(shard) {
            return Result{}, pulse.NoValue() // 这一份没有产出：整个实例跳过，不是失败
        }
        return work(shard, doc)
    })
```

`Join` 和 `FanOut` 的 `description` 位于 `id` 之后，原样作为节点说明，空串合法。`FanOut` 的所有实例共用这份说明，仍由 `id-1` 至 `id-N` 区分。

`Join` 在装配时复制 `ins` 接线列表；后续替换或重排原切片，不会改变图的输入声明或回调中的批次顺序。这里复制的是 Key 接线，不深拷贝运行时的业务值。

名字叫 `FanOut` 而不是 `Spread`，是因为它**不切分数据**：N 个实例看到的是同一条输入，「各做一份」靠 `shard` 自己挑。

两个回调的第一个参数都是**本节点的 `*RunCtx`**（与手写节点同一个）：取消看 `rc.Context()`，归因看 `rc.NodeID()`。糖不该把运行上下文藏起来——要调 HTTP / 数据库的实例，得能在整图被取消时醒过来。

**语义一个字都不改**：糖产出的图与手写 `NewNode` 的图，观测记录**逐字段一致**（`observe` 侧有等价锚用例钉着）。

| 糖 | 展开成 | 语义来源 |
|---|---|---|
| `Join` | 一个节点：`Requires(ins...)` + `Provides(out)`，fn 把输入收成 `Batch` | 门的「到几个收几个」；全跳过时节点自己跳过、fn 不执行 |
| `FanOut` | N 个节点：各自 `Requires(in)` + `Provides(outs[i])` | 每实例一个 goroutine；`NoValue()` = 整个实例跳过 |
| `FanOut` 的装配 | 一批 `addAll`：全量校验通过才一次提交 | **整批原子**：任一个装不进去就整个失败，图上不留半个 fan-out |
| `Batch.WaitAll()` | fn 直接 `return` 它 | 与 `pulse.WaitAll` 同口径：**整节点**以跳过收尾 |
| `NoValue()` | 一条 `*SkipError`（`errors.Is(err, ErrSkipped)` 成立） | 「这一次没有值」：**整节点**跳过，输出槽随之跳过；下游 `Join` 在 `Batch.Missing()` 里看得见 |

`NoValue()` 与 `Skip(rc, key)` **不是**同一件事：后者只把那一条输出槽标成跳过，节点自己照常返回 nil、终态是 `completed`；前者是节点级终态声明，一返回整个节点就以 `skipped` 结束（已发布的输出不回滚）。终态差别有用例并排钉着。

**缺项在类型上可见，来源也在**：`Batch` 是一张**清单**——`Items []BatchItem[T]`（`Key` + `Value` + `Present`，按 `ins` 声明顺序排），**每一条声明都在**，缺项也占一行。只给一束值，宿主就分不清「这一路没值」「这一路本来就不在」「这个值出自哪条槽」；`Len()` / `Values()` / `Missing()` / `Get(k)` 是这张清单上的四种读法。

**`Get(k)` 收的是 Key 对象，不是名字**，三种结果分得很开——「调错了」不许混进「这一路没值」：带值到达给 `(值, nil)`；那一路以跳过到达给 `(零值, *SkipError)`（与 `pulse.Get` 同口径，`return` 出去本节点就跳过，即单路版的严格）；传了一条**不在这张清单里**的 Key 给 `ErrUndeclared`（写错了，不是没值）。要整束严格仍用 `WaitAll()`；要「到几个收几个」就 `Values()`。

**判据（一个糖该不该存在）**：出发点必须是「**开发者究竟在哪里容易写错**」，不是「应该怎样实现类型安全」——糖要消灭的是**具体的写错**（同一条名字写三遍、同类型多槽位读错一条、字符串拼错静默拿零值），顺带把重复劳动减掉。反过来，按 arity 把类型参数铺开的构造函数（`Node1` / `Node2`…）锁住的多半是编译器本来就会替你查的东西：越铺越重，而真正会写错的地方一个没解决——#279 的接线糖形态 1 就是因此被否的（重新设计见 #282）。**说不出消灭哪一类写错的糖，不该存在。**

**边界（写清楚，别让下一个人以为抓得到）**：编译期锁住的是**元素类型**（`ins []Key[T]` 与 fn 的 `Batch[T]` 必须同一个 `T`）与**个数**（`Join` 收一束、`FanOut` 按输出槽开实例，N 在装配期固定）；**同类型多槽位之间的顺序锁不住**——`Keys(a, b)` 与 `Keys(b, a)`（a、b 都是 `Key[string]`）**都编译**。同一段图换个声明顺序各跑一次：

```text
Keys(a,b) -> Values()[0]="from-a"  Get(a)=("from-a", <nil>)
Keys(b,a) -> Values()[0]="from-b"  Get(a)=("from-a", <nil>)
两种顺序都编译通过；按位置读变了，按 Key 取没变
```

<details>
<summary>复现上面这份输出（自包含，<code>go run main.go</code> 即可）</summary>

```go
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/Luo-root/pulse"
)

func main() {
	a := pulse.NewKey[string]("probe.a")
	b := pulse.NewKey[string]("probe.b")
	out := pulse.NewKey[string]("probe.out")

	run := func(swapped bool) (byPos, byKey string) {
		g, err := pulse.New("probe", "Keys 顺序对照探针")
		if err != nil {
			panic(err)
		}
		// 外部输入：先在定义上声明成入口，值在运行前喂给**这一次运行**的 Inputs。
		for _, p := range []pulse.Port{pulse.NewPort(a), pulse.NewPort(b)} {
			if err := g.DeclareInput(p); err != nil {
				panic(err)
			}
		}
		var in pulse.Inputs
		if err := pulse.Seed(&in, a, "from-a"); err != nil {
			panic(err)
		}
		if err := pulse.Seed(&in, b, "from-b"); err != nil {
			panic(err)
		}
		ins := pulse.Keys(a, b)
		if swapped {
			ins = pulse.Keys(b, a) // 只是换了声明顺序：照样编译
		}
		if err := pulse.Join(g, "collect", "按输入来源收集结果", ins, out, func(rc *pulse.RunCtx, m pulse.Batch[string]) (string, error) {
			byPos = m.Values()[0] // 按位置读：写反了这里就错
			v, err := m.Get(a)    // 按来源 Key 取：与声明顺序无关
			byKey = fmt.Sprintf("%q, %v", v, err)
			return strings.Join(m.Values(), ","), nil
		}); err != nil {
			panic(err)
		}
		if err := g.Run(context.Background(), in); err != nil {
			panic(err)
		}
		return byPos, byKey
	}

	p1, k1 := run(false)
	p2, k2 := run(true)
	fmt.Printf("Keys(a,b) -> Values()[0]=%q  Get(a)=(%s)\n", p1, k1)
	fmt.Printf("Keys(b,a) -> Values()[0]=%q  Get(a)=(%s)\n", p2, k2)
	fmt.Printf("两种顺序都编译通过；按位置读变了，按 Key 取没变\n")
}

```

</details>

写反顺序的结果只是**声明顺序**变了：按位置读会错位，**按 `Get(a)` 取永远拿到对的那条**——这正是 `Batch` 带来源名要买的东西。要按位置读，声明顺序即语义顺序。

`FanOut` 的 N 是**装配期固定**的：引擎的拓扑不随数据变，数据条数不定的并行请在节点内部做业务循环，别指望运行时长出节点。

### 受管流：`ProduceStream` / `ConsumeStream` / `TeeStream`

当前主路是 `Key[Stream[T]]`。端点在运行中发布；消息带从 1 起的 Sequence，Receive 得到 Delivery，显式 Ack 才释放容量。Nack 作废当前交付、同序重投，不释放容量或推进确认前沿；旧交付再用返回 ErrDeliveryExpired。取消不自动确认，已完成 Send/Receive 在重新取额期间取消仍保留成功。

| 入口 | 展开成的普通节点 | 边界 |
|---|---|---|
| ProduceStream | Provides(out)，OpenStream 后调用 `fn(*RunCtx, *StreamWriter[T]) error` | Send/SendAt 逐条接纳，完整 Aspect 链决定终态 |
| ConsumeStream | Requires(in)，逐条 Receive/HandleMessage | `fn(*RunCtx,T) error` 成功才 Ack；空流成功、缺值正常跳过 |
| TeeStream | Requires(in) + Provides(outs...) | 一条输入的有限分支批次提交后 Ack；outs 装配时复制 |

三者在 g、id 后接 description，允许空串，WithStreamAspects 使用普通 NewNode 链。图级、执行级、本地切面由外到内，重复配置按声明顺序追加。选项隔离切片，闭包业务状态不深拷贝。手写节点直接 OpenStream/Send/Receive/Ack；OpenStream 不接受构造糖的 WithStreamAspects 或 WithMessageHandling。写端留在实际节点同步链中，同一 RunCtx（含 Fork）串行 I/O。

WithBuffer 限制排队加已读取未确认的总在途量，受管默认 1。ConsumeStream 不建缓冲，正数配置装配拒绝。受管 I/O 仅在阻塞等待期间暂时归还业务额度，业务继续、取消返回或 panic 展开前重新取回，纯受管 pipeline 额度 1 可推进。任意业务 I/O 不自动让额，其他节点不协作持额时取消返回仍可能等它。Messages 直接暴露原生 Delivery channel，无转发 goroutine；它仅支持单读取者，原生 select 用只等待的 rc.WaitStream 回调，业务放回调外。多 worker FanOut 竞争必须用 Receive。

生产完成和读取完成分别记账。Snapshot 给 Terminal/Err、LastSequence、连续 AckedThrough、InFlight、Stopped；Done 在**实际写端完整 Aspect 链**结束后关闭，别名共享同一本账，祖先 Sub 不是 EOF 边界。正常 child 清理不撤销尾部。Receive 仅在正常完成且交付义务已清，或显式 Stop 后返回 io.EOF，真实失败返回原因；Messages 关闭后也要读 Snapshot.Err。空流是 ready 端点、零消息，不等于 skipped。

Stop 显式结束读取组，清理不算 Ack。ConsumeStream 回调返回精确 ErrStreamStopped 表示当前条成功，先 Ack 再 Stop；包装错误仍失败。ProduceStream 只有写端确实 stopped 且回调返回精确 ErrStreamStopped 才正常结束。TeeStream 单支 Stop 后其他继续，全支 Stop 后正常 Stop 输入；真实错误不能洗成正常停止。最后读取者提前返回、既未完成消费确认也未 Stop，会明确报读取义务错误。

宿主用 SubscribeStream(e,key)，必须在 NewExecution 后、Start 前登记并参与实际读取组/别名检查。Wait 等全部节点和必需宿主读取组完成、Ack 或 Stop，不代为排空。仅宿主读取的根流应 NewExecution → Subscribe → Start；可先 Validate 冻结定义。Graph.Validate/Topology 仅对已声明的 managed 根出口省略缺读者诊断，允许稍后接宿主，不算真实 host，不放行未导出流，也不改变原生 channel。Execution.Start 仍按实际订阅严格检查缺读者及静默分流，因此 Durable 可先冻结 schema，再在 BeforeStart 登记。被订阅的输入输出透传需本轮 Seed/SkipSeed，漏值启动前报错，skip 正常。ReadOutput 仍在 Wait 后读结果，没有启动后补订阅、补历史或无限缓冲。

HandleMessage 是显式有限原子输出边界：默认一次，最多 64 条暂存，每输出批次不得超过容量，超限当场失败、不等自身回调结束。失败 Set/Skip/Send 不发布，重试同一条后再下一条；成功才确认输入，零输出与正常 NoValue/Skip 也成功，耗尽或提交错误不能吞成成功。WithMessageHandling(WithMessageRetry/WithMessageLimit) 仅配置 ConsumeStream/TeeStream，ProduceStream 拒绝非空配置。闭包、引用与外部副作用不回滚。渐进 token 在该边界外逐条 Send/SendAt，已发前缀不撤销；可续读来源显式 WithStreamSource，业务用 Position 游标续读，否则失败或新 Execution，不能自动整节点 Retry 拼两版。

普通 Retry 在实际外接流 I/O 之前拒绝，确定 skip 无数据路径仍正常。整条有限内部流水线仅以普通值跨 Sub 时，可在挂载边界重试，内部节点不能继承 Retry。流的增量检查点由可选 StreamHooks 提供，不编码本地 channel，完整契约见 [checkpoint.md](checkpoint.md)，Durable #21 负责真实持久后端与进程恢复。

### 原生 channel 兼容边界与静态校验

旧 Key[<-chan T] 与 Produce/Consume/Tee 保留，节点活着收发、发送端返回时关闭 channel，支持 WithStreamAspects。它们仍整段持额，按真实额度池保守要求流参与者同时存活；WithBuffer 默认 0，仅限制排队。无 Ack、无自动倒带或消息持久性，WithStreamSource/WithMessageHandling 明确拒绝。Produce/Tee 在写暂存内因延迟发布 channel 而立即拒绝；旧 Consume 的整节点重跑不提供当前消息重放保证。

静态校验按挂载位置展开，沿 In/Out/透传追踪原生和受管流别名，数实际读取者；Sub 不作读取者或额度参与者。同一挂载 FanOut 组是显式竞争，广播用 TeeStream/Tee。声明根出口不算实际消费者；定义检查仅允许已声明 managed 根出口等待宿主连接，Start 仍加入本次有效额度、Aspects、skip 和真实宿主订阅，严格查缺读者与分流。实时流 Out 提前绑定，普通 Out 才依赖 child.Wait，生命周期图按真实关系检查并报告具体等待环。不同独立额度池即使数字相同也不合并。

校验模型只用于诊断，不调度；不推断业务/Aspect 隐藏等待、手写 channel close 时机或 Fix 的实际指针别名。确定 skip 按挂载保留，有效 Aspect 不凭空推断输出缺失；逆向索引 worklist 传播事实，避免按声明顺序反复扫全图。无流树逐层只读探测，嵌套 Topology 共享根检查事实。YAML 可登记同一 Key[Stream[T]] 与 Run 工厂，复用 core 执行器；Requires/Provides 归 YAML 显式声明，不能藏 channel 后声称拓扑齐全。中英文完整可运行主路、原生 select、host 和跨 Sub 片段见 [流指南](../../site/guide/streams.md)。

### 排他分支：`Only`

```go
if cond {
    return pulse.Only(rc, OutA, v) // 写 A，本节点其余 Provides 全部作废
}
return pulse.Only(rc, OutB, w)
```

它**不判断条件**——走哪条还是调用方 `if` 出来的。它消灭的是**漏表态**：手写分支要 1 次 `Set` 加 N−1 次 `Skip`，少写一边是静默的——实测「只 `Skip(B)`、忘了 `Set(A)`」时 A、B 两条下游**都不跑**（未写的 `Provide` 被自动跳过），而 `Run()` 仍返回 `nil`、没有任何提示。用 `Only` 的调用方不再自己写 `Skip`，这个错在形态上就不存在了。

反面：重复表态会**变吵**——先 `Set` 过别的出口再 `Only`，会给那条已就绪的槽补一次 `Skip` → `ErrConflict`（而手写 `Set` 两次是被静默忽略的）。所以 `Only` 要**代替** `Set`，需要同时写出多个值的节点继续手写 `Set`。

### 图即节点：`Sub`（静态组合）

一张图可以当**一个节点**嵌进更大的图（「图即节点」），于是流程按层组合，而不是铺成一张越来越大的平图。**主路径是静态组合**：子图是**装配期就建好**的定义，`Sub` 把它挂到父图的一个节点上——A / B / C 分开构建、D 把三张组合起来之后，D 在**运行之前**就看得见整棵子树，也报得出子树里的静态毛病。

```go
child, _ := pulse.New("enrich", "富化这一步")
// …给 child 装节点：读 topicKey、写 summaryKey…
if err := child.DeclareInput(pulse.NewPort(topicKey)); err != nil { // 入口：父图可以喂进来
	return err
}
if err := child.DeclareOutput(pulse.NewPort(summaryKey)); err != nil { // 出口：父图可以接出去
	return err
}
err := pulse.Sub(parent, "step1", "交给子图跑的一步",
	[]pulse.SubBind{pulse.In(topic, topicKey), pulse.Out(summaryKey, summary)}, child)
```

**端口是唯一的接线面**：`In(父, 子)` 的子侧端必须是 `DeclareInput` 声明过的**入口**，`Out(子, 父)` 的子侧端必须是 `DeclareOutput` 声明过的**出口**；接到子图**内部的中间 Key**（某个节点 `Provides` 但没声明成出口的）是**静态错**——想让父图用那条 Key，就显式声明成出口（意图明确）；不声明等于「这是我的实现细节，不承诺」。声明过的入口在静态校验里**不再算「无来源」**：`Validate` 因此分得清「你忘了接线」与「这本来就是外部输入」，静态拓扑里它们的来源是 `pulse.SourceInput`——`Seed` 是**运行期**的动作（写的是这一次运行的输入），不该出现在定义的结构里。**出口对称地多一条判据**：`DeclareOutput` 了一条**没人产出**的键（既没节点 `Provides`、也不是同名入口透传）是静态错——装配期就报，而不是等子图跑完由 `bridgeOutputs` 说一句 `never arrived`（那时已经跑了一半，病根也不在那句话里）。端口是**静态**的：它只参与拓扑与校验，不携带任何一次运行的值。

**接线的形状判据（`checkBinds`，三个入口共用一份：`Sub` / `Validate` / `Topology().Problems`）**：

- **同一条子侧进口只许绑一次**：两次种同一条槽，第二次被幂等首写**静默忽略**（父侧传进去的值不见了，一句报错都没有）——所以 `In` + `In`、`In` + `Fix` 打在同一条进口上，装配期就是一句 `bound twice`。**`Fix` / `FixSkip`** 给的是**属于这一处挂载**的固定值（同一张子图挂两处要不同的默认值时用它）；它不是子图定义的一部分——这正是旧的那个「往子图里 `Seed`」入口消失的原因，而**每一处挂载都要**的默认值该放进子图自己（一个产出它的节点）。出口**不去重**——同一条出口推给两个父侧键谁也不丢（推给同一个父侧键早在 `Add` 那层就被「declares ... twice in Provides」拒了），所以 **`In(父,k)` + `Out(k,父)` 打在同一条端口上是合法的**——那就是透传（同一条键既声明成入口又声明成出口）；
- **进口在子图内部有来源时，`In` 直接判死**：子图内部自己产出的键**不是能绑的端口**，报文点明它不是子图的输入端口（那是子图的实现细节，要对外就用 `DeclareOutput` 说出来）。而「一条键既是声明过的进口、又有节点产出它」那个状态**根本建不出来**——`DeclareInput` 与 `Add` 两个方向都在声明那一刻撞 `ErrDuplicateSource`（一条键恰好一个来源），所以声明的进口永远是能用的进口。判据只看定义，不看任何一次运行留下的痕迹；
- **反向那一遍：子图需要的进口，这一处挂载都得给**（`In` / `Fix` / `FixSkip` 都算）。两种「需要」都是读一遍声明就能定的：① **子图里有节点 `Requires` 它**——漏绑的话子图这次跑不起来，而 `newChildExecution` 报那句「has no value for this run」是在**运行期**（父图别的节点可能已经跑完，`Validate()` 还已经把这个定义封了口）；② **它是这一处绑出去的出口的唯一来源**（同一条键既是进口又是出口 = 透传，而声明过的进口不可能再被节点产出）——漏绑的话 `bridgeOutputs` 只能等子图跑完才报一句 `never arrived`。两个边界：没人 `Requires`、也没被这一处绑出去的进口是**可选**的（别的挂载点的接口，或这一处用不到的透传端）；义务由**承诺**定，所以第 ② 种只看**绑出去**的出口。
- **整批判完才提交**（与 `Add` 同一条纪律）：报文点着子侧键名，判据与报文只写一份。

**切面落在父侧那一步上**（`Sub(..., aspects...)`），例如 `pulse.Timeout(30*time.Second)` = 整张子图限时、`pulse.Retry(3, 0)` = 整张子图最多跑三次。**`Retry` 现在装得上了**（阶段 3）：它给每一次尝试开一条独立的 attempt 写集，而每一次尝试都会**重新建一份子执行**——第一次那轮桥回父侧的结果属于第一次那条写集，失败就整条作废，父侧只拿得到**成功那一次**的输出版本。所以「桥回发生在第一次尝试里」不再是问题，「一半第一次、一半第二次」的混装也不会出现；`Retry` 照旧只报**最后一次**尝试的错误（那是它的契约，与任何节点一样）。实际外接流输入/输出不能借普通 Retry 倒带或撤回；完整内部有限流水线只桥普通值才可整体重试，内部节点不继承该 Retry（见 §5 流式）。声明式装配那边（`pulse/yaml`）有 spec 可读，`graph:` 节点上的 `retry:` 照装照跑，同样是每次尝试一份新子执行。

**装配完即可见、不必运行**：`Topology()` 沿静态挂载边**递归**（`Nodes[i].Sub` 是一棵完整子树，`Binds` 是这一处的接线），路径由**挂载位置**推导（`outer` / `outer/inner`），**读它不会改动任何定义**（「同一个子图挂两处、路径互相覆盖」那类坑到此为止）；`Validate()` 同样递归——子图的静态毛病在**父图**上就报，**按引用点**各报一条并点明挂载位置：

```
pulse: subgraph "inner" (mounted at "step1/toInner"): pulse: node "leaf" requires "c.nope" but nothing provides it (declare it with DeclareInput if the value comes from outside)
```

**组合环在装配期拒**：`Sub` 挂载那一刻沿子树查「父图是不是已经在它的子树里」（自挂也算），是就报 `pulse: subgraph cycle: A -> B -> A`。判据只看**当前这条递归路径上的祖先集合**——同一张子图挂在**不同位置**是合法复用，不能用全局 visited 误拒（那是复用，不是环）。

**这条判据的「查」与「挂」在同一个全局临界区里**（`graph/Add` 附近的 `assemblyMu`，改定义的入口与 `Validate` / `Start` 的「判 + 封」共用它）：组合环是两张图各自动作的**合成**结果——A 里挂 B 与 B 里挂 A 各看各的都对，只有放在一起才成环，所以单看任何一张图的锁都拦不住（两个 goroutine 各自先查过「没有环」再各挂一步，环就进去了）。同一把锁还挡着第二件事：**「判完整棵树」与「封上这棵树」是两趟**，每一层各自取放锁，两趟之间那些锁是空的——窗口里别人改一下子图，封住的就成了没校验过的那份（实测：窗口里给子图加一个「要一条没人提供的输入」的节点，`Validate()` 返回 nil 而封住的子图带 1 条毛病）。装配是一次性动作、临界区里没有用户代码，代价是零。**递归展开另有一道兜底判环**（`mountChain`）：万一环还是进去了，`Topology()` / `Validate()` 要报一句能照着改的错，而不是一路递归到爆栈（单线程）或两层互等对方的锁（并发）。

**封口规则**：`Validate()` **通过才封口，而且封的是整棵子树**——此后 `Add` / `DeclareInput` / `DeclareOutput` / 挂子图在这一层与它挂着的每一层子图上都被拒（`ErrGraphStarted`）；**校验失败一层都不封**，补上缺的那块再校验正是要支持的用法。没有 `Seal()`：它与「成功即封口」职责重叠。封口只挡「改定义」——`Seed` 完全不在这条线上（它写的是这一次运行的输入，不属于任何定义），**已经封口的子图照样能被别处挂载，而且能在那儿跑**（封口冻结的是定义，不是「用过了」）。

**另一个封口时机是「跑起来」**：`(*Execution).Start()` 成功同样**封整棵定义子树**，那四个改定义入口都按 `Graph.sealed` 拒绝：少了这一步，持有子图指针的调用方能在「父图已经跑起来、子图还没轮到」之间接着 `Add` / `DeclareInput`（实测都进得去），于是真正跑的就不是观察与拓扑看到的那张图。`Graph.sealed` 与 `Execution.started` 分属两个对象，**不是先看 started、再看 sealed 的优先级关系**：改定义遇到 sealed 与同一份执行二次 `Start` 是两个独立入口，共用 `ErrGraphStarted`，报文分别指出封口或已经启动。封口之后定义照旧能用（建执行、`Validate`），跑起来之后只是**这一份执行**不能再 `Start`——**别的执行不受影响**：同一张定义随时可以再跑一份（§2），这正是阶段 2 拆开的那件事。

**三处手工搭会静默出错的地方，正是这一步要消灭的**：

| 手工搭要记得 | 漏了的后果 | 收进一次调用后的做法 |
|---|---|---|
| 把观察者挂到子图 | 父图照常跑完、`Run()` 返回 `nil`，子图的记录**一条都不出现** | 子图没挂观察者时**自动继承**父图的（按**有效值**继承：父执行环境里那个，子图自己声明过就用它自己的） |
| 子图的取消域 | 手工搭时子图在谁的取消域里跑全靠自己记得，忘了就吞掉取消 | 子执行的 ctx 从**挂载那一步的 ctx** 派生：父侧取消（含这一步上的 `Timeout`）天然传下去，**没有转发 goroutine** |
| 手写两端桥接 | 父子槽来源、实时端点与普通结果的桥接时机需要保持一致 | In 稳定绑定来源槽；实时 Out 提前接通，普通 Out 在 child.Wait 成功后桥回；宿主按实时订阅或最终 ReadOutput 读取 |

**运行语义**：Sub 无整体输入门，In 稳定绑定父来源槽，可 pending；内部节点按各自 Requires 激活，全入口 skipped 仍创建 child，独立自产分支可运行。Stream 与原生 channel Out 在 child.Wait 前绑定，普通结果仍等 child.Wait 成功后桥回；挂载步骤本身仍等 child 最终完成且不占额。实际写端完整链决定流终态，祖先挂载不是 EOF 边界，child 正常清理不撤销尾部。真实 child 错误原样冒泡；全部绑定输出 skipped 则挂载正常 NoValue。两处要记住：

1. **一处挂载一份子执行**：每一次跑到这一步，都为**这个挂载位置**建一份自己的子执行（ctx / 槽位 / 首错互不干扰），所以同一张子图挂两处时**两处各跑一份**（阶段 2 落地；旧版本是「一次性认领 + 运行期回写子图字段」，只跑得了一处、两处还会互相踩配置）。定义始终只读——有效观察者 / 名额 / 图级切面是**建子执行时算出来的**，不写回子图的字段。
2. **取消域**：子执行的 ctx 从**挂载那一步的 ctx** 派生（这一步上的 `Timeout` 也在其中），父侧取消天然传下去——没有转发 goroutine，也不会出现「子图建在 `context.Background()` 上、把取消吞掉、父图跑满整段还报成功」那类形状（旧版本实测 300ms + 报成功；现在是 51ms + `context canceled`）。

`aspects` 落在**父侧那个节点**上：`pulse.Timeout(30*time.Second)` 就是给整张子图限时。父图的**图级**切面还会**继承进子图**（见 §4）。

**观察者那条链自动接上**：父图挂了 observer 时子图什么都不用写，同一个出口就看得见嵌套的全过程，两图靠 `pulse.graph` 分得开。子图的图 id 由**建它的那一步**定（`pulse.New` 的 graphID）——沿用 `<父图 id>/<节点 id>` 这套写法，观测里读起来最顺（声明式装配就是这么定的）。层级归因是另一件事：**挂载位置随每条回调由引擎发出来**（`Site.Path`），出口直接读它就带上了 `pulse.path`——不必再按层各建一个出口（`ObserveConfig.Path` 只在 site 给不出位置时兜底），见 §8。

## 6. 并发与读语义

- 节点各自一个 goroutine；`RunCtx` 的 context 是唯一取消通道；
- **定义**由互斥量保护：`Graph` 的 `Add` / `DeclareInput` / `DeclareOutput` / `Sub` 取 `g.mu`，而「查一遍整棵树 + 封口」与它们共用一把包级装配锁（见 §5），封口之后一律拒绝；**这一次运行的输入**（`Inputs`）在 `NewExecution` 那一刻被拷成内部只读表，此后改那个值不影响已经建好的执行——所以它不需要任何锁，也不是共享状态；
- 槽位读写由各自的互斥量与 `done` channel 保护；
- 观测回调**在节点 goroutine 上同步执行**（见第二部分）——所以观察者必须并发安全且不得长时间阻塞。

---

# 第二部分 · 观测

## 6.1 静态形状与运行读数

`g.Topology()` 回答「图长什么样」：Nodes 按 Add 声明序，完整子树在 Sub，Binds 保存父子接线，Graph.Path 按挂载位置推导。Graph.InPorts / OutPorts 保留完整声明接口，按首次成功声明序；unused 进口和透传两侧都在，普通内部键不自动成为出口。进口 From 为 SourceInput，出口 From 为 SourceNone，来源从节点或透传入口推导。副本中的结构与切片不影响定义。

`e.Snapshot()` 回答「本轮跑到哪」，无需订阅 Observer：

| 结构 | 字段与含义 |
|---|---|
| GraphState | ID / Description 是定义；Execution 是本次运行；Path 是挂载位置；Status / Err 是进度与错误，收尾前记录首错，收尾后与 Wait 的最终结果一致 |
| NodeState | ID / Description / Status / Inputs / Outputs / Child / Err；节点与 Topology 同声明序同 ID |
| KeyState | Name / Type / From / State；只给类型、来源与到达状态，输入上的 From 与 Requires 共用词表 |

GraphStatus 是 pending / running / completed / failed / canceled；NodeStatus 还含 waiting / skipped；SlotState 是 pending / ready / skipped。节点四个终态与 NodeFinishReason 逐字相同，ready 与 skipped 都是到达。waiting 若仍有 pending 输入，就是在等这些输入；输入全到达（或没有输入）则在等进入 Run 的机会，切面可能延长这段等待，不能保证它正在阻塞于名额。

状态在 Observer 回调前迁移，回调内读得到这次迁移。Retry 不重复 waiting / running，也不倒退状态。图创建后 pending，成功 Start 后 running，在 Wait 收尾时确定终态；只 Start 不 Wait 时，即使所有节点已结束仍为 running。正常收尾的 cancel 不污染 completed；尚未被节点观察到的 ctx 取消不提前当作终态错误。

Child 是挂载点最新已创建尝试的活 `*Execution`，创建后、Start 前就记录，失败或取消仍保留；未建出时为 nil。继续查看它时再调 Child.Snapshot，取得另一个时刻的读数。运行态按节点定义指针记录在 Execution 上，不挂 Node 或 Graph；Start 前新增的节点可见为 pending，成功启动封口后节点集合固定。

Snapshot **不携带业务值，没有带值开关**。除 Child 与 error 引用外，结构与切片都隔离；读取不建槽、不推进运行、不发新事件。定义、执行状态与各槽位分别在自己的锁里采样，**不是原子切面**：running 节点已有 ready / skipped 输出、其他输出仍 pending 都合法。Retry 暂存中的写尚未发布，不出现在执行槽位读数中；它不是 checkpoint、恢复或结果读取协议。

## 6.2 宿主结果读取：`ReadOutput`

```go
value, state, err := pulse.ReadOutput(e, result)
```

这是泛型包函数，签名为 `ReadOutput[T any](e *Execution, k Key[T]) (T, SlotState, error)`。它只读本轮执行所属图 `DeclareOutput` 声明过的同名同类型键；Key 没有图来源身份，同名同类型等价。内部 Key、未声明出口、零 Key 与错误类型拒绝，包装 ErrUndeclared；nil / 零 Execution 直接报错。它不复用节点 Get 的权限，不开放字符串 any 黑板。

成功 Wait 收尾后，ready 返回业务值（包括零值、nil interface 与 typed nil），skipped 返回零值与 skipped，未喂且无人 Requires 的可选透传返回零值与真实 pending；三者均为 nil error。尚未收尾立即报错，不等待、不返回部分值；失败或取消返回执行原因。错误非 nil 时值与状态无效，状态为空串；不把正常缺值编码成失败。GraphFinished 回调前终态已确定，所以回调内可读；Wait 正常释放 ctx 不改变结果。

Wait 会合登记的可靠流工作后，在同一执行锁内先锁存结果再置 done：此时已锁存的树级 CheckpointError 优先并归为 failed，否则保持普通首错或当前真实取消；不重新遍历用户错误。Err、GraphFinished、Snapshot 与后续重复 / 并发 Wait 复用该最终结论，既保留“所有节点已退出、收尾前才取消”的真实原因，也不把自己收尾的 cancel 反抓成失败（#327）。各子执行在自己的收尾边界应用同一规则，已确定的终态不会被其他执行稍后的失败改写；这不构成整树原子快照。

读取只在定义、执行与槽位各自的锁内取已有数据，不造槽、不推进、不发事件。业务值不深拷贝；slice/map/指针等仍引用宿主对象，宿主负责同步和所有权。Snapshot 与 Observer 仍不带业务值；Durable 全节点提交/恢复另走 #307 的受校验契约，不借用结果读取绕开内部键权限。

## 6.3 Mermaid：从静态结构生成图

```go
structure := g.Topology().Mermaid()
progress := g.Topology().Mermaid(pulse.WithMermaidSnapshot(e.Snapshot()))
_, _ = structure, progress
```

序列化归根包，不让手写 Go 图为了出图依赖 YAML 或 observe。图、节点、端口与接线沿视图声明序展开，内部 ID 由遍历序数生成；map 只查找，不决定输出顺序。用户名字、描述、类型与 Key 名均作显示文本编码，不进入 Mermaid 指令位置。未使用口、透传、固定输入与同一子图多处挂载均有独立端点；Fix / FixSkip 只显示 fixed，不显示值。

没有快照就只出结构，不猜状态、不着色。给 WithMermaidSnapshot 时，只按图 ID + Path + Node ID 匹配当前结构；未知状态和错图 / 错路径快照忽略。子状态从活 Child 逐层再采样，不能称为整树同一瞬间的快照；错误正文和业务值不进入图。nil Topology 返回 flowchart 声明，nil Snapshot 与未提供选项一致。渲染时调用方不能并发改传入的 Topology / Snapshot 副本。

## 6.4 可靠检查点桥接

`WithExecCheckpoint` 显式启用整棵执行树的检查点模式：完整 Aspect 链外开节点写集，Retry 成功仅交外层暂存，最终完整输出经同步 Commit 确认之后才逐键发布。可靠钩子失败锁存到整棵执行树，即使含 ErrSkipped 或被祖先切面吞错也不能成为正常成功。

`CheckpointNodes` / `CheckpointPorts` 提供真实类型声明副本，`CheckpointInputs` 只复制已接收外部输入；内部值仅通过受校验完整节点包交付。`RestoreNodes` 登记整批计划，Start 在封口同一临界区复验并全部恢复，合法恢复节点不重跑体、切面或提交。

子图采用方案 A：先认回持久根尝试与嵌套 Retry 游标，再打开对应子作用域；进程中断复用已确认子成果，失败新尝试不能借旧成果，Resume 不补预算。OpenChild.Bindings 保存稳定 In 来源（可 pending），Fix 核对实际值。可选 StreamHooks 六项独立保存消息/游标/确认/状态/输出；完整节点的流输出保存 descriptor，实际写端成功终态随完整 Commit 原子确认。CheckpointAttempt.Values 仅在 active 完整节点根 attempt 返回已确认消息贡献的普通输出子集；Core 在业务前恢复到 checkpoint 外帧，完整 Node Commit 成功后才发布，Retry/OpenMessage 的嵌套 Attempt.Values 必须空，无流与失败祖先规则不变。completed 来源不跑业务体但重建 Done 与尾部，active 来源无显式续读承诺则拒绝，旧代际不能推进新账。原生及普通值中嵌套 channel 仍拒绝。存储与进程恢复由 Durable 提供，集成跟踪见 Durable #21，发布以实际 tag/release 为准；完整机制见 [检查点桥接与子图恢复](checkpoint.md)。

## 7. 分层

```
pulse（引擎）          不认识观测
   ↑ 实现 Observer seam
pulse/observe          Record / Sink / 折叠适配
```

引擎通过以下 `Observer` **seam** 提供生命周期回调（每条回调带一个 `Site`：定义身份 + **本次运行身份** + 挂载位置）：

```go
type Site struct {
    Graph     string // 定义身份（New 的 graphID）
    Execution string // 本次运行的身份，同一个挂载点重试两次也不一样
    Path      string // 挂载位置，根执行是 ""
}

type Observer interface {
    OnGraphStarted(site Site)
    OnGraphFinished(site Site, reason NodeFinishReason, err error)
    OnNodeWaiting(site Site, nodeID string)
    OnNodeRunning(site Site, nodeID string)
    OnNodeFinished(site Site, nodeID string, reason NodeFinishReason, err error)
}
```

`Execution` 进回调是阶段 2 的事：同一张定义跑两次时，只按 `pulse.graph`（再加 `pulse.path`）记账会把两次运行折叠成一份，而 `Execution` 每次都不同——观测因此天然分得开。

图级不超过两条（`GraphStarted ≤ 1`、`GraphFinished ≤ 1`），节点不超过三次回调（`Waiting ≤ 1`、`Running ≤ 1`、`Finished = 1`）；`Retry` 的多次 attempt **不重复这三条节点生命周期回调**。时序是**夹住**：`GraphStarted` 在提交任何节点 goroutine 之前发出（启动校验失败的执行没有启动，不发），`GraphFinished` 在全部节点终止之后、`Wait` 返回之前发出——只 `Start` 不 `Wait` 的宿主拿不到它；并发 / 重复 `Wait` 都在这一发返回之后才返回。图级终态只会是 `completed` / `failed` / `canceled`（跳过是节点级的事实，升不到这一层）。图默认 no-op，`WithObserver` 挂载（定义声明），`WithExecObserver` 只改**这一次运行**的有效出口，需要多个时用 `MultiObserver` 组合。

安装的观察者还可实现 `AspectObserver`，通过 `OnAspectEvent` 接收逐次尝试、重试、退避与超时到期事实；`ObserverFunc.Aspect` 提供同一可选入口。这些额外事实不改变上述生命周期次数。具体阶段和用法见[观测指南](../../site/guide/observability.md)（[English](../../site/en/guide/observability.md)）。

节点回调在**节点自己的 goroutine** 上执行，图级两条在 **`Start` / `Wait` 的调用方 goroutine** 上执行——两边都同步。由此两条并发约束：`WaitGroup` 计数必须在 `started` 对别的 goroutine 可见**之前**登记（含 `started` 那一发自身），否则并发 `Wait` 会在节点还没提交时就返回；收尾那一发走 `sync.Once`，好让后来的 `Wait` 等第一次调用结束，而不是各自返回、把出口先收掉。

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

Flush 的入队序号固定本次排空范围，后续 Write 不延长它。唯一 worker 串行执行 inner.Write/Flush/Err；不同快照边界立即按序登记，同目标共享、较早目标不扩充，取消无人等待的未开始边界，边界数至多容量加一。ctx 覆盖等待内层刷新，取消 Flush 不丢记录。Close 先停收，首次调用控制排空与最终结果，并发调用可用自己的 ctx 结束等待。无 ctx 的内层调用不能强杀，取消后最多这个 worker 继续留在调用里，等返回后收尾。Flush/Err panic 在这个 worker 边界捕获并作为固定错误返回，不格式化 payload；error payload 经 Unwrap 保留，已交付记录不误计 Dropped，后续记录仍能继续处理。Slog 的 duration_ms 为数值毫秒，保留亚毫秒精度，零耗时省键。

## 10. 图适配：`NewRecordObserver`

`observe` 实现引擎的 seam，把**一次运行折成两条运行级记录**；节点按实际经历的等待段、执行段产出记录，未进入等待段的失败另发终态记录：

| 事件 | 何时 | 内容 |
|---|---|---|
| `pulse.graph_started` | 提交任何节点之前 | `Duration = 0`；`Status = "running"` |
| `pulse.graph_finished` | 全部节点终止之后（`Wait` 返回前） | `Duration` = 整轮耗时；`Status` = 运行终态 |
| `pulse.node_wait_finished` | 等待完成（进入执行，或以 skip/失败终结） | `Duration` = 等待段；进入执行时 `Status = "running"`，否则为 finish reason |
| `pulse.node_run_finished` | 执行完成 | `Duration` = 执行段；`Status` = finish reason，含业务体返回 NoValue 的 skipped |
| `pulse.node_finished` | 切面在调用 next 前失败或取消，未进入等待段 | `Duration = 0`（未测量）；`Status = "failed"` / `"canceled"`；保留节点归因与 Err |

运行级两条把本轮的节点记录**夹在中间**。一轮里全部节点都跳过，整轮仍是 `completed`。输入全跳过、未进业务体的节点只有一条 skipped 等待记录；业务体返回 `NoValue()` 时保留等待与 skipped 执行记录，因为确实进入过执行段。正常 NodeSkipped 的 `Record.Err` 为 nil；可靠故障即使包装 ErrSkipped 也保留 failed 身份与原因链。

Finished-only 的 completed/skipped 恢复节点不伪造业务计时记录，恢复事实由 Durable 事件提供。Finished-only 的 failed/canceled 则产出 `pulse.node_finished`，保留完整 Site 和节点，未知耗时留零。

归因维度走 Attrs：`pulse.graph`（定义 ID）+ `pulse.node`（节点 ID）+ `pulse.execution`（本次执行 ID）。**key 契约由引擎定义**（事实归属包），`observe` 只消费不定义。节点记录带图与节点，运行级两条不带节点；每条记录都带 execution，同一张定义在同一位置跑两次也分得开。

嵌套时再加第三个维度 `pulse.path`：不透明字符串、`/` 连接各层，**段是子图在它父图里的那个节点 id**（不是图 id）。两者分工是硬的——`pulse.graph` 是**你起的图 id**，`pulse.path` 是**引擎记的层级**：同一份子图定义挂在两处，path 不同；同一挂载位置跑两轮则 path 相同、execution 不同，靠执行身份分得开；反过来，id 里带不带斜杠都影响不到 path。段是节点 id 还有一条硬理由：**子图的记录正好挂在父图那个节点的记录下面**——父图 `step1` 那条记录没有 path、节点 id 就是 `step1`，子图记录的 path 也是 `step1`，拼树时不用去解析任何图 id。规则三条：

- **取值面在引擎手里**：这一层的**挂载路径**（根执行 `""`；一层子图 `"step1"`；两层 `"outer/inner"`）随每条回调由引擎发出来（`Site.Path`，装配时就定得下来），出口直接读它，**不必再按层各建一个出口**；`ObserveConfig.Path` 只在 site 给不出位置时兜底（旧装配的用法）；
- **出口写记录**：非空时**每一条**记录（含运行级两条）都带 `pulse.AttrPath`；**为空不写**这个 key——空串会让「根」与「忘了传」长得一样；
- **记录层级自己拆**：按 `/` 分出挂载路径；Record 保持平面信封，静态树与活子执行分别从 Topology / Execution 快照查看。

这条维度以前只能靠「按层各建一个出口」拿到（出口实例上带一个固定 path），阶段 2 之后引擎把位置随回调发出来，宿主少一层装配负担——`observe` 不认识图结构，它只是把回调里的 `Site.Path` 抄进记录。

**耗时的口径**：四段都是墙钟，且**执行段包含「等待段那条记录写出口」的耗时**——回调在节点 goroutine 上同步执行，`Running` 里先给 `runStart` 打点、再落等待段记录，于是慢出口把自己的写开销记进了执行段。实测同一张图、同一个空节点（`noop`）：`MemorySink` 四段都是 `0s`；出口换成每条 `Write` 睡 20ms 的假出口时，执行段 `20.57ms`（就是那次写出的代价）；**整轮那条 `62.24ms`**——运行级的窗口覆盖「提交节点 → 全部终止」，这一轮里三次写出（运行级 started、节点等待段、节点执行段）都在窗口内，所以它天然比宿主的掐表少一点（不含装图、`Seed` 与 `Flush`），又天然比节点段之和大一些。两条推论：**耗时列偏大先怀疑出口**（慢出口套 `AsyncSink`）；段耗时为 `0`（内置版式渲染 `-`）表示该段耗时被计时精度取整为 0，而不是「没有这一段」。

```go
obs, err := observe.NewRecordObserver(observe.ObserveConfig{
    Sink: sink, HostID: "host-1", TraceID: observe.NewTraceID(),
})
g, _ := pulse.New("demo", "演示图", pulse.WithObserver(obs))
```

单实例可复用于多图并发、也可复用于同一张图的多次运行：内部按 `(本次运行, 节点 id)` 记账（`Site.Execution` 进键），所以同一张图并发跑两次不串扰——记录靠 `pulse.execution` 分得开。

## 11. 宿主自带出口

列式版式是**默认**的，不是唯一的。宿主想让域事实进列时用 `WithRenderer` 换掉行体渲染器，用导出的编码原语拼自己的列——**出口仍然不认识任何业务语义**，域语义留在宿主手里。

六条编码原语（`AppendDuration` / `AppendTextValue` / `AppendAttrs` / `AppendAttrsExcept` / `AppendPadding` / `DisplayWidth`）与内置版式**同形**：同一条记录在两种版式下，耗时、属性组、列补齐逐字节一致。一致性由基座保证，宿主只决定「我这个域有哪些列」。

渲染器的契约只有四条：只产行体（行首标识与结尾换行由 sink 加）；拿到的 `color` 是 sink 解析好的结论；不缓冲、不写 writer；**在 sink 的内部锁内被调用**——别在渲染器里回调 sink 的 `Write`/`Flush`/`Err`（`sync.Mutex` 不可重入，会安静挂住）。

## 12. TraceID

由宿主**单一生成源**注入：宿主每次运行调用一次 `NewTraceID()` 即构成单一生成源，也可以完全自带方案（如 hostID 前缀 + 自增序号）。返回值无契约语义，消费方不要解析其结构。同一次运行的全部记录共享同一个 TraceID——这是运行级关联的全部机制。

接 W3C trace context / OTel 的宿主**注入自己的 trace id**（OTel 的 `span.SpanContext().TraceID().String()`，或按规范自行生成 32 位小写 hex），不要指望 `NewTraceID()` 的形状——它是「时间戳-随机段-序号」，不是 W3C 格式。两条边界：① 只有 trace-id 发不出一条合法 `traceparent`，`parent-id` 与 `trace-flags` 是 span 语义（本层只有「一次运行」，没有 span），归宿主；② 入站续接 = 把收到的 trace-id 填进 `ObserveConfig.TraceID`，非法 `traceparent` 整条忽略由宿主负责。

## 13. 隐私边界

`Record` 没有 `map[string]any` 逃生舱：`Attrs` 的写入面只有泛型 `Set`（标量约束 `~string|~int64|~float64|~bool`），因此不能直接附加任意业务对象、切片或附件字节；引擎也不会自动把输入输出值写成属性。

**类型约束不提供内容过滤。** 字符串属性仍能携带 prompt、密钥或思维链；`Err` 也由调用方传入。宿主须先做内容筛选和脱敏，适配层不得把敏感原文或上游原始错误体直接写入，应只记录允许公开的标量与已分类摘要。

---

# 附 · 冻结面与不提供的东西

**冻结契约**（破坏要 minor 版本 + release notes）：

- 槽位三态与「跳过是到达」语义；
- `Aspect` 的 `func(rc, next)` 形态与推进闸约束（**并发进入拒**；顺序重入只在**新的尝试边界**上放行，见 §4）；
- 哨兵错误的判据：`ErrUndeclared` / `ErrConflict` / `ErrGraphStarted` /
  `ErrGraphNotStarted` / `ErrDuplicateSource` / `ErrSkipped` / `ErrNextCalledTwice`
  （同一份清单也在 `AGENTS.md` 的 Freeze contract 一节）；
- `Observer` 的回调次数契约（图级两条 + 节点三条）与「panic 不升格」——阶段 2 换掉了回调的**载荷**（`Site` 取代裸的图 id，破坏性签名变更，随 minor 版本走），**次数**契约不变；
- 六条编码原语与 `LineRenderer` 的字节级同形承诺。

**刻意不提供**：

| 不提供 | 理由 |
|---|---|
| 同一份执行重跑 / 重置 | 见 §2——那是存储语义，归调用方（要再跑一遍就再建一份执行） |
| 熔断 / 错误吞没切面 | 降级是节点显式写出的数据，不是切面偷偷吞掉的错误 |
| 图内 IO（读文件 / 读 env / 网络） | 引擎不做 IO；`SeedPlan` 把取值交给宿主 |
| 具名观测字段的扩展 | 见 §8——业务维度走 `Attrs` |
| `map[string]any` 请求逃生舱 | 类型系统是隐私边界的第一道防线 |
| 节点自动命名 | 节点 ID 是观测归因键，自动名会在重构后漂移 |
