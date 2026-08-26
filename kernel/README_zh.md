# kernel

pulse v2 的插件化内核：一切皆插件的底座。

本包是 [cordiverse/paper](https://github.com/cordiverse/paper)《A Programming Paradigm for Spatiotemporal Composability》在 Go 下的工程化落地，**不是逐字移植**。详细取舍见 [`docs/design/plugin-kernel-v2.md`](../docs/design/plugin-kernel-v2.md)。

## 两个维度

| 维度 | 含义 | 落地 |
|---|---|---|
| 时间可组合 | 卸载即还原 | 对环境的每次修改都以 `Context.Effect` 登记，携带撤销函数；作用域销毁时按 LIFO unwind |
| 空间可组合 | 依赖响应式 | `Plugin.Inject` 声明依赖；满足则装载，消失则卸载，恢复则重装。没有手工启动序列 |

## 五个概念

```
context.go   Context     服务仓库 + 效应跟踪器 + 作用域树节点
service.go   ServiceKey  类型安全的服务键，Provide / Get
events.go    EventKey    事件总线：Emit / Waterfall / Parallel
plugin.go    Plugin/Fiber 插件声明与惯性生命周期
loader.go    Loader      声明式配置树 + 增量调和
```

## 最小用法

```go
ctx := kernel.New()
defer ctx.Dispose()

var Key = kernel.NewServiceKey[*MySvc]("pulse.example")

// Provide 本身是一条效应：ctx 销毁时绑定自动撤除。
dispose, err := kernel.Provide(ctx, Key, &MySvc{})
_ = dispose

svc, ok := kernel.Get(ctx, Key)
```

插件形态：

```go
type FSPlugin struct{}

func (p *FSPlugin) Inject() []kernel.Dependency {
    return []kernel.Dependency{kernel.Require(llm.ServiceKey)}
}

func (p *FSPlugin) Apply(c *kernel.Context) error {
    // 在 c 上 Provide / On / Effect 的一切，随实例卸载自动回收。
    return nil
}

host := kernel.New()
fiber := kernel.Use(host, &FSPlugin{})
// 依赖满足 → Active；llm 服务撤除 → 自动 Unloading。
```

声明式装配：

```go
loader := kernel.NewLoader(host)
loader.Register("fs", func() kernel.Plugin { return &FSPlugin{} })
_ = loader.Load([]kernel.Entry{
    {ID: "fs-1", Name: "fs", Config: map[string]any{"root": "."}},
})
```

## 事件三种派发

| 入口 | 签名 | 用途 |
|---|---|---|
| `On` + `Emit` | `func(*P)` | 观察：计量、审计、轨迹。并发不保证顺序 |
| `OnWaterfall` + `Waterfall` | `func(P, next func(P) P) P` | around：改写请求、HITL 短路。按注册顺序 |
| `OnParallel` + `Parallel` | `func(*P)` | 并发观察。Go 同步调用已能串行累积，不另设 serial |

事件名全局唯一，同名不同类型的键在注册时被拒绝。waterfall **不支持 prepend**。

## 有意钉死的语义

- 服务仓库**全局唯一**，绑定写在根作用域（对齐 Cordis runtime-store）。作用域树管生命周期与事件传播，不管「谁提供谁可见」。
- 覆盖 = 撤旧装新，**不还原前值**（有测试背书）。
- 运行期收敛用脏标记 + 单飞 goroutine，避免 Go 同步模型下的卸载环死锁。`Use` 首装同步。
- 锁序固定：`ctx.mu → bus.mu`，`bus.mu` 是叶子锁。
- 不做 realm / isolate 多租户，不做代码级 HMR（Go 卸不掉已加载代码）。Loader 的「重载」是状态级：dispose 旧 Fiber，同一工厂重建。
- `Name` 或 `Config` 变化 → 整实例重建，不原地 diff。仅 `Disabled` 翻转才卸载 / 恢复。

## 不做

realm、HMR、prepend、按作用域隔离的服务可见性。
