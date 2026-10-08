// Package pulse 把「依赖声明即拓扑，数据到达即调度」做成一个一次性运行的
// 图引擎：失败显式，取消能打断等数据。
//
// # 一次运行一个世界
//
// Graph 是模板的一次实例，不是可重跑的容器：数据槽、取消、首错随 Run
// 而生、随结束而灭。模板复用 ≠ 实例复用——同一张图要跑第二次，正确做法
// 是 New 第二次（类比 CI/CD：workflow 定义被 run 无数遍，每遍一个独立
// run）。跨运行的状态（历史、缓存、会话）不属于这个世界：pulse 持有
// 「这一轮正在流动的数据」，不持有历史。
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
// Provide 主动 Skip（分支）。Run 成功返回后漏写的 Provides 自动 Skip；
// 节点 error / 输入 Skip 结束时，未写 Provide 也可被清理 Skip（只为
// 解阻塞）。error 取消整图，Run/Err 仍返回原错误，不会伪装成 ErrSkipped。
//
// # 切面
//
// Aspect 是 func(rc, next) 形态（见 aspect.go）；不调 next 即短路。
// 内建 Timeout / Retry。CircuitBreaker 与 ErrorSwallow 不提供。
//
// # 观测
//
// WithObserver 挂自有 typed Observer（默认 no-op），发出 NodeWaiting /
// NodeRunning / NodeFinished。引擎不依赖任何观测包：把这三条回调折成
// 观测记录是 Observer 实现方（宿主或官方适配）的事。
//
// # 装配
//
// NewRegistry 登记 Run 工厂与 Key；声明式装图在子包 yaml
// （YAML 拥有拓扑，Factory 只给 Run）。
//
// 并发默认无限；WithMaxRunning(n) 只限制同时进入 Run 的节点数，
// 等数据不占名额（排队等名额也会被 ctx 取消打断）。
//
// 每个 Key 至多一种来源身份：外部 Seed/SkipSeed 或一个节点的 Provides。
package pulse
