# 输入与输出

本页面向需要把请求数据传给流程、并在运行后取得业务结果的开发者。开始前应了解 [Graph、Execution 与键](./concepts.md)，并使用[迁移参考](../reference/migration.md)指定的开发版。

图的端口声明描述它接受和产出什么；`Inputs` 与 `Execution` 保存某次运行的数据。业务结果通过声明出口读取，进度通过快照查看，两者使用不同的 API。

## 声明接口与节点读写

以下片段放在返回 `error` 的装配函数内，需要导入 `github.com/Luo-root/pulse`。`g` 是尚未封闭的图，`request` 和 `result` 是已有的 `Key[string]`。

```go
if err := g.DeclareInput(pulse.NewPort(request)); err != nil {
	return err
}
if err := g.DeclareOutput(pulse.NewPort(result)); err != nil {
	return err
}
if err := g.Add(pulse.NewNode("format", "格式化输入",
	pulse.Requires(request), pulse.Provides(result),
	func(rc *pulse.RunCtx) error {
		value, err := pulse.Get(rc, request)
		if err != nil {
			return err
		}
		return pulse.Set(rc, result, "result: "+value)
	})); err != nil {
	return err
}
```

`DeclareInput` 允许宿主向该键提供输入。`DeclareOutput` 允许宿主或父图读取该键；它不会创建生产者。一个出口必须由图内节点生产，或同时声明为入口形成透传，否则 `Validate` 和 `Start` 会拒绝。

`Requires` 和 `Provides` 是节点的权限声明：节点可读取自己声明的键，只能写自己的 `Provides`。同名同类型的键等价，同名不同类型会在装配时拒绝。不同类型的多路声明可以用 `pulse.Deps` 拼接。

## 为每次运行准备输入

继续在同一个宿主函数中准备输入；此处 `request` 已声明为入口。

```go
var in pulse.Inputs
if err := pulse.Seed(&in, request, "hello"); err != nil {
	return err
}
```

`Inputs` 的零值可用。`Seed` 给值，`SkipSeed` 明确表示正常无值。若这次没有请求值，用下面的片段替换上面的 `Seed`，不要向同一份输入连续写两次：

```go
var in pulse.Inputs
if err := pulse.SkipSeed(&in, request); err != nil {
	return err
}
```

`SkipSeed` 是到达声明，下游会按跳过语义推进；完全不提供一条必需输入则是启动条件错误。`Seed`、`SkipSeed` 在调用时拒绝 nil `*Inputs`、零键和重复写入。宿主的 `Seed` 还拒绝 nil 接口值；带具体类型的 nil 指针或 nil 切片仍是值。不要用 nil 接口代替跳过。

创建执行时，输入条目被复制到本次执行。之后添加其他输入条目不会改变已创建的执行；业务值中的切片、映射和指针不会深拷贝。不要在执行读取这些对象时并发修改它们。

## 区分定义校验与本次输入校验

`Validate` 检查定义结构，并在成功时封闭整棵子树。它不要求调用方提供任何一次运行的值。

`NewExecution` 检查本次输入：提供的键必须是入口，不能是内部生产的键；每条被节点 `Requires` 使用的入口都必须有 `Seed` 或 `SkipSeed`。没有节点使用的入口可以不提供。

`Start` 在封闭前会再次检查本次输入。原因是创建执行后、启动之前，尚未封闭的定义仍可能添加一个需要新入口的节点。若因此缺少输入，启动失败且不封闭；准备完整 `Inputs` 并创建新的执行即可。修改原来的输入变量不能补充旧执行。

## 启动、等待与读取结果

以下片段继续使用已经装配好的 `g`、输入 `in` 和已声明出口 `result`。宿主函数需要 `context.Context` 类型的 `ctx`，以及 `fmt` 导入。

```go
e, err := g.NewExecution(ctx, in)
if err != nil {
	return err
}
defer e.Cancel()
if err := e.Start(); err != nil {
	return err
}
if err := e.Wait(); err != nil {
	return err
}
value, state, err := pulse.ReadOutput(e, result)
if err != nil {
	return err
}
switch state {
case pulse.SlotStateReady:
	fmt.Println(value)
case pulse.SlotStateSkipped:
	fmt.Println("本次没有结果")
case pulse.SlotStatePending:
	fmt.Println("可选透传入口未提供")
}
```

先检查 `err`，再检查状态。错误时 `value` 和 `state` 都无效。正常结果有三种：

| 状态 | 含义 | 宿主处理 |
|---|---|---|
| `ready` | 有值；零数值、空字符串、合法 nil 值也属于有值 | 使用返回值 |
| `skipped` | 正常无值，流程仍可能成功 | 处理缺省结果或省略该项 |
| `pending` | 未提供的可选透传出口，没有节点需要它 | 根据接口约定处理未提供 |

`ReadOutput` 不等待、不创建槽位、不产生观测事件。必须在 `Wait` 完成收尾后读取；`OnGraphFinished` 回调也可以读取已经确定的结果。未收尾、失败或真实取消的执行不提供部分业务结果。返回的可变对象仍是原业务对象，宿主负责它的后续同步。

运行中读取受管流用 `SubscribeStream(e, key)`，必须在 NewExecution 后、Start 前登记。它是真实读取者，宿主需 Receive 并 Ack，或明确 Stop/Cancel；Wait 不替宿主排空。Graph.Validate/Topology 允许声明的 managed 根出口等待宿主连接，仅省略其缺读者诊断，便于提前冻结 schema；未导出流和原生 channel 不变，Start 仍以实际订阅严格校验缺读者和分流。输入输出透传被订阅后，本轮必须 Seed 或 SkipSeed。ReadOutput 继续只读 Wait 后结果；实时完整片段见[流指南](./streams.md)。

## 常见错误

- 仅调用 `Seed`，没有声明入口：先 `DeclareInput`，再创建执行。
- 试图输入一条内部输出：改用上游入口，或移除该键的内部生产者。
- 用 `Get` 读取宿主结果：`Get` 需要节点上下文；宿主使用 `ReadOutput(e, key)`。
- 以返回值是否为零判断缺失：零值可能是 `ready`，应检查 `SlotState`。
- 用 `Snapshot` 获取业务值：快照不包含值，声明出口才是业务结果接口。

## 下一步

[分支与汇聚](./branching.md)展示输入跳过如何传播；[静态子图](./subgraphs.md)将端口用作父子边界。生命周期和错误分类分别见[生命周期参考](../reference/lifecycle.md)与[错误参考](../reference/errors.md)。
