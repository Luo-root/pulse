package openai

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Luo-root/pulse/llm"
	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/shared"
)

// NewCompletions 构造 Chat Completions 线协议的模型实例
// （provider 名 "openai"）。
func NewCompletions(cfg llm.Config) (llm.ChatModel, error) {
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	return &completionsModel{client: client, provider: cfg.Provider, model: cfg.Model}, nil
}

// completionsModel 是无状态的 ChatModel 实现：并发使用安全。
type completionsModel struct {
	client   sdk.Client
	provider string
	model    string
}

// 编译期断言：满足 ChatModel 契约。
var _ llm.ChatModel = (*completionsModel)(nil)

// tcAcc 累积一个流式工具调用（OpenAI 以 delta.Index 分片传输）。
type tcAcc struct {
	index int // 对外事件序号：delta.Index+1（0 保留给文本/思维链）
	id    string
	name  string
	args  strings.Builder
	begun bool
}

func (m *completionsModel) Generate(ctx context.Context, req *llm.GenerateRequest) (*llm.Response, error) {
	params, err := m.buildParams(req)
	if err != nil {
		return nil, err
	}
	completion, err := m.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return nil, mapError(m.provider, err)
	}
	if len(completion.Choices) == 0 {
		return nil, llm.NewError(llm.ErrProvider, m.provider, 0, nil, "响应不含任何 choice")
	}
	choice := completion.Choices[0]
	return &llm.Response{
		Message:      mapCompletionsMessage(&choice.Message),
		FinishReason: mapFinishReason(choice.FinishReason),
		Usage:        mapUsage(completion.Usage),
	}, nil
}

func (m *completionsModel) Stream(ctx context.Context, req *llm.GenerateRequest) (<-chan llm.StreamEvent, error) {
	params, err := m.buildParams(req)
	if err != nil {
		return nil, err
	}
	stream := m.client.Chat.Completions.NewStreaming(ctx, params)
	ch := make(chan llm.StreamEvent, 16)
	go m.pump(ctx, stream, ch)
	return ch, nil
}

// buildParams 把 llm.GenerateRequest 翻译为 Chat Completions 请求。
func (m *completionsModel) buildParams(req *llm.GenerateRequest) (sdk.ChatCompletionNewParams, error) {
	params := sdk.ChatCompletionNewParams{
		Model: shared.ChatModel(m.model),
		// 流式/非流式统一带 include_usage：末块回传整次调用计量。
		StreamOptions: sdk.ChatCompletionStreamOptionsParam{IncludeUsage: param.NewOpt(true)},
	}
	msgs := make([]sdk.ChatCompletionMessageParamUnion, 0, len(req.Messages))
	for _, msg := range req.Messages {
		converted, err := m.convertMessage(msg)
		if err != nil {
			return params, err
		}
		msgs = append(msgs, converted...)
	}
	params.Messages = msgs

	if req.Temperature != nil {
		params.Temperature = param.NewOpt(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = param.NewOpt(*req.TopP)
	}
	if req.MaxTokens != nil {
		// 用新版字段 max_completion_tokens：max_tokens 已废弃且与
		// o 系列推理模型不兼容。
		params.MaxCompletionTokens = param.NewOpt(int64(*req.MaxTokens))
	}
	if len(req.StopSequences) > 0 {
		params.Stop = sdk.ChatCompletionNewParamsStopUnion{OfStringArray: req.StopSequences}
	}
	for _, t := range req.Tools {
		fd := shared.FunctionDefinitionParam{Name: t.Name}
		if t.Description != "" {
			fd.Description = param.NewOpt(t.Description)
		}
		if len(t.Parameters) > 0 {
			var schema map[string]any
			if err := json.Unmarshal(t.Parameters, &schema); err != nil {
				return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, err,
					"工具 %s 的参数 Schema 不是合法 JSON 对象", t.Name)
			}
			fd.Parameters = schema
		}
		params.Tools = append(params.Tools, sdk.ChatCompletionFunctionTool(fd))
	}
	if req.ToolChoice != nil {
		switch req.ToolChoice.Mode {
		case llm.ToolAuto:
			params.ToolChoice.OfAuto = param.NewOpt("auto")
		case llm.ToolNone:
			params.ToolChoice.OfAuto = param.NewOpt("none")
		case llm.ToolAny:
			params.ToolChoice.OfAuto = param.NewOpt("required")
		case llm.ToolSpecific:
			params.ToolChoice.OfFunctionToolChoice = &sdk.ChatCompletionNamedToolChoiceParam{
				Function: sdk.ChatCompletionNamedToolChoiceFunctionParam{Name: req.ToolChoice.Name},
			}
		default:
			return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
				"未知 ToolChoiceMode %q", req.ToolChoice.Mode)
		}
	}
	if rf := req.ResponseFormat; rf != nil {
		switch rf.Type {
		case llm.FormatJSONObject:
			v := shared.NewResponseFormatJSONObjectParam()
			params.ResponseFormat.OfJSONObject = &v
		case llm.FormatJSONSchema:
			name := rf.Name
			if name == "" {
				// 线格式必填 schema 名；词汇表中 Name 可选。
				name = "response"
			}
			var schema any
			if err := json.Unmarshal(rf.Schema, &schema); err != nil {
				return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, err,
					"ResponseFormat.Schema 不是合法 JSON")
			}
			params.ResponseFormat.OfJSONSchema = &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{Name: name, Schema: schema},
			}
		case llm.FormatText, "":
			// 纯文本是默认行为，不下发。
		default:
			return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
				"未知 ResponseFormatType %q", rf.Type)
		}
	}
	if len(req.Metadata) > 0 {
		// 线格式只收字符串键值对；非字符串值跳过。
		md := shared.Metadata{}
		for k, v := range req.Metadata {
			if s, ok := v.(string); ok {
				md[k] = s
			}
		}
		if len(md) > 0 {
			params.Metadata = md
		}
	}
	return params, nil
}

// convertMessage 翻译单条消息。工具结果在 OpenAI 线格式中必须是
// 顶层 tool 消息，因此一条 llm.Message 可能展开为多条。
func (m *completionsModel) convertMessage(msg *llm.Message) ([]sdk.ChatCompletionMessageParamUnion, error) {
	switch msg.Role {
	case llm.RoleSystem:
		return []sdk.ChatCompletionMessageParamUnion{sdk.SystemMessage(joinText(msg.Parts))}, nil

	case llm.RoleAssistant:
		var texts []string
		var calls []sdk.ChatCompletionMessageToolCallUnionParam
		for i := range msg.Parts {
			p := &msg.Parts[i]
			switch p.Kind {
			case llm.PartText:
				texts = append(texts, p.Text)
			case llm.PartToolCall:
				tc := p.ToolCallValue
				if tc == nil {
					return nil, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
						"assistant 消息第 %d 块 ToolCallValue 为空", i)
				}
				calls = append(calls, sdk.ChatCompletionMessageToolCallUnionParam{
					OfFunction: &sdk.ChatCompletionMessageFunctionToolCallParam{
						ID: tc.ID,
						Function: sdk.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      tc.Name,
							Arguments: string(tc.Arguments),
						},
					},
				})
			case llm.PartReasoning:
				// 思维链不回传：线格式无对应概念，DeepSeek 明确要求
				// 不回传 reasoning_content。
			default:
				return nil, unsupportedPart(m.provider, msg.Role, p.Kind)
			}
		}
		asst := &sdk.ChatCompletionAssistantMessageParam{ToolCalls: calls}
		if len(texts) > 0 {
			asst.Content.OfString = param.NewOpt(strings.Join(texts, "\n"))
		}
		return []sdk.ChatCompletionMessageParamUnion{{OfAssistant: asst}}, nil

	case llm.RoleUser, llm.RoleTool:
		return m.convertUserSide(msg)

	default:
		return nil, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil, "未知消息角色 %q", msg.Role)
	}
}

// convertUserSide 翻译 user/tool 消息：文本与图像累积为一条 user
// 消息，工具结果展开为独立的顶层 tool 消息（线格式要求）。
func (m *completionsModel) convertUserSide(msg *llm.Message) ([]sdk.ChatCompletionMessageParamUnion, error) {
	out := make([]sdk.ChatCompletionMessageParamUnion, 0, 2)
	var parts []sdk.ChatCompletionContentPartUnionParam
	var texts []string
	onlyText := true
	flush := func() {
		if len(parts) == 0 {
			return
		}
		if onlyText {
			out = append(out, sdk.UserMessage(strings.Join(texts, "\n")))
		} else {
			out = append(out, sdk.UserMessage(parts))
		}
		parts, texts, onlyText = nil, nil, true
	}
	for i := range msg.Parts {
		p := &msg.Parts[i]
		switch p.Kind {
		case llm.PartText:
			parts = append(parts, sdk.TextContentPart(p.Text))
			texts = append(texts, p.Text)
		case llm.PartImage:
			ref, err := imageRef(m.provider, p.Image)
			if err != nil {
				return nil, err
			}
			parts = append(parts, sdk.ImageContentPart(
				sdk.ChatCompletionContentPartImageImageURLParam{URL: ref}))
			onlyText = false
		case llm.PartCustom:
			// 开放模态块：仅 image/* 可降级映射为图像，其余显式拒绝。
			if p.Media == nil || !strings.HasPrefix(p.Media.MediaType, "image/") {
				return nil, unsupportedPart(m.provider, msg.Role, p.Kind)
			}
			ref, err := imageRef(m.provider, &llm.ImageSource{
				Data: p.Media.Data, URL: p.Media.URL, MediaType: p.Media.MediaType,
			})
			if err != nil {
				return nil, err
			}
			parts = append(parts, sdk.ImageContentPart(
				sdk.ChatCompletionContentPartImageImageURLParam{URL: ref}))
			onlyText = false
		case llm.PartToolResult:
			flush()
			tr := p.ToolResultValue
			if tr == nil {
				return nil, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
					"%s 消息第 %d 块 ToolResultValue 为空", msg.Role, i)
			}
			out = append(out, sdk.ToolMessage(joinText(tr.Content), tr.ToolCallID))
		case llm.PartReasoning:
			// 输入侧思维链不回传。
		default:
			return nil, unsupportedPart(m.provider, msg.Role, p.Kind)
		}
	}
	flush()
	return out, nil
}

// pump 消费 SDK 流并翻译为 llm.StreamEvent；任何退出路径都保证
// channel 关闭（EventError/EventDone 后 close）。
func (m *completionsModel) pump(ctx context.Context, stream *ssestream.Stream[sdk.ChatCompletionChunk], ch chan<- llm.StreamEvent) {
	defer close(ch)
	send := func(ev llm.StreamEvent) bool {
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	var (
		text      strings.Builder
		reasoning strings.Builder
		calls     []*tcAcc
		byIdx     = map[int64]*tcAcc{}
		finish    string
		usage     sdk.CompletionUsage
		usageSeen bool
	)
	for stream.Next() {
		chunk := stream.Current()
		if chunk.Usage.JSON.PromptTokens.Valid() {
			usage, usageSeen = chunk.Usage, true
		}
		for _, choice := range chunk.Choices {
			d := choice.Delta
			if d.Content != "" {
				text.WriteString(d.Content)
				if !send(llm.StreamEvent{Kind: llm.EventTextDelta, Index: 0, Text: d.Content}) {
					return
				}
			}
			var rc string
			// 注意：ExtraFields 里的字段 Valid() 恒为 false（SDK 对
			// 未知字段不设 status），只看 Raw() 是否非空。
			if f, ok := d.JSON.ExtraFields["reasoning_content"]; ok {
				_ = json.Unmarshal([]byte(f.Raw()), &rc)
			}
			if rc != "" {
				reasoning.WriteString(rc)
				if !send(llm.StreamEvent{Kind: llm.EventReasoningDelta, Index: 0, Text: rc}) {
					return
				}
			}
			for _, dt := range d.ToolCalls {
				acc := byIdx[dt.Index]
				if acc == nil {
					acc = &tcAcc{index: int(dt.Index) + 1}
					byIdx[dt.Index] = acc
					calls = append(calls, acc)
				}
				if acc.id == "" {
					acc.id = dt.ID
				}
				if acc.name == "" {
					acc.name = dt.Function.Name
				}
				if !acc.begun && acc.id != "" {
					// 首个携带 ID 的分片即 begin（OpenAI 协议中 ID 与
					// 工具名同片到达）。
					acc.begun = true
					if !send(llm.StreamEvent{Kind: llm.EventToolCallBegin, Index: acc.index,
						CallID: acc.id, ToolName: acc.name}) {
						return
					}
				}
				if dt.Function.Arguments != "" {
					acc.args.WriteString(dt.Function.Arguments)
					if !send(llm.StreamEvent{Kind: llm.EventToolCallDelta, Index: acc.index,
						Text: dt.Function.Arguments}) {
						return
					}
				}
			}
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
		}
	}
	if err := ctx.Err(); err != nil {
		send(llm.StreamEvent{Kind: llm.EventError, Err: mapError(m.provider, err)})
		return
	}
	if err := stream.Err(); err != nil {
		send(llm.StreamEvent{Kind: llm.EventError, Err: mapError(m.provider, err)})
		return
	}

	msg := &llm.Message{Role: llm.RoleAssistant}
	if reasoning.Len() > 0 {
		msg.Parts = append(msg.Parts, llm.Reasoning(reasoning.String()))
	}
	if text.Len() > 0 {
		msg.Parts = append(msg.Parts, llm.Text(text.String()))
	}
	for _, acc := range calls {
		args := acc.args.String()
		if args == "" {
			// 无参调用：词汇表要求 Arguments 为合法 JSON。
			args = "{}"
		}
		msg.Parts = append(msg.Parts, llm.Call(llm.ToolCall{ID: acc.id, Name: acc.name, Arguments: json.RawMessage(args)}))
	}
	resp := &llm.Response{Message: msg, FinishReason: mapFinishReason(finish)}
	if usageSeen {
		resp.Usage = mapUsage(usage)
	}
	send(llm.StreamEvent{Kind: llm.EventDone, Response: resp})
}

// mapCompletionsMessage 翻译非流式响应消息。思维链取 reasoning_content
// 兼容字段（DeepSeek 等网关风格，官方 SDK 未建类型，走原始 JSON）。
func mapCompletionsMessage(msg *sdk.ChatCompletionMessage) *llm.Message {
	parts := make([]llm.Part, 0, len(msg.ToolCalls)+2)
	var reasoning string
	if f, ok := msg.JSON.ExtraFields["reasoning_content"]; ok {
		// 同上：ExtraFields 不吃 Valid()，以 Raw() 为准。
		_ = json.Unmarshal([]byte(f.Raw()), &reasoning)
	}
	if reasoning != "" {
		parts = append(parts, llm.Reasoning(reasoning))
	}
	if msg.Content != "" {
		parts = append(parts, llm.Text(msg.Content))
	}
	for _, tc := range msg.ToolCalls {
		// 本包只声明 function 工具，custom 变体不会出现。
		if tc.Type == "function" {
			parts = append(parts, llm.Call(llm.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: json.RawMessage(tc.Function.Arguments),
			}))
		}
	}
	return &llm.Message{Role: llm.RoleAssistant, Parts: parts}
}

// mapFinishReason 映射结束原因；function_call 是已废弃的旧值，
// 语义等同 tool_calls。
func mapFinishReason(s string) llm.FinishReason {
	switch s {
	case "", "stop":
		return llm.FinishStop
	case "tool_calls", "function_call":
		return llm.FinishToolCalls
	case "length":
		return llm.FinishLength
	case "content_filter":
		return llm.FinishContentFilter
	default:
		return llm.FinishError
	}
}

// mapUsage 映射 token 计量；缓存命中数取 prompt_tokens_details。
func mapUsage(u sdk.CompletionUsage) llm.TokenUsage {
	return llm.TokenUsage{
		InputTokens:       int(u.PromptTokens),
		OutputTokens:      int(u.CompletionTokens),
		CachedInputTokens: int(u.PromptTokensDetails.CachedTokens),
	}
}

// joinText 拼接全部文本块（\n 连接）；无文本块返回空串。
func joinText(parts []llm.Part) string {
	var sb strings.Builder
	for i := range parts {
		if parts[i].Kind == llm.PartText {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(parts[i].Text)
		}
	}
	return sb.String()
}
