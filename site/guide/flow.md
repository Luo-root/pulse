# flow 编排

`kernel/flow` 是数据就绪驱动的节点编排器：节点声明自己**要什么 / 给什么**，图在数据就绪时调度——不关心拓扑顺序，只关心槽位。

## 三个核心类型

```go
g := flow.New(ctx)

kIn := flow.NewKey[string]("in")     // Key：类型化槽位句柄
kOut := flow.NewKey[string]("out")

node := flow.NewNode("pass",         // 节点名
	flow.Requires[string](kIn),      // Requires：要哪些槽位（AND 语义）
	flow.Provides[string](kOut),     // Provides：产出哪些槽位
	func(rc *flow.RunCtx) error {    // 节点函数：数据就绪才执行
		v, err := flow.Get(rc, kIn)
		if err != nil {
			return err
		}
		return flow.Set(rc, kOut, v)
	},
)
_ = g.Add(node)
_ = flow.Seed(g, kIn, "war payload") // Seed：为源槽位赋初值
err := g.Run()                       // 阻塞到全部节点终止（一次性运行）
```

## 槽位三态

每个槽位只有三种状态：**pending → ready → skipped**。

- Skip 是**到达**而非失败——上游节点产出 skip 值，下游按契约决定跳过路径；
- 节点错误会取消整个图，**绝不**被改写成 skip；
- AND 汇聚 = 节点 Requires 多个 key，全部就绪才执行。

## 分支汇聚（DAG）

1 源 → 2 并行分支 → AND 汇聚：

```go
kA := flow.NewKey[string]("a")
kB := flow.NewKey[string]("b")
join := flow.NewNode("join", flow.Requires[string](kA, kB), nil, func(rc *flow.RunCtx) error {
	a, _ := flow.Get(rc, kA)
	b, _ := flow.Get(rc, kB)
	out = a + b // join 闭包写终端结果
	return nil
})
```

终端结果经 join 闭包写出——Graph 没有 Run 后的公开读槽，这是 flow README 声明的契约权宜。

## 与 kernel 自由组装

flow **不 import kernel**，可零依赖独立跑图；需要时在装配层把 kernel 宿主 / 服务以闭包注入节点，编排步骤内直接取用注册能力。三种用法正交：

1. **独立运行**（如 eval/war 的编排对比）；
2. **kernel 组装**（节点闭包捕获 `*kernel.Context`，步骤内 `kernel.Get` 取服务）；
3. **YAML 声明式装图**（`kernel/flow/yaml`，E2）：

```yaml
nodes:
  - name: a
    requires: [in]
    provides: [a]
  - name: join
    requires: [a, b]
```

## 性能：图执行机的价格

同机同任务的跨框架对比（详见[评测页](/eval)）：

| 任务 | Pulse flow | Eino compose | 倍数 |
|---|---|---|---|
| T3 线性链（3 透传节点） | 8.6–8.9 µs / 73 allocs | 17.9–18.1 µs / 323 allocs | ~2.0× |
| T4 分支汇聚 DAG | 9.0–9.3 µs / 73 allocs | 30.3–37.9 µs / 411–462 allocs | ~3.3–4.1× |

**AND 槽位让 fan-out 免费**：DAG 与线性链同价同 allocs。对比方的 join 调度较其自身线性链贵 ~1.7×，字段映射层再 +15–20%。

详见 [flow 包文档](/packages/kernel/flow/) 与 [flow/yaml 包文档](/packages/kernel/flow/yaml/)。
