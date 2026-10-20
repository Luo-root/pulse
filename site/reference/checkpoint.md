# 检查点

本页面向实现持久执行集成、存储或 Codec 的开发者。需要理解[生命周期](./lifecycle.md)、[重试写集](../guide/retry-cancellation.md)和[静态子图](../guide/subgraphs.md)，并使用[开发版](./migration.md)。普通业务宿主需要恢复时，优先使用独立的 [pulse-durable](https://github.com/Luo-root/pulse-durable) Run/Resume，而不是自行拼装可靠钩子。

Durable 提供检查点 Run/Resume、本机 filestore，以及受管流的存储与进程恢复，集成跟踪见 [Durable #21](https://github.com/Luo-root/pulse-durable/issues/21)。本页描述 Core 桥契约；运行时请使用匹配的本地检出，发布状态以维护者的 tag/release 为准。持久等待、分布式运行管理属于后续能力。依赖单向：Durable 导入 Pulse，Pulse 不依赖 Durable。

## 确认后发布

启用 `WithExecCheckpoint(scope, hooks)` 后，整棵执行树使用检查点模式。它独立于 Aspect，不能通过 WithoutAspects 关闭子树中的可靠提交。

一次未恢复节点按以下顺序执行：

1. 同步打开或认回完整节点链的持久尝试。
2. 在整个切面链之外创建输出写集，体与切面的 Set/Skip 都先暂存。
3. 完整链成功或正常跳过后，补齐所有未写 Provides 为 skipped；普通业务节点成功短路也等待声明依赖到达；Sub 按实时 In 接线规则提交。
4. 核验完整终态、每条输出及发布目标。
5. 同步调用 Commit，由持久层确认完整节点记录已经保存。
6. 确认成功后逐键发布输出，允许下游看到成果。

即使内层 Retry 成功，只要外层切面随后失败，完整链的输出仍不发布。节点返回 nil（含逐键 Skip）为 completed，NoValue 或全输入跳过为 skipped。正常跳过也要保存完整成果；零输出节点同样保存终态。

持久层应将终态和全部输出原子保存为一个记录。确认一份完整记录不意味着多个内存槽位同时可见；发布仍逐键进行。普通执行没有这层完整链暂存，保留即时 Set/Skip 的路径。

## 公开桥接入口

| 入口 | 用途 |
|---|---|
| `Graph.CheckpointNodes` | 递归复制静态节点、挂载路径、Requires/Provides 和真实 reflect.Type |
| `Graph.CheckpointPorts` | 复制全部声明端口，含未使用与透传端口 |
| `Execution.CheckpointInputs` | 复制本执行实际接收的外部输入，含 ready/skipped，不读取内部输出 |
| `WithExecCheckpoint` | 为本次执行及子执行启用完整链可靠模式；受管流另需 StreamHooks |
| `Execution.RestoreNodes` | 核验并替换启动前整份恢复计划，不立即安装输出 |

需要稳定 schema 时，先完成装配并成功 Validate。Graph.Validate/Topology 允许声明的 managed 根出口稍后接宿主，仅省略其缺读者诊断，不算实际 host，不放行未导出流或改变原生 channel。这样可先冻结 schema，再在 BeforeStart 订阅；Start 仍按真实订阅严格查缺读者与分流。返回容器与声明隔离，但业务值不深拷贝；集成层应同步编码，不得修改或异步保存引用后提前确认。

可靠钩子的实际类型如下，需要 `context` 及 Pulse 的相应载荷类型；这是接口定义，不是可直接使用的存储实现：

```go
type CheckpointHooks struct {
	OpenAttempt func(context.Context, CheckpointAttemptRequest) (CheckpointAttempt, error)
	FailAttempt func(context.Context, string) error
	OpenChild   func(context.Context, CheckpointChild) ([]NodeCheckpoint, error)
	Commit      func(context.Context, CheckpointCommit) error
	StreamHooks *CheckpointStreamHooks // 可选；提供时六项必须完整。
}
```

四个钩子必须全部提供，并使用非空根 scope。它们在引擎锁外同步调用，可以并发，必须线程安全。OpenAttempt 返回持久 Scope、Index 与 Exhausted，active 完整节点根 attempt 还可带下文的 Values 子集；FailAttempt 先确认真实失败，才允许下一次尝试；OpenChild 核对实际子图及输入并返回该作用域的恢复成果；Commit 确认完整节点链成果。

OpenChild 有一个严格的业务结果信号：桥在完整校验历史、可靠确认 scope 内已存在最终业务失败后，可直接返回 `*CheckpointScopeFailure{Scope: request.Attempt}`，且不返回成果。Core 在安装恢复计划之前将它交给父完整链，父 Retry 可按剩余预算推进，父切面也可接住它；不重新运行已确认别名或重开失败流。只有非 nil、非空且精确匹配请求的直接信号被接受；包装、错 scope、同时返回成果或其他钩子使用该类型仍是可靠失败。根 scope 由集成层在解码、恢复事件与 Start 之前返回相同业务失败；这时执行尚未启动，不应伪造完成状态。恢复不还原原业务错误文本或身份。

跨更深层 Sub 的已确认流别名还可恢复来源的已确认 failed descriptor，但只能建立不含消息、不会调用 Open 的本地只读失败端点；接收传播业务失败，不返回成功 EOF，不允许生产或确认。只有 Sub 别名可携带这种失败引用，普通来源节点不能以 failed descriptor 冒充成功成果；canceled 引用仍不作为已确认业务失败接受。集成层必须先核对历史身份、来源根 failed 和有效祖先，不得为了恢复放宽 Open/Send/Ack。

## 子图作用域与重试预算

持久 Scope 不等于挂载 Path，也不等于进程内 Execution ID。集成层为持久尝试分配稳定身份；同一挂载路径在不同父尝试下具有不同作用域。重启后活跃尝试认回同一 Scope/Index，已确认失败后才能推进 Index。

假设父步骤内有下载、提取、摘要三个子节点：下载和提取已确认后进程退出，Resume 创建新的父子执行，恢复前两步，摘要从函数开头重跑。若子节点全已确认而父步骤尚未确认，父包装与桥回重新执行，子业务不重做。

若父 Retry 明确失败并开始下一次尝试，旧尝试下的子成果不可借用。失败祖先使整支后代逻辑失效，不依赖历史记录已经被删除。OpenChild 核对 Graph、Path、Fix 实际值与 Bindings 的稳定 In 来源。In 可仍 pending，不等待全部输入；Bindings 内的键不参与值指纹，Inputs 中已到达 In 仅保留观察兼容。持久层结合根输入指纹、拓扑/Version 和绑定身份判兼容；变化必须拒绝混用旧成果，不能使用本次 Execution ID 作为稳定身份。

Retry 的持久边界由有效切面链位置和父 Scope 区分，不能从函数指针或本次调用次数猜测。根完整链 Limit 为 1；最终已确认失败后，Resume 不得给它刷新根作用域或重试预算。新的完整业务运行应使用新的 RunID。

## 恢复计划先整批核验

NodeCheckpoint 的 Reason 只能是 completed 或 skipped。Outputs 必须恰好覆盖该节点全部 Provides，含隐式 skipped；状态只能为 ready/skipped，键名、真实类型和值可赋值性必须匹配。pending 不是已确认成果。合法 nil 接口与 typed nil 保持 ready。

恢复集合必须对普通值依赖闭合：普通业务节点所依赖的内部普通生产者也要有恢复成果。受管流通过独立的来源记录恢复，不要求写端完整节点先完成；Sub 的 Requires 表达 In 接线，不是整个挂载点的完成门，未使用入口仍可 pending。RestoreNodes 核验并替换整份计划，失败保留原计划；Start 在校验与封闭同一边界按实际定义复验。全部通过才安装输出、封闭与启动，末条记录非法也不会部分恢复前面记录。

以下是供集成层使用的调用顺序；`g`、`ctx`、`in`、`scope`、完整 `hooks` 与已读取解码的 `records` 均由宿主提供，函数返回 `error`：

```go
e, err := g.NewExecution(ctx, in, pulse.WithExecCheckpoint(scope, hooks))
if err != nil {
	return err
}
defer e.Cancel()
if err := e.RestoreNodes(records); err != nil {
	return err
}
if err := e.Start(); err != nil {
	return err
}
return e.Wait()
```

已恢复节点不运行业务体、切面或 Commit。GraphStarted 后仅迁移终态并发一次 Finished，不伪造 Waiting、Running、未创建的 Child 或子图生命周期。Snapshot 不包含值，也不承担恢复计划职责。

桥仅在调用上下文已取消、且任何可靠操作尚未开始时，可直接返回 `*CheckpointNotStarted`。Core 还要求本次调用适用的业务域或控制域已取消，才将其保持为中断；同一父取消传到两域没有先后保证。两域仍健康、包装信号或普通 context 错误仍为可靠失败。尤其不能把已经调用 Store 后的错误改成此信号，即使原因是 context.Canceled，也可能写成未确认。此信号不保存业务失败或补充预算。

## 失败、中断与不确定确认

钩子或恢复协议的运行期失败使用 CheckpointError。即使错误链包含 ErrSkipped 或 context 哨兵，仍为可靠失败；整棵树共享失败锁存，父 Retry 或切面无法通过吞错让它变为成功。启动前的配置与计划错误则直接返回契约错误，且不启动。

宿主取消、进程退出、可靠钩子故障保留活跃尝试，由恢复时读取的权威记录决定下一步。节点本地 Cancel 后返回 nil 或正常 skip，若完整节点链自身的上下文已取消，也保留 active：不提交成功/跳过终态，不确认业务失败，不消耗持久预算。只清理内层局部上下文、完整链上下文仍有效时，保留既得结果；真实的本层 Timeout 与父域取消仍生效。

真实业务失败即使伴随节点本地清理，仍使用所属 Execution 的上下文确认失败，不因此刷新预算。业务 RPC 返回 context.Canceled/DeadlineExceeded、但所属执行上下文仍正常时，也属于业务失败。分类不能只查看 RPC 错误链。

可靠操作接纳前检查其适用的业务域和控制域，任一已取消时不开始新操作；真实失败确认仍以所属 Execution 为准，不受节点本地清理影响。接纳后的整个同步调用使用对应层的控制域，内部业务 fail-fast 立即唤醒业务节点，但不取消已进入的可靠调用。宿主取消、Execution.Cancel、父节点显式 Cancel 及该调用适用的 deadline/Timeout 仍能打断可靠等待，context values 保留。共享流 Open 归根执行所有，单次 Receive 取消只中断等待；宿主 Ack 使用宿主 Execution 的域，父端尾部确认使用实际调用者的域。存储实际写成却返回错误时，本次停止，不自动重试 Commit 或撤销记录。Resume 重读权威结果。已确认记录也不会因随后发生的取消被撤回。持久尝试无需保存可能包含业务敏感内容的原始错误文本。

完成或正常跳过的节点结论在可靠 Commit 前确定。Commit 与发布成功后，节点收尾和引擎观测复用已确认结论，不再检查同一业务错误对象来改判。宿主后续取消或其他节点的真实失败仍按各自的整轮终态处理，已确认记录保留。

## 受管流的增量确认

普通四钩子仍是合法配置；出现 `Key[Stream[T]]` 时必须额外提供完整 StreamHooks。它在引擎锁外同步调用，沿用并发安全、panic 隔离和可靠失败锁存要求：

| StreamHooks 项 | 必须确认的事实 |
|---|---|
| `Open` | 打开或认回 descriptor、消息尾部、连续确认前沿和来源游标 |
| `Append` | 原子保存消息及 SendAt 来源位置，确认后才能本地交付 |
| `OpenMessage` | 认回当前消息的持久尝试次数与上次已提交的显式状态 |
| `FailMessage` | 确认这次真实失败，之后才能使用下一次预算 |
| `Commit` | 一次确认输入 Ack、显式状态、普通值成果、输出消息或 Stop |
| `Finish` | 确认实际写端完整链的失败终态；成功终态随完整节点 Commit 原子保存 |

流消息、游标、确认、状态与输出分别记录，本地 channel 永不序列化。完整 NodeCheckpoint 的流输出使用 `CheckpointOutput.Stream` descriptor，与 Value 互斥；`CheckpointCommit.Streams` 只含实际写端终态，并与完整节点成果原子确认。Sub 的别名不拥有写端，不能提前发布 EOF。纯流透传 Sub 可以早于实际来源完成，历史引用的空终态不表示来源已经结束。恢复时必须通过权威 Open 认回来源：已知成功终态不能倒退，活跃来源必须 Resumed 且声明可按 Position 续读。恢复别名和继续生产的写端共用整树中同一端点，Open、尾部安装及交付账只建立一次；后到的每条别名仍核验稳定来源、代际及已知终态。

HandleMessage 是显式有限事务：失败输出不发布，耗尽失败不能被吞成成功。默认一次、最多 64 条暂存，每个输出批次不超过流容量。输入确认、`GetMessageState` / `SetMessageState` 的显式状态、普通值成果和输出消息经一次可靠 Commit 后，才发布流消息并本地 Ack；普通 Set/Skip 仍并入完整链外帧，待完整链确认才发布。闭包与引用修改不回滚，外部副作用不在事务内。状态 API 仅用于持久 HandleMessage 回调，修改时构造新值并显式 Set。

若消息已经确认而完整节点尚未 Commit，`CheckpointAttempt.Values []CheckpointOutput` 允许 **active 完整节点根 attempt** 返回这些已确认消息贡献的普通输出子集，不含流端点。Core 在业务链前把它恢复到 checkpoint 外帧，不提前发布到执行槽，仍须完整 Node Commit 成功后才发布。它不是完整 NodeCheckpoint，也不补齐未贡献的输出；Retry attempt 和 OpenMessage 返回的嵌套 Attempt.Values 必须为空。无流路径与失败祖先使后代失效的规则不变。

流身份由 Scope/NodeID/Key/Group 与持久 Generation 构成，Execution ID 仅供观察。别名共享同一本账；旧代际交付不得推进新代际。AckedThrough 是连续前沿，不能用最大确认序号替代。持久消息预算按逻辑来源、读取组、序号、处理节点与父尝试识别，恢复提升代际也不补次数。

已 completed 的来源不重跑业务体，但恢复分支会在 Finished 之前重建端点、Done、Snapshot 与未确认尾部；正常 child 清理不撤销尾部。活跃来源必须通过 WithStreamSource 声明可按 Position/SendAt 续读，未承诺续读的 active source 明确拒绝恢复。有限消息重试与渐进 token 的使用区别见[流指南](../guide/streams.md)。

## 能力边界与常见错误

原生 channel 声明、原生流糖，以及普通值或接口/容器中隐藏的实际 channel 仍拒绝；受管端点也必须独立声明为 Key[Stream[T]]，不能藏进普通值绕过协议。静态声明在启动前递归检查，动态值在输入、恢复和提交边界检查。循环对象不会仅因循环结构被此检查拒绝，是否能编码由 Codec 决定。

Version、Codec ID/版本、拓扑兼容性、输入编码指纹与运行所有权由持久层负责。业务闭包、固定值与切面语义无法由 Topology 自动识别，改变语义时宿主必须更新业务版本。读取、解码或版本错误应停止，不能当作空历史。

MemoryStore 只证明进程内契约。本机 filestore 支持进程退出后的记录恢复和 OS 独占锁；它不承诺断电持久性、跨机器租约或分布式派发。

检查点不会恢复 Go 调用栈或任意代码行。未确认节点从函数开头重做，仍可能重复外部副作用，因此不承诺 exactly-once 或确定性回放。业务需使用幂等键、事务、防重或 outbox。

## 下一步

接入业务应用请继续查看 [Durable 文档与源码](https://github.com/Luo-root/pulse-durable)。核心桥接的完整设计背景位于[检查点设计](https://github.com/Luo-root/pulse/blob/main/docs/design/checkpoint.md)；错误处理见[错误参考](./errors.md)。
