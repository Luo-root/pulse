// Package anthropic 提供 Anthropic Messages 线协议的 llm.ChatModel 适配器：
//
//	ProviderAnthropic = "anthropic"   Messages（POST /v1/messages）
//
// 传输层使用官方 SDK github.com/anthropics/anthropic-sdk-go（本包内以
// sdk 别名引用）；适配器只做 llm 词汇表与 SDK 类型之间的语义映射。
//
// # 与 OpenAI 线格式的关键差异（映射时已处理）
//
//   - system 不是消息角色，而是顶层 System 参数：llm 的 system 消息
//     全部合并进 System []TextBlockParam；
//   - MaxTokens 是必填参数，provider 没有默认值：req.MaxTokens 为 nil
//     时显式 ErrBadRequest（不设魔法默认值）；
//   - 工具结果不是独立角色，而是 user 消息里的 tool_result 块，且
//     必须位于该 user 轮的首位——RoleTool 消息整体映射为一个只含
//     tool_result 的 user 消息；
//   - 音频 / 视频不支持：PartCustom(audio/*|video/*) 显式 ErrBadRequest；
//   - PDF 走 document 块（base64 或 URL）；图片 base64 / URL 均可；
//   - 输入侧 thinking 块不回传：回传需要 signature 原样配对，词汇表
//     不承载签名，有状态思维链接见"不做"。
//
// # 有意钉死的语义
//
//   - SDK 自动重试默认关闭（Options["max_retries"] 可显式打开）；
//   - Config.BaseURL 非空即整体替换端点；
//   - StopReason 映射：end_turn/stop_sequence/pause_turn→stop、
//     tool_use→tool_calls、max_tokens 与 model_context_window_exceeded→
//     length、refusal→content_filter。
//
// # 不做
//
// Prompt caching 断点（cache_control）、thinking 开关（ThinkingConfig）、
// 服务端工具（web_search 等）、token counting 端点、batch API、
// Azure/Bedrock/Vertex 专用签名。
package anthropic
