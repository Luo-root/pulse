# 生命周期

本页面向控制 Execution、实现 Observer 或解释快照状态的开发者，描述[开发版](./migration.md)的精确契约。可运行示例见[输入与输出](../guide/inputs-outputs.md)、[子图](../guide/subgraphs.md)和[观测](../guide/observability.md)。

## 定义的装配与封闭

Graph 的生命周期是创建、装配、封闭；它不保存本次运行的上下文、值或首错。

| 动作 | 检查或作用 | 是否封闭定义 |
|---|---|---|
| `Add`、端口声明、`Sub` | 登记节点、类型、唯一来源或挂载绑定 | 否 |
| `Topology` | 读取结构、端口、绑定、子树与 Problems | 否 |
| `Validate` 失败 | 返回结构问题 | 任何层都不封闭 |
| `Validate` 成功 | 完整静态校验 | 封闭本图及整棵挂载子树 |
| `NewExecution` | 收下输入并计算有效配置 | 否 |
| `Start` 失败 | 返回结构、输入或恢复计划错误 | 不因本次失败封闭 |
| `Start` 成功 | 校验后开始本次运行 | 封闭整棵定义树 |

封闭后，Add、DeclareInput、DeclareOutput 和 Sub 修改定义会返回包装 `ErrGraphStarted` 的错误。已经封闭的子图仍可挂到其他未封闭父图，定义也能顺序或并发运行多次。校验与封闭使用同一装配边界，封闭的就是被校验的定义。

静态校验检查 Requires 来源、声明出口的生产者或透传入口、依赖环、挂载绑定及可见流式关系。它不执行回调，不验证业务函数的计算正确性。只需问题列表时使用 Topology；Validate 成功会改变装配状态。

## 一次执行的控制

`NewExecution(ctx, in, opts...)` 创建一次执行，并复制输入条目。它检查每条被 Requires 使用的外部入口是否有值或显式跳过。有效配置在创建时确定；输入条目后续变更不影响该执行。

Start 在封闭前按当前定义再次检查本次输入。通过后同步发 GraphStarted，再为节点启动 goroutine。Start 只允许成功一次；再次调用返回 ErrGraphStarted。失败启动不标记 started，也不产生启动事件。结构可以补充时可修正后再启动；需要补充输入时创建新执行。

Wait 必须在成功 Start 后调用，否则返回 ErrGraphNotStarted。它等待所有节点终止，以及 Start 前登记的必需宿主流读取组完成并 Ack 或明确 Stop，并会合登记的可靠流工作，随后锁存最终结果，发 GraphFinished，再释放执行上下文。可靠模式优先采用此时已锁存的树级 CheckpointError，图归为 failed；否则保持普通首错或真实取消。Err 与图级观测复用该最终结果。各子执行在自己的收尾边界应用同一规则，已经确定的终态不被其他执行稍后的失败改写。Wait 不替宿主排空；只读宿主流应先 NewExecution → SubscribeStream → Start，持续读取确认后再 Wait。并发或重复 Wait 返回同一锁存结果，并都等待 GraphFinished 回调返回；正常资源释放的 cancel 不会把成功改成取消。

只 Start 而不 Wait，即使节点已结束，图快照仍显示 running，也不会产生 GraphFinished。Cancel 只发送信号，不能代替 Wait。

## 图、节点与槽位状态

| 对象 | 状态 |
|---|---|
| 图执行 | `pending`、`running`、`completed`、`failed`、`canceled` |
| 节点 | `pending`、`waiting`、`running`、`completed`、`skipped`、`failed`、`canceled` |
| 数据槽位 | `pending`、`ready`、`skipped` |

图级没有 skipped。全部节点正常跳过时，执行仍为 completed。

节点正常路径为 pending → waiting → running → 终态；全输入跳过时直接从 waiting 到 skipped。切面短路与恢复节点可以只出现终态，不能据此补造等待或执行段。状态先更新，再发对应回调；Retry 不使状态倒退。

槽位从 pending 一次性到 ready 或 skipped。两者都是到达，不会重新打开。重复写相同状态保留首次写入；相反状态的写入返回 ErrConflict。输入集合的重复写则提前拒绝，不使用槽位幂等来静默接受。

## 节点进入业务体的条件

1. 等待全部 Requires 到达：ready 或 skipped。
2. 若有输入且全部 skipped，节点正常跳过。
3. 否则申请运行额度；挂载子图步骤自身不占额度。
4. 在取消检查通过后进入业务体。

以上输入门用于普通业务节点。Sub 无整体输入门：In 稳定绑定 pending/ready/skipped 来源槽，内部各节点自行判门，全入口 skipped 也创建 child，独立自产分支仍可运行。实时 Stream/原生 channel Out 提前绑定，普通 Out 等 child.Wait 成功后桥回；挂载本身仍等 child 终结且不占额。

无 Requires 的源节点不适用“全部跳过”的判断。等待输入和额度均可被上下文取消。受管流阻塞等待暂时让额，恢复业务（含取消返回与 panic 展开）前取回；同一 RunCtx（含 Fork）不得并发 I/O。默认汇聚允许部分有值；要求全部有值的节点必须显式返回 WaitAll 的缺值结果。已经发布的值不会因稍后正常跳过撤回。

未写出的 Provides 在收尾时变为 skipped。节点返回 nil 仍是 completed；返回正常 SkipError 是 skipped；真实错误捕获为首错并取消执行。并行错误的首个捕获顺序不由声明顺序保证；最终执行结果遵守上文的可靠首错优先规则。

## 受管流的两类完成

流槽位 ready 表示端点可用，不表示消息发完。实际写端完整 Aspect 链结束才关闭 Done，并设置 completed/failed/canceled/stopped 与 Err；祖先 Sub 不是 EOF 边界。Snapshot 另给连续确认前沿、在途量和读取组 Stopped。Done 可以先于尾部 Ack，child 成功清理不撤销尾部。

Receive 不确认；Ack 释放容量，Nack 同序重投，取消不自动 Ack。正常生产结束且读取义务已清，或显式 Stop 后才返回 io.EOF；真实失败保留原因。Stop 清理未确认消息，不推进确认前沿。原生 Messages 只支持单读者，select 等待用只等待的 WaitStream 回调；FanOut 竞争用 Receive。完整使用方式见[流指南](../guide/streams.md)。

## Observer 次数与顺序

以下次数针对成功启动且最终等待结束的执行：

| 回调 | 次数 | 调用位置 |
|---|---|---|
| GraphStarted | 1 | Start 调用方 goroutine，任何节点回调之前 |
| GraphFinished | 1 | Wait 调用方 goroutine，全部节点回调之后 |
| NodeWaiting | 每节点至多 1 | 节点 goroutine |
| NodeRunning | 每节点至多 1 | 节点 goroutine |
| NodeFinished | 每节点 1 | 节点 goroutine |

Retry 多次尝试不重复 Waiting 或 Running。启动校验失败不发图或节点事件。回调同步执行，可跨节点并发，Observer 必须并发安全并避免长时间阻塞。panic 被隔离，Observer 没有可影响运行的错误返回。

不要在回调里对同一执行调用 Start/Wait；回调位于它们的调用链上，可能发生自等待。GraphFinished 时最终结果已确定，可以调用 ReadOutput。

## 可选切面事实

`AspectObserver.OnAspectEvent(AspectEvent)` 补充实际切面控制点，不扩充 Observer 的五方法或改变上表次数。通过同一 WithObserver/WithExecObserver 安装，沿执行环境继承；MultiObserver 只向实现了该接口的成员隔离扇出。回调在引擎锁外执行，可读 Snapshot，panic 不改变业务结果。

Retry 在实际分配/认回游标后报告 attempt_started，结束时报 attempt_finished；只有真正分配后续尝试才报 retry。正常 skip 不重试、不记失败。Timeout 的 deadline 来自自己的计时器 cause，父取消或外层到期不会冒充本层；finished 在内层确实退出后发出，panic/可靠失败保持 failed。NamedAspect 显式登记自定义切面，只自动报告进出。未配置观察者时不输出日志。

`RunCtx.Site()` 与回调、运行快照共享真实 Site。边界号只在本次节点执行内唯一，直接父边界和父尝试另外关联；持久 Scope 与恢复标记来自检查点桥，完整恢复不补历史。字段及阶段见[observe 包参考](../packages/observe/index.md)。

## observe 的事件与耗时

| 事件 | 产生条件 | Duration |
|---|---|---|
| `pulse.graph_started` | 图开始 | 0 |
| `pulse.graph_finished` | Wait 收尾 | 两个图级回调间的墙钟耗时 |
| `pulse.node_wait_finished` | 等待结束、进入执行或在等待后终结 | 等待段 |
| `pulse.node_run_finished` | 已进入业务体后的终结 | 执行段 |
| `pulse.node_finished` | 没有 Waiting/Running 的失败或取消 | 0，未测量 |
| `pulse.aspect` | 可选切面事实 | 实际调用、尝试、等待或到期报告的耗时；进入和 retry 为 0 |

正常 skipped 的 Record.Err 为 nil。只有 Finished 的 completed/skipped 不生成节点耗时记录，包括恢复和成功短路。切面在 next 前失败时用 node_finished 保留归因。

耗时包括同步观察回调和出口在计时窗口内的开销，不是纯业务 CPU 时间。图级耗时不包含装配、输入准备和 Wait 返回后的 Flush。

## 视图与恢复边界

Snapshot 可在创建后、运行中或收尾后读取。节点和槽位分别采样，子执行句柄为活对象，因此不是整树的原子读数，也不是持久化记录；所有进度视图都不携带业务值。

恢复节点在 GraphStarted 后仅迁移终态并发 Finished，不重新执行体、切面或 Commit。已恢复的父挂载节点不创建子执行，因此不会补造 Child 或子图事件。可靠恢复契约见[检查点](./checkpoint.md)。

## 常见错误与下一步

将 Graph 的封闭与 Execution 的启动混为一谈，会错误地拒绝第二次运行；应创建新的 Execution。忘记 Wait 会遗漏图终态与收尾；在观察回调中等待自身会阻塞。错误含义与修正步骤见[错误参考](./errors.md)。
