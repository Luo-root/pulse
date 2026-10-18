# 参考

本部分面向已经使用 Pulse、需要核对行为边界或实现集成的开发者。操作步骤与完整程序位于[使用指南](../guide/orchestration.md)；参考页集中说明生命周期、错误、检查点协议和版本迁移。

## 版本范围

本文档对应开发版对象模型。最新正式版本为 **v0.4.1**；下一次发布尚未完成。运行本站示例时，按[安装页](../guide/installation.md)选择包含对应接口的主线版本或本地源码；升级差异见[迁移参考](./migration.md)。

## 按问题查阅

| 需要确认的问题 | 页面 |
|---|---|
| 最新版与 Eino、Temporal 的执行、观测、资源和恢复成本 | [统一工作流对照](./production-comparison.md) |
| 什么时候封闭定义、节点有哪些状态、回调发生几次 | [生命周期](./lifecycle.md) |
| 错误表示什么、应该修改声明还是处理运行失败 | [错误](./errors.md) |
| 何时确认与发布输出、恢复哪些成果、重试预算如何保留 | [检查点](./checkpoint.md) |
| 如何从 v0.4.1 或过渡开发 API 迁移 | [版本迁移](./migration.md) |
| v0.5.0 准备发布哪些能力、哪些变化不兼容 | [发布说明](./release-v0.5.0.md) |

## 对象与接口位置

| 对象或入口 | 责任 | 实际用法 |
|---|---|---|
| `Graph` | 可复用的定义、端口、静态挂载与默认配置 | New、DeclareInput/Output、Add、Sub、Validate |
| `Inputs` | 一次运行的外部输入 | Seed、SkipSeed |
| `Execution` | 一次运行的状态、槽位、错误与取消 | NewExecution、Start、Wait、Cancel |
| `ReadOutput` | 成功收尾后的声明出口读取 | 先检查 error，再处理三态 |
| `Topology` | 定义结构与静态问题 | Topology、Mermaid，不启动也不封闭 |
| `Snapshot` | 一次运行的无值状态样本 | Execution.Snapshot，不用于恢复 |
| `Observer` | 生命周期回调 | WithObserver 或 WithExecObserver |
| `CheckpointHooks` | 同步可靠确认与恢复桥接 | WithExecCheckpoint，通常由 Durable 集成 |

普通宿主只需根包。记录适配与 YAML 装配分别位于 `pulse/observe` 与 `pulse/yaml`，依赖方向始终由外围包指向根包。存储、Codec、持久 RunID 和运行所有权由独立 Durable 层负责。

## 查阅和排错顺序

先确认自己使用的版本，再定位动作所处阶段：定义装配、本次输入准备、启动校验、节点执行、结果读取或检查点恢复。只有这些前提一致，错误名与状态才有准确含义。

- 定义阶段的缺失来源、无效出口、依赖环：修正结构，重新校验。
- 本次必需输入没有提供：准备完整 Inputs，创建新执行。
- 正常 skipped：按接口的缺值策略处理，整轮可以成功。
- 真实运行失败或取消：保留原因，等待所有节点结束，再决定业务后续操作。
- 可靠确认结果不确定：停止本次发布，从持久层权威记录恢复。

公开符号的完整类型说明可通过[包文档](../packages/index.md)继续查阅。本参考不重新定义历史设计文档；需要设计背景时可阅读仓库中的[当前核心设计](https://github.com/Luo-root/pulse/blob/main/docs/design/pulse.md)和[对象模型设计](https://github.com/Luo-root/pulse/blob/main/docs/design/object-model.md)。

## 下一步

执行控制从[生命周期](./lifecycle.md)开始，故障处理从[错误](./errors.md)开始。正在升级项目时，先阅读[版本迁移](./migration.md)，再返回相应操作主题。
