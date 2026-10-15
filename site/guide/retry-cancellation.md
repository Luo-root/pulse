# 重试、超时与取消

本页面向需要调用外部服务、处理短暂故障或控制运行时长的开发者。前置条件是理解[输入与输出](./inputs-outputs.md)和正常跳过语义，并使用[开发版 API](../reference/migration.md)。

`Retry` 与 `Timeout` 都是 `Aspect`：包裹节点“等待输入、等待额度、进入业务体”的函数。切面排列顺序决定时限与重试的范围；取消是一种需要业务体配合的信号。

## 给外部调用安装切面

以下是可放入现有程序的装配函数。需要导入 `context`、`time` 和 `github.com/Luo-root/pulse`。`fetch` 是宿主提供的业务函数，必须将传入的上下文用于请求；`input` 应有来源，`result` 尚未被其他节点生产。

```go
func addFetch(g *pulse.Graph, input, result pulse.Key[string],
	fetch func(context.Context, string) (string, error)) error {
	return g.Add(pulse.NewNode("fetch", "调用外部服务",
		pulse.Requires(input), pulse.Provides(result),
		func(rc *pulse.RunCtx) error {
			request, err := pulse.Get(rc, input)
			if err != nil {
				return err
			}
			value, err := fetch(rc.Context(), request)
			if err != nil {
				return err
			}
			return pulse.Set(rc, result, value)
		},
		pulse.Timeout(2*time.Second),
		pulse.Retry(3, 100*time.Millisecond)))
}
```

这里 `Timeout` 在外，`Retry` 在内：总共最多尝试三次，尝试之间等待 100 ms，但等待输入、排队、调用和重试间隔都在同一个两秒时限内。只要 `Retry` 所在层的上下文已取消，它就不会继续尝试。

将顺序改为 `Retry` 在外、`Timeout` 在内，会为每次尝试分别创建两秒时限。总耗时可能接近三次时限加重试间隔。每次尝试仍受宿主执行上下文限制。

`Retry(attempts, delay)` 的 `attempts` 包含首次调用；`attempts <= 0` 按一次处理。普通错误会重试，最终返回最后一次错误；正常跳过不重试。等待阶段的执行取消也不重试。

## 失败尝试的输出不会发布

每次 Retry 尝试都有独立的输出写集，即暂存本次 `Set` 与 `Skip` 的集合。假设一次业务体先写结果，然后发现后续检查失败：

| 阶段 | 尝试暂存 | 下游可见结果 |
|---|---|---|
| 第一次写入 `"draft"`，随后返回错误 | 第一次写集丢弃 | 没有看到 `"draft"` |
| 第二次写入 `"final"`，返回 nil | 成功写集提交 | 看到 `"final"` |
| 所有尝试均失败 | 各次写集丢弃 | 执行失败，不提供宿主部分结果 |

成功或正常跳过的尝试会提交已有写入；跳过不会撤回已经生成的值。嵌套 Retry 的内层提交到外层写集，只有外层成功后才继续发布。Retry 外面的切面写入不属于它的尝试边界。

多条输出在提交前一起核验，任何一条冲突都不会发布该批次；真正发布仍是逐键进行，没有“所有槽位同一瞬间可见”的保证。写入应留在节点及切面的同步调用链中。不要让自建 goroutine 在节点返回后继续写输出，也不要并发写同一条输出。

在同一次尝试内可以读回自己暂存的写入；下一次尝试读不到已丢弃的值。不要依赖上一轮的输出作为隐含状态。

## 外部副作用需要业务保证

写集只管理 Pulse 输出。数据库提交、发送请求、写文件等已发生的外部操作不会回滚。例如第一次请求已经成功，但响应丢失导致业务函数返回错误，Retry 可能再次发起请求。

需要避免重复时，为业务操作提供幂等键、数据库事务、去重、outbox 或补偿机制。仅把外部操作放在函数末尾，不能消除“操作成功但确认未收到”的窗口。检查点也有这一边界，见[检查点参考](../reference/checkpoint.md)。

## 超时等待业务体退出

`Timeout(d)` 为内层设置截止时间；到期会取消内层上下文，切面始终等待内层函数退出后才返回。若业务体忽略上下文且运行 300 ms，即使时限为 40 ms，也可能到业务体退出后才返回。超时限制的是允许继续工作的时间，不是强制终止 goroutine。这种等待确保节点收尾不会与仍在运行的业务体并发写同一输出。

内层退出后，Timeout 统一复查自己的上下文：已到截止时间时，返回包含节点名和时限的错误，该错误为 `failed`，不包装 `context.DeadlineExceeded`。即使内层已经观察到到期却返回 nil，或退出与到期同时就绪，也不会把超时记成成功。未到期且未取消时保留内层原错误；父侧取消则返回上下文取消原因。

因此不能只凭 context 哨兵或 `canceled` 状态认定宿主中断，应同时核对宿主上下文和安装的时限。`Timeout(0)` 设置已到期的时限，不表示关闭超时；需要关闭时限时不要安装该切面。

普通执行中已经即时发布的输出不会因 Timeout 撤回。需要隔离失败尝试时使用 Retry 的写集；需要完整链确认后发布时使用检查点模式。

## 取消一次运行

以下宿主片段需要现有的 `g`、`in` 和 `ctx`，放在返回 `error` 的函数中：

```go
e, err := g.NewExecution(ctx, in)
if err != nil {
	return err
}
defer e.Cancel()
if err := e.Start(); err != nil {
	return err
}
// 应用决定停止本次运行。
e.Cancel()
return e.Wait()
```

`Cancel` 发出取消信号，`Wait` 等全部节点和回调结束；不能用 Cancel 代替 Wait。取消本次执行不会改变定义，也不影响同一图的其他执行。HTTP、数据库和业务循环都应使用 `rc.Context()`；取消无法强制中断不配合的阻塞操作。

## 子图、图级切面与流

子图挂载点上的 Timeout 为整张子图设置时限，包含子节点等待额度的时间。挂载点上的 Retry 每次创建新的子执行；失败尝试的父侧桥回输出随写集丢弃。

`WithAspects` 的图级切面会包裹本层每个节点，也会继承到子图节点。相同 Timeout 会同时限制普通节点、整个子图挂载步骤和子图内部节点；需要不同尺度的时限时，应分别放在节点或挂载点。`WithoutAspects` 只截断子图内部的继承，不移除父侧挂载步骤上的切面。

`Produce`、`Consume`、`Tee` 都通过 `WithStreamAspects(aspects...)` 接收节点级切面，使用与普通节点相同的执行链。可以放日志、观测或协作式 `Timeout`；到期后等待内层退出，执行保留失败，已经发送的前缀不会撤回。宿主上下文仍控制整轮总时长。

图级、执行级或节点级 Retry 等实际写暂存作用于 Produce/Tee 时，仍在运行期拒绝：channel 必须在生产者结束前发布，而写集只能在尝试结束后发布，两者互相等待。消费者上的普通 Retry 重跑整段循环，无法倒带已经取走的当前消息，不能代替消息级重试。需要重做有限流水线时，可在只向父侧桥回普通结果的 Sub 边界重试。详见[流式处理](./streams.md)。

## 常见错误与下一步

自定义切面不得并发调用 `next`，也不得通过连续调用 `next` 自行实现重试；缺少新的尝试边界会触发 `ErrNextCalledTwice`。使用内置 Retry，并让自定义切面每次至多推进一次或明确短路。

需要复用完整处理阶段，继续阅读[静态子图](./subgraphs.md)。错误分类与后续处理见[错误参考](../reference/errors.md)，状态和回调顺序见[生命周期参考](../reference/lifecycle.md)。
