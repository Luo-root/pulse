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
- 一个节点同时 `Requires` 和 `Provides` 同一个 Key → `Add` 报错（自环不是合法的数据流）。

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

`Requires` 是 AND：两个输入都到达才进入 `Run`。跑通时 `g.Err()` 是 `nil`。

## 分支：对未选中的路调 Skip

没有 `if` 原语，分支就是「把没走的那些 `Provide` 标成跳过」：

```go
if lang == "zh" {
	return pulse.Skip(rc, Translated) // 下游因输入跳过而不执行
}
return pulse.Set(rc, Translated, translate(summary))
```

区分两个终态很重要：**写出跳过的节点自己是 `completed`**；因输入跳过而没执行的**下游**才是 `skipped`。细节与真实记录见[核心概念 · 槽位三态](/guide/concepts)。

## 超时

```go
pulse.NewNode("wait-forever", requires, provides, run, pulse.Timeout(50*time.Millisecond))
```

切面包住「**等输入 + 执行**」整段，所以超时能打断一个**还在等数据**的节点，而不只是打断执行。上例中 `Never` 没有任何生产方（槽位永远 pending）：

```text
timeout err = pulse: node wait-forever timeout after 50ms
```

超时是**失败**，走失败路径：取消整图，`Run` 返回上面这条错误。

## 重试

```go
pulse.NewNode("flaky", nil, pulse.Provides(Topic), run, pulse.Retry(3, 10*time.Millisecond))
```

`Retry` 只对**执行错误**重试。跳过**不**重试——跳过是到达，不是失败；等待阶段的取消也不重试。实测第三次成功：

```text
retry   attempts = 3 err = <nil>
```

## 首错即取消

任一节点返回非跳过错误 → 记录**首错** + 取消整图（含还在等数据的节点）：

```text
run err = boom
pulse.node_wait_finished       node=boom     status=running    err=<nil>
pulse.node_run_finished        node=boom     status=failed     err=boom
pulse.node_wait_finished       node=waiter   status=canceled   err=context canceled
```

三件事同时成立：`Run` 返回**原错误**（没被改写成跳过）；失败的节点是 `failed`；被杀掉的等待者是 `canceled`，且**只有等待段**那一条记录。

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
