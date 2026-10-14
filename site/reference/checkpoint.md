# 检查点

本页面向实现持久执行集成、存储或 Codec 的开发者。需要理解[生命周期](./lifecycle.md)、[重试写集](../guide/retry-cancellation.md)和[静态子图](../guide/subgraphs.md)，并使用[开发版](./migration.md)。普通业务宿主需要恢复时，优先使用独立的 [pulse-durable](https://github.com/Luo-root/pulse-durable) Run/Resume，而不是自行拼装可靠钩子。

当前 Durable 已实现检查点 Run/Resume 和本机 filestore。持久等待、分布式运行管理属于后续能力。依赖单向：Durable 导入 Pulse，Pulse 不依赖 Durable。

## 确认后发布

启用 `WithExecCheckpoint(scope, hooks)` 后，整棵执行树使用检查点模式。它独立于 Aspect，不能通过 WithoutAspects 关闭子树中的可靠提交。

一次未恢复节点按以下顺序执行：

1. 同步打开或认回完整节点链的持久尝试。
2. 在整个切面链之外创建输出写集，体与切面的 Set/Skip 都先暂存。
3. 完整链成功或正常跳过后，补齐所有未写 Provides 为 skipped；成功短路也等待声明依赖到达。
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
| `WithExecCheckpoint` | 为本次执行及子执行启用非流式可靠模式 |
| `Execution.RestoreNodes` | 核验并替换启动前整份恢复计划，不立即安装输出 |

需要稳定 schema 时，先完成装配并成功 Validate。返回容器与声明隔离，但业务值不深拷贝；集成层应同步编码，不得修改或异步保存引用后提前确认。

可靠钩子的实际类型如下，需要 `context` 及 Pulse 的相应载荷类型；这是接口定义，不是可直接使用的存储实现：

```go
type CheckpointHooks struct {
	OpenAttempt func(context.Context, CheckpointAttemptRequest) (CheckpointAttempt, error)
	FailAttempt func(context.Context, string) error
	OpenChild   func(context.Context, CheckpointChild) ([]NodeCheckpoint, error)
	Commit      func(context.Context, CheckpointCommit) error
}
```

四个钩子必须全部提供，并使用非空根 scope。它们在引擎锁外同步调用，可以并发，必须线程安全。OpenAttempt 返回持久 Scope、Index 与 Exhausted；FailAttempt 先确认真实失败，才允许下一次尝试；OpenChild 核对实际子图及输入并返回该作用域的恢复成果；Commit 确认完整节点链成果。

## 子图作用域与重试预算

持久 Scope 不等于挂载 Path，也不等于进程内 Execution ID。集成层为持久尝试分配稳定身份；同一挂载路径在不同父尝试下具有不同作用域。重启后活跃尝试认回同一 Scope/Index，已确认失败后才能推进 Index。

假设父步骤内有下载、提取、摘要三个子节点：下载和提取已确认后进程退出，Resume 创建新的父子执行，恢复前两步，摘要从函数开头重跑。若子节点全已确认而父步骤尚未确认，父包装与桥回重新执行，子业务不重做。

若父 Retry 明确失败并开始下一次尝试，旧尝试下的子成果不可借用。失败祖先使整支后代逻辑失效，不依赖历史记录已经被删除。OpenChild 还需核对 Graph、Path 与实际绑定输入；输入变化必须拒绝混用旧成果。

Retry 的持久边界由有效切面链位置和父 Scope 区分，不能从函数指针或本次调用次数猜测。根完整链 Limit 为 1；最终已确认失败后，Resume 不得给它刷新根作用域或重试预算。新的完整业务运行应使用新的 RunID。

## 恢复计划先整批核验

NodeCheckpoint 的 Reason 只能是 completed 或 skipped。Outputs 必须恰好覆盖该节点全部 Provides，含隐式 skipped；状态只能为 ready/skipped，键名、真实类型和值可赋值性必须匹配。pending 不是已确认成果。合法 nil 接口与 typed nil 保持 ready。

恢复集合还必须对图内依赖闭合：恢复节点所依赖的内部生产者也要有恢复成果。RestoreNodes 核验并替换整份计划，失败保留原计划；Start 在校验与封闭同一边界按实际定义复验。全部通过才安装输出、封闭与启动，末条记录非法也不会部分恢复前面记录。

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

## 失败、中断与不确定确认

钩子或恢复协议的运行期失败使用 CheckpointError。即使错误链包含 ErrSkipped 或 context 哨兵，仍为可靠失败；整棵树共享失败锁存，父 Retry 或切面无法通过吞错让它变为成功。启动前的配置与计划错误则直接返回契约错误，且不启动。

业务函数返回 context.Canceled，但实际 Execution 上下文仍正常时，是已知业务失败，需要确认失败与预算。宿主取消、进程退出、可靠钩子故障则保留活跃尝试，由恢复时读取的权威记录决定下一步。分类不能只查看 RPC 错误链。

存储实际写成却返回错误时，本次停止，不自动重试 Commit 或撤销记录。Resume 重读权威结果。已确认记录也不会因随后发生的取消被撤回。持久尝试无需保存可能包含业务敏感内容的原始错误文本。

## 能力边界与常见错误

检查点模式拒绝流式节点、channel 类型声明和接口/容器中隐藏的实际 channel。静态声明在启动前递归检查，动态值在输入、恢复和提交边界检查。循环对象不会仅因循环结构被此检查拒绝，是否能编码由 Codec 决定。

Version、Codec ID/版本、拓扑兼容性、输入编码指纹与运行所有权由持久层负责。业务闭包、固定值与切面语义无法由 Topology 自动识别，改变语义时宿主必须更新业务版本。读取、解码或版本错误应停止，不能当作空历史。

MemoryStore 只证明进程内契约。本机 filestore 支持进程退出后的记录恢复和 OS 独占锁；它不承诺断电持久性、跨机器租约或分布式派发。

检查点不会恢复 Go 调用栈或任意代码行。未确认节点从函数开头重做，仍可能重复外部副作用，因此不承诺 exactly-once 或确定性回放。业务需使用幂等键、事务、防重或 outbox。

## 下一步

接入业务应用请继续查看 [Durable 文档与源码](https://github.com/Luo-root/pulse-durable)。核心桥接的完整设计背景位于[检查点设计](https://github.com/Luo-root/pulse/blob/main/docs/design/checkpoint.md)；错误处理见[错误参考](./errors.md)。
