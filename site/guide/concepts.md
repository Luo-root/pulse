# 核心概念

Pulse 只有三样东西：**Key**（数据槽）、**Node**（计算单元）、**Graph**（一次运行的世界）。理解它们和槽位的三态，剩下的都是推论。

## 三件事

| 概念 | 一句话 | 关键性质 |
|---|---|---|
| **Key** | 类型化的数据槽 | `Key[T]` 把名字和类型绑在一起：`Get` 直接拿到 `T`，不用断言；同名 Key 必须以同一个 `T` 注册，不做「同名换类型」的静默覆盖 |
| **Node** | 声明读什么、写什么 | 只声明 `Requires` / `Provides`，**不声明下一个节点是谁**——依赖由 Key 的生产与消费隐式形成 |
| **Graph** | 一次运行的世界 | 节点集合 + 数据槽 + 首错 + 取消。数据随 `Run` 而生、随结束而灭 |

节点长这样：

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

不同类型的输入用一个节点接：`pulse.Deps(pulse.Requires(A), pulse.Requires(B))`——`Requires[T]` 一次只能收同一类型的 Key。

## 槽位三态

```
未就绪(pending) | 已就绪(ready, 值) | 已跳过(skipped)
```

**就绪和跳过都是「到达」。** 等待者被唤醒后区分这两种到达，而不是把「永远不到」伪装成一个假值。这是这类引擎最容易做错的一处：把跳过当失败，会让「分支」这个基本操作无处安放。

由此推出三条规则：

- **到几个收几个**：等**全部**输入到达（就绪或跳过）后才判门——只要有一条输入真的到了值，节点就带着到了的那些进入 `Run`；
- 一条值都没到（全部输入都以跳过到达）→ **不执行 `Run`**，全部输出跳过；
- `Run` 成功返回后**漏写的 `Provides` 自动跳过**（否则下游永远等不到到达）。

由此也推出一个容易说反的事实——**跳过是槽位的事实，不是节点的事实**：

| 情形 | 终态 | 观测记录 |
|---|---|---|
| 节点对某条 `Provide` 调 `Skip` 后正常返回 | `completed` | 等待段 + 执行段两条 |
| 节点因**一条值都没到**而没执行 | `skipped` | **只有一条**等待记录（`err="pulse: skipped [key]"`），无执行段 |

看真实输出（两个节点：`translate` 对 `translated` 调 `Skip`，`publish` 依赖 `translated`）：

```text
PULSE | 2026/10/09 - 10:42:50.550 | running    |         - | pulse.graph_started | source=observe | pulse.graph=branch-demo | host=quickstart | trace=branch-demo
PULSE | 2026/10/09 - 10:42:50.550 | running    |         - | pulse.node_wait_finished | source=observe | pulse.graph=branch-demo pulse.node=translate | host=quickstart | trace=branch-demo
PULSE | 2026/10/09 - 10:42:50.550 | completed  |         - | pulse.node_run_finished | source=observe | pulse.graph=branch-demo pulse.node=translate | host=quickstart | trace=branch-demo
PULSE | 2026/10/09 - 10:42:50.550 | skipped    |         - | pulse.node_wait_finished | source=observe | pulse.graph=branch-demo pulse.node=publish | host=quickstart | err="pulse: skipped [translated]" | trace=branch-demo
PULSE | 2026/10/09 - 10:42:50.550 | completed  |   524.5µs | pulse.graph_finished | source=observe | pulse.graph=branch-demo | host=quickstart | trace=branch-demo
```

`translate` 写了跳过，但它自己正常结束了；`publish` 才是那个「没执行」的节点。整轮那条是 `completed`——**有节点跳过，这一轮仍然是成功的**。

### 汇聚：到几个收几个

门看的是「**有没有值**」，不是「有没有跳过」。等全部输入到达之后：只要有一条真的到了值，节点就带着到了的那些进入 `Run`；一条值都没到，才是它自己跳过。

```go
pulse.NewNode("join",
	pulse.Requires(A, B, C), // 这一轮只有 A、B 有值
	pulse.Provides(Joined),
	func(rc *pulse.RunCtx) error {
		var parts []string
		for _, k := range []pulse.Key[string]{A, B, C} {
			v, ok, skipped, err := pulse.TryGet(rc, k)
			if err != nil {
				return err
			}
			switch {
			case ok:
				parts = append(parts, v)
			case skipped: // 这一路没有值，跳过它
			default:
				return fmt.Errorf("输入既未就绪也未跳过")
			}
		}
		return pulse.Set(rc, Joined, strings.Join(parts, "+"))
	})
```

两条边界：

- 读到一条没值的输入得到的是 `*SkipError`（`errors.Is(err, ErrSkipped)` 成立）——不是零值，也不是失败，`Get` 读它也不阻塞。逐条问「这一路到了没有」用 `TryGet` 最省事。
- 反过来，**要「缺一条就别跑我」的节点自己表态**：把 `WaitAll` 的返回值直接返回出去。它仍会进入 `Run`（跳过是它自己的结论，不是被门挡住的），终态是 `skipped`、`Run`/`Err` 不报错、`Retry` 不重试。注意**已经发布的输出不回滚**：节点体先 `Set` 过的那几条照常就绪，只有还没写的 `Provides` 会被跳过（一次性槽位契约）。

这段手写汇聚有等价的糖：`pulse.Join` 把 N 路同类型输入收成一束 `pulse.Batch[T]`（每条输入带来源名，缺项也占一行）——见[编排](/guide/orchestration)。

### 分支怎么写

没有 `if` 原语。分支 = **对未选中的 `Provide` 调用 `Skip`**：

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

**两边都要表态。** 只 `Skip` 未选中的那条是不够的：`Run` 成功返回后漏写的 Provides 会被自动跳过，于是「没选 A」变成「A、B 都没到」——两条下游都不跑。选中的 `Set`、没选中的 `Skip`，缺一不可。

写入语义：`Set` / `Skip` 都是**幂等首写**——已就绪时再 `Set` 会被忽略（不比对值），已跳过时再 `Skip` 也忽略；同一个槽位先 `Set` 后 `Skip`（或反过来）报 `ErrConflict`。`Seed` 同理。

## 一次运行一个世界

> `Graph` 是模板的一次实例，不是可重跑的容器。

`Start()` 第二次调用返回 `ErrGraphStarted`；槽位一旦到达即关闭、不会重开；图启动后 `Seed` 被拒。**没有 `Reset`，也不会加**。

判据一句话：**pulse 持有「这一轮正在流动的数据」，不持有历史。** 一旦为了跑第二轮而要保存上轮的值，引擎就得回答「什么该留、什么该清」——那是存储语义，是调用方的事。三类需求都不需要引擎来存：

| 需求 | 正确表达 |
|---|---|
| 同一张图跑 N 个独立请求 | 每次 `New` |
| 一次请求内跑 3 个候选 | 一张图内 fan-out（多 `Provides`） |
| 长会话多轮 | 每轮一张图，拓扑来自 YAML |

## 失败显式

`error` 与 `skipped` 走**两个出口**，不复用：

- 任一节点返回非跳过错误 → 记录**首错** + 取消整图，所有等待者被唤醒；
- `Run()` / `Err()` 返回原错误，**绝不**把失败改写成 `ErrSkipped`；
- `panic` 不穿透：节点 panic 被转成节点错误，走同一条失败路径；
- `Err()` 不含单纯的跳过——**全图都跳过是合法结果**。

取消来源有两个：外层 `ctx` 取消（`pulse.New` 的 ctx），或切面超时（`Timeout`）。两者都让还在等数据的节点立刻返回。

## RunCtx：只有声明过的槽位

`RunCtx` 是一次运行里节点能看到的世界：**声明过的槽位** + 本层可取消的 context。拿不到整个黑板：

- `Get` 一个没在 `Requires` 里声明的 Key → `ErrUndeclared`；
- 写一个不在 `Provides` 里的 Key → `ErrUndeclared`。

`RunCtx.Fork()` 只派生可取消 context，**共享**声明权限与写入记录——它不是独立写入事务。

## 切面

```go
type Aspect func(rc *RunCtx, next func(*RunCtx) error) error
```

切面包住节点的「**等输入 + 执行**」整段——所以 `Timeout` 能打断还在等数据的节点，而不只是打断执行。不调 `next` 即短路。

- `Timeout(d)`：超时取消本层 ctx；
- `Retry(attempts, delay)`：只对执行错误重试；**等待阶段的取消不重试，以跳过收尾的也不重试**（一条值都没到而没执行、或自己把 `WaitAll` 的跳过返回出去）——跳过是到达，不是失败；
- 顺序：全局切面（`pulse.WithAspects`）先于节点切面，**先写的更靠外**——所以 `Timeout` 在外、`Retry` 在内。

**门闩约束**：单节点的 `Run` 不得**并发**进入（两个 goroutine 同时跑同一节点必然抢同一批槽位），违反返回 `ErrNextCalledTwice`。**顺序重入是合法的**——`Retry` 正依赖它（1→0→1）。所以判据是「重叠」而不是「多次」。

## 三个包的分工

| 包 | 是什么 | 依赖 |
|---|---|---|
| `pulse`（根） | 图引擎 | **零**（只用标准库） |
| `pulse/observe` | 图观测：把引擎的 `Observer` 回调折成结构化记录 | `pulse` |
| `pulse/yaml` | 声明式装图：YAML → 图 | `pulse` + `yaml.v3` |

**依赖箭头单向且不许反向**：引擎不 import 任何观测包，只暴露一个 `Observer` seam。不需要观测的宿主只 import 根包——它的依赖闭包是空的。

下一步：看[编排](/guide/orchestration)怎么把这些拼成真实拓扑，或直接看[图观测](/guide/observability)怎么接出口。
