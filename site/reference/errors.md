# 错误

本页面向处理装配、启动或运行错误的开发者，适用于[v0.5.2](./migration.md)。先确认错误发生的阶段，再决定修正定义、补充输入、处理业务失败或恢复持久记录。不要依据错误字符串唯一判断行为；公开哨兵使用 `errors.Is`，具体类型使用 `errors.As`。

## 哨兵错误与处理步骤

| 错误 | 含义 | 下一步 |
|---|---|---|
| `ErrUndeclared` | 节点访问未声明键、写非 Provides；宿主读取未声明或类型不符的出口 | 对照 Requires/Provides 或 DeclareOutput，复用正确 Key 对象 |
| `ErrConflict` | 一个槽位已经 ready 又写 skipped，或已经 skipped 又写 ready | 每条输出明确一种状态；检查 Only 是否与其他写入混用 |
| `ErrDuplicateSource` | 同键有两个节点生产，或输入端口与内部生产者冲突 | 保留一个来源，为独立结果使用不同键 |
| `ErrGraphStarted` | 重复启动同一 Execution，或修改已封闭的 Graph | 再运行创建新 Execution；换拓扑创建新 Graph，报文区分两种情况 |
| `ErrGraphNotStarted` | 成功 Start 之前调用 Wait | 检查 Start 错误；只有成功启动后才 Wait |
| `ErrNextCalledTwice` | 切面重叠推进，或没有新尝试边界的再次推进 | 不并发调用 next；重试使用内置 Retry |
| `ErrSkipped` | 正常无值到达或节点主动无值 | 按缺值策略处理，不能当作普通故障重试 |
| `ErrStreamStopped` | 读取组显式 Stop 的生产停止信号 | 手写生产者明确处理；ProduceStream 仅在实际 stopped 且回调返回精确哨兵时正常结束 |
| `ErrDeliveryExpired` | Nack/Stop 已使该交付失效 | 使用新的 Delivery，不以旧凭据推进确认 |

`ErrNextCalledTwice` 的两种触发共用哨兵，报文说明具体原因。再次运行同一定义不属于节点重入；每个 Execution 都有独立推进状态。

## 正常跳过

`Get`、`WaitAll` 和 `Batch.Get` 可返回 `*SkipError`，其中 Keys 是正常无值的键名。错误链满足 `errors.Is(err, pulse.ErrSkipped)`。普通节点将它返回后以 skipped 收尾，未发布输出补为 skipped，整轮不会因此失败。

`Skip(rc, key)` 只跳过一条输出；节点返回 nil 仍是 completed。`NoValue` 表示整个节点本次正常无值。真实业务故障不得转换为 NoValue；这会让调用方失去失败原因。

单层包装的 skip、以及每个分支都正常 skip 的 `errors.Join` 或多 `%w` 错误仍是正常到达。混合错误不同：`errors.Join(pulse.NoValue(), businessErr)` 含真实失败，整棵树仍失败，不因其中一条路径匹配 ErrSkipped 而成功。包装层自定义 Is 也不能盖住 Unwrap 中的失败分支；无内层原因的叶子仍可通过 Is 声明正常跳过。Retry 会按真实失败继续尝试，失败写集不发布；检查点不把这次失败保存成成功或 skip。普通模式下失败前已直接发布的值不回滚。

可靠检查点错误和 panic 即使包装 ErrSkipped 也仍是失败。`errors.Is` / `errors.As` 只能说明某条路径匹配，不能据此给混合错误或执行终态定性；应用展示应读取下面的实际执行结果。

## 按实际执行终态分类

以下函数可放入导入 `errors` 和 `pulse` 的程序，用于应用展示。成功 Start 并等待 Wait 返回后再调用；Snapshot 中的图级终态已经区分完成、失败和取消。它不决定持久重试预算：

```go
func classify(e *pulse.Execution) string {
	state := e.Snapshot().Graph
	switch state.Status {
	case pulse.GraphStatusCompleted:
		return "success"
	case pulse.GraphStatusCanceled:
		return "canceled"
	case pulse.GraphStatusFailed:
		var checkpointErr *pulse.CheckpointError
		if errors.As(state.Err, &checkpointErr) {
			return "checkpoint failure"
		}
		return "failure"
	default:
		return "not finalized"
	}
}
```

正常跳过不会让整轮 Wait 返回错误。节点内部需要判断缺值时，可对本次 Get / WaitAll 的返回值用 `errors.As` 匹配 `*pulse.SkipError`，先排除 `CheckpointError`，其余错误原样返回。这个局部缺值判据不能用于给 Wait 的结果定终态：`panic(pulse.ErrSkipped)` 是失败，执行上下文仍有效时独立 RPC 包装的 `context.Canceled` 也是失败，不能只凭错误链将它们降格。

CheckpointError 保留 Operation 与原始原因。钩子返回错误或 panic 会停止整棵执行树；父 Retry 或外层切面不能通过吞掉该错误使执行成功。存储可能已经写成却未返回确认，因此停止后应重新读取权威记录，而不是直接重新 Commit 或清空历史。

判断是否为宿主中断时要检查实际 Execution 上下文。业务 RPC 可能包装 context.Canceled，但执行上下文仍有效，此时仍是需要确认的业务失败；不能只凭错误链给恢复刷新预算。详见[检查点](./checkpoint.md)。

## 没有专用哨兵的错误

并非每个契约错误都有公开哨兵。以下错误需要结合发生阶段和报文定位：

| 阶段与问题 | 下一步 |
|---|---|
| New 缺少图 ID，Add 节点重复 ID、空 ID 或键类型不一致 | 修正构造参数与命名，重新装配 |
| Seed 的 nil 输入指针、零键、nil 接口或重复条目 | 使用零值 Inputs 的地址、完整 Key、SkipSeed 或新输入集合 |
| 输入不是声明入口，或本次缺少必需入口 | 修正端口并准备完整 Inputs，创建新执行 |
| Requires 无来源、出口无人生产 | 添加生产者或正确端口声明；修正键名 |
| 依赖环或子图组合环 | 调整结构；无 Remove API，已装入的环需要重建定义 |
| 子入口漏绑、重复绑定或方向/类型错误 | 核对 In/Out/Fix 与子端口 |
| 流无人消费、多独立读取者、原生额度不足或生命周期环 | 补真实消费者/启动前宿主订阅，使用 TeeStream/FanOut，按原生或受管边界调整额度与完成依赖 |
| ReadOutput 未收尾 | 成功 Start 后 Wait；该错误不是 ErrGraphNotStarted |
| YAML 装配、注册表或输入解析错误 | 先检查 GraphID、uses、name/type，再检查 resolver 和值类型 |
| 检查点模式缺 scope/钩子，含 channel，恢复计划不合法 | 修正集成配置、值或恢复集合；启动失败不会部分恢复 |

静态问题可先从 Topology.Problems 查看全部，再用 Validate 确认。Validate 成功会封闭定义；不要为了查询问题而忽略封闭边界。

## 受管流的具体错误

Messages 只支持单读取者；FanOut 多 worker 用 Receive。未打开端点、未声明读取、提前退出却未履行读取义务、缺少完整 StreamHooks、活跃来源没有续读承诺，以及 HandleMessage 输出超限，均给出具体错误。超限立即失败，不等自己的回调；消息耗尽或提交失败会锁存，吞错也不成功。

ConsumeStream 回调返回精确 ErrStreamStopped 时先成功 Ack 当前条再 Stop；包装后的停止错误仍失败。普通 Retry 不能撤回实际流输出或倒带输入，确定 skip 无数据仍正常。仅宿主消费的流先登记 SubscribeStream，再 Start；Graph.Validate/Topology 允许声明的 managed 根出口等待宿主连接，仅省略其缺读者诊断，不改变未导出流或原生 channel 检查。Start 仍按实际订阅严格查缺读者与分流，定义通过不替代启动检查。订阅透传入口缺 Seed/SkipSeed 会在 Start 前报错。

## 运行失败、取消与输出

节点真实错误会锁存首错并取消执行，Wait 默认在所有节点结束后返回该原因。其他并发节点可能完成或取消，普通错误不覆盖已捕获首错。可靠模式有一条例外：Wait 会合登记的可靠流工作后，优先采用此时已锁存的树级 CheckpointError，并将图归为 failed；Err、GraphFinished、Snapshot 和并发/重复 Wait 复用这一最终结论，不重新遍历用户错误。各子执行按自己的收尾边界确定结果，已确定的终态不被其他执行稍后的失败改写。业务体 panic 会转换为节点错误。引擎错误分类期间，自定义错误的 Is/As/Unwrap 方法 panic 也会转为节点失败，并停止遍历该故障 error；这一保护不覆盖用户自建 goroutine 的 panic。

Timeout 等内层退出后判断本层时限与父域取消：本层时限真正到期时返回包含节点名与时限的错误，状态为 failed，且不包装 context.DeadlineExceeded；两条结束路径同时就绪也遵守这一判据。内层局部 Cancel 清理不覆盖既得的成功、正常 skip 或真实失败；父域取消仍保留上下文取消原因。canceled 仍应结合宿主上下文、节点时限与实际错误原因判断。

宿主取消、宿主截止时间到期或执行首错导致的取消，也可以表现为 canceled。取消是协作式的，不响应上下文的业务体可能延迟 Wait 返回；Timeout 同样等内层退出，不会强制终止业务 goroutine。

ReadOutput 首先核对出口声明，随后检查执行结果。合法出口在未收尾或真实失败/取消时不给部分值；错误返回时 value/state 均无效。skipped 和可选透传 pending 的正常结果均不报错。

## 观察出口错误

Observer 回调没有错误返回，panic 被隔离。观察出口失败不会自动变成业务失败。LineSink 的 Flush/Err、AsyncSink 的 Flush/Close 等由宿主检查；NewRecordObserver 的 `observe.ErrNilSink` 表示缺少出口，需要提供非 nil Sink。

## 下一步

返回[输入与输出](../guide/inputs-outputs.md)检查宿主边界，或阅读[重试、超时与取消](../guide/retry-cancellation.md)选择执行策略。恢复错误处理见[检查点参考](./checkpoint.md)。
