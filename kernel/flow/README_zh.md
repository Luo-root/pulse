# kernel/flow

`flow` 是 Pulse v2 在**一次运行**时间尺度上的数据就绪编排包。

它不是维护显式边表、预先做拓扑排序的 DAG 执行器。节点只声明读取哪些 `Key`、产出哪些 `Key`；运行时一次性提交全部节点，输入数据到达后节点自然继续。`Requires` 是 **AND 前置**：全部输入就绪才会进入节点 `Run`。

```go
import "github.com/Luo-root/pulse/kernel/flow"
```

设计契约与验收清单见 [`../../docs/design/flow-v2-design.md`](../../docs/design/flow-v2-design.md)。

## 适用范围

适合在单个 `context.Context` 生命周期内组织有确定输入输出关系的工作：

- 并行准备数据后汇聚；
- 分类、路由等条件分支；
- 需要显式取消、超时、重试与并发上限的单次任务；
- 将一次 Agent 执行拆成可观察的计算节点。

不适合承担跨运行持久状态。会话、缓存、熔断器、断点续跑、分布式调度与服务治理不属于 `flow`。

## 心智模型

```text
节点声明 Requires / Provides
          │
          ▼
Graph.Start / Run 一次性提交全部节点
          │
          ▼
每个节点等待全部 Requires 到达（AND）
          │
          ├── 全部 ready ──► 进入 Run ──► Set / Skip Provides
          │
          └── 任一 skipped ─► 不进入 Run，全部 Provides 自动 Skip

节点返回 error ─► 记录首错并取消整图
```

槽位只有三态：

```text
pending（未到达） | ready（值） | skipped（跳过）
```

`ready` 与 `skipped` 都是到达。Skip 是正常分支结果，不是失败；节点 error 才是失败。

## 最小示例：线性数据流

```go
package main

import (
    "context"
    "fmt"

    "github.com/Luo-root/pulse/kernel/flow"
)

var (
    Source = flow.NewKey[string]("example.source")
    Result = flow.NewKey[string]("example.result")
)

func main() {
    g := flow.New(context.Background())

    if err := g.Add(flow.NewNode(
        "prepare",
        nil,
        flow.Provides(Source),
        func(rc *flow.RunCtx) error {
            return flow.Set(rc, Source, "hello")
        },
    )); err != nil {
        panic(err)
    }

    if err := g.Add(flow.NewNode(
        "format",
        flow.Requires(Source),
        flow.Provides(Result),
        func(rc *flow.RunCtx) error {
            value, err := flow.Get(rc, Source)
            if err != nil {
                return err
            }
            return flow.Set(rc, Result, value+" world")
        },
    )); err != nil {
        panic(err)
    }

    if err := g.Run(); err != nil {
        panic(err)
    }

    // 输出槽通常由后续节点消费。此例将 Result 再交给外部调用方即可。
    fmt.Println("flow completed")
}
```

节点中的 `Get` / `Set` 只允许访问已声明的 Key：

- `Get` / `TryGet` / `WaitAll` 可读节点 `allowed`（Requires∪Provides）；
- `Set` / `Skip` 只能写入 `Provides`；
- 未声明、同名不同类型等错误会显式返回，不能把数据静默写到共享黑板。
- 惯例：不要 `Get` 自己尚未写出的 Provide（会死等）；读 Provides 主要给切面/诊断。

当同一个节点需要不同泛型类型的多个输入或输出时，用 `Deps` 合并声明：

```go
flow.NewNode(
    "compose",
    flow.Deps(flow.Requires(Title), flow.Requires(Count)),
    flow.Provides(Summary),
    func(rc *flow.RunCtx) error {
        // ...
        return nil
    },
)
```

## 外部输入：Seed / SkipSeed

外部调用方在运行前写入输入：

```go
g := flow.New(ctx)
if err := flow.Seed(g, Request, input); err != nil {
    return err
}
if err := g.Run(); err != nil {
    return err
}
```

若外部输入代表某条分支不适用，可用 `SkipSeed`：

```go
if err := flow.SkipSeed(g, OptionalInput); err != nil {
    return err
}
```

### 来源身份规则

每个 Key **至多一种来源身份**：

1. 外部 `Seed` / `SkipSeed`；或
2. 一个节点的 `Provides`。

二者不可并存；冲突返回 `ErrDuplicateSource`。同一个外部来源可重复声明：重复 `Seed` 遵循槽位首写，重复 `SkipSeed` 幂等；`Seed` 与 `SkipSeed` 的值/跳过冲突返回 `ErrConflict`。

因此，外部输入不应同时再由节点计算：

```go
// 错误：Request 已被外部来源占用，Add 返回 ErrDuplicateSource。
_ = flow.Seed(g, Request, input)
_ = g.Add(flow.NewNode("load", nil, flow.Provides(Request), load))
```

## Skip：条件分支与自动收尾

条件分支不需要 OR 调度。分类节点为选中分支 `Set`，为未选中分支 `Skip`；下游节点只要依赖被跳过的输入，就不会执行，输出会继续 Skip。

```go
var (
    Left  = flow.NewKey[string]("left")
    Right = flow.NewKey[string]("right")
)

split := flow.NewNode(
    "split",
    nil,
    flow.Deps(flow.Provides(Left), flow.Provides(Right)),
    func(rc *flow.RunCtx) error {
        if chooseLeft() {
            if err := flow.Set(rc, Left, "selected"); err != nil {
                return err
            }
            return flow.Skip(rc, Right)
        }
        if err := flow.Skip(rc, Left); err != nil {
            return err
        }
        return flow.Set(rc, Right, "selected")
    },
)
```

规则如下：

- `Set` 是幂等首写；第二次 `Set` 静默忽略。
- `Set` 与 `Skip` 不能混用在同一槽位上；冲突返回 `ErrConflict`。
- 节点的任一 `Requires` 为 skipped 时，框架不会执行该节点 `Run`，而是把该节点所有 `Provides` 标记为 skipped。
- `Run` 正常返回但遗漏某个 `Provides` 时，框架自动 `Skip` 漏写输出，避免下游永远等待。
- 节点返回 error 时：`Graph.Run` / `Err` 仍返回该 error（**不会**伪装成 `ErrSkipped`）；框架仍可对**未写 Provide** 做 Skip，只为解开等待者（取消清理），不等于把失败翻译成跳过。
- `ErrSkipped` 不是 `Graph.Run` 的失败结果；它只表示某次读取或等待遇到了跳过。

`Get` 在读取 skipped 槽位时返回可被 `errors.Is(err, flow.ErrSkipped)` 判断的错误。`TryGet` 用于不阻塞检查：

```go
value, ready, skipped, err := flow.TryGet(rc, Input)
```

## 并行汇聚

所有节点都会被提交；等待数据不占执行名额。多个无依赖节点可并行完成，汇聚节点只会在全部输入 ready 后运行：

```go
join := flow.NewNode(
    "join",
    flow.Deps(flow.Requires(LeftOutput), flow.Requires(RightOutput)),
    flow.Provides(Merged),
    func(rc *flow.RunCtx) error {
        left, err := flow.Get(rc, LeftOutput)
        if err != nil {
            return err
        }
        right, err := flow.Get(rc, RightOutput)
        if err != nil {
            return err
        }
        return flow.Set(rc, Merged, left+right)
    },
)
```

默认情况下，真正进入用户 `Run` 的节点数不受限制。若要限制运行并发：

```go
g := flow.New(ctx, flow.WithMaxRunning(4))
```

`WithMaxRunning(n)` 中 `n <= 0` 表示无限并发（默认）。等待 `Requires` 时不占名额；全部输入到达后才占一个名额。

## 生命周期：Run、Start、Wait 与取消

```go
// 同步执行：Start 后 Wait。
if err := g.Run(); err != nil {
    return err
}

// 异步执行：适合调用方需要并行做其他工作。
if err := g.Start(); err != nil {
    return err
}
if err := g.Wait(); err != nil {
    return err
}
```

- `Run` = `Start` + `Wait`。
- 图只能启动一次；启动后 `Add`、`Seed`、`SkipSeed` 都返回 `ErrGraphStarted`。
- 创建 Graph 时传入的 `ctx` 被取消，会打断所有等待与正在协作的节点。
- 节点返回任意非 `ErrSkipped` 的 error，Graph 记录首错、取消整图，并由 `Run` / `Wait` 返回该错误。
- `Graph.Err()` 返回首个节点错误或上游 context 的取消原因；单纯 Skip 不会让它失败。

## 切面：超时与重试

`Aspect` 与 `kernel.Waterfall` 同构：切面不调用 `next` 即可短路后续执行。每次 `Around` 内禁止**并发/重叠**调用 `next`（返回 `ErrNextCalledTwice`）；允许**顺序多次**（`Retry` 需要）。全局切面通过 `flow.WithAspects` 安装；节点切面作为 `flow.NewNode` 的末尾参数传入。全局切面在外层、节点切面在内层。E1 `Observer` 生命周期事件由每节点门闩保证 Waiting/Running 仍至多一次。

```go
g := flow.New(ctx,
    flow.WithAspects(loggingAspect),
)

node := flow.NewNode(
    "call-model",
    flow.Requires(Prompt),
    flow.Provides(Answer),
    callModel,
    flow.Timeout(10*time.Second),
    flow.Retry(3, 200*time.Millisecond),
)
```

内建行为和可选切面：

| 能力 | 语义 |
|---|---|
| Recovery | 内建且不可关闭。`Run` panic 会转为节点错误，取消整图。 |
| `Timeout(d)` | 覆盖等待输入和节点执行；超时会取消该节点的 `RunCtx`，因此能打断 `Get` / `WaitAll`。 |
| `Retry(attempts, delay)` | 对内层执行错误重试；`attempts <= 0` 归一为 1。等待输入阶段的取消不重试。 |

`RunCtx.Fork()` 只派生可取消 context，**共享**声明权限和写入记录；它不是独立写入事务。自定义切面可通过 `rc.Context()` 获取当前 context。

## 生命周期观察（E1）

`Observer` 是 flow **自有** typed 观察者，默认 no-op。它不走 `kernel.Emit`，也不写 `observability.Sink`——正式观测包不认识 flow；装配层桥（如 `demoapp.FlowObserver`）订阅后再折成两条 Record。

```go
obs := flow.ObserverFunc{
    Waiting:  func(id string) { /* 进入 WaitAll 前 */ },
    Running:  func(id string) { /* acquire 后、用户 Run 前 */ },
    Finished: func(id string, reason flow.NodeFinishReason, err error) { /* skip 清理后 */ },
}
g := flow.New(ctx, flow.WithObserver(obs))
```

| 事件 | 何时 | 备注 |
|---|---|---|
| `OnNodeWaiting` | 进入 `WaitAll` 前 | 每节点 ≤ 1 |
| `OnNodeRunning` | `WaitAll` 成功且 `acquire` 之后 | Skip / 超时打断 Wait → **不发** |
| `OnNodeFinished` | 终止态确定且 skip 清理之后 | 原因：`completed` / `skipped` / `failed` / `canceled` |

契约要点：

- 每节点 Waiting/Running/Finished 至多一次；`Retry` 多次 attempt 不重复打点。
- observer panic **不得**变成节点失败。
- 桥侧常见落地：`flow.node_wait_finished` / `flow.node_run_finished` 两条 Record，各用 `Duration`；节点身份放 `FiberName`（不扩官方信封）。见 `examples/04-flow`（记录定义在 `examples/internal/demoapp/bridge.go`，04-flow README 已表格化）。

## 声明式装图（E2）

**YAML only** 装图在子包 [`yaml`](yaml/README_zh.md)：`Load` / `SeedPlan`。拓扑归属 A——YAML 必填 `id` / `uses` / `requires` / `provides`；`uses` 对应 `Registry` 上的 Run 工厂。不补 JSON 解析器。

- duration 字段用 Go `ParseDuration` 形态：`30s`、`100ms`（不要写裸数字当秒的歧义形式）。
- `observer:` 文档提示位，`Load` **忽略**；观察者走 `LoadOptions.Graph`（如 `WithObserver`）。
- `version` 缺省或 `1`；其它值拒绝。

### 图结束后没有公开读槽 API

`Graph` 没有 Run 后的公开 `Get`。叶子若不能双 Provide 同一输出 Key（单生产者），又不能靠 AND 汇聚两条 Final（Skip 会级联吃掉汇聚节点），示例层可用闭包写出终端结果——这是**契约权宜**，不是「输出都走闭包」的惯例。详见 [`examples/04-flow/README.md`](../../examples/04-flow/README.md)「闭包写 Final」三条约束。

## 声明期校验

`Graph.Add` 在启动前尽早拒绝以下无效声明：

- nil 节点、空节点 ID、重复节点 ID；
- 同一节点重复 `Requires` 或重复 `Provides`；
- 同一节点同时 `Requires` 与 `Provides` 同一个 Key；
- 多个节点 `Provides` 同一个 Key；
- Key 同名但泛型类型不同；
- 节点来源与 `Seed` / `SkipSeed` 来源冲突。

这避免将生产者竞争、同名类型冲突等问题留到并发运行期。

## 导出 API

| 分类 | 符号 | 用途 |
|---|---|---|
| 图 | `New` / `Graph.Add` / `Run` / `Start` / `Wait` / `Err` | 构造、声明与执行一次运行 |
| 配置 | `WithMaxRunning` / `WithAspects` / `WithObserver` | 执行并发、全局切面、生命周期观察 |
| 注册表（E2） | `Registry` / `NewRegistry` / `Register` / `RegisterKey` / `Lookup` / `ResolveKey` / `SeedByName` | 具名 Run 工厂 + Key 类型对账；实例非全局 |
| Key | `Key[T]` / `NewKey` / `Name` | 类型化槽位标识 |
| 节点 | `Node` / `NewNode` / `ID` | 声明节点 |
| 依赖 | `Requires` / `Provides` / `Deps` | 声明节点输入与输出 |
| 外部输入 | `Seed` / `SkipSeed` | 运行前解析槽位 |
| 节点读写 | `Get` / `TryGet` / `WaitAll` / `Set` / `Skip` | 读取、等待、写入或跳过声明的 Key |
| 切面 | `Aspect` / `AspectFunc` / `Timeout` / `Retry` | 环绕节点等待与执行 |
| 观察者 | `Observer` / `ObserverFunc` / `MultiObserver` / `WithObserver` | E1 节点生命周期（Waiting/Running/Finished）；默认 no-op |
| 运行上下文 | `RunCtx.Context` / `NodeID` / `Fork` / `Cancel` | 读取节点 context 与编写切面 |
| 错误 | `ErrSkipped` / `ErrConflict` / `ErrUndeclared` / `ErrDuplicateSource` / `ErrNextCalledTwice` / `ErrGraphStarted` / `ErrGraphNotStarted` | 判断预期错误类别 |

## 不做

`flow` 当前刻意不提供：

- 显式边对象与拓扑排序器；
- OR 依赖、`WaitAny`、竞速汇聚；
- 节点重跑、`SetOrUpdate` 或持续 reactive graph；
- 持久化、断点续跑、分布式执行；
- 熔断、默认吞错等跨运行服务治理；
- 未声明 Key 的黑板式任意读写；
- 在 **核心包** 内依赖 yaml 解码库（E2 解析在子包 [`yaml`](yaml/)：`Load` / `SeedPlan`；拓扑归属 A：Factory 只给 Run）。
