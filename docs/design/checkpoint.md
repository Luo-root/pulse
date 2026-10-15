# 检查点桥接与子图恢复（方案 A）

服务 [Pulse #307](https://github.com/Luo-root/pulse/issues/307)、[Pulse #370](https://github.com/Luo-root/pulse/issues/370) 与 [Durable #2](https://github.com/Luo-root/pulse-durable/issues/2)。本文描述当前代码的实现契约：Core 提供完整节点链及可选增量流检查点桥；Durable 提供 Store/Codec、Run/Resume、方案 A 子图恢复、本机文件后端，以及受管流存储与进程恢复，集成跟踪见 [Durable #21](https://github.com/Luo-root/pulse-durable/issues/21)。使用与文档匹配的本地检出，发布状态以维护者实际 tag/release 为准。首次桥接发布目标仍是 Pulse v0.5.0 / Durable v0.1.0，发版由维护者决定。持久等待和分布式运行管理不在本次实现内。

## 谁使用这个入口

普通宿主继续使用 Graph、Inputs 与 Execution；需要业务结果时读取声明出口。需要检查点恢复的宿主使用独立 `pulse-durable` 的 Run/Resume。它通过本文的可靠钩子接入 Core，不复制槽位推进机制，也不需要访问内部黑板。

Graph 始终是可复用定义。每次 Run/Resume 创建新的 Execution；RunID、持久尝试身份和子图作用域由 Durable 保存，与进程内 ExecutionID 分开。Core 仍只依赖标准库。

## 两道提交关口

Retry 每轮开 attempt 写集，失败丢弃，成功交给外层写集。显式检查点模式再在完整 Aspect 链外开节点写集：即使内层 Retry 成功，外层切面事后失败，整个节点的输出仍不会发布。

完整链成功或正常跳过之后，Core 补齐每条 Provides 的最终状态；正常未写的是 skipped。返回 nil（含逐键 Skip）仍是 completed，NoValue 或全输入跳过是 skipped。零输出节点也有完整终态。

Core 先核验整批输出与发布目标，再同步调用 Commit；只有存储确认成功才逐键发布。检查点作为一份完整记录原子保存，不代表多个内存槽位在同一瞬间可见。未启用模式时，普通 Set/Skip 的即时发布路径保持不变。

成功短路的切面也要等声明输入到达后才提交检查点，保证恢复集合对依赖闭合。这不会伪造 Waiting/Running；它是可靠提交的要求，普通模式的短路行为不变。

## Core 的公开桥接面

| 入口或载荷 | 责任 |
|---|---|
| `WithExecCheckpoint` | 非空根 scope 与四个可靠钩子；作用于整棵执行树，不受 WithoutAspects 截断 |
| `CheckpointNodes` | 按静态挂载位置复制节点、Requires/Provides 和真实 reflect.Type；包含流式节点标记 |
| `CheckpointPorts` | 复制全部真实类型端口，包括未使用和同键透传端口 |
| `CheckpointInputs` | 复制本 Execution 已接收的外部输入，用于指纹；不提供内部输出读取 |
| `RestoreNodes` | 核验并替换整份启动前计划，不立即灌槽，失败保留原计划 |
| `OpenAttempt` | 同步认回 Scope、Index、Exhausted；仅 active 完整节点根 attempt 可带已确认消息贡献的普通输出 Values 子集 |
| `FailAttempt` | 同步确认真实失败，先确认再允许下一轮 |
| `OpenChild` | 按当前最深尝试打开子图，核对固定输入与稳定 Bindings 来源并取得恢复计划，In 可仍 pending |
| `Commit` | 保存完整节点终态和全部 ready/skipped 输出，再允许 Core 发布 |

完整类型定义及 godoc 见 `checkpoint.go`。声明与载荷容器均复制，真实业务对象不深拷贝；宿主负责同步和所有权。钩子须并发安全、同步编码并确认，不得修改业务对象或异步保存引用后提前返回成功。

钩子运行在 assemblyMu、Graph.mu、Execution.mu 之外。恢复的类型/状态检查不调用 Codec 或任何业务方法。

## A 怎样认回子进度

父图 P2 是“处理文档”，子图依次为 C1 下载、C2 提取、C3 摘要。

| 中断或失败位置 | 恢复行为 |
|---|---|
| C1/C2 已确认，C3 尚未确认时进程退出 | 新建父/子 Execution，认回原持久尝试；恢复 C1/C2，C3 从函数开头重跑 |
| 三个子节点全确认，父 P2 尚未确认时退出 | 重进父包装/切面与桥回，三个子业务都不重做 |
| 父 Retry 第一次明确失败，第二次开始 | 确认旧尝试失败后持久分配新 Scope；三个子节点属于新尝试，不借用旧成果 |
| 第一次失败已确认，尚未打开第二次时退出 | 从存储游标推进到第二次，不从 1 重新获得次数 |
| 第二次正在执行时退出 | 认回第二次 Scope/Index，保留该次已经确认的子成果 |

Scope 不是 Path 或 ExecutionID 的拼接。Durable 在原子存储变更中分配随机持久身份；同一挂载路径的不同父 attempt 有不同 Scope。每层 Retry 的边界来自有效 Aspect 链的固定位置，嵌套层再由父 Scope 区分，不使用函数指针或本次调用次数猜历史身份。

同一活跃尝试的子图再次打开时，核对 Graph、Path、固定输入与稳定绑定来源。CheckpointChild.Bindings 完整描述每条 In 的父 scope、有效 attempt 和父/子真实键；Source.Execution 仅供观察，不能充当持久身份。In 可以仍 pending，不能为等待全部值而恢复整体输入门；Bindings 中的键不参与值指纹。Inputs 含 Fix/FixSkip 与此刻已到达的 In，后者只为旧桥观察兼容保留。集成层结合根输入指纹、拓扑/Version、固定值和绑定身份判兼容；变化必须拒绝混用旧成果。失败祖先使后代逻辑失效，正确性不依赖旧记录已经被删除。

内层 Retry 成功后保持可认回，直到完整节点检查点确认；重启可重进包装代码以重建节点暂存。外层 Retry 明确失败后，它下面整支作用域失效。没有 Go 调用栈恢复或任意代码行续跑。

## 失败、取消与次数

根完整链的 Limit=1。未被切面处理的最终真实失败会确认根失败；以后 Resume 不能通过换根作用域刷新内层 Retry 次数，新的 RunID 才获得新的完整运行预算。内层失败若被外层 Retry 处理，外层按自己的持久次数继续。

宿主上下文取消、进程突然退出和可靠钩子失败留下活跃尝试，恢复时从权威记录继续。业务 RPC 返回 context.Canceled/DeadlineExceeded、但执行上下文仍正常时，是已知业务失败，仍要确认根失败；不能仅凭错误链含 context 哨兵推断宿主中断。失败确认使用该节点所属 Execution 的持久化上下文，与最终失败分类一致；业务 `defer rc.Cancel()` 只清理节点上下文，不阻断已知失败的确认，不让后续 Resume 重新获得根预算。

Store/Codec/恢复钩子的错误或 panic 是 CheckpointError。即使错误链含 ErrSkipped，仍必须失败，errors.Is/As 保留原原因。整棵执行树共享可靠失败锁存，父 Retry 或外层切面吞掉返回值也不能让运行继续提交并假称成功。

已知取消时不开始新提交；确认已经成功后，随后发生的取消不撤销记录。Store 实际写成却报错时，本次停止，不重试、不回滚，Resume 重读权威记录。持久尝试只记录失败状态，不自动保存可能含业务敏感值的错误文本；跨进程遇到耗尽状态返回明确的次数耗尽错误。

## 恢复先整批核验

RestoreNodes 检查本层节点归属与重复、completed/skipped 终态、恰好覆盖全部 Provides、ready/skipped 状态、真实 Go 类型、值可赋值性和依赖闭合。nil interface 与 typed nil 是合法 ready 值，pending 不是持久成果。

Start 在校验与封口同一临界区按实际定义再次核验计划，全部通过才灌槽、封口并启动。末条非法时，前几条也不灌入，不标 started、不发事件、不跑业务，不因本次失败封口。

合法恢复节点跳过切面、业务体及 Commit；输出先装好才允许业务 goroutine 进入。GraphStarted 之后仅迁移终态并发一次 Finished，不伪造 Waiting/Running、未创建的 Child 或子图生命周期。现有 observe 不把单独 Finished 当作真实业务耗时；Durable 通过自身 EventRestored/记录查询提供恢复事实。Snapshot 仍不含值，不作为恢复协议。

## 受管流增量桥（#370）

`CheckpointHooks.StreamHooks` 是可选扩展，普通四钩子配置继续合法；受管 `Key[Stream[T]]` 需要六项完整钩子，普通值中的 channel 禁令不放宽。钩子仍在引擎锁外同步调用，须线程安全，错误与 panic 走整树可靠失败锁存。

| 钩子 | 责任 |
|---|---|
| Open | 分配或认回稳定流/读取组身份、descriptor、Pending 尾部、前沿与 Cursor |
| Append | 消息内容和来源位置一起确认，之后 Core 才本地交付 |
| OpenMessage | 在回调前认回逻辑消息的持久预算和上次提交的显式状态 |
| FailMessage | 先同步确认这次失败，再允许下次尝试 |
| Commit | 原子确认 Inputs、State、Values、Outputs 与显式 Stops |
| Finish | 同步确认实际写端完整链的真实失败；成功随完整节点 Commit 原子确认 |

本地端点不交给普通 Codec 编码。消息、来源 Cursor、确认前沿、显式状态与输出有独立记录；完整 NodeCheckpoint 中 `CheckpointOutput.Stream` 与 Value 互斥，仅保存 descriptor。成功的实际写端终态通过 `CheckpointCommit.Streams` 与完整节点成果一次确认，之后才开放本地终态。祖先 Sub 只绑定别名，既不是写端也不是 EOF 边界。

HandleMessage 在有限回调内暂存 Set/Skip/Send，默认一次、总计最多 64 条且每输出批次不超过容量，超限 Send 当场失败，不等待自身提交。一次可靠 Commit 确认输入 Ack、显式 GetMessageState/SetMessageState 状态、普通值成果和输出消息，之后才发布流消息和本地 Ack；普通值也并入完整链外帧，仍待完整链确认才发布。失败尝试不发布，耗尽或提交失败锁存为失败，吞错无效。任意闭包、引用修改和外部副作用不回滚；显式状态修改应构造新值再 Set。

消息确认与完整节点确认之间退出时，`CheckpointAttempt.Values []CheckpointOutput` 仅允许 active 完整节点根 attempt 返回已确认消息贡献的普通输出子集，不包含流端点，也不是完整 NodeCheckpoint。Core 在业务链前恢复到 checkpoint 外帧，不提前发布到执行槽；仍待完整 Node Commit 成功后才发布，不补齐尚未贡献的输出。Retry attempt 和 OpenMessage 返回的嵌套 Attempt.Values 必须空。无流路径以及失败祖先使后代失效的规则不变。

持久身份为 Scope/NodeID/Key/Group 加 Generation，不能用本次 ExecutionID 代替。In/Out/透传别名共用同一本账；每次改记录要核验代际及全部祖先，旧代际交付不能推进新代际。AckedThrough 只跨连续确认，Pending 与 LastSequence 的补集表示已确认缺口；不能把最大 Ack 序号当作连续前沿。Stop 只结束读取义务，不伪装逐条确认。处理预算按持久逻辑来源、组、序号、处理节点与父尝试识别，恢复提升 Generation 也不补次数。

completed 来源不跑业务体，但 restored 节点分支在 Finished 之前重建端点、Done/Snapshot 和未确认尾部。写端完整链结束与尾部消费是两件事；child 正常 Wait 清理不会撤销尾部，也不影响父侧确认。活跃来源没有 `WithStreamSource(id, true)` 的续读承诺时，恢复必须明确拒绝。承诺可续读的业务自行从 `StreamWriter.Position().Cursor` 续读、用 SendAt 保存消息和外部位置，不能保存 Go 闭包或调用栈。逐 token 输出在有限 HandleMessage 之外逐条发布，已外发前缀不撤回；无法续读就失败或新建 Execution，不能自动整节点 Retry 拼接两版。

普通 Retry 在实际外接流的 I/O 之前拒绝；确定 skip 没有数据不升格失败。完整有限流水线封在 child 内、只桥普通值时，可在 Sub 边界重试，内部节点不继承该 Retry。普通四钩子与流六钩子均只定义可靠边界；存储、编码与进程恢复由 Durable 提供，集成跟踪见 Durable #21，本地契约验证不能替代真实持久后端验收。

## 持久性、兼容性与边界

Durable 要求显式业务 Version，并核对拓扑/真实类型 Codec ID 与版本、输入编码字节指纹。函数、闭包、固定值和切面语义不能靠 Topology 自动识别，宿主必须升级业务版本。Resume 重新提供相同输入；缺 Codec、读取/解码错误和版本不匹配全部停止，不当作空历史。

原生流糖、channel 声明及普通值中隐藏的实际 channel 继续拒绝，受管端点也只能通过独立 Key[Stream[T]] 进入协议。静态声明在启动前递归检查，接口和容器中的实际动态值在输入/恢复/提交共用校验中穿透检查。值遍历带循环检测，合法自指业务对象不因这一检查被拒；它能否编码另由 Codec 决定。

MemoryStore 只证明进程内契约。`pulse-durable/filestore` 使用本机 OS 独占锁与完整 JSON 的同步原子替换，进程退出后锁由 OS 释放；真实工作流的退出/重启验收见 Durable 的 integration 用例。它不承诺断电持久性、跨机器租约/fencing 或分布式派发。

未确认节点仍可能重复外部副作用，业务需要幂等键、事务、防重或 outbox；检查点恢复不承诺 exactly-once，也不是 deterministic replay。
