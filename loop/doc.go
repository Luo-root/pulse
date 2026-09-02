// Package loop 是 pulse v2 的 ReAct 循环：一个无状态的回合执行器。
//
// Agent 接收对话历史与本轮输入，驱动「模型 ↔ 工具」循环至自然结束
// （模型不再发起工具调用），返回本回合新增的全部消息。它刻意不做
// 三件事：不管会话存储（调用方持有历史；会话真相见 memory/session，
// P2 已落地，loop 不 import memory）、不做重试与 failover（上层编排
// 职责，llm 包的错误分类是弹药）、不硬接任何模型或工具实现。
//
//	agent := loop.NewAgent(model, loop.WithToolSet(tools))
//	res, err := agent.Run(ctx, history, llm.UserText("帮我查一下…"))
//	// res.Final.Text()  → 最终回复
//	// res.Messages      → 本回合新产生的消息（assistant/tool 交替）
//	// 多轮对话 = history = append(history, input..., res.Messages...)
//
// # 扩展点全部事件化（请求级 Local）
//
// 结构化轨迹通过内核事件暴露（订阅即插拔、随作用域回收）。
// 派发一律走 EmitLocal / WaterfallLocal（只本 scope）；监听必须挂在
// 与 Agent 相同的 reqScope 上，否则听不到：
//
//	pulse.loop.turn_start        EmitLocal       回合开始（携带输入）
//	pulse.loop.step_start        EmitLocal       每个推理-行动步开始
//	pulse.loop.after_model       EmitLocal       模型响应就绪（含 Usage）
//	pulse.loop.before_tool_call  WaterfallLocal  工具执行前——可改写参数，
//	                                             置 Rejected 并短路即拒绝执行
//	                                             （HITL 审批 / 权限策略挂载点）
//	pulse.loop.after_tool_call   EmitLocal       工具执行完成（含时长与错误）
//	pulse.loop.turn_end          EmitLocal       回合结束（终止原因、累计用量）
//
// 与 llm 层的拦截事件分工：llm.before_generate 管 token 级关注点
// （路由、限流、脱敏），loop 层管 agent 决策级关注点（审批、审计、
// 轨迹）——两层不重复。Agent 调模型前会 llm.WithEventScope(ctx, scope)。
//
// 请求字段：Agent 组装的 GenerateRequest 主要带 Messages/Tools；
// Temperature / MaxTokens 等由调用方经 before_generate 或显式请求补齐
// （Anthropic 的 MaxTokens 必填，见 llm/anthropic）。
//
// 文本增量（流式 UI）走独立的 onDelta 回调，与结构化事件分离。
package loop
