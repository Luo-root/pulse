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
Consume(g, id, description, in, fn, opts...)
Tee(g, id, description, in, outs, opts...)
```

这些是参数形状示意。`FanOut` 的所有 worker 使用同一份说明，仍以原有的 `id-1` 至 `id-N` 区分。说明只作为静态元数据出现在 Topology / Snapshot；没有运行时 Setter。

这是公开签名变更，按 0.x 约定随 minor 发布，不能随 patch 发布。旧公开快照 `1f67045d6c42` 使用旧签名；试用尚未合并的修改时使用[对应本地源码](../guide/installation.md)，不要仅更改示例而继续安装旧快照。

## 流节点增加本地切面选项

`Produce`、`Consume`、`Tee` 统一通过 `WithStreamAspects(aspects ...Aspect) StreamOption` 配置本节点的切面。Produce/Tee 可与已有 `WithBuffer` 混用；Consume 新增 `opts ...StreamOption`，原有不传选项的直接调用仍可用。若把实例化后的 Consume 函数存为固定函数类型，需将函数类型同步改为带可变 StreamOption，或通过包装函数适配。

多次 `WithStreamAspects` 按声明顺序追加；图级、执行级切面依次在本地切面之外。选项创建时复制切片，每次应用与节点装配继续隔离；选项可复用，切面闭包内的业务状态仍由调用方同步。空配置合法，nil StreamOption 忽略。

Consume 不创建发送缓冲，正数 `WithBuffer` 会在装配时返回错误且不留下节点；零和负数沿用不改变配置的规则。节点级 Timeout 覆盖等输入、收发及回调，到期取消后等待内层退出，流被截断仍是本次执行失败。Produce/Tee 的 Retry 写暂存拒绝保留；Consume 的整节点 Retry 不能倒带或重放已消费元素。完整示例见[流式处理](../guide/streams.md)。

## 受管流、实时 Sub 与增量恢复

| 迁移位置 | 原生兼容行为 | 当前受管契约 |
|---|---|---|
| 键与糖 | Key[<-chan T]、Produce/Consume/Tee 保留 | Key[Stream[T]]、ProduceStream/ConsumeStream/TeeStream，均在 id 后接 description |
| 读取 | 收到即离开 channel，无确认 | Receive 得 Delivery，显式 Ack 释放总在途容量，Nack 同序重投 |
| 缓冲与额度 | 默认无缓冲、整段持额、保守容量检查 | 默认总在途 1，受管等待让额，业务继续前取回，纯受管 pipeline 额度 1 可推进 |
| 重试 | 无自动倒带或消息可靠性 | 有限 HandleMessage；渐进 SendAt 按显式来源游标续读 |
| Sub | 旧文档描述统一等待桥回 | In 稳定绑定可 pending；Stream/原生 channel Out 提前绑定；普通 Out 等 child.Wait 成功 |
| 宿主 | ReadOutput 仅 Wait 后读取 | 实时用 SubscribeStream，NewExecution 后、Start 前登记，必须消费 Ack 或 Stop |
| 检查点 | 原生和嵌套 channel 仍拒绝 | 普通四 hooks 保持合法，可选完整六项 StreamHooks，消息与 descriptor 分开记录 |

旧流糖支持 WithStreamAspects，但拒绝 WithStreamSource/WithMessageHandling，不会静默升级可靠性。ConsumeStream 正 WithBuffer 拒绝，ProduceStream 拒绝非空消息处理配置。用 WithMessageHandling 给受管消费/Tee 配有限尝试；失败成果不发布，闭包和副作用不回滚。

Sub 无整体输入门，全入口 skip 仍创建 child，独立自产分支可运行。普通 Retry 拒绝实际外接流，沿嵌套绑定可证明缺失的路径仍正常；完整有限内部流水线仅桥普通值才可整体重试。completed 流多个 Out 别名共用恢复账，不重跑来源体，先重建 Done 与尾部；active 来源没有续读承诺则拒绝恢复。宿主订阅使透传 inlet 成本轮必需输入，要 Seed/SkipSeed；Wait 不替宿主排空。完整程序见[流指南](../guide/streams.md)，持久集成边界见[检查点](./checkpoint.md)。这些 API 使用匹配本地检出；存储与进程恢复由 Durable 提供，集成跟踪见 Durable #21，发布状态以实际 tag/release 为准。

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

## 文本日志的 Unicode 与 UTF-8 引号规则

`observe.AppendTextValue` 现在按 slog.TextHandler 的口径检查非 ASCII 文本：Unicode 空白、不可打印字符和非法 UTF-8 会触发 Go 字符串引号与转义。例如，含 U+0085、U+009B、U+2028 或 U+00A0 的 `a…b` 会加引号并转义该字符，字节 FF 编码为 `\xff`。合法的替换字符 U+FFFD 也会保守地触发引号；普通中文和可打印 emoji 保持原样。

这条规则同时用于属性键与字符串值、source、host、error、trace，以及调用这些原语的宿主渲染器。内置 LineSink 与 AppendTextValue / AppendAttrs / AppendAttrsExcept 的相应片段保持逐字节一致。解析文本日志或比较固定输出的调用方，应更新包含这些输入的预期值，并按 Go 字符串字面量解码带引号的字段。

编码原语属于冻结面，这次输出字节变更必须随下一个 minor 发布，不能作为 patch 发布；当前仅描述开发实现，尚未发布新 tag。

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
