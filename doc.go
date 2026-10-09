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
// Requires 是 AND 前置：全部输入到达（就绪或跳过）后才判门。Graph 把全部
// 节点一次性提交，每个节点阻塞在输入槽位上；上游 Set 或 Skip 都是「到达」，
// 下游被唤醒后区分值和跳过。没有 OR 调度；分支靠对未选中 Provide
// 调用 Skip，让下游因一条值都没到而不执行。
//
// 槽位三态（第一版一次设计完，避免以后为分支再改契约）：
//
//	未就绪 | 已就绪(值) | 已跳过
//
// 判门是**到几个收几个**：只要有一条输入真的到了值，节点就带着到了的那些
// 进入 Run（缺值的那几路读回来是 *SkipError，逐条问用 TryGet）；一条值都
// 没到（全部输入以跳过到达）才不执行 Run、全部输出跳过（级联）。判据是
// 「有没有值」，不是「有没有跳过」——上游某一路没有值，不该让手里还有数据
// 的下游跟着停。想让「缺一条就别跑我」的节点把 WaitAll 的返回值直接返回
// 出去，那是显式的 fan-in 策略声明。
// 节点可对单条 Provide 主动 Skip（分支）；但**两边都要表态**——Run 成功
// 返回后漏写的 Provides 会被自动 Skip，只 Skip 一边等于两条下游都不跑。
// 节点 error / 输入 Skip 结束时，未写 Provide 也可被清理 Skip（只为
// 解阻塞）。error 取消整图，Run/Err 仍返回原错误，不会伪装成 ErrSkipped。
// **取消优先于到达**：ctx 已取消时等待一律返回 ctx.Err()（与「到达」同时
// 就绪也以取消为准），所以因首错而没跑的下游稳定报 canceled 而不是 skipped。
//
// # 切面
//
// Aspect 是 func(rc, next) 形态（见 aspect.go）；不调 next 即短路。
// 内建 Timeout / Retry——超时是协作式的（等内层返回），重试有前提
// （失败前没写过 Provide），两条契约都在 aspect.go 的 godoc 里。
//
// # 观测
//
// WithObserver 挂自有 typed Observer（默认 no-op），发出图级两条
// （GraphStarted / GraphFinished：前者在提交任何节点之前、后者在 Wait 返回
// 之前，把本轮的节点事件夹在中间）与每节点三条（NodeWaiting / NodeRunning /
// NodeFinished，Retry 不重复打点）。引擎不依赖任何观测包：把这些回调折成
// 观测记录是 Observer 实现方（宿主或官方适配）的事。
//
// # 装配
//
// NewRegistry 登记 Run 工厂与 Key；声明式装图在子包 yaml
// （YAML 拥有拓扑，Factory 只给 Run）。
//
// 装配糖 FanOut / Join 把「一个输入 → N 个并行实例」与「N 路同类型 → 一束」
// 收进函数签名（见 sugar.go）：语义与手写 NewNode 逐字段一致，缺项与**来源**在
// Batch 里可见（每条一条 BatchItem），严格汇聚显式 WaitAll()。
//
// 并发默认无限；WithMaxRunning(n) 只限制同时进入 Run 的节点数，
// 等数据不占名额（排队等名额也会被 ctx 取消打断）。
//
// 每个 Key 至多一种来源身份：外部 Seed/SkipSeed 或一个节点的 Provides。
package pulse
