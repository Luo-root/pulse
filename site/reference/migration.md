# 版本迁移

本页面向从正式 v0.4.1 升级，或跟踪过中间开发提交的维护者。目标是迁移到当前“可复用 Graph 定义、每次独立 Execution”的 API。升级前应保留现有测试，并先核对模块版本。

## 正式版本与开发文档

最新正式 tag 是 **v0.4.1**。新对象模型、检查点桥与下列语法糖签名尚未随新 tag 发布；本站描述开发模型，不表示 v0.5.0 已发布。正式用户应按 v0.4.1 对应文档使用。已合并的开发接口可从主线安装；尚未合并的分支必须按[安装页](../guide/installation.md)使用对应本地源码：

```sh
go get github.com/Luo-root/pulse@main
go list -m github.com/Luo-root/pulse
```

项目需要 Go 1.25+。从 main 安装后保留 Go 解析出的具体伪版本，并确认它包含所需改动；不要手工编造提交或伪版本。使用本地替换时则核对源码目录。后续正式发布时再按发布说明选择 tag。

## 节点语法糖增加说明参数

`Join`、`FanOut`、`Produce`、`Consume`、`Tee` 现在统一在 `g, id` 后接 `description string`。已有调用在节点 ID 后补业务说明或 `""`，原来的后续参数整体右移一位；回调、切面和流选项的含义不变。

```go
Join(g, id, description, ins, out, fn, aspects...)
FanOut(g, id, description, in, outs, fn, aspects...)
Produce(g, id, description, out, fn, opts...)
Consume(g, id, description, in, fn)
Tee(g, id, description, in, outs, opts...)
```

这些是参数形状示意。`FanOut` 的所有 worker 使用同一份说明，仍以原有的 `id-1` 至 `id-N` 区分。说明只作为静态元数据出现在 Topology / Snapshot；没有运行时 Setter。

这是公开签名变更，按 0.x 约定随 minor 发布，不能随 patch 发布。旧公开快照 `1f67045d6c42` 使用旧签名；试用尚未合并的修改时使用[对应本地源码](../guide/installation.md)，不要仅更改示例而继续安装旧快照。

## 先迁移对象职责

| 关注点 | v0.4.1 模型 | 当前开发模型 |
|---|---|---|
| Graph | 一次运行的定义、槽位、上下文与错误 | 可复用的静态定义 |
| 上下文 | 创建图时绑定 | 创建 Execution 或调用 Run 时传入 |
| 外部数据 | 写入图对象 | 先声明入口，再写本次 Inputs |
| 运行控制 | Graph 承担启动、等待与错误 | Execution 承担 Start、Wait、Err、Cancel |
| 重复运行 | 为下一轮重建运行对象 | 同一 Graph 创建多个 Execution |
| 观察回调 | 以图 ID 标识 | Site 的 Graph、Execution、Path 三个维度 |

不要给定义添加 Reset 或缓存上次 Execution。需要保存跨运行状态时由业务宿主管理，不能让它隐式进入 Graph 的运行槽位。

## 当前入口写法

以下片段放在返回 `error` 的装配函数内，导入 `pulse`。说明字段可以为空，但必须保留其参数位置：

```go
request := pulse.NewKey[string]("request")
result := pulse.NewKey[string]("result")
g, err := pulse.New("migration-demo", "可复用处理流程")
if err != nil {
	return err
}
if err := g.DeclareInput(pulse.NewPort(request)); err != nil {
	return err
}
if err := g.DeclareOutput(pulse.NewPort(result)); err != nil {
	return err
}
if err := g.Add(pulse.NewNode("copy", "转送请求",
	pulse.Requires(request), pulse.Provides(result),
	func(rc *pulse.RunCtx) error {
		value, err := pulse.Get(rc, request)
		if err != nil {
			return err
		}
		return pulse.Set(rc, result, value)
	})); err != nil {
	return err
}
var in pulse.Inputs
if err := pulse.Seed(&in, request, "hello"); err != nil {
	return err
}
```

不需要结果句柄时，用 `g.Run(ctx, in)` 并处理返回错误。需要结果或进度时，创建新的 Execution，成功 Start 后 Wait，再调用 `pulse.ReadOutput(e, result)`。完整运行片段见[输入与输出](../guide/inputs-outputs.md)。

Seed、SkipSeed 和按名称提供输入的两个入口，其第一个参数都改为 Inputs 的地址。端口声明属于定义，值属于本次输入；必需入口漏值会在 NewExecution/Start 报出，而不是等待超时。零键、重复输入和 nil 接口值也会明确拒绝。

## 迁移拓扑与子图

装配完成后 Validate 成功会封闭整棵子树，成功 Start 也会封闭。将原来“运行前还可能继续追加”的声明放在首次成功校验之前。查询结构与问题时用 Topology，而不是依靠校验后继续改定义。

子图使用 `Sub` 挂载现成定义，提供 ID、说明、绑定与 child；接口必须是 DeclareInput/DeclareOutput 端口。父值通过 In 进入，子值通过 Out 桥回；每处固定输入通过 Fix/FixSkip 提供。每次运行和每处挂载都有独立子执行，取消自动从父节点上下文派生。

若跟踪过过渡开发提交，动态子图构建入口已删除，应改为装配期构建静态子树；这是过渡 API 的迁移项，不是 v0.4.1 原有能力。不要将固定运行值留在子图定义上。

## 迁移观测、结果与重试

自定义 Observer 的每个回调接收 Site。记录中保留 Execution 与非空 Path，不能只按 Graph 汇总并发运行。宿主每次运行应使用新的 TraceID；通过 WithExecObserver 给本次运行覆盖配置更适合复用定义。

快照位于 Execution，且不包含业务值。静态结构位于 Topology；状态 Mermaid 在 Topology 上传入 WithMermaidSnapshot。宿主结果只读取声明出口，先判断 error，再区分 ready、skipped 和可选透传 pending。跟踪过开发期含值快照的调用方应迁移到 ReadOutput。

Retry 的失败尝试输出现在先暂存并丢弃，下游看不到失败版本。成功或正常跳过才提交。自定义切面反复调用 next 不再是合法重试，应使用内置 Retry。外部副作用仍由业务自行保证幂等，详见[重试、超时与取消](../guide/retry-cancellation.md)。

Timeout 在到期后取消内层并等待体退出。原来依赖“时限到达即函数强制返回”的业务，需要将阻塞操作改为响应 rc.Context，并确保节点返回前结束自身工作。

## 迁移 YAML 与恢复集成

YAML Load 返回定义和 SeedPlan，装配选项不绑定执行上下文。每次运行调用 plan.Inputs，再把 ctx 与 Inputs 传给运行入口。根图补充声明应在 Validate 前完成。

Analyze 接收 YAML 字节与 LoadOptions，不需要 Registry；类型只是文档标签，仍需 Load 和 Inputs 核对真实类型与值。运行中的观察者与 per-run 配置通过定义或 Execution 入口安装，不能依赖声明预览执行这些回调。

Load 与 Analyze 现在拒绝未知配置字段和额外 YAML 文档。将自定义业务数据放在 `from.value` 中，或由自定义 `from.kind` 的宿主 resolver 解释；不要把它作为配置结构上的未知字段。多份文档应分别调用入口，不能再依赖只读取第一份的旧行为。

恢复集成使用显式 WithExecCheckpoint 与 RestoreNodes；Snapshot 不承担持久恢复。稳定 RunID、版本、Codec、存储与所有权属于 Durable。普通执行不必提供可靠钩子，参考[检查点](./checkpoint.md)。

## 升级后验证

1. 编译所有调用点，确认上下文、说明字段、输入地址和执行句柄已经迁移。
2. 为正常值、正常跳过、真实失败和取消分别验证业务结果。
3. 用同一定义运行两次，并测试并发运行或多处子图挂载中的业务闭包状态。
4. 检查 Retry 失败写入是否不再可见，以及外部操作的幂等边界。
5. 同步更新中英文说明和可复制示例，运行项目测试。

仅编译通过不能验证跳过和失败被正确区分，也不能证明共享业务对象并发安全。遇到运行失败，先对照[错误参考](./errors.md)，再检查[生命周期](./lifecycle.md)中的启动和收尾条件。

## 下一步

按[使用指南](../guide/orchestration.md)更新具体主题。查看实现或历史设计时，以本页版本范围区分已发布 API、当前开发实现与后续计划，不要将计划当作可调用能力。
