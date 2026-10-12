# 编排

编排的全部内容就是：**声明读什么、写什么，然后让数据到达去驱动执行**。这一页讲拓扑怎么形成，以及并发、超时、重试、限流、取消各自的语义。

## 拓扑从哪来

节点之间没有边对象，也没有拓扑排序这一步。依赖关系在装配期由 Key 的**生产与消费**确定：

```
        ┌── zh ──┐
topic ──┤        ├── join
        └── en ──┘
```

- 一个 Key 允许**一个来源**：声明过的进口（`DeclareInput`；这一轮的值由 `Seed` / `SkipSeed` 给），或恰好一个节点的 `Provides`——两者**互斥**：给一条已经有节点产出的键声明进口（或反过来，让节点去产出已经声明成进口的键）在声明那一刻就撞来源冲突，不会留下一个喂不进去的「死进口」；
- 同一个 Key 在两个节点里 `Provides` → `Add` 直接报 `ErrDuplicateSource`；
- 一个节点同时 `Requires` 和 `Provides` 同一个 Key → `Add` 报错（自环不是合法的数据流）；
- 反过来，**每个 `Requires` 都必须有来源**：没有来源的槽永远不会被写入，那张图不可能跑完——`Start()`（`Run` 内部走的就是它）会当场拒掉并指出是哪个节点的哪个 Key（留到运行时就是挂死，没法排查）。
- **依赖关系也不能成环**：有来源不等于能满足。环里每条 `Requires` 都有生产者，但没有任何节点能先进入 `Run`（门要等全部输入到达），所有槽永远停在 `pending`——所以 `Start()` 同样当场拒掉，报出一条具体的环：`pulse: dependency cycle: A -> B -> A (A requires "y", B requires "x")`。声明过的入口由这一轮的 `Seed` / `SkipSeed` 在启动前备齐，不算边。

这两条判据也有一个**只读入口** `(*Graph).Validate()`：跑的是同一套检查，但**不建 goroutine、不产生执行**，所以问完照样能 `Run` / `NewExecution`（输入是**某一次运行**的值，不属于定义）。**通过即封口**——那次之后 `Add` / `DeclareInput` / `DeclareOutput` / 挂子图都被拒（`ErrGraphStarted`，封口说的是「这张**定义**不再变化」）；封的是**整棵子树**：挂上去的子图是这张定义的一部分，所以它也一起封（同一张子图照样能挂到别处——挂载不改它）。**一次运行的 `Start()` 成功同样封整棵树**：少了这一步，持有子图指针的调用方能在父图已经跑起来、子图还没轮到之间接着改它——跑起来之后定义还在变，观测与拓扑看到的那张图与真正在跑的就成了两张。**校验失败一层都不封**，补上缺的那块再校验正是要支持的用法。它是给**装配期**用的——[YAML](/guide/assembly) 那种声明式装配在 `Load` 期就把子图装上，`Validate` 沿静态挂载边**递归**、按引用点校验并点明**挂载路径**，拿到的还是 `Start` 之后会说的那句话。

## 源节点与 Seed

图里的输入只有两种来路：

```go
// 1) 宿主注入：先把键声明成入口，再把这一轮的值写进 Inputs
_ = g.DeclareInput(pulse.NewPort(Topic))
var in pulse.Inputs
_ = pulse.Seed(&in, Topic, "slot contract")

// 2) 源节点：没有 Requires，提交后立刻执行
_ = g.Add(pulse.NewNode("zh", "注入词表", nil, pulse.Provides(Words), func(rc *pulse.RunCtx) error {
	return pulse.Set(rc, Words, []string{"数据", "到达", "即", "调度"})
}))
```

`Inputs` 里同一条键只许写一次（值或跳过）——第二次写会被当场拒：它最终撞上的正是槽位「幂等首写」，到运行期只会被静默丢掉。值在 `NewExecution` 那一刻拷成内部只读表，此后再改 `Inputs` 与已经创建的执行无关；要换一组值就是再来一次运行。

## fan-out / fan-in

fan-out 就是「多个节点写不同的 Key」，fan-in 就是「一个节点 `Requires` 多个 Key」——**不需要额外原语**。多类型输入用 `Deps` 拼一条声明（`Requires[T]` 一次只收同一类型的 Key）：

```go
_ = g.Add(pulse.NewNode("join", "把词与标签拼起来",
	pulse.Deps(pulse.Requires(Words), pulse.Requires(Label)),
	pulse.Provides(Joined),
	func(rc *pulse.RunCtx) error {
		words, err := pulse.Get(rc, Words)
		if err != nil {
			return err
		}
		label, err := pulse.Get(rc, Label)
		if err != nil {
			return err
		}
		return pulse.Set(rc, Joined, fmt.Sprintf("%s:%d", label, len(words)))
	}))
```

`Requires` 是 AND：**全部输入到达**（就绪或跳过）才算这一关过了；过了之后是**到几个收几个**——只要有一条真的到了值就进入 `Run`，一条值都没到才不执行（见[核心概念](/guide/concepts)）。跑通时 `g.Run(ctx, in)` 返回 `nil`。

## 语法糖：FanOut / Join

上面那种手写装图要三次对齐同一个名字（`NewNode` 里的声明、`Get` 读、`Set` 写）。糖把名字收进**函数签名**：

```go
// fan-in：N 条同类型输入 → 一束
err = pulse.Join(g, "collect", pulse.Keys(a, b, c), out,
	func(rc *pulse.RunCtx, m pulse.Batch[string]) (Report, error) {
		headline, err := m.Get(a) // 单路严格：缺 a 就让本节点跳过
		if err != nil {
			return Report{}, err
		}
		return report(headline, m.Values(), m.Missing()), nil // Values()：到几个收几个
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

叫 `FanOut` 而不是 `Spread`，是因为它**不切分数据**：N 个实例看到的是同一条输入，「各做一份」靠 `shard` 自己挑。

糖只是糖——它产出的图与手写 `NewNode` 的图**观测记录逐字段一致**（`observe` 侧有等价锚用例钉着：同一拓扑两种装法，事件名 / 节点归因 / 终态 / `Err` / 信封全对上）。

| 你写的 | 展开成 | 语义 |
|---|---|---|
| `Join(..., Keys(a,b,c), out, fn)` | 一个节点：`Requires(a,b,c)` + `Provides(out)` | 门是「到几个收几个」；全跳过时 fn 不执行，节点自己跳过 |
| `FanOut(..., in, Keys(r1,r2), fn)` | 两个节点（`worker-1` / `worker-2`），各自 `Requires(in)` | 各自一个 goroutine；实例个数在装配期固定；**整批一次提交**，装不完就整个失败、图上不留半个 fan-out |
| `pulse.NoValue()` | 一条 `*SkipError` | 「这一次没有值」：**整个节点**跳过，它的输出槽随之跳过；下游 `Join` 在 `Batch.Missing()` 里看得见 |
| `m.WaitAll()` | 直接 `return` 它 | 显式的严格 fan-in：缺一条就以跳过收尾（不是失败） |

两个回调的第一个参数都是**本节点的 `*RunCtx`**：要调 HTTP / 数据库的实例靠 `rc.Context()` 感知取消（引擎取消整图时会叫醒它），归因靠 `rc.NodeID()`。

`pulse.NoValue()` 与 `Skip(rc, key)` 不是一回事：后者只把那一条输出槽标成跳过，节点自己照常 `completed`；前者是**节点级**终态声明，一返回整个节点就以 `skipped` 结束。

**缺项与来源都在类型上可见**：`pulse.Batch[T]` 是一张清单——`Items []BatchItem[T]`（`Key` + `Value` + `Present`，按声明顺序排），**每一条声明都在**，缺项也占一行；`Len()` / `Values()` / `Missing()` / `Get(k)` 是这张清单上的四种读法。只给一束值，宿主就分不清「这一路没值」「这一路本来就不在」「这个值出自哪条槽」。

`Get(k)` 收的是 **Key 对象**而不是名字（字符串写错一个字母只会静默变成零值），三种结果分得很开：带值到达给值；那一路以跳过到达回 `*SkipError`（`return` 出去本节点就跳过）；传了一条不在这张清单里的 Key 回 `ErrUndeclared`——「写错了」不会混进「这一路没值」。

**编译期锁住什么**：元素类型（`Keys(...)` 与 `Batch[T]` 必须同一个 `T`）与个数（`Join` 收一束、`FanOut` 按输出槽开实例）。**锁不住同类型多槽位的顺序**——`Keys(a, b)` 与 `Keys(b, a)`（都是 `Key[string]`）**都编译**。实测：按位置读会错位（`Values()[0]` 从 `"from-a"` 变成 `"from-b"`），按 `Get(a)` 取两次都是 `"from-a"`——这就是 `Batch` 带来源名要买的东西，写反顺序只是声明顺序变了。

## 图即节点：把一张图当一步

一张图可以当**一个节点**嵌进更大的图——流程按层组合，而不是铺成一张越来越大的平图。**唯一的公共入口是 `pulse.Sub`**：装配期把一张**现成的**子图挂成父图上的一个节点，于是父图在**运行之前**就看得见整棵子树。

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

**`In` / `Out` 是箭头读法**（来源在前、去向在后）：`In(父, 子)` 进父侧节点的 `Requires`、`Out(子, 父)` 进它的 `Provides`——边界写在接线处，两端同一个 `T` 编译期就锁住。**端口是唯一的接线面**：`In(父, 子)` 的子侧端必须是 `DeclareInput` 声明过的**入口**，`Out(子, 父)` 的子侧端必须是 `DeclareOutput` 声明过的**出口**；接到子图**内部的中间 Key**（某个节点 `Provides` 但没声明成出口的）是**静态错**——想让父图用那条 Key，就显式声明成出口（意图明确）；不声明等于「这是我的实现细节，不承诺」。声明过的入口在静态校验里**不再算「无来源」**：`Validate` 因此分得清「你忘了接线」与「这本来就是外部输入」，静态拓扑里它们的来源是 `pulse.SourceInput`（`SourceSeed` 已随阶段 2 删除——外部值属于**一次运行**的 `Inputs`，定义层「外部来源」只有这一种写法）。**出口对称地多一条判据**：`DeclareOutput` 了一条**没人产出**的键（既没节点 `Provides`、也不是同名入口透传）是**静态错**——装配期就报，不必等子图跑完才由 `bridgeOutputs` 说一句 `never arrived`。端口是**静态**的：只参与拓扑与校验，不携带任何一次运行的值。

**接线的形状判据只有一份**（`Sub` / `Validate` / `Topology().Problems` 共用）：① 同一条子侧**进口**只许绑一次——第二次那笔会被幂等首写**静默吃掉**；② 进口在子图里**已经有来源**时直接判死：子图内部自己产出的键**不是能绑的端口**（报文点明「它不是子图的输入端口」）——读一遍声明就能定的事，别留到这一步真跑起来。而「一条键既是声明过的进口、又有节点产出它」这个状态**根本建不出来**：`DeclareInput` 与 `Add` 两个方向都在声明那一刻撞 `ErrDuplicateSource`（一个键恰好一个来源），所以声明过的进口永远是能用的进口。出口**不去重**：同一条出口推给两个父侧键谁也不丢（推给同一个父侧键早在 `Add` 那层就被拒），所以 **`In(父, k)` + `Out(k, 父2)` 打在同一条端口上是合法的**——那就是透传。

**装配完即可见、不必运行**：`Topology()` 沿静态挂载边**递归**（`Nodes[i].Sub` 是一棵完整子树，`Binds` 是这一处的接线），路径由**挂载位置**推导（`outer` / `outer/inner`），**读它不会改动任何定义**；`Validate()` 同样递归——子图的静态毛病在**父图**上就报，**按引用点**各报一条并点明挂载位置：

```text
pulse: subgraph "inner" (mounted at "step1/toInner"): pulse: node "leaf" requires "c.nope" but nothing provides it (declare it with DeclareInput if the value comes from outside)
```

**组合环在装配期拒**：`Sub` 挂载那一刻沿子树查「父图是不是已经在它的子树里」（自挂也算），是就报 `pulse: subgraph cycle: A -> B -> A`。判据只看**当前这条递归路径上的祖先集合**——同一张子图挂在**不同位置**是合法复用，不能用全局 visited 误拒（那是复用，不是环）。

**封口规则**：`Validate()` **通过才封口，而且封的是整棵子树**——此后 `Add` / `DeclareInput` / `DeclareOutput` / 挂子图都被拒（`ErrGraphStarted`），挂着的子图同样（它是这张定义的一部分）；**`Start()` 成功同样封整棵树**（跑起来之后定义还在变，观察与拓扑看到的就不是真正跑的那张图）；**校验失败一层都不封**，补上缺的那块再校验正是要支持的用法。没有 `Seal()`：它与「成功即封口」职责重叠。封口只挡「改定义」——它不挡运行：同一张定义照样 `Run` / `NewExecution` 很多次（输入是每一次运行的值，不属于定义），而且**已经封口的子图照样能被别处挂载**（挂载不改它的定义）。

**这三件事不用你再记**（手工搭的时候，漏了都不会报错）：

| 手工搭要记得 | 漏了的后果 | `Sub` 一次调用收进去的做法 |
|---|---|---|
| 把观察者挂到子图 | 父图照常跑完、`Run()` 返回 `nil`，子图的记录**一条都不出现** | 子图没挂观察者时**自动继承**父图的（挂了自己的就用它自己的） |
| 子图执行挂到父节点这一层 | 手工搭的话子图在另一份取消域里跑，父图取消它看不见——实测父图耗时 300ms 且报**成功** | `Sub` 把子执行的 ctx 从父节点这一层派生，取消天然传下去 |
| 手写两端桥接 | 子 → 父只能让子图的节点把结果写进**闭包变量**（`Graph` 没有公开读槽 API），谁绑谁全靠人记 | `In` / `Out` 声明一次，跑完自动桥回 |

**运行语义与手写嵌套一样**：子图成功 → 本节点成功（输出就绪的 `Set`、跳过的 `Skip`）；**全部输出都跳过** → 本节点以 `skipped` 收尾（不是失败）；子图失败 → 本节点失败、**首错原样冒泡**（`errors.Is` 成立）；取消 → 子图看得见。**挂上去之后有两处边界**：

1. **子图是定义，可以挂多处**：同一张子图挂两处时两处各跑一份——每个挂载位置一份**子执行**（自己的 ctx / 槽位 / 状态 / 首错），各自的输入输出互不干扰；这张定义也可以顺序 / 并发跑很多次。挂子图那一步里的桥回在第一次 attempt 里就做完了（`Retry` 因此进不了这一步，见下）。
2. **取消域**：子执行的 ctx 从**父节点这一层**派生（含这一步上的 `Timeout` 切面），取消天然传下去。不派生的话子图会跑满整段并把取消吞掉——旧版本实测过：父图耗时 300ms 且报**成功**；派生之后是 51ms + `context canceled`。

`aspects` 落在**父侧那个节点**上——`pulse.Timeout(30*time.Second)` 就是给整张子图限时。父图的**图级**切面（`WithAspects`）还会**继承进子图**（前置在子图节点自己的切面外面，叠加；不想要就 `WithoutAspects()`）——但别靠它给子树限时，见下面的[切面顺序](#切面顺序)。

**名额整棵树共享一份**：`WithMaxRunning(n)` = 这棵树里同时最多 n 个节点在 `Run` 里干活——子图没自己声明就继承父图那份（同一份计数），而挂子图那一步（`Sub`）自己不吃名额（它整段都在等子图跑完）。子图自己声明 `WithMaxRunning(m)` 是**这一支退出父表、自己另算**（那一层及其后代换成它自己那份，`m<=0` 表示这一子树不限）——**不是「在父表之上再卡一道更紧的上限」**：父 `n=1`、子 `m=2` 时，这一支内部仍能同时跑 2 个。声明式装配用 `LoadOptions.MaxRunning` 给整棵树定额度。

**另外两处要记住**：① 观察者自动继承，而**挂载位置（`pulse.path`）现在随每条回调由引擎发出来**（`Site.Path`）——继承下去的出口直接读它，每层的记录各带自己的层级，不必再按层各建一个出口（旧版本靠出口实例上的 `ObserveConfig.Path`，那份写法退居兜底——见[观测](/guide/observability)的「嵌套层级」）。② 节点 id 里不能有 `/`——它是 `path` 的层级分隔符，挂子图那一步写 `a/b` 与「`a` 里嵌 `b`」会拼出**同一条** `path`，所以挂子图那一步在装配期直接拒（普通节点 id 含 `/` 没关系——它不进 path）。子图的图 id 由**建它的那一步**定（`pulse.New` 的 graphID）——沿用 `<父图 id>/<节点 id>` 这套写法读起来最顺（声明式装配里由 `Load` 按挂载路径生成：`<根 id>/<挂载路径>`）。

## 流式：Produce / Consume / Tee

`Key[<-chan T]` 早就能用，缺的不是「再包一层 channel」，而是每个生产 / 消费节点都要手写的六件事（创建与发布、关闭责任、循环、取消、错误回传、背压与名额）。最典型的一处写错是 **`Set` 出 channel 就 `return`**：节点已经 `completed`，真正的发送留在没人观测的后台 goroutine 里——错误回不到图上、取消也叫不醒它。

```go
// 最简单的形状是 Produce + Consume 一对：一个出口接一个消费端
err := pulse.Produce(g, "src", stream, func(rc *pulse.RunCtx, send func(int) error) error {
	for _, v := range values {
		if err := send(v); err != nil { // 取消能从堵住的发送里出来
			return err
		}
	}
	return nil
})
// 要 N 个下游就在中间加一个 Tee：它把一根流复制成 N 条出口，每条出口只喂一个下游
err = pulse.Tee(g, "fan", stream, pulse.Keys(sA, sB)) // 广播：两个下游各拿完整数据
err = pulse.Consume(g, "sinkA", sA, func(rc *pulse.RunCtx, v int) error {
	return handle(v)
})
err = pulse.Consume(g, "sinkB", sB, func(rc *pulse.RunCtx, v int) error {
	return archive(v)
})
```

- **生产端活着发完**：channel 的关闭只在节点返回时的 `defer` 里（成功 / 出错 / 取消 / panic 都走它，不会双次 close），`send` 在发送与 `rc.Context().Done()` 上 `select`。
- **消费端用 `select`，不是 `for range`**：后者在取消之后会把缓冲区里的值**继续算完**，而且整轮还报成功；`select` 在取消那一刻退出并返回取消原因。
- **空流不是跳过**：一次都没发、出口正常关闭 → 下游照样进入 `Run`（零次回调、`completed`、整轮成功）。
- **名额**：流的两端必须同时活着——装配那一刻糖只能做一次快速检查，`Start()` 还会按**整张流图**复核：流节点与**每条流出口的读取者**（消费者也可以手写）要同时有名额，否则直接拒（不留到运行时卡死）。所以「小流量 + 大缓冲」这种凑合跑的配置会被拒——它的成败取决于数据量，不该被依赖。另外**每条流出口都得有人消费、且只喂一个下游**：漏挂下游同样在 `Start()` 被拒（没人读的出口会让发送端永久堵在那里）；第二个读取者也会被拒——两个各写各的读者会在**静默**里瓜分值，要广播就每条下游开一条 `Tee` 出口，要「抢」就用一个 `FanOut`。
- **`FanOut` 与 `Tee` 别混**：抢（每个数据只做一次）用 `FanOut`；每个下游都要拿到全部数据用 `Tee`。普通值不需要 `Tee`——一个 `Provides` 加 N 个 `Requires` 本来就是广播，只有 channel 会被瓜分。

## 分支：对未选中的路调 Skip

没有 `if` 原语，分支就是「把没走的那些 `Provide` 标成跳过」：

```go
if lang == "zh" {
	return pulse.Skip(rc, Translated) // 下游只依赖这一条 → 一条值都没到 → 不执行
}
return pulse.Set(rc, Translated, translate(summary))
```

这里是**单槽**可选输出：两个分支都对同一个槽表态（写或跳过），所以只有一条下游。多槽分支要**两边都表态**——选中的 `Set`、没选中的 `Skip`；只 Skip 一边，漏写的那些会被自动跳过，两条下游都不跑（见[核心概念](/guide/concepts)的分支例子）。

`pulse.Only` 把这一句说完：`return pulse.Only(rc, OutA, v)` = 写 A，本节点其余出口自动作废。N 条出口从「1 次 `Set` + N−1 次 `Skip`」变成 1 行，而且**调用方不再自己写 `Skip`**——上面那句「漏表态」的错在形态上就不存在了。它**不判断条件**：走哪条还是你 `if` 出来的。反面也值得知道：重复表态会变吵，先 `Set` 过别的出口再 `Only` 会直接 `ErrConflict`（手写 `Set` 两次是静默忽略的）。

区分两个终态很重要：**写出跳过的节点自己是 `completed`**；因**一条值都没到**而没执行的**下游**才是 `skipped`。细节与真实记录见[核心概念 · 槽位三态](/guide/concepts)。

## 超时

```go
pulse.NewNode("wait-forever", "等到超时的那一个", requires, provides, run, pulse.Timeout(50*time.Millisecond))
```

切面包住「**等输入 + 执行**」整段，所以超时能打断一个**还在等数据**的节点，而不只是打断执行。这里的形状是「有生产方、但比超时慢」——上游迟早会写，只是还没写。反过来，「永远等不到」的图是造不出来的：没人会写的 Key 在 `Start()` 就被拒了。

```text
timeout err = pulse: node wait-forever timeout after 50ms
```

超时是**失败**，走失败路径：取消整图，`Run` 返回上面这条错误。

超时是**协作式**的：到期先取消本层 ctx，**再等节点体返回**——`Run` 不会在节点体还在跑的时候就返回（切面与父层共享写入记录，提前返回会让收尾和还在跑的执行并发碰同一批槽）。节点体不看 ctx（裸 `time.Sleep`、阻塞 IO）时 `Timeout` 只能等它结束，别当硬性看门狗用。另外**已发布的槽不撤回**：节点体在超时前 `Set` 过的输出，下游可能已经据此跑起来了——超时是失败，不是回滚。

## 重试

```go
pulse.NewNode("flaky", "偶尔失败：演示重试", nil, pulse.Provides(Topic), run, pulse.Retry(3, 10*time.Millisecond))
```

`Retry` 只对**执行错误**重试。跳过**不**重试——跳过是到达，不是失败；等待阶段的取消也不重试。实测第三次成功：

```text
retry   attempts = 3 err = <nil>
```

**重试安全的前提**：失败前**没有写过**任何 Provide，也没有不可重入的副作用。槽位是「到达即发布、幂等首写」的：前一次 attempt 一旦 `Set`/`Skip` 过，下游就已经被唤醒，后续 attempt 的写会被**静默忽略**，槽位也不会回滚——回滚等于重开槽位，与「一次运行一个世界」冲突。需要事务性重试就把输出挪到确定成功的那次：**先算完，再 `Set`**。

**不能装在「挂载子图」那一步上**（`pulse.Sub` 装出来的节点）：重试要重入这一步的体，而桥回父侧是在第一次 attempt 里做完的——父侧槽位又是幂等首写，凡第一次已经桥过去的键，第二次 attempt 的写会被静默丢掉（父图手里成了「一半第一次、一半第二次」的混装）。装配期认不出一个切面是不是 `Retry`（`Aspect` 是裸函数类型，闭包没有可查的身份），所以判据放在 `Retry` **进 `Run` 的第一件事**：当场返回一句点明改法的错，那时这一步还没读父侧输入、也没写任何东西。不这么做的话，那句错还会盖掉第一次失败的真正原因（`Retry` 返回的是**最后一次** attempt 的错误）。[声明式装配](/guide/assembly)那边有 spec 可读，`graph:` 节点上的 `retry:` 在 `Load` 期就拒同一个形状。

## 首错即取消

任一节点返回非跳过错误 → 记录**首错** + 取消整图（含还在等数据的节点）：

```text
run err = boom
pulse.graph_started            node=-        status=running   err=<nil>
pulse.node_wait_finished       node=boom     status=running   err=<nil>
pulse.node_run_finished        node=boom     status=failed    err=boom
pulse.node_wait_finished       node=waiter   status=canceled  err=context canceled
pulse.graph_finished           node=-        status=failed    err=boom
```

三件事同时成立：`Run` 返回**原错误**（没被改写成跳过）；失败的节点是 `failed`；被杀掉的等待者是 `canceled`，且**只有等待段**那一条记录。失败轮也不缺头尾——运行级两条照发，`pulse.graph_finished` 的 `Status` 是 `failed`、`Err` 就是首错。

失败路径还会给该节点**没写过的 Provide** 补一条跳过——那只是把还在等的下游解开，不是下游的终态。终态由**「取消优先」**决定：ctx 已取消时等待一律返回 `ctx.Err()`，「到达与取消同时就绪」也以取消为准。所以「因首错而没跑」的下游稳定报 `canceled`，不会随调度在 `skipped` / `canceled` 之间抖。

同一个 `canceled` 还覆盖另外两种「这一轮被从外面拆了」：父 ctx 被取消**或截止时间到期**、以及在排队等名额期间被取消。反过来，节点**自己的** `Timeout` 到期算它的 `failed`——那是这个节点没在时限内完成。四个终态的判据写在 `pulse.NodeFinishReason` 的 godoc 里。

## 限流

```go
g, _ := pulse.New("demo", "演示图", pulse.WithMaxRunning(4))
```

`WithMaxRunning(n)` 限制**同时在 `Run` 里干活**的节点数（挂子图那一步不算：`Sub` 整段都在等子图），而且这份额度是**整棵图树共享的**：子图没自己声明就继承父图那份——**在没有人自己声明过**时，根图声明 4 = 整棵树同时最多 4 个节点在干活。父图的节点与子图的节点争的是**同一份**额度、互不让路：额度够就并发，不够才排队；挂子图那几步在等的时候，观察者眼里 `running` 的行数会比 n 多，那不是并发度超了。子图自己声明 `WithMaxRunning(m)` 是**这一支退出父表、自己另算**（`m<=0` = 这一子树不限，同样是退出）——**不是「在父表之上再卡一道更紧的上限」**：父 1 + 子 2 的树里，这一支内部仍能同时跑 2 个。声明式装配用 `LoadOptions.MaxRunning`。**等数据不占名额**——否则限流会退化成死锁（占满名额的节点都在等数据，没人能推进）。

**排队等名额同样会被取消打断**：ctx 取消（或首错触发整图取消）时，还没进入 `Run` 的排队节点直接以 `canceled` 收尾（只有等待段那一条记录），空出来的名额不会交给一个已取消的节点。

## 切面顺序

```
全局切面（pulse.WithAspects） → 节点切面（NewNode 的 aspects…） → 内核（等输入 + 执行）
```

先写的更靠外。所以 `pulse.Timeout(d)` 加在 `pulse.Retry(...)` 前面，得到的是「**总时长**受限，每次尝试各自重试」——超时在外、重试在内。反过来写则是「每次尝试各自限时」。

**跨图**：父图的图级切面会**继承进子图**，拼在子图节点自己的切面**外面**（叠加，不是替换）。所以三层嵌套时，最内层节点身上的链 = 这棵树的图级切面（前置）+ 它自己声明的，**不会一层叠一遍**。子图不想要父图那批就写 `pulse.WithoutAspects()`（自己 `WithAspects` 写的照挂；只写它 = 这一层一个图级切面都不要）。

**图级切面慎用，尽量用节点级切面。** `WithAspects` 一次落在这一层**所有**节点上——包括挂子图那一步（`Sub`，它背后是一整张子图），而且会继续往下叠到子图里的每个节点。`Timeout` 最容易被这里的量级差咬到：一个图级 `Timeout(d)` 同时是「父图普通节点各自的时限」「挂子图那一步（= 整张子图）的时限」「子图里每个节点的时限」——三个**不是一个量级**的东西共用一个 `d`，而**子图天生比普通节点更容易超时**。要限时就写在该限时的那一个节点上：普通节点用 `NewNode(..., pulse.Timeout(...))`，整张子图用 `Sub` 的 `aspects`；图级留给「全树都要一致施加」的那类（追踪 / 指标）。另外**切断只解开「继承到子图节点上」这一段**：父图那一步仍然被父图的图级切面包着——「这棵子树要长跑」不能只靠切断。

两条容易算错的账：① **子图的「总时间」先被挂子图那一步上的 `d` 掐死**，不会变成「每个节点各 `d`、加起来 `N*d`」——继承下来的 `d` 虽然套在子图每个节点上，但它只是更外层那个 `d` 里面的一段；② 名额紧时，**超时可能发生在子图拿到名额之前**——挂子图那一步不吃名额，可它的 `d` 从进入 `Run` 就开始计时，而此刻子图的节点还在排队等额度。

**门闩**：单节点的 `Run` 不得并发进入（违反 → `ErrNextCalledTwice`），但顺序重入合法——`Retry` 正是靠它。

## 接线书写规范：让静默的错写不出来

上面几节讲的是「引擎怎么判」，这一节讲**怎么写字**。五条规矩，每条都对应一次实测到的**静默**错误——不报错、不告警，整轮还返回 `nil`。规矩没有强制手段（不做 lint / vet 提示：那要单独做成一个二进制、让使用方改自己的 CI，而把 `rc` 传进 helper 里读 Key 的情形认不出来），所以它写在这里。

**出口多于一条：整批表态，别只写一半。** 节点有 A / B 两条出口、意图走 A，却只写了 `Skip(B)`、忘了 `Set(A)`：

```go
return pulse.Skip(rc, OutB) // 忘了 Set(rc, OutA, v)
```

实测：**两条下游都不跑**（A 没被写、B 被跳过），整轮仍返回 `nil`。写法是用 `Only` 一次说完——写 A，本节点其余出口全部作废：

```go
return pulse.Only(rc, OutA, v) // 调用方不再自己写 Skip，「漏写」在形态上就不存在
```

要同时写多个值就继续手写 `Set`（`Only` 说的是「只走这一条」）。

**同类型多槽位：按来源 Key 取，不要按位置。** `Keys(a, b)` 与 `Keys(b, a)` 都编译——顺序编译期锁不住。实测：按位置读 `b.Values()[0]`，把声明顺序写反就从 `"from-a"` 变成 `"from-b"`，值长得像的时候根本看不出来；按 Key 取不受影响：

```go
v, err := b.Get(SourceA) // 与声明顺序无关；这一路以跳过到达时回 *SkipError
```

**声明与读取放在一起。** `Requires(X)` 声明的 Key，就在同一段 `Run` 里读。注意**「只等到达、不取值」是合法用法**（实测：声明了不读，节点照样执行）——闸门节点正是靠它成立的；要避免的是**别让人看不出是哪一种**：确实只当闸门，就写一行注释说清楚。

**长任务盯 `rc.Context()`；`Set` 完别就 `return`。** 两条实测：

- **没人看 ctx = 取消被吞成成功**：外部中途取消、节点全程不看 `rc.Context()`，`Run()` 返回 `nil`（没有任何节点看见取消，口径见[核心概念](/guide/concepts)）。
- **活留给后台 goroutine = 错误无处安放**：`Set` 出 channel 就 `return`，真正的发送留在无人观测的 goroutine 里。实测：图报成功（`Run() = nil`），而那个 goroutine 干完活之后**失败了**——图上一点痕迹都没有；忘了 `defer close` 更要命，下游 `for range` 等关闭，`Run()` 永远不返回（外部 ctx 超时也救不了：节点自己不返回，图就不返回）。

写法：长循环里 `select` 一下 `rc.Context().Done()`；要传流就用 `Produce` / `Consume` / `Tee`，关闭责任交给它们。

**一根流出口只喂一个下游。** 同一条流出口挂两个消费者，值会被**静默瓜分**（两个都跑、各自只拿到一部分）。现在 `Start()` 会直接拒（报错点名是哪几个节点），但写法上应该一次写对——**广播**用 `Tee`（一条出口一个下游），**抢**用 `FanOut`（同一组 worker 共读是显式声明）：

```go
err := pulse.Produce(g, "src", stream, sendAll)        // 一条流
err = pulse.Tee(g, "fan", stream, pulse.Keys(sA, sB))  // 复制成两条出口
err = pulse.Consume(g, "sinkA", sA, handle)            // 一条出口一个消费者
err = pulse.Consume(g, "sinkB", sB, archive)
```

消费端**用 `select` 而不是 `for range`**：实测同一场景（生产端也不看 ctx、缓冲 8 发 5 个、15ms 时取消）——`for range` 把 5 个值全算完并且报成功；`select` 在第 4 个值上停下并返回 `context canceled`。要如实知道的一点：「取消时缓冲区里剩下的值算不算」是**尽力而为**——两个分支同时就绪时 `select` 随机挑，这不是硬保证。

## 结果从哪读

`Graph` 没有 `Run` 之后的公开读槽。产物归调用方，两条常规做法：

```go
var report string // 1) 节点闭包写出——终端产物用这个
// ...

if err := g.Run(ctx, in); err != nil { return err } // 2) 失败看返回值
```

`Run(ctx, in)` / `Execution.Err()` 的语义：返回**首错**，**不含单纯的跳过**（全图都跳过是合法结果，`Err()` 为 `nil`）。**取消是协作式的**——引擎只承诺「还在等数据 / 还在等名额」的节点立刻返回；已经在 `Run` 里的节点要不要看 ctx，由它自己决定。所以**取消只有在被某个节点看见、变成它的错误时，才成为运行结果**：节点都没抬头、各自正常返回的那一轮按完成算（`Err()` 仍为 `nil`），哪怕父 ctx 中途已经被取消。

下一步：拓扑也可以不写在 Go 里——见[声明式装图](/guide/assembly)；想看每个节点花了多久等待、多久执行，见[图观测](/guide/observability)。
