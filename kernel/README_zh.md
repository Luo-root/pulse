# kernel

pulse v2 的插件化内核：一切皆插件的底座。

本包把 [cordiverse/paper](https://github.com/cordiverse/paper)《A Programming Paradigm for Spatiotemporal Composability》落到 Go：**卸载即还原**（时间）+ **依赖响应式装载**（空间）。它不是逐字移植 Cordis。完整取舍见 [`docs/design/plugin-kernel-v2.md`](../docs/design/plugin-kernel-v2.md)。

读完这篇应能：建作用域、Provide/Get 服务、写一个插件、用 Loader 调和配置、订阅三类事件。

## 它解决什么

组件对环境的修改如果靠开发者自觉清理，卸载就会漏。组件之间如果靠手工排启动顺序，依赖一变整棵树就乱。

kernel 用同一套 `Context` 同时记住「现在有什么」和「曾经改过什么」：

| 维度 | 含义 | 落地 |
|---|---|---|
| 时间可组合 | 卸载即还原 | 每次修改以 `Effect` 登记撤销函数；作用域 `Dispose` 时按 LIFO unwind |
| 空间可组合 | 依赖响应式 | `Plugin.Inject` 声明依赖；满足则装载，消失则卸载，恢复则重装。没有手工启动序列 |

五个源文件对应五个概念：

| 文件 | 概念 |
|---|---|
| `context.go` | `Context`：服务仓库 + 效应跟踪器 + 作用域树 |
| `service.go` | `ServiceKey[T]`：类型安全的服务键，`Provide` / `Get` |
| `events.go` | `EventKey[P]`：`On` + `Emit` / `Waterfall` / `Parallel` |
| `plugin.go` | `Plugin` / `Fiber`：声明与惯性生命周期 |
| `loader.go` | `Loader`：声明式条目 + `Reconcile` 增量调和 |

## 1. 作用域与效应

```go
ctx := kernel.New()
defer ctx.Dispose() // 根销毁 → 子作用域逆序级联回收 + 本层效应 LIFO unwind
```

`Derive` 派生子作用域：共享**全局**服务绑定与事件广播；子作用域上的注册在 `Dispose` 时收回，并从父层摘除自己。宿主已销毁时返回 `kernel.ErrDisposed`。

任意可逆副作用都走 `Effect`。`Provide` / `On` 最终也归约为一次 `Effect`：

```go
undo, err := ctx.Effect(func() (func(), error) {
    ln, err := net.Listen("tcp", ":0")
    if err != nil {
        return nil, err
    }
    go http.Serve(ln, mux)
    return func() { ln.Close() }, nil
})
// undo() 可提前撤销（幂等）；没手动调的，Dispose 兜底。
```

作用域已销毁时 `Effect` / `Derive` / `Use` 都返回 `ErrDisposed`。

## 2. 服务：Provide / Get

服务命名空间**全局唯一**，绑定写在根作用域仓库（对齐 Cordis runtime-store）。作用域树管生命周期与事件传播，**不管「谁提供谁可见」**。

```go
var Key = kernel.NewServiceKey[string]("pulse.example")

ctx := kernel.New()
defer ctx.Dispose()

dispose, err := kernel.Provide(ctx, Key, "hello")
if err != nil { /* ... */ }
_ = dispose // 可提前撤；ctx.Dispose 也会撤

v, ok := kernel.Get(ctx, Key) // ok==false 表示未提供
```

同名覆盖 = 撤旧装新，**不还原前值**（有测试背书）。同名不同类型在 Provide 时被拒绝。name 建议带包前缀，如 `pulse.llm`。

## 3. 插件：Use / Fiber

```go
type Plugin interface {
    Inject() []kernel.Dependency // 全部满足才装载
    Apply(c *kernel.Context) error
}

func Require[T any](k kernel.ServiceKey[T]) kernel.Dependency
func Func(apply func(c *kernel.Context) error) kernel.Plugin // 零依赖适配
```

`Apply` 收到的是实例**私有作用域**：在上面 `Provide` / `On` / `Effect` 的一切，随卸载自动回收。`Apply` 报错 → Fiber 进入 `Failed`。

```go
type Greeter struct{}

func (p *Greeter) Inject() []kernel.Dependency {
    return []kernel.Dependency{kernel.Require(Key)} // Key 见上一节
}
func (p *Greeter) Apply(c *kernel.Context) error {
    v, _ := kernel.Get(c, Key)
    fmt.Println("greet:", v)
    return nil
}

host := kernel.New()
defer host.Dispose()

_, _ = kernel.Provide(host, Key, "world")

fiber, err := kernel.Use(host, &Greeter{})
if err != nil {
    // 宿主已销毁 → ErrDisposed，fiber == nil
    // Apply 失败 → err 非 nil，fiber 仍返回且处于 Failed，依赖变化后会自动重试
}
_ = fiber.State() // inactive / loading / active / unloading / failed
```

`Use` 返回 `(*Fiber, error)`。首次装载**同步**：依赖满足则返回时已是 `Active`；不满足则 `Inactive` 挂起等待；宿主已销毁返回 `ErrDisposed`。

运行期依赖变化走脏标记 + 单飞 goroutine（避免 Go 同步模型下的卸载环死锁）。装配期 `Use` 同步，运行期用 `WaitState` 等收敛。

## 4. Loader：声明式调和

实际入口是 **`Reconcile`**（没有 `Load` / `LoadFile` / `LoadJSON`）。

```go
loader := kernel.NewLoader(host)
if err := loader.Register("greeter", func() kernel.Plugin { return &Greeter{} }); err != nil {
    // 同名工厂冲突
}

err := loader.Reconcile([]kernel.Entry{
    {ID: "g1", Name: "greeter", Config: map[string]any{"lang": "zh"}},
})
// 单个条目失败不阻断其余；返回 errors.Join 聚合错误。
```

条目若实现 `Configurable`，`Reconcile` 在 `Use` 之前把该条目的 `Config` 交给实例——配置是实例私有的，不进全局服务仓库。

调和规则：

| 变化 | 行为 |
|---|---|
| 新增 ID | 装载 |
| 移除 ID | 卸载并注销 |
| `Name` 或 `Config` 变 | **整实例重建**（不原地 diff） |
| 仅 `Disabled` 翻转 | 卸载 / 恢复，不动其他字段 |

`MustRegister` 冲突时 panic，给包级 `init` 用。

## 5. 事件

观察型监听用 **`On`**，waterfall 用 **`OnWaterfall`**。没有 `OnParallel`：`Emit` 和 `Parallel` 共用 `On`。

| 派发 | 监听 | 语义 |
|---|---|---|
| `Emit` | `On` | **同步、按注册顺序**。`*P` 就地改，前序对后续可见（Go 同步调用即 serial，不另设 Serial） |
| `Waterfall` | `OnWaterfall` | around：`next(p)` 委托后续；不调 `next` 即短路。按注册顺序 |
| `Parallel` | `On` | 各拿**独立副本**并发；panic 收成 `[]error`，下标对应监听顺序 |

```go
var Tick = kernel.NewEventKey[int]("pulse.example.tick")

_, _ = kernel.On(ctx, Tick, func(n *int) { *n++ })           // Emit / Parallel
_, _ = kernel.OnWaterfall(ctx, Tick, func(n int, next func(int) int) int {
    return next(n) + 1
})

kernel.Emit(ctx, Tick, 0)              // 同步累积
_ = kernel.Waterfall(ctx, Tick, 0)     // around 链回流
_ = kernel.Parallel(ctx, Tick, 0)      // 并发；返回 []error 或 nil
```

事件名全局唯一；同名不同类型在注册时被拒绝。waterfall **不支持 prepend**。派发是全树广播（先序：根到叶），监听随作用域销毁自动摘除。`On` 与 `OnWaterfall` 混用时两类独立派发、互不干扰。

`Emit` 里单个监听器 panic **向上传播**；`Parallel` 把 panic 收成 error。

## 有意钉死

- 服务仓库全局唯一；跨 Context 树不互通是 by design。
- 覆盖不还原前值。
- 锁序固定：`ctx.mu → bus.mu`，`bus.mu` 是叶子锁。
- 不做 realm / isolate，不做代码级 HMR。Loader「重载」= dispose 旧 Fiber + 同一工厂重建。

## 不做

realm、HMR、prepend、`OnParallel`、`Load`/`LoadFile`/`LoadJSON`、按作用域隔离的服务可见性。
