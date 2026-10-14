# 错误

本页面向处理装配、启动或运行错误的开发者，适用于[开发版](./migration.md)。先确认错误发生的阶段，再决定修正定义、补充输入、处理业务失败或恢复持久记录。不要依据错误字符串唯一判断行为；公开哨兵使用 `errors.Is`，具体类型使用 `errors.As`。

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

`ErrNextCalledTwice` 的两种触发共用哨兵，报文说明具体原因。再次运行同一定义不属于节点重入；每个 Execution 都有独立推进状态。

## 正常跳过

`Get`、`WaitAll` 和 `Batch.Get` 可返回 `*SkipError`，其中 Keys 是正常无值的键名。错误链满足 `errors.Is(err, pulse.ErrSkipped)`。普通节点将它返回后以 skipped 收尾，未发布输出补为 skipped，整轮不会因此失败。

`Skip(rc, key)` 只跳过一条输出；节点返回 nil 仍是 completed。`NoValue` 表示整个节点本次正常无值。真实业务故障不得转换为 NoValue；这会让调用方失去失败原因。

可靠检查点错误即使包装 ErrSkipped 也仍是失败。需要分类时，必须先判断 CheckpointError。

## 可靠错误优先分类

以下函数可放入导入 `context`、`errors` 和 `pulse` 的程序，用于应用展示。它只给错误分类，不决定持久重试预算：

```go
func classify(err error) string {
	if err == nil {
		return "success"
	}
	var checkpointErr *pulse.CheckpointError
	if errors.As(err, &checkpointErr) {
		return "checkpoint failure"
	}
	if errors.Is(err, pulse.ErrSkipped) {
		return "normal absence"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "cancellation or deadline"
	}
	return "failure"
}
```

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
| 流无人消费、多独立读取者、容量不足或生命周期环 | 补真实消费者，使用 Tee/FanOut，调整额度或跨图完成依赖 |
| ReadOutput 未收尾 | 成功 Start 后 Wait；该错误不是 ErrGraphNotStarted |
| YAML 装配、注册表或输入解析错误 | 先检查 GraphID、uses、name/type，再检查 resolver 和值类型 |
| 检查点模式缺 scope/钩子，含 channel，恢复计划不合法 | 修正集成配置、值或恢复集合；启动失败不会部分恢复 |

静态问题可先从 Topology.Problems 查看全部，再用 Validate 确认。Validate 成功会封闭定义；不要为了查询问题而忽略封闭边界。

## 运行失败、取消与输出

节点真实错误会锁存首错并取消执行，Wait 等所有节点结束后返回该原因。其他并发节点可能完成或取消，不会覆盖已捕获首错。业务体 panic 会转换为节点错误。

Timeout 自行处理截止时间时，返回包含节点名与时限的错误，该错误为 failed 且不包装 context.DeadlineExceeded。先取得内层结果时，保留内层原错误；内层退出与截止时间同时就绪时，可能直接返回 context.DeadlineExceeded，并呈现 canceled。因此 canceled 不能单独证明宿主取消，应结合宿主上下文、节点时限与实际错误原因判断。

宿主取消、宿主截止时间到期或执行首错导致的取消，也可以表现为 canceled。取消是协作式的，不响应上下文的业务体可能延迟 Wait 返回；Timeout 同样等内层退出，不会强制终止业务 goroutine。

ReadOutput 首先核对出口声明，随后检查执行结果。合法出口在未收尾或真实失败/取消时不给部分值；错误返回时 value/state 均无效。skipped 和可选透传 pending 的正常结果均不报错。

## 观察出口错误

Observer 回调没有错误返回，panic 被隔离。观察出口失败不会自动变成业务失败。LineSink 的 Flush/Err、AsyncSink 的 Flush/Close 等由宿主检查；NewRecordObserver 的 `observe.ErrNilSink` 表示缺少出口，需要提供非 nil Sink。

## 下一步

返回[输入与输出](../guide/inputs-outputs.md)检查宿主边界，或阅读[重试、超时与取消](../guide/retry-cancellation.md)选择执行策略。恢复错误处理见[检查点参考](./checkpoint.md)。
