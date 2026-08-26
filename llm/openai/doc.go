// Package openai 提供 OpenAI 两个线协议变体的 llm.ChatModel 适配器：
//
//	ProviderCompletions = "openai"            Chat Completions（POST /v1/chat/completions）
//	ProviderResponses   = "openai-responses"  Responses（POST /v1/responses）
//
// 传输层使用官方 SDK（github.com/openai/openai-go/v3，本包内以 sdk
// 别名引用）；适配器只做 llm 词汇表与 SDK 类型之间的语义映射——
// 消息/内容块、工具声明与调用、流事件、计量、错误分类。线协议细节
// （SSE 解析、重试传输、类型生成）由 SDK 承担。
//
// # 基本用法
//
//	reg := llm.NewRegistry(ctx)
//	openai.Register(ctx, reg) // 登记两个 provider（各自是可逆效应）
//	reg.Declare("main", llm.Config{Provider: "openai", Model: "gpt-4o",
//	    APIKey: "sk-...", BaseURL: ""}) // BaseURL 留空用官方端点；
//	                                    // 填网关地址即接 OpenAI 兼容服务
//	model, err := reg.Open("main")
//
// # 两个变体的分工
//
//   - openai（Chat Completions）：生态默认接口，兼容网关最多；
//     思维链以 reasoning_content 兼容字段透传（DeepSeek 等网关风格）。
//   - openai-responses（Responses）：OpenAI 主推的新接口，原生
//     reasoning 块与 reasoning summary；不支持 stop 序列（线格式
//     没有该参数，请求携带时显式报错）。
//
// # 有意钉死的语义
//
//   - SDK 传输层自动重试默认关闭（Options["max_retries"] 可显式
//     打开）：重试与 failover 属上层编排，llm.Error.Kind 是决策依据。
//   - Config.BaseURL 非空即整体替换端点——Azure/代理/私有网关由
//     调用方自行拼接；本包不做专用云端点签名。
//   - 流事件 Index：0 固定为文本/思维链块，1 起为工具调用（按到达
//     顺序编号），供消费方区分并发工具调用。
//   - 输入侧的 reasoning 块不回传（Completions 线格式无此概念且
//     DeepSeek 明确要求不回传；Responses 的有状态 reasoning 链接
//     依赖 PreviousResponseID/encrypted_content，见"不做"）。
//   - 多模态在适配层按 MIME 打通：image/*、audio/*、application/pdf
//     走官方块；video/* 走兼容网关的 video_url / input_video（官方
//     端点会 bad_request）。调用方只给 PartImage / PartCustom。
//
// # 不做
//
// PreviousResponseID/Conversations 有状态链（loop 无状态传全量历史）、
// 内置工具（web_search/file_search/code_interpreter/MCP）、对话接口
// 之外的独立语音端点（/v1/audio/speech、/v1/audio/transcriptions——
// ASR/TTS 走对话线格式：audio 输入用 input_audio 块，TTS 用官方
// audio 输出模态）、Azure/Bedrock 专用签名。
package openai
