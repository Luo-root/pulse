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
- **同一套判据也有只读入口 `(*Graph).Validate()`**（0.5 新增）：跑的就是上面那两条（外加流式名额那条），但**不改图的状态**——不置 `started`、不建 goroutine，所以问完还能接着 `Add` / `Seed` / `Start`。给**装配期**用：声明式装配（`pulse/yaml`）装一张嵌套图时子图是运行到才建的，没有这个入口，「子图里这条键没人提供」与「子图里依赖成环」就只能等它第一次运行才炸（详见 §5 装配）。报文与 `Start` 逐字相同——一份判据、两个入口；已经启动的图直接通过（它在 `Start` 那一刻就过了同一套判据）；
- **首错即取消**：任一节点返回非跳过错误 → 记录首错 + `cancel()` 整图，所有等待者被唤醒；**取消优先于到达**——ctx 已取消时等待一律返回 `ctx.Err()`（「到达与取消同时就绪」也以取消为准），所以因首错而没跑的下游稳定报 `canceled`，不随调度在 `skipped` / `canceled` 之间抖。失败节点未写的 Provide 仍会补一条跳过，那只是解开阻塞，不是下游的终态；
- **`WithMaxRunning(n)`** 限制**一棵图树里**同时进入 `Run` 的节点数（`n<=0` 无限）：名额**整棵树共享一份**——子图没自己声明时继承父图那份，`Sub` 那一步**不吃名额**（它整段都在等子图，占着等于把额度锁在「等」上；共享之后那就是死锁）；子图自己声明 `WithMaxRunning(m)` 就是显式覆盖，那一层及其后代换成它自己那份（`m<=0` = 这一子树不限）。**等数据不占名额**（否则限流会退化成死锁）；**排队等名额也能被取消打断**（排队节点以 `canceled` 收尾，不进入 `Run`；名额空出与取消同时就绪时以取消为准——拿到名额后再复查一次 ctx）；
- **四个终态的判据**（`NodeFinishReason`）：`completed` = Run 正常返回（返回后未写的 Provide 被自动跳过不算失败）；`skipped` = 一条输入值都没到而没进入 `Run`（全部 `Requires` 以跳过到达），或自己 `Skip` 了输出、把 `WaitAll` 的跳过返回了出去；`failed` = 节点真实错误，**含 panic 与 `Timeout` 切面的节点超时**；`canceled` = 这一轮被从外面拆了——首错取消、**父 ctx 被取消或截止时间到期**、排队等名额期间被取消。宿主按 reason 分流，所以「父 ctx 到期」不能报成 `failed`：那会把它显示成一个并不存在的节点缺陷；
- **panic 不穿透**：节点 panic 被转成节点错误，走同一套失败路径；
- **`Err()` 不含单纯的跳过**：全图都跳过是合法结果；`Wait()` 返回后图自己的 ctx 会被取消（它是 `New` 从父 ctx 派生的子 ctx，不释放就会一直挂在长生命周期的父 ctx 上），这次收尾的取消**不算运行结果**；
- **取消是协作式的，只有被看见才算结果**：引擎只承诺「还在等数据 / 还在等名额」的节点立刻返回；已经在 `Run` 里的节点要不要看 ctx 由它自己决定（不看也是合法的，和引擎里其它不看 ctx 的 `Run` 一样）。所以父 ctx 中途被取消、而没有任何节点把它变成错误时，这一轮**按完成算**（`Run()` 返回 `nil`）——`Wait()` 是先置 `done` 再读结果，取消进入运行结果唯一的路径是某个节点 `fail`。站点 `orchestration` / `quickstart` 中英四处按这条口径写（见 #286）。

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

它展开成 `pulse.Sub`（见下面「图即节点」）：父侧那个节点的 `Requires` / `Provides` 由 `in` / `out` 推出来，引擎的静态校验照常生效。**YAML 只是把引擎已有的能力写出来**——引擎没有的能力它变不出来；这一层新增的是**装图期的对账**：静态可判定的问题全部在 `Load` 报，不留到运行期。

| 检查 | 判据 |
|---|---|
| 引用 | `graph:` 指向的名字必须在同一份文档的 `graphs` 里；子图必须有节点 |
| 环 | 子图之间「图引用图」的环（含自引用）→ 报出一条**具体**路径 `a -> b -> a`；递归本身允许、深度不设限 |
| 类型 | 两端各按 `{name, type}` 查表，且用**同一个** type 记号——父侧那条键登记成别的类型就报 |
| 名字 | `in` / `out` 的子侧键必须真被那张图里的某个节点读 / 写，否则报 |
| 形状 | 工厂节点不能写 `in` / `out`、子图节点不能写 `requires` / `provides`——边界只有一处说法；写错那一侧就是**静默不生效**，所以在装图期吵 |
| 来源 | `in:` 喂的那条子侧键**不能又被子图自己的节点提供**——一条键恰好一个来源，跑到就是 `ErrDuplicateSource` |
| seed | 子图的 `seeds` 里，**要种值**的那条只允许 `literal`（`env` / `file` / `context` 要靠宿主 IO，而 `SeedPlan` 是**父图**的产物；`skip: true` 的那条 `from` 没有语义，与顶层 `doc.Seeds` 一个口径）；键名 / 类型按登记表对账；**同一条键在同一张图里声明两次**也报（第二次会被幂等首写静默忽略，谁赢取决于声明顺序） |
| 跑得起来 | 每张装好的图再过一遍引擎的只读校验 `(*Graph).Validate()`——子图里「某条 `Requires` 没人提供、也没人 seed」与「依赖成环」也在 `Load` 报，**报文与 `Start` 那次逐字相同**（一份判据，两个入口） |

这些检查**对每一张 `graphs:` 声明都生效，包括谁也引用不到的那些**，而且按**引用点**各来一遍：装进图的只有根层那批节点，子图要等运行到才装——只查根层的话，第 2 层往后写错的毛病会「装图全过、跑到一半才炸」，没人引用的那张更是永远不炸。

做法是 `Load` 期给每个引用点起一张**只装不跑**的校验图，把同一段装图代码走一遍（`pulse.Sub` / `Add` 在装配期做的检查：节点 id、重复、来源冲突、绑定），再把它那一层拿得到的**来源**种上——spec 自己的 `seeds`，加上父侧 `in:` 喂进来的那几条键——最后请引擎自己判一遍（`Validate`）。**按引用点而不是按 spec**：同一张图被两处引用时两边的 `in:` 可以不一样，按并集校验等于说「另一处喂了 = 这一处也喂了」，那张图要等真的跑到那一层才由引擎拒掉。

**校验图（含根层那张）都建在 `context.Background()` 上，而根图要等所有可能失败的校验都过完才建**：校验图不会跑、也没人会 cancel 它们，从宿主 ctx 派生出来的子 ctx 只会一直挂着（热重载反复 Load = 越攒越多）；而 `Load` 失败时返回 `nil, nil, err`——图不交出去，**调用方连清理的把手都没有**，所以那之前一步都不动宿主的 ctx。根图建出来之后交出去的那张照旧继承宿主 ctx（取消必须从宿主传得进去），宿主跑到 `Wait` 就把它释放。

子图节点**不能**再写 `requires` / `provides`（反过来，工厂节点写了 `in` / `out` 也在装图期报）：边界只有一处说法。切面（`timeout` / `retry`）落在**父侧那个节点**上——`timeout: 30s` 就是给整张子图限时。

**观测按层给**：`LoadOptions.ObserverFor(path)` 按层建出口（一个出口实例只能带一条 `pulse.path`，所以「按层各建一个」由宿主决定）。同一张子图被引用两次就是两个独立实例，靠 graph id 与 `pulse.path` 分得开（见第二部分）。

### 语法糖：`FanOut` / `Join`

手写装图要三次对齐同一个名字（`NewNode` 声明、`Get` 读、`Set` 写），三处都可能漂移。糖把名字收进**函数签名**：

```go
// fan-in：N 条同类型输入 → 一束
err := pulse.Join(g, "collect", pulse.Keys(a, b, c), out,
    func(rc *pulse.RunCtx, m pulse.Batch[string]) (Report, error) {
        headline, err := m.Get(a) // 单路严格：缺 a 就让本节点跳过
        if err != nil {
            return Report{}, err
        }
        return report(headline, m.Values()), nil // Values()：到几个收几个
    })

// fan-out：一个输入 → N 个并行实例，各自一条输出槽（节点名 id-1 … id-N）
err = pulse.FanOut(g, "worker", docs, pulse.Keys(r1, r2, r3),
    func(rc *pulse.RunCtx, shard int, doc string) (Result, error) {
        if nothingFor(shard) {
            return Result{}, pulse.NoValue() // 这一份没有产出：整个实例跳过，不是失败
        }
        return work(shard, doc)
    })
```

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
		g, err := pulse.New(context.Background(), "probe")
		if err != nil {
			panic(err)
		}
		if err := pulse.Seed(g, a, "from-a"); err != nil {
			panic(err)
		}
		if err := pulse.Seed(g, b, "from-b"); err != nil {
			panic(err)
		}
		ins := pulse.Keys(a, b)
		if swapped {
			ins = pulse.Keys(b, a) // 只是换了声明顺序：照样编译
		}
		if err := pulse.Join(g, "collect", ins, out, func(rc *pulse.RunCtx, m pulse.Batch[string]) (string, error) {
			byPos = m.Values()[0] // 按位置读：写反了这里就错
			v, err := m.Get(a)    // 按来源 Key 取：与声明顺序无关
			byKey = fmt.Sprintf("%q, %v", v, err)
			return strings.Join(m.Values(), ","), nil
		}); err != nil {
			panic(err)
		}
		if err := g.Run(); err != nil {
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

### 流式：`Produce` / `Consume` / `Tee`

`Key[<-chan T]` 早就能用，缺的不是「再包一层 channel」，而是每个生产 / 消费节点都要手动协调的六件事（创建与发布、发送端的关闭责任、生产 / 消费循环、取消响应、错误回传、背压与名额）。手写这套形状最典型的一处写错是 **`Set` 出 channel 就 `return`**：节点已经 `completed`，真正的发送留在无人观测的后台 goroutine 里——错误回不到图上、取消也叫不醒它，`defer close` 还经常漏。

```go
// 生产：channel 的创建 / 发布 / 关闭 / 取消都在这次 Run 里，节点活着发完
err := pulse.Produce(g, "src", stream, func(rc *pulse.RunCtx, send func(int) error) error {
    for _, v := range values {
        if err := send(v); err != nil { // 取消能从堵住的发送里出来
            return err
        }
    }
    return nil
})

// 最简单的形状是 Produce + Consume 一对（一个出口接一个消费端）。
// 消费：循环是 select（收到值 / ctx 取消），不是 for range
err = pulse.Consume(g, "sink", stream, func(rc *pulse.RunCtx, v int) error {
    return handle(v)
})

// 广播：一根流复制给 N 个下游，每个都拿到完整、同序的数据。
// 注意 Tee 的输入是**另一条**流：一条出口只喂一个下游，要 N 个下游
// 就得让 Tee 提供 N 条出口，再一条接一个消费端（装配校验见下表最后一行）
err = pulse.Tee(g, "fan", otherStream, pulse.Keys(sA, sB, sC))
err = pulse.Consume(g, "sinkA", sA, func(rc *pulse.RunCtx, v int) error { return handle(v) })
err = pulse.Consume(g, "sinkB", sB, func(rc *pulse.RunCtx, v int) error { return archive(v) })
err = pulse.Consume(g, "sinkC", sC, func(rc *pulse.RunCtx, v int) error { return audit(v) })
```

| 件 | 展开成 | 语义来源 |
|---|---|---|
| `Produce` | 一个节点：`Provides(out)`，`Set` 出 channel 后**活着发完** | 关闭只在返回时的 `defer`（成功 / 出错 / 取消 / panic 都走它，没有双次 close）；`send` 在发送与 `rc.Context().Done()` 上 `select` |
| `Consume` | 一个节点：`Requires(in)`，`select` 循环读 | **不能用 `for range`**：实测它会「取消之后把缓冲区算完」并且整轮报成功（没人看见取消 → `Run()` 返回 `nil`，见 #286） |
| `Tee` | 一个节点：`Requires(in)` + `Provides(outs...)`，逐条转发 | 广播：每个下游拿到完整同序数据；**背压按最慢的下游算** |
| 装配校验 | `checkStreamSlots`（装配期快速失败）+ `Graph.checkStreamLocked`（`Start` 的权威校验） | 「流的两端必须同时活着」是**组合**性质的约束：糖在装配那一刻看不全整张图，所以局部检查只做即时反馈，`Start` 按**整张流图**要名额——流节点 + **每条流出口的读取者**（消费者可以手写，它同样得占一个名额才读得到；实测：`maxRun=2` 下 `Produce → Tee → Consume` 与 `Produce → Tee → 手写 sink` 都会装配全过、运行期死锁），并要求每条流出口都有人 `Requires`、且**只喂一个下游**（没人读的出口会让发送端永久堵住；两个各写各的读取者会在静默里瓜分值——同一个 `FanOut` 组的 worker 共读是显式声明，不算多消费者） |

**为什么「要复制」的只有流这一类**（实测四种分发形态）：普通值——一个 `Provides`、N 个下游各自 `Requires` 同一个 Key——3 个下游都拿到完整值；一组值（slice 当一个值）同理；**只有 channel 会抢**：同一条 channel 给 3 个下游，6 个值被瓜分（合计 6，不是 18）；复制成 3 条 channel 才是 18。所以「抢」用 `FanOut`、「每个都拿到」用 `Tee`，两者不互相替代。

**空流不是跳过**：一次都没 `send`、出口正常关闭 → 下游**照样进入 `Run`**（零次回调、终态 `completed`、整轮成功）。槽位里放的是 channel，走的是「值到了」这条路；想要「没有值」的语义就别 `Set` 那条 channel。

**切面不开**：`Produce` / `Consume` 都先不给切面。`Retry` 撞「已发布的槽」（幂等首写 → 第二次 attempt 直接 `ErrConflict`；消费侧则因为流的位置不可回退，重试只会拿到后半段），`Timeout` 只把流掐断、消费端看到的只是「提前关闭」——两个切面在这里都没有干净语义。

### 排他分支：`Only`

```go
if cond {
    return pulse.Only(rc, OutA, v) // 写 A，本节点其余 Provides 全部作废
}
return pulse.Only(rc, OutB, w)
```

它**不判断条件**——走哪条还是调用方 `if` 出来的。它消灭的是**漏表态**：手写分支要 1 次 `Set` 加 N−1 次 `Skip`，少写一边是静默的——实测「只 `Skip(B)`、忘了 `Set(A)`」时 A、B 两条下游**都不跑**（未写的 `Provide` 被自动跳过），而 `Run()` 仍返回 `nil`、没有任何提示。用 `Only` 的调用方不再自己写 `Skip`，这个错在形态上就不存在了。

反面：重复表态会**变吵**——先 `Set` 过别的出口再 `Only`，会给那条已就绪的槽补一次 `Skip` → `ErrConflict`（而手写 `Set` 两次是被静默忽略的）。所以 `Only` 要**代替** `Set`，需要同时写出多个值的节点继续手写 `Set`。

### 图即节点：`Sub`

一张图可以当**一个节点**嵌进更大的图，于是流程按层组合，而不是铺成一张越来越大的平图。手工搭今天就能跑（父节点里 `pulse.New` + 桥接 + `child.Run()`），缺的是把建图 / 桥接 / 取消域 / 观察者四件事收进一次调用：

```go
err := pulse.Sub(parent, "step1",
    []pulse.SubBind{pulse.In(topic, childIn), pulse.Out(childOut, summary)},
    func(sc *pulse.SubCtx) (*pulse.Graph, error) {
        child, err := pulse.New(sc.Context(), sc.GraphID(), pulse.WithMaxRunning(2))
        // 不声明就继承父图那份名额；这里声明了 = 这一子树换成自己这份
        if err != nil {
            return nil, err
        }
        // …给 child 装节点：读 childIn、写 childOut…
        return child, nil
    })
```

`In` / `Out` 是**箭头读法**：来源在前、去向在后（`In(父, 子)`、`Out(子, 父)`）。父图那个节点的 `Requires` / `Provides` 由它们推出来——**边界写在接线处**，读父图一眼看清这一步吃什么、吐什么；两端同一个 `T` 编译期就锁住（`In(a, b)` 里一个是 `Key[string]`、一个是 `Key[int]` 直接编译不过），父图的静态校验（来源 / 环 / 流式名额）照常生效。

**三处手工搭会静默出错的地方，正是它要消灭的**：

| 手工搭要记得 | 漏了的后果 | `Sub` 的做法 |
|---|---|---|
| 把观察者挂到子图 | 父图照常跑完、`Run()` 返回 `nil`，子图的记录**一条都不出现** | 子图没挂观察者时**自动继承**父图的（子图挂了自己的就用它自己的） |
| 子图 ctx 派生自 `rc.Context()` | 用 `context.Background()` 建子图时父图取消子图看不见，它会照常跑完 | `sc.Context()` 派生自本节点 |
| 手写两端桥接 | 父 → 子是 `Seed`；子 → 父只能让子图的节点把结果写进**闭包变量**（`Graph` 没有公开读槽 API），谁绑谁全靠人记 | `In` / `Out` 声明一次，跑完自动桥回 |

**两处边界要记住**：① 观察者虽然自动继承，但**继承不带层级归因**——出口实例上的 `pulse.path` 是建它那一刻定下的，继承来的那份带的还是**父层**的路径（实测两层嵌套里，最内层那 4 条记录全被记成中间层的 `path`）；要 `pulse.path` 就得在 `build` 里按 `sc.Path()` 给每一层**各建一个出口**。② 节点 id 里不能有 `/`——它是 `path` 的层级分隔符，`Sub` 的 `a/b` 与「`a` 里嵌 `b`」会拼出**同一条** `path`（连图 id 也一样），装配期直接拒。

语义与手写嵌套逐条一致：子图成功 → 本节点成功，输出按槽位桥回（就绪 → `Set`，跳过 → `Skip`）；**全部输出都跳过** → 本节点 `NoValue()`（节点级跳过，不是失败）；子图失败 → 本节点失败、**首错原样冒泡**（`errors.Is` 成立）；父图 / 本节点取消 → 子图看得见（子 ctx 是派生的）。

**名额整棵树共享一份**：`WithMaxRunning(n)` 的语义是「这棵树里同时最多 n 个节点在 `Run` 里干活」。子图没自己声明时**继承**父图那份（同一份计数），而 `Sub` 那一步自己**不吃名额**（它整段都在等子图跑完）——所以「根图声明 1」= 整棵树同时只有 1 个节点在干活（旧行为是各管各的：父 `maxRun=1` + 子 `maxRun=2` 的嵌套实测峰值 **3**，那是本轮改掉的坑）。子图自己声明 `WithMaxRunning(m)` 是**显式覆盖**：那一层及其后代换成它自己那份，`m<=0` 表示这一子树**不限**。声明式装配用 `LoadOptions.MaxRunning` 给整棵树定额度。

**子图是一次性的**：`build` 每次运行都要造一张新图。把 `pulse.New` 写在闭包外面复用，报出来的是一句能照着改的话（`ErrGraphStarted` 仍在错误链里）：

```
pulse: Sub "b": the child graph was already started:
a graph runs once, so build must return a new one on every run (pulse: graph already started)
```

`aspects` 落在**父侧那个节点**上：`pulse.Timeout(30*time.Second)` 就是给整张子图限时（切面覆盖「等输入 + 执行」整段）。

观察者那条链自动接上——父图挂了 observer 时 `build` 里什么都不用写，同一个出口就看得见嵌套的全过程，两图靠 `pulse.graph` 分得开（实测 10 条，父图的两条把子图的两条夹在中间）：

```
start:P
wait:P/step1 → run:P/step1
    start:P/step1
    wait:P/step1/inner → run:P/step1/inner → done:P/step1/inner=completed
    finish:P/step1=completed
done:P/step1=completed
finish:P=completed
```

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

图级不超过两条（`GraphStarted ≤ 1`、`GraphFinished ≤ 1`），节点不超过三次回调（`Waiting ≤ 1`、`Running ≤ 1`、`Finished = 1`）；`Retry` 的多次 attempt **不重复打点**。时序是**夹住**：`GraphStarted` 在提交任何节点 goroutine 之前发出（启动校验失败的图没有启动，不发），`GraphFinished` 在全部节点终止之后、`Wait` 返回之前发出——只 `Start` 不 `Wait` 的宿主拿不到它；并发 / 重复 `Wait` 都在这一发返回之后才返回。图级终态只会是 `completed` / `failed` / `canceled`（跳过是节点级的事实，升不到这一层）。图默认 no-op，`WithObserver` 挂载，需要多个时用 `MultiObserver` 组合。

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

嵌套时再加第三个维度 `pulse.path`：不透明字符串、`/` 连接各层，**段是子图在它父图里的那个节点 id**（不是图 id）。两者分工是硬的——`pulse.graph` 是**你起的图 id**，`pulse.path` 是**引擎记的层级**：同一张子图模板跑两遍就是两个图实例、两条 path，靠它分得开；反过来，id 里带不带斜杠都影响不到 path。段是节点 id 还有一条硬理由：**子图的记录正好挂在父图那个节点的记录下面**——父图 `step1` 那条记录没有 path、节点 id 就是 `step1`，子图记录的 path 也是 `step1`，拼树时不用去解析任何图 id。规则三条：

- **引擎给值**：`SubCtx.Path()`（根图 `""`；一层子图 `"step1"`；两层 `"outer/inner"`），出口把它填进 `ObserveConfig.Path`；
- **出口写记录**：非空时**每一条**记录（含运行级两条）都带 `pulse.AttrPath`；**为空不写**这个 key——空串会让「根」与「忘了传」长得一样；
- **层级自己拆**：引擎不提供结构化树、不定义「第几段」的语义，按 `/` 分即可。

它落在**出口实例**上，所以嵌套就是按层各建一个出口（`Sub` 的 `build` 里正合适——那里本来就要建子图）；两层套两层就是「谁建谁写」（见 §5 的图即节点）。

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
