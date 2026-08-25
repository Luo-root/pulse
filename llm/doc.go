// Package llm 是 pulse v2 的模型适配层：provider 中立的对话模型
// 词汇表 + 适配器注册中心。
//
// 分层与 DSH 的 ctx.llm 同构：
//
//	adapter 插件（openai/anthropic/deepseek…）
//	    │ RegisterProvider(scope, "openai", factory)   // scope = adapter 自己的 Apply ctx
//	    ▼
//	Registry（本包，作为 kernel 服务 pulse.llm 提供）
//	    │ Open / OpenDefault
//	    ▼
//	ChatModel（agent-loop 等消费方只面向此接口）
//
// 设计要点：
//   - 消息采用 content-block 模型（对齐 Anthropic 的 block 语义，
//     各 provider adapter 负责向自家线格式转换），文本 / 图像 /
//     工具调用 / 工具结果 / 思维链统一表达；
//   - 请求是完整的 [GenerateRequest]（温度、停止序列、工具选择、
//     结构化输出……），而不是裸消息列表；
//   - 流式统一为一组 [StreamEvent]，消费方不接触任何 SSE 细节；
//   - 错误带分类与可重试标记（[Error]/[KindOf]/[IsRetryable]），
//     供上层 failover 与退避决策；
//   - 拦截点是内核事件而非接口继承：监听 pulse.llm.before_generate
//     （waterfall，可改写请求）即可实现计量、日志、限流、路由，
//     无需包裹每个模型实例。
package llm
