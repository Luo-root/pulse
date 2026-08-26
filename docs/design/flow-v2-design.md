# flow v2：数据就绪驱动的节点编排

> 状态：Accepted（设计拍板 2026-08-26）
> 包位置：`kernel/flow/`
> 前置：v1 `components/flowchart` 已按 breaking 决策删除；本篇从源码提炼理念并给出 v2 契约。

## 理念（四条）

> flow 是 kernel 的响应式理念在「一次运行」这个时间尺度上的应用：
> 依赖声明即拓扑，数据到达即调度；失败显式，取消能打断等数据。

1. **数据驱动，没有边。** 节点只声明读哪些 Key、写哪些 Key。没有 next、没有 DAG 边、没有调度器。
2. **一次运行一个世界。** 数据槽、取消、首错随 `Run` 而生、随结束而灭。跨运行状态（熔断计数、会话、缓存）不属于这个世界。
3. **和 kernel 同构，不同层。** 洋葱链用同样的 `next` 签名；层级取消对齐作用域树。kernel 管服务装卸载，flow 管数据到达。
4. **失败是一等公民。** 节点返回 error → 记录首错 + 取消整次运行。降级是节点自己显式写出的数据，不是切面偷偷吞掉的错误。

槽位因此有**三种**到达形态，第一版一次设计完，避免以后为分支再改契约：

```
槽位 = 未就绪 | 已就绪(值) | 已跳过
```

就绪和跳过都是「到达」。等待者被唤醒后区分这两种到达，而不是把「永远不到」伪装成一个假值。

## 与 kernel 的同构

| 维度 | kernel | flow |
|---|---|---|
| 时间尺度 | 服务生命周期（稳定态） | 一次运行内的数据流（瞬态） |
| 依赖声明 | `Inject() []Dependency` | `Requires(...Key)` |
| 响应式触发 | 依赖满足 → Fiber 装载 | 槽位到达（值或跳过）→ 节点继续 |
| 拦截 | `Waterfall(p, next)` | `Aspect.Around(rc, next)` |
| 层级 | 作用域树 Dispose | RunCtx.Fork 可取消 |
| 共享状态 | ServiceKey 仓库 | 运行级槽位（不进服务仓库） |

flow 可独立使用（只吃 `context.Context`），也可把取消挂到 `kernel.Context` 上。运行数据**不进**服务仓库。

## API 草案

```go
package flow // import "github.com/Luo-root/pulse/kernel/flow"

var Summary = flow.NewKey[string]("summary")
var Docs    = flow.NewKey[[]Doc]("docs")

g := flow.New(ctx) // ctx 可以是 context.Background()
g.Add(flow.NewNode("summarize",
    flow.Requires(Docs),
    flow.Provides(Summary),
    func(rc *flow.RunCtx) error {
        docs, err := flow.Get(rc, Docs) // 类型安全，无断言
        if err != nil {                 // 含 ErrSkipped
            return err
        }
        return flow.Set(rc, Summary, summarize(docs))
    },
))
g.Seed(Docs, input)
err := g.Run()
```

### Key

- `Key[T]`：name + 类型。同名不同类型在 `NewKey` / 注册期拒绝（对齐 `ServiceKey`）。
- 节点只能 `Get` 声明过的 Requires、`Set` 声明过的 Provides。碰未声明的 Key → 显式错误，不是静默写到黑板上。

### 槽位写入

| 操作 | 语义 |
|---|---|
| `Set` / `SetOnce` | 幂等首写为「已就绪(值)」；二次调用静默忽略 |
| `SetOrUpdate` | 覆盖为最新值并再广播；已跳过的槽位不允许再写成值 |
| `Skip` | 标记「已跳过」并广播；已就绪(值)的槽位不允许再 Skip |
| 互斥 | 同一槽位不能既有值又跳过。先到者为准，后到者显式错误（Skip/Set 冲突）或静默忽略（SetOnce 二次）——**冲突走错误**，避免假成功 |

### 槽位读取

| 操作 | 语义 |
|---|---|
| `Get` / `Wait` | 阻塞到到达；值为 `T`，跳过为 `ErrSkipped` |
| `WaitAll` | AND：全部到达。任一跳过 → 返回 `ErrSkipped`（带哪些 Key 被跳过） |
| `WaitAny` | OR：任一到达。先到的是值 → 返回该值；先到的是跳过 → 继续等其余；全部跳过 → `ErrSkipped` |
| `TryGet` / `IsReady` | 非阻塞。返回 (值, ready) / (零值, skipped, pending) 三态，不把跳过伪装成未就绪 |

`WaitAny` 用每个槽位的 done channel + select，不使用 `reflect.Select`。

### 跳过传播（第一版就钉死）

节点被提交后阻塞在 Requires 上。醒来时：

1. **全部 Requires 已就绪(值)** → 执行 `Run`。`Run` 返回 nil 且 Promotes 都已 Set 或 Skip，节点正常结束。未写的 Provides 在 `Run` 返回后**自动 Skip**（节点有责任写完；漏写 = 这条输出被跳过，不是永远阻塞下游）。
2. **任一 Requires 为跳过** → **不执行 `Run`**，全部 Provides 标记跳过，级联。
3. **`Run` 返回 error** → 记录首错 + 取消整次运行。不把失败翻译成跳过。
4. **节点自己对某条 Provide 调用 `Skip`** → 只跳过那一条，其余 Provides 仍按 1 处理。这是分支的一等表达：分类节点对未选中的边 `Skip`，对选中的边 `Set`。

工作流结束条件：所有节点进入终止态（跑完 / 被跳过 / 因首错取消）。没有人永远等。

`ErrSkipped` 不是工作流失败。`g.Err()` / `g.Run()` 只反映首个节点 error 或取消；跳过是正常终止路径。

### 调度

- 全量提交 + 阻塞等数据。去掉 ants 池、`GetStats`、`ResizePool`。
- `WithMaxRunning(n)`：`n<=0` 无限（默认）；`n>0` 为进入 `Run` 前的信号量。等数据不占名额，拿到全部输入、真正执行时才 Acquire。
- `Graph`（原 Workflow）API：`New` / `Add` / `Seed` / `Run` / `Start`+`Wait` / `Err`。

### 切面

```go
type Aspect interface {
    Around(rc *flow.RunCtx, next func(*flow.RunCtx) error) error
}
```

- `rc.Fork()` 派生本层可取消的 RunCtx；core 用切面级 ctx 做 `Get`/`WaitAll`，超时能打断等数据。
- 全局切面 + 节点切面，先全局后节点，外层先跑。
- **内建、不可关**：Recovery（panic → 首错 + 取消）。
- **可选**：Timeout、Retry。
- **不移植**：
  - CircuitBreaker：状态跨运行存活，是服务治理，不属于「一次运行一个世界」。
  - ErrorSwallow：把节点失败变成工作流成功，与「失败显式」冲突。需要降级就在节点里 `Set` 降级值，或 `Skip` 未走的边。

### 与 loop

正交。节点函数里可以构造 `loop.Agent` 跑模型回合；`loop` 不感知 flow。

## 语义钉死

- 同名不同类型的 Key 拒绝。
- SetOnce 二次静默忽略；Set 与 Skip 冲突显式报错。
- 漏写的 Provides 在 Run 返回后自动 Skip。
- 输入被跳过 → 不跑 Run，输出全跳过。
- 节点 error ≠ 跳过。error 取消整图；跳过只影响依赖链。
- `Run()` 的 error 不含 `ErrSkipped`。
- 运行数据不进 kernel 服务仓库。

## 不做

持久化、断点续跑、分布式、服务治理（熔断）、默认吞错、字符串 key、拓扑排序器、固定 goroutine 池。

条件节点、循环节点是建立在跳过语义上的语法糖，可以同一版提供薄封装，但不另开语义。

## 验收

1. 线性：A→B→C，类型安全 Get/Set，二次 SetOnce 忽略。
2. 并行汇聚：C 的 Requires(Aout, Bout) 在 A、B 都完成后才跑。
3. 分支：分类节点 Set 一边、Skip 另一边；未选中下游不执行，工作流正常结束。
4. 级联跳过：A skip → B（依赖 A）不跑且输出 skip → C（依赖 B）不跑。
5. WaitAny：先到值则返回值；一边 skip 一边值则拿到值；两边都 skip → ErrSkipped。
6. 节点 error → Run() 非 nil，未跑完的节点因取消结束，不把失败写成 skip。
7. Timeout 切面取消能打断 Wait；Recovery 把 panic 收成首错。
8. `WithMaxRunning(1)` 时两个无依赖节点串行进入 Run。
9. `go test -race ./kernel/flow/` 全绿。
