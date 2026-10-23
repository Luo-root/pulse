# Pulse v0.5.0 发布说明

本文整理从 v0.4.1 升级到 v0.5.0 的变化。安装使用 `go get github.com/Luo-root/pulse@v0.5.0`；发布状态与完整记录见对应 [GitHub Release](https://github.com/Luo-root/pulse/releases/tag/v0.5.0)。环境要求、开发版和本地源码用法见[安装页](../guide/installation.md)。

## 先看不兼容变化

v0.5.0 是 0.x 的 minor 升级，不是可以不迁移调用点的 patch。完整步骤见[版本迁移](migration.md)。

- **定义与运行分离。** `Graph` 保存可复用定义，`Execution` 保存单次运行的上下文、输入、槽位、错误和状态。`New` 不再接收上下文；运行使用 `g.Run(ctx, in)`，或 `NewExecution`、`Start`、`Wait`。不要在 Graph 上保留运行态或增加 Reset。
- **输入与子图接线显式化。** 定义声明入口/出口，本轮值写入 `Inputs`，`Seed` / `SkipSeed` 及按名输入入口接收 Inputs 的地址。`Sub` 挂载现成子图，通过端口和 `In` / `Out` / `Fix` / `FixSkip` 接线；成功校验会封闭整棵子树。过渡开发版本的动态子图入口已经移除。
- **Retry 隔离尝试输出。** 失败尝试的写入不再对下游可见；成功或正常 skip 才提交。自定义切面不能在同一尝试边界反复推进 `next`。错误链包含 `CheckpointError` 时立即停止 Retry，即使当前图没有启用检查点也一样。
- **Observer 携带运行位置。** 回调使用 `Site` 的 Graph、Execution 和 Path，调用方需迁移自定义 Observer。Snapshot 属于 Execution，不含业务值；Topology 包含完整声明端口。跟踪过含值快照的开发版本时，改用声明出口的 `ReadOutput`。含端口切片的结构不能再依赖整体 `==` 比较。
- **节点语法糖增加说明参数。** `Join` / `FanOut` / `Produce` / `Consume` / `Tee` 在 `g, id` 后接 `description`。流节点本地切面通过 `WithStreamAspects` 配置；Consume 的函数类型增加可变 StreamOption。
- **YAML 跟随定义/运行模型。** `Load` 与每轮 SeedPlan 输入解析分离；`Analyze` 不需要 Registry。未知字段、额外 YAML 文档和非整数的整数配置明确拒绝，不再静默忽略。
- **文本编码更严格。** 非 ASCII 空白、控制字符、非法 UTF-8 和替换字符的引用/转义规则与 slog.TextHandler 对齐。依赖固定日志字节的解析器和断言需要迁移；普通中文和可打印 emoji 不变。

## 新的使用方式

### 同一份流程定义反复运行

同一 Graph 可顺序或并发创建独立 Execution，也可在多个位置挂载为 Sub。输入、取消和状态按执行隔离；业务闭包捕获的可变对象仍由调用方同步。

宿主在 Wait 成功后用 `ReadOutput` 读取声明出口，明确区分 ready、正常 skipped 和可选透传 pending。真实失败不交付部分值。静态结构看 Topology，运行进度看无值 Snapshot，Mermaid 可叠加执行状态。

### 受管消息流与实时子图

`ProduceStream` / `ConsumeStream` / `TeeStream` 提供显式 Ack/Nack、在途容量、业务等待让额和有限消息尝试。纯受管流水线可以在业务额度 1 下推进。宿主在 Start 前登记 `SubscribeStream`，运行中接收并确认或 Stop。

Sub 的流出口实时接通，普通值仍等子执行成功后桥回。原生 channel 和旧流糖继续保留兼容行为，不自动获得确认、重放或持久性。

### 完整节点与增量流检查点桥

Core 提供显式整图检查点模式、完整链暂存、确认后发布、启动前恢复校验，以及受管流消息/状态/游标的桥接边界。存储、Codec、稳定 RunID 和 Run/Resume 由独立的 [Pulse Durable](https://github.com/Luo-root/pulse-durable) 提供。

恢复重用已确认成果，不恢复 Go 调用栈。重试预算持久保存，Resume 不重置已耗尽预算。外部副作用依然需要业务防重；检查点不能自动提供外部 exactly-once。

### 自动记录真实的切面控制事实

切面可用 `rc.Site()` 读取位置。安装 `observe.NewRecordObserver` 后，内置 Retry / Timeout 自动记录尝试、真正开始的下一次重试、退避和本层超时；`NamedAspect` 显式记录自定义切面的进出。

原有图/节点生命周期回调次数不变。异常经过保护后才报告终态；日志错误或 Observer panic 不改变业务执行。Durable 的恢复身份由其桥接补充，不能用进程内 Execution ID 冒充稳定 RunID。

### 审批、批量与 Agent 循环

[业务组合指南](../guide/composition.md) 说明如何用切面等待审批、由宿主登记等待并 Cancel/Resume，以及在一个节点内循环调用可复用 Graph 工具。输入决定的批次数量可在业务体或宿主层处理，无需修改运行中的共享定义。

这些组合不自动增加业务任务目录、唤醒服务、逐轮持久记录或跨机器租约；按实际应用需要提供对应宿主协议。

## 可靠性修复

本版包含流错误链诊断重入的锁保护修正、异常终态与 Timeout 清理、正常缺失和真实失败的区分、确认后发布与读取义务修正、类型化输入和装配校验，以及观测出口的异常隔离与丢弃载荷释放。对应回归与组合用例随代码保留。

根包运行依赖仍只有标准库，`observe` 不增加第三方依赖，`yaml` 仍使用 yaml.v3；Durable 继续单向依赖 Core。内部文件收拢没有新增包路径或改变这些边界。

## 升级与发布顺序

1. 按[迁移页](migration.md)调整调用点，分别验证正常值、skip、失败和取消。
2. 核对同一定义复用时的业务闭包状态，以及外部副作用防重。
3. 发布时以进入 `main` 的完整集成为准，不能只因每张叠加 PR 标为 MERGED 就打 tag；合入旧前层分支的代码可能尚未进入 main。
4. 核对发布准备中的[正式版 SQLite 与恢复测量](sqlite-formal-20261023.md)的固定版本、未完成测点和证据边界；该报告的 `COMPLETED_WITH_ERRORS` 状态必须原样保留。旧规模与优化原始证据不作为当前文档站结论，也不与本轮测量拼接。
5. Pulse 与 Durable 分别判断发布条件。Core 正式 tag 可下载后，可以让 Durable 对齐该版本并复验公开依赖组合；**Durable 本轮暂不发版，须先完成并验收 [Durable #40](https://github.com/Luo-root/pulse-durable/issues/40) 的正式数据库 Store，再考虑首次发布。** 评测专用 SQLite 实现不满足该发布前置。

运行需要 Go 1.25+。具体构建、race、文档及跨仓恢复结果由发布时的提交和 CI 记录确定。此版本继续 0.x 演进，不表示公开 API 已完成 v1.0 冻结。
