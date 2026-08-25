// Package loop 是 pulse v2 的 ReAct 循环：一个无状态的回合执行器。
//
// Agent 接收对话历史与本轮输入，驱动「模型 ↔ 工具」循环至自然结束
// （模型不再发起工具调用），返回本回合新增的全部消息。它刻意不做
// 三件事：不管会话存储（调用方持有历史，P2 的 session 层再补）、
// 不做重试与 failover（上层编排职责，llm 包的错误分类是弹药）、
// 不硬接任何模型或工具实现。
//
//	agent := loop.NewAgent(model, loop.WithToolSet(tools))
//	res, err := agent.Run(ctx, history, llm.UserText("帮我查一下…"))
//	// res.Final.Text()  → 最终回复
//	// res.Messages      → 本回合新产生的消息（assistant/tool 交替）
//	// 多轮对话 = history = append(history, input..., res.Messages...)
//
// # 扩展点全部事件化
//
// 结构化轨迹通过内核事件暴露（订阅即插拔、随作用域回收）：
//
//	pulse.loop.turn_start        emit      回合开始（携带输入）
//	pulse.loop.step_start        emit      每个推理-行动步开始
//	pulse.loop.after_model       emit      模型响应就绪（含 Usage）
//	pulse.loop.before_tool_call  waterfall 工具执行前——可改写参数，
//	                                         置 Rejected 并短路即拒绝执行
//	                                         （HITL 审批 / 权限策略挂载点）
//	pulse.loop.after_tool_call   emit      工具执行完成（含时长与错误）
//	pulse.loop.turn_end          emit      回合结束（终止原因、累计用量）
//
// 与 llm 层的拦截事件分工：llm.before_generate 管 token 级关注点
// （路由、限流、脱敏），loop 层管 agent 决策级关注点（审批、审计、
// 轨迹）——两层不重复。
//
// 文本增量（流式 UI）走独立的 onDelta 回调，与结构化事件分离。
package loop
