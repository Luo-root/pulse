# 重试、超时与取消

本页面向需要调用外部服务、处理短暂故障或控制运行时长的开发者。前置条件是理解[输入与输出](./inputs-outputs.md)和正常跳过语义，并使用 v0.5.2 API。

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

`Retry(attempts, delay)` 的 `attempts` 包含首次调用；`attempts <= 0` 按一次处理。普通业务错误会重试，最终返回最后一次错误；正常跳过、本层上下文已取消或到期时不继续重试。

错误链包含 `CheckpointError` 时立即停止，即使本图没有启用检查点。例如业务体返回外部工具的可靠提交失败，`Retry(3, 0)` 也只执行一次。可靠操作可能已经写成、但没有拿到确认；继续重试会重复不确定的操作，应由宿主读取权威记录后决定如何恢复。

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

仅把外部操作放在函数末尾，不能消除“操作成功但确认未收到”的窗口。需要避免重复时，使用稳定的业务幂等键或可靠去重；采用防重记录时，业务变更与防重记录须在同一原子边界内提交，单独一笔业务事务并不足够。

outbox 须可靠地共同提交业务变更与消息，保持事件身份稳定，并配合消费端幂等或去重；补偿是事后修复，不能保证不重复。检查点也有这一边界，见[检查点参考](../reference/checkpoint.md)。

## 超时等待业务体退出

`Timeout(d)` 为内层设置截止时间；到期会取消内层上下文，切面始终等待内层函数退出后才返回。若业务体忽略上下文且运行 300 ms，即使时限为 40 ms，也可能到业务体退出后才返回。超时限制的是允许继续工作的时间，不是强制终止 goroutine。这种等待确保节点收尾不会与仍在运行的业务体并发写同一输出。

内层退出后，Timeout 判断本层时限与父域取消：本层时限真正到期时，返回包含节点名和时限的错误，该错误为 `failed`，不包装 `context.DeadlineExceeded`。即使内层已经观察到到期却返回 nil，或退出与到期同时就绪，也不会把超时记成成功。内层用 `rc.Cancel()` 清理局部上下文时，保留已经取得的成功、正常 skip 或真实失败；父域取消仍返回上下文取消原因。

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

`ProduceStream`、`ConsumeStream`、`TeeStream` 及旧原生流糖都通过 `WithStreamAspects(aspects...)` 使用普通节点切面链。日志、观测与协作式 Timeout 可用；到期等内层退出，真实失败保留，已经发送的前缀不会撤回。

受管流用 `HandleMessage` 显式重试当前条，或给 ConsumeStream/TeeStream 配置 `WithMessageHandling(WithMessageRetry(...))`。一条失败只重做这一条，暂存的失败输出不发布；默认一次、最多 64 条，每个批次不得超过输出容量，超限立即失败。耗尽失败不能吞掉，闭包、引用与外部副作用不回滚。逐 token 需在此有限边界外逐条 Send/SendAt，按显式来源游标续读或新建 Execution，不自动拼接两次生成。

普通 Retry 的写集不能倒带实际外接流输入或撤销已发布出口，在 I/O 前拒绝；确定 skip 没有数据仍正常。完整有限流水线全部封在 Sub 内、只桥普通结果时可整体重试，但内部节点不能继承该 Retry。旧 Produce/Tee 仍拒绝延迟发布 channel 的写暂存；旧 Consume 的整节点重跑不提供 Ack 或倒带保证。完整例子和迁移矩阵见[流式处理](./streams.md)。

## 常见错误与下一步

自定义切面不得并发调用 `next`，也不得通过连续调用 `next` 自行实现重试；缺少新的尝试边界会触发 `ErrNextCalledTwice`。使用内置 Retry，并让自定义切面每次至多推进一次或明确短路。

需要复用完整处理阶段，继续阅读[静态子图](./subgraphs.md)。错误分类与后续处理见[错误参考](../reference/errors.md)，状态和回调顺序见[生命周期参考](../reference/lifecycle.md)。
