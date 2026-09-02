// Package eval 是 Pulse 的工程能力 property test 套件：把设计文档里的
// 不变式变成随机输入下的可执行断言（评测三步走·第二步，Issue #99）。
//
// 与单元测试的分工：memory/ / loop/ 各包的 *_test.go 覆盖固定用例（给定
// 输入断言输出）；本包覆盖**不变式**（任意合法输入序列下，性质必须保持）。
// 两者互补，本包不重复任何固定用例。
//
// 四个主题（对应 agent-framework-evaluation.md §5.2）：
//
//   - session_recovery_test.go    崩溃恢复：任意截断点的撕裂识别、合法
//     前缀保持、续写能力、二次恢复幂等（memory/session）
//   - compaction_budget_test.go   压缩事务：任意窗口下 Compact 要么事务
//     成功要么零落盘失败，raw log 只增不减；合理摘要引擎下 token 下降
//     （memory/compaction）
//   - memory_governance_test.go   记忆治理：随机生命周期操作序列下禁物理
//     删除、Search 仅见 Active、revision 单调、审批链 taint 不变
//     （memory/store + memory/candidate）
//   - loop_rejection_test.go      拒绝语义：任意 before_tool_call 拒绝都
//     以 IsError 回传且回合不失败、被拒工具绝不执行（loop）
//
// 可复现性：所有随机序列由固定 seed 驱动（math/rand/v2 PCG）。默认 seed
// 在 CI 与本地一致，保证失败可重现；设 EVAL_SEED 环境变量可换种子探索
// 新路径。断言失败时打印 seed 与迭代号，可直接回放。
//
// 本包是被测包的**外部观察者**：只 import 公开 API，不碰任何被测包的
// 内部；若 property 抓出被测包真 bug，另开票修复而不是在这里放松断言。
package eval
