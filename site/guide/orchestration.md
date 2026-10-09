# 编排

编排的全部内容就是：**声明读什么、写什么，然后让数据到达去驱动执行**。这一页讲拓扑怎么形成，以及并发、超时、重试、限流、取消各自的语义。

## 拓扑从哪来

节点之间没有边对象，也没有拓扑排序这一步。依赖关系在装配期由 Key 的**生产与消费**确定：

```
        ┌── zh ──┐
topic ──┤        ├── join
        └── en ──┘
```

- 一个 Key 允许**一个来源**：`Seed`/`SkipSeed`，或恰好一个节点的 `Provides`；
- 同一个 Key 在两个节点里 `Provides` → `Add` 直接报 `ErrDuplicateSource`；
- 一个节点同时 `Requires` 和 `Provides` 同一个 Key → `Add` 报错（自环不是合法的数据流）；
- 反过来，**每个 `Requires` 都必须有来源**：没有来源的槽永远不会被写入，那张图不可能跑完——`Start()` 会当场拒掉并指出是哪个节点的哪个 Key（留到运行时就是挂死，没法排查）。
- **依赖关系也不能成环**：有来源不等于能满足。环里每条 `Requires` 都有生产者，但没有任何节点能先进入 `Run`（门要等全部输入到达），所有槽永远停在 `pending`——所以 `Start()` 同样当场拒掉，报出一条具体的环：`pulse: dependency cycle: A -> B -> A (A requires "y", B requires "x")`。`Seed`/`SkipSeed` 的 Key 在启动前就到齐了，不算边。

## 源节点与 Seed

图里的输入只有两种来路：

```go
// 1) 宿主注入：运行前写死初值
_ = pulse.Seed(g, Topic, "slot contract")

// 2) 源节点：没有 Requires，提交后立刻执行
_ = g.Add(pulse.NewNode("zh", nil, pulse.Provides(Words), func(rc *pulse.RunCtx) error {
	return pulse.Set(rc, Words, []string{"数据", "到达", "即", "调度"})
}))
```

`Seed` 是**幂等首写**：重复 `Seed` 同一个 Key 忽略，重复 `Seed` 与 `SkipSeed` 冲突报 `ErrConflict`。图启动后 `Seed` 被拒（`ErrGraphStarted`）。

## fan-out / fan-in

fan-out 就是「多个节点写不同的 Key」，fan-in 就是「一个节点 `Requires` 多个 Key」——**不需要额外原语**。多类型输入用 `Deps` 拼一条声明（`Requires[T]` 一次只收同一类型的 Key）：

```go
_ = g.Add(pulse.NewNode("join",
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

`Requires` 是 AND：**全部输入到达**（就绪或跳过）才算这一关过了；过了之后是**到几个收几个**——只要有一条真的到了值就进入 `Run`，一条值都没到才不执行（见[核心概念](/guide/concepts)）。跑通时 `g.Err()` 是 `nil`。

## 分支：对未选中的路调 Skip

没有 `if` 原语，分支就是「把没走的那些 `Provide` 标成跳过」：

```go
if lang == "zh" {
	return pulse.Skip(rc, Translated) // 下游只依赖这一条 → 一条值都没到 → 不执行
}
return pulse.Set(rc, Translated, translate(summary))
```

这里是**单槽**可选输出：两个分支都对同一个槽表态（写或跳过），所以只有一条下游。多槽分支要**两边都表态**——选中的 `Set`、没选中的 `Skip`；只 Skip 一边，漏写的那些会被自动跳过，两条下游都不跑（见[核心概念](/guide/concepts)的分支例子）。

区分两个终态很重要：**写出跳过的节点自己是 `completed`**；因**一条值都没到**而没执行的**下游**才是 `skipped`。细节与真实记录见[核心概念 · 槽位三态](/guide/concepts)。

## 超时

```go
pulse.NewNode("wait-forever", requires, provides, run, pulse.Timeout(50*time.Millisecond))
```

切面包住「**等输入 + 执行**」整段，所以超时能打断一个**还在等数据**的节点，而不只是打断执行。这里的形状是「有生产方、但比超时慢」——上游迟早会写，只是还没写。反过来，「永远等不到」的图是造不出来的：没人会写的 Key 在 `Start()` 就被拒了。

```text
timeout err = pulse: node wait-forever timeout after 50ms
```

超时是**失败**，走失败路径：取消整图，`Run` 返回上面这条错误。

超时是**协作式**的：到期先取消本层 ctx，**再等节点体返回**——`Run` 不会在节点体还在跑的时候就返回（切面与父层共享写入记录，提前返回会让收尾和还在跑的执行并发碰同一批槽）。节点体不看 ctx（裸 `time.Sleep`、阻塞 IO）时 `Timeout` 只能等它结束，别当硬性看门狗用。另外**已发布的槽不撤回**：节点体在超时前 `Set` 过的输出，下游可能已经据此跑起来了——超时是失败，不是回滚。

## 重试

```go
pulse.NewNode("flaky", nil, pulse.Provides(Topic), run, pulse.Retry(3, 10*time.Millisecond))
```

`Retry` 只对**执行错误**重试。跳过**不**重试——跳过是到达，不是失败；等待阶段的取消也不重试。实测第三次成功：

```text
retry   attempts = 3 err = <nil>
```

**重试安全的前提**：失败前**没有写过**任何 Provide，也没有不可重入的副作用。槽位是「到达即发布、幂等首写」的：前一次 attempt 一旦 `Set`/`Skip` 过，下游就已经被唤醒，后续 attempt 的写会被**静默忽略**，槽位也不会回滚——回滚等于重开槽位，与「一次运行一个世界」冲突。需要事务性重试就把输出挪到确定成功的那次：**先算完，再 `Set`**。

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
g, _ := pulse.New(ctx, "demo", pulse.WithMaxRunning(4))
```

`WithMaxRunning(n)` 限制**同时进入 `Run`** 的节点数。**等数据不占名额**——否则限流会退化成死锁（占满名额的节点都在等数据，没人能推进）。

**排队等名额同样会被取消打断**：ctx 取消（或首错触发整图取消）时，还没进入 `Run` 的排队节点直接以 `canceled` 收尾（只有等待段那一条记录），空出来的名额不会交给一个已取消的节点。

## 切面顺序

```
全局切面（pulse.WithAspects） → 节点切面（NewNode 的 aspects…） → 内核（等输入 + 执行）
```

先写的更靠外。所以 `pulse.Timeout(d)` 加在 `pulse.Retry(...)` 前面，得到的是「**总时长**受限，每次尝试各自重试」——超时在外、重试在内。反过来写则是「每次尝试各自限时」。

**门闩**：单节点的 `Run` 不得并发进入（违反 → `ErrNextCalledTwice`），但顺序重入合法——`Retry` 正是靠它。

## 结果从哪读

`Graph` 没有 `Run` 之后的公开读槽。产物归调用方，两条常规做法：

```go
var report string // 1) 节点闭包写出——终端产物用这个
// ...

if err := g.Run(); err != nil { return err } // 2) 失败看返回值
```

`Run()` / `Err()` 的语义：返回**首错**或 ctx 取消原因，**不含单纯的跳过**；全图都跳过是合法结果（`Err()` 为 `nil`）。

下一步：拓扑也可以不写在 Go 里——见[声明式装图](/guide/assembly)；想看每个节点花了多久等待、多久执行，见[图观测](/guide/observability)。
