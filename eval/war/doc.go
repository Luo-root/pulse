// Package war 是 Go 框架内战对比（评测三步走·第一步第二阶段，Issue #103）：
// 同机、同任务、等薄 stub 模型下，测量各 agent 框架的基建开销。
//
// 独立 module：本包自带 go.mod（用户拍板），Eino 等对比框架的依赖只
// require 在这里，Pulse 主 module 的 go.mod 零污染——主 module 的
// go build ./... 天然不触嵌套 module。Pulse 本体经 replace ../.. 引用。
//
// 公平性口径（等价性声明，评审可查）：
//   - **stub 模型等薄**：各框架的 stub 除「按序返回脚本响应」外零逻辑
//     （一次互斥锁 + 下标推进 + 浅拷贝）；Pulse 侧直接用 llm.NewScripted
//     （其实现即此语义），Eino 侧 einoStubModel 按同语义实现——两边都
//     是「 mutex + 索引 + 浅拷贝」的最薄形态。
//   - **任务集等价**：T1 单步文本回合（user → assistant 文本）；
//     T2 工具往返（user → assistant(tool_call) → 执行 → 回填 → assistant）。
//     工具本体同样最薄：一次 map 写计数 + 返回固定 JSON。
//   - **构造计入（上界口径）**：脚本化 stub 耗尽后停在末条，两步脚本
//     必须每轮重建模型与 agent——两边构造都计入，数字是各自「含构造的
//     完整回合上界」，上界 vs 上界对比有效。
//   - **生产入口**：Pulse 用 loop.Agent.Run；Eino 用 adk.Runner.Run
//     （ADK 官方指示 agent.Run 不直接用于生产）——各框架生产装配链。
//   - **事件面**：两边都不挂观测监听（Pulse 不装 Bootstrap 桥；Eino 不
//     配 callback）——测的是框架核心路径，不含观测旁路（Pulse 观测开销
//     已在 #102 L1 层单独量化）。
package war
