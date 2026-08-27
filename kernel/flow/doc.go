// Package flow 是 kernel 响应式理念在「一次运行」时间尺度上的应用：
// 依赖声明即拓扑，数据到达即调度；失败显式，取消能打断等数据。
//
// 节点不声明下一个节点是谁，只声明 Requires / Provides 哪些 Key。
// Requires 是 AND 前置：全部就绪才进入 Run。Graph 把全部节点一次
// 性提交，每个节点阻塞在输入槽位上；上游 Set 或 Skip 都是「到达」，
// 下游被唤醒后区分值和跳过。没有 OR 调度；分支靠对未选中 Provide
// 调用 Skip，让下游因输入跳过而不执行。
//
// 槽位三态（第一版一次设计完，避免以后为分支再改契约）：
//
//	未就绪 | 已就绪(值) | 已跳过
//
// 任一输入跳过 → 不执行 Run，全部输出跳过（级联）。节点可对单条
// Provide 主动 Skip（分支）。Run 漏写的 Provides 返回后自动 Skip，
// 下游不会永远等待。节点返回的 error 取消整图，不会被翻译成跳过。
//
// 与 kernel 同构、不同层：kernel 管服务装卸载，flow 管一次运行内的
// 数据到达。运行数据不进服务仓库。切面签名对齐 Waterfall：
// Around(rc, next)。Recovery 内建；Timeout / Retry 可选。
// CircuitBreaker 与 ErrorSwallow 不提供。
//
// 并发默认无限；WithMaxRunning(n) 只限制同时进入 Run 的节点数，
// 等数据不占名额。
//
// 每个 Key 至多一种来源身份：外部 Seed/SkipSeed 或一个节点的 Provides。
package flow
