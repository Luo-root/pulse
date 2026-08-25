package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/Luo-root/pulse/llm"
	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// NewResponses 构造 Responses 线协议的模型实例
// （provider 名 "openai-responses"）。
func NewResponses(cfg llm.Config) (llm.ChatModel, error) {
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	return &responsesModel{client: client, provider: cfg.Provider, model: cfg.Model}, nil
}

// responsesModel 是无状态的 ChatModel 实现：并发使用安全。
//
// 本变体不使用 PreviousResponseID/Conversations 服务端状态：
// Store 显式置 false，每回合由调用方全量传入历史。
type responsesModel struct {
	client   sdk.Client
	provider string
	model    string
}

// 编译期断言：满足 ChatModel 契约。
var _ llm.ChatModel = (*responsesModel)(nil)

// rtcAcc 累积一个流式工具调用（按 output item 的 ID 关联分片）。
type rtcAcc struct {
	index int // 对外事件序号：1 起按到达顺序（0 保留给文本/思维链）
	id    string
	name  string
	args  strings.Builder
}

func (m *responsesModel) Generate(ctx context.Context, req *llm.GenerateRequest) (*llm.Response, error) {
	params, err := m.buildParams(req)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Responses.New(ctx, params)
	if err != nil {
		return nil, mapError(m.provider, err)
	}
	if resp.Error.Code != "" {
		// 服务端把失败编码在 200 响应体内（非流式路径）。
		return nil, llm.NewError(classifyStatus(0, string(resp.Error.Code), resp.Error.Message),
			m.provider, 0, nil, "%s: %s", resp.Error.Code, resp.Error.Message)
	}
	msg, finish, usage := mapResponsesResponse(resp)
	return &llm.Response{Message: msg, FinishReason: finish, Usage: usage}, nil
}

func (m *responsesModel) Stream(ctx context.Context, req *llm.GenerateRequest) (<-chan llm.StreamEvent, error) {
	params, err := m.buildParams(req)
	if err != nil {
		return nil, err
	}
	stream := m.client.Responses.NewStreaming(ctx, params)
	ch := make(chan llm.StreamEvent, 16)
	go m.pump(ctx, stream, ch)
	return ch, nil
}

// buildParams 把 llm.GenerateRequest 翻译为 Responses 请求。
func (m *responsesModel) buildParams(req *llm.GenerateRequest) (responses.ResponseNewParams, error) {
	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(m.model),
		// 无状态适配：不依赖服务端会话存储，历史由调用方全量传入。
		Store: param.NewOpt(false),
	}

	var instructions []string
	items := make(responses.ResponseInputParam, 0, len(req.Messages))
	for _, msg := range req.Messages {
		switch msg.Role {
		case llm.RoleSystem:
			// Responses 的系统提示走顶层 instructions 字段。
			instructions = append(instructions, joinText(msg.Parts))

		case llm.RoleAssistant:
			var texts []string
			for i := range msg.Parts {
				p := &msg.Parts[i]
				switch p.Kind {
				case llm.PartText:
					texts = append(texts, p.Text)
				case llm.PartToolCall:
					tc := p.ToolCallValue
					if tc == nil {
						return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
							"assistant 消息第 %d 块 ToolCallValue 为空", i)
					}
					items = append(items, responses.ResponseInputItemUnionParam{
						OfFunctionCall: &responses.ResponseFunctionToolCallParam{
							CallID:    tc.ID,
							Name:      tc.Name,
							Arguments: string(tc.Arguments),
						},
					})
				case llm.PartReasoning:
					// 不回传思维链：有状态 reasoning 链接依赖
					// PreviousResponseID/encrypted_content，本适配无状态。
				default:
					return params, unsupportedPart(m.provider, msg.Role, p.Kind)
				}
			}
			if len(texts) > 0 {
				items = append(items, easyMessage(responses.EasyInputMessageRoleAssistant, strings.Join(texts, "\n"), nil))
			}

		case llm.RoleUser, llm.RoleTool:
			var parts []responses.ResponseInputContentUnionParam
			var texts []string
			flushUser := func() {
				if len(texts) == 0 && len(parts) == 0 {
					return
				}
				items = append(items, easyMessage(responses.EasyInputMessageRoleUser, strings.Join(texts, "\n"), parts))
				texts, parts = nil, nil
			}
			for i := range msg.Parts {
				p := &msg.Parts[i]
				switch p.Kind {
				case llm.PartText:
					texts = append(texts, p.Text)
				case llm.PartImage:
					ref, err := imageRef(m.provider, p.Image)
					if err != nil {
						return params, err
					}
					parts = append(parts, responses.ResponseInputContentUnionParam{
						OfInputImage: &responses.ResponseInputImageParam{ImageURL: param.NewOpt(ref)},
					})
				case llm.PartCustom:
					part, err := m.convertCustomPart(p)
					if err != nil {
						return params, err
					}
					parts = append(parts, part)
				case llm.PartToolResult:
					flushUser()
					tr := p.ToolResultValue
					if tr == nil {
						return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
							"%s 消息第 %d 块 ToolResultValue 为空", msg.Role, i)
					}
					items = append(items, responses.ResponseInputItemUnionParam{
						OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
							CallID: tr.ToolCallID,
							Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
								OfString: param.NewOpt(joinText(tr.Content)),
							},
						},
					})
				case llm.PartReasoning:
					// 不回传。
				default:
					return params, unsupportedPart(m.provider, msg.Role, p.Kind)
				}
			}
			flushUser()

		default:
			return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil, "未知消息角色 %q", msg.Role)
		}
	}
	params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: items}
	if len(instructions) > 0 {
		params.Instructions = param.NewOpt(strings.Join(instructions, "\n\n"))
	}

	if req.Temperature != nil {
		params.Temperature = param.NewOpt(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = param.NewOpt(*req.TopP)
	}
	if req.MaxTokens != nil {
		params.MaxOutputTokens = param.NewOpt(int64(*req.MaxTokens))
	}
	if len(req.StopSequences) > 0 {
		// Responses 线格式没有 stop 参数——显式报错，不静默丢弃。
		return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
			"Responses 协议不支持 StopSequences：请改用 %s provider 或去掉该字段", ProviderCompletions)
	}
	for _, t := range req.Tools {
		fd := responses.FunctionToolParam{
			Name:   t.Name,
			Strict: param.NewOpt(false), // 线格式必填；默认不强制 schema 严格校验
		}
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
		params.Tools = append(params.Tools, responses.ToolUnionParam{OfFunction: &fd})
	}
	if req.ToolChoice != nil {
		switch req.ToolChoice.Mode {
		case llm.ToolAuto:
			params.ToolChoice.OfToolChoiceMode = param.NewOpt(responses.ToolChoiceOptionsAuto)
		case llm.ToolNone:
			params.ToolChoice.OfToolChoiceMode = param.NewOpt(responses.ToolChoiceOptionsNone)
		case llm.ToolAny:
			params.ToolChoice.OfToolChoiceMode = param.NewOpt(responses.ToolChoiceOptionsRequired)
		case llm.ToolSpecific:
			params.ToolChoice.OfFunctionTool = &responses.ToolChoiceFunctionParam{Name: req.ToolChoice.Name}
		default:
			return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
				"未知 ToolChoiceMode %q", req.ToolChoice.Mode)
		}
	}
	if rf := req.ResponseFormat; rf != nil {
		switch rf.Type {
		case llm.FormatJSONObject:
			v := shared.NewResponseFormatJSONObjectParam()
			params.Text.Format = responses.ResponseFormatTextConfigUnionParam{OfJSONObject: &v}
		case llm.FormatJSONSchema:
			name := rf.Name
			if name == "" {
				name = "response" // 线格式必填 schema 名
			}
			var schema map[string]any
			if err := json.Unmarshal(rf.Schema, &schema); err != nil {
				return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, err,
					"ResponseFormat.Schema 不是合法 JSON")
			}
			params.Text.Format = responses.ResponseFormatTextConfigParamOfJSONSchema(name, schema)
		case llm.FormatText, "":
			// 纯文本是默认行为，不下发。
		default:
			return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
				"未知 ResponseFormatType %q", rf.Type)
		}
	}
	if len(req.Metadata) > 0 {
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

// easyMessage 构造 EasyInput 消息项：纯文本走字符串内容，图文混合
// 走内容块列表。
func easyMessage(role responses.EasyInputMessageRole, text string, extraParts []responses.ResponseInputContentUnionParam) responses.ResponseInputItemUnionParam {
	msg := responses.EasyInputMessageParam{Role: role}
	if len(extraParts) == 0 {
		msg.Content.OfString = param.NewOpt(text)
	} else {
		list := make(responses.ResponseInputMessageContentListParam, 0, len(extraParts)+1)
		if text != "" {
			list = append(list, responses.ResponseInputContentUnionParam{
				OfInputText: &responses.ResponseInputTextParam{Text: text},
			})
		}
		list = append(list, extraParts...)
		msg.Content.OfInputItemContentList = list
	}
	return responses.ResponseInputItemUnionParam{OfMessage: &msg}
}

// convertCustomPart 按 MIME 家族映射：image → input_image；pdf → input_file；
// video → input_video；audio → input_audio（后两者是兼容网关扩展）。
func (m *responsesModel) convertCustomPart(p *llm.Part) (responses.ResponseInputContentUnionParam, error) {
	var empty responses.ResponseInputContentUnionParam
	if p.Media == nil {
		return empty, unsupportedPart(m.provider, llm.RoleUser, p.Kind)
	}
	kind := classifyMIME(p.Media.MediaType)
	switch kind {
	case mediaImage:
		ref, err := mediaRef(m.provider, p.Media.Data, p.Media.MediaType, p.Media.URL)
		if err != nil {
			return empty, err
		}
		return responses.ResponseInputContentUnionParam{
			OfInputImage: &responses.ResponseInputImageParam{ImageURL: param.NewOpt(ref)},
		}, nil
	case mediaPDF:
		file := responses.ResponseInputFileParam{
			Filename: param.NewOpt(mediaFilename(p.Media, "document.pdf")),
		}
		if len(p.Media.Data) > 0 {
			file.FileData = param.NewOpt("data:application/pdf;base64," + base64.StdEncoding.EncodeToString(p.Media.Data))
		} else if p.Media.URL != "" {
			file.FileURL = param.NewOpt(p.Media.URL)
		} else {
			return empty, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil, "PDF 块既无 Data 也无 URL")
		}
		return responses.ResponseInputContentUnionParam{OfInputFile: &file}, nil
	case mediaVideo:
		ref, err := mediaRef(m.provider, p.Media.Data, p.Media.MediaType, p.Media.URL)
		if err != nil {
			return empty, err
		}
		part, err := overrideResponsesPart(map[string]any{
			"type": "input_video", "video_url": map[string]string{"url": ref},
		})
		if err != nil {
			return empty, llm.NewError(llm.ErrBadRequest, m.provider, 0, err, "序列化 input_video 失败")
		}
		return part, nil
	case mediaAudio:
		ref, err := mediaRef(m.provider, p.Media.Data, p.Media.MediaType, p.Media.URL)
		if err != nil {
			return empty, err
		}
		part, err := overrideResponsesPart(map[string]any{
			"type": "input_audio", "input_audio": map[string]string{"data": ref, "format": audioFormat(p.Media.MediaType)},
		})
		if err != nil {
			return empty, llm.NewError(llm.ErrBadRequest, m.provider, 0, err, "序列化 input_audio 失败")
		}
		return part, nil
	default:
		return empty, unsupportedPart(m.provider, llm.RoleUser, p.Kind)
	}
}

// pump 消费 SDK 流并翻译为 llm.StreamEvent。
//
// 事件映射：output_text.delta → text_delta；reasoning_text/summary_text.delta →
// reasoning_delta；output_item.added(function_call) → tool_call_begin；
// function_call_arguments.delta → tool_call_delta；completed/incomplete →
// 聚合 done；failed/error → error。
func (m *responsesModel) pump(ctx context.Context, stream *ssestream.Stream[responses.ResponseStreamEventUnion], ch chan<- llm.StreamEvent) {
	defer close(ch)
	send := func(ev llm.StreamEvent) bool {
		return sendEvent(ctx, ch, m.provider, ev)
	}

	var (
		text      strings.Builder
		reasoning strings.Builder
		accs      []*rtcAcc
		byItem    = map[string]*rtcAcc{}
		done      bool
	)
	toolEvent := func(acc *rtcAcc, kind llm.StreamEventKind, fragment string) bool {
		return send(llm.StreamEvent{Kind: kind, Index: acc.index, Text: fragment,
			CallID: acc.id, ToolName: acc.name})
	}
	for stream.Next() {
		switch ev := stream.Current().AsAny().(type) {
		case responses.ResponseTextDeltaEvent:
			text.WriteString(ev.Delta)
			if !send(llm.StreamEvent{Kind: llm.EventTextDelta, Index: 0, Text: ev.Delta}) {
				return
			}

		case responses.ResponseReasoningTextDeltaEvent:
			reasoning.WriteString(ev.Delta)
			if !send(llm.StreamEvent{Kind: llm.EventReasoningDelta, Index: 0, Text: ev.Delta}) {
				return
			}

		case responses.ResponseReasoningSummaryTextDeltaEvent:
			reasoning.WriteString(ev.Delta)
			if !send(llm.StreamEvent{Kind: llm.EventReasoningDelta, Index: 0, Text: ev.Delta}) {
				return
			}

		case responses.ResponseOutputItemAddedEvent:
			fc, ok := ev.Item.AsAny().(responses.ResponseFunctionToolCall)
			if !ok {
				continue // 消息/思维链等项的 added 事件不产生决策级事件
			}
			acc := &rtcAcc{index: len(accs) + 1, id: fc.CallID, name: fc.Name}
			accs = append(accs, acc)
			byItem[ev.Item.ID] = acc
			if !send(llm.StreamEvent{Kind: llm.EventToolCallBegin, Index: acc.index,
				CallID: acc.id, ToolName: acc.name}) {
				return
			}

		case responses.ResponseFunctionCallArgumentsDeltaEvent:
			acc := byItem[ev.ItemID]
			if acc == nil {
				// 未见过 added 事件（异常服务端）：兜底建账，ID/名留空。
				acc = &rtcAcc{index: len(accs) + 1}
				accs = append(accs, acc)
				byItem[ev.ItemID] = acc
				if !send(llm.StreamEvent{Kind: llm.EventToolCallBegin, Index: acc.index}) {
					return
				}
			}
			acc.args.WriteString(ev.Delta)
			if !toolEvent(acc, llm.EventToolCallDelta, ev.Delta) {
				return
			}

		case responses.ResponseCompletedEvent:
			msg, finish, usage := mapResponsesResponse(&ev.Response)
			if !send(llm.StreamEvent{Kind: llm.EventDone, Response: &llm.Response{
				Message: msg, FinishReason: finish, Usage: usage}}) {
				return
			}
			done = true

		case responses.ResponseIncompleteEvent:
			msg, _, usage := mapResponsesResponse(&ev.Response)
			if !send(llm.StreamEvent{Kind: llm.EventDone, Response: &llm.Response{
				Message:      msg,
				FinishReason: incompleteFinish(&ev.Response),
				Usage:        usage}}) {
				return
			}
			done = true

		case responses.ResponseFailedEvent:
			send(llm.StreamEvent{Kind: llm.EventError, Err: llm.NewError(
				classifyStatus(0, string(ev.Response.Error.Code), ev.Response.Error.Message),
				m.provider, 0, nil, "response failed: %s: %s",
				ev.Response.Error.Code, ev.Response.Error.Message)})
			return

		case responses.ResponseErrorEvent:
			send(llm.StreamEvent{Kind: llm.EventError, Err: llm.NewError(
				classifyStatus(0, ev.Code, ev.Message), m.provider, 0, nil,
				"%s: %s", ev.Code, ev.Message)})
			return
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
	if !done {
		// 流在完成事件前静默结束：对消费方这是基础设施失败。
		send(llm.StreamEvent{Kind: llm.EventError, Err: llm.NewError(
			llm.ErrProvider, m.provider, 0, nil, "流在 completed 事件前结束")})
	}
}

// mapResponsesResponse 翻译非流式响应对象（Generate 与流式 done 共用）。
func mapResponsesResponse(resp *responses.Response) (*llm.Message, llm.FinishReason, llm.TokenUsage) {
	msg := &llm.Message{Role: llm.RoleAssistant}
	for _, item := range resp.Output {
		switch v := item.AsAny().(type) {
		case responses.ResponseOutputMessage:
			for _, c := range v.Content {
				switch c.Type {
				case "output_text":
					if c.Text != "" {
						msg.Parts = append(msg.Parts, llm.Text(c.Text))
					}
				case "refusal":
					if c.Refusal != "" {
						msg.Parts = append(msg.Parts, llm.Text(c.Refusal))
					}
				}
			}
		case responses.ResponseFunctionToolCall:
			args := v.Arguments
			if args == "" {
				args = "{}"
			}
			msg.Parts = append(msg.Parts, llm.Call(llm.ToolCall{
				ID: v.CallID, Name: v.Name, Arguments: json.RawMessage(args)}))
		case responses.ResponseReasoningItem:
			var sb strings.Builder
			for _, s := range v.Summary {
				sb.WriteString(s.Text)
			}
			for _, c := range v.Content {
				sb.WriteString(c.Text)
			}
			if sb.Len() > 0 {
				msg.Parts = append(msg.Parts, llm.Reasoning(sb.String()))
			}
		}
	}
	usage := llm.TokenUsage{
		InputTokens:       int(resp.Usage.InputTokens),
		OutputTokens:      int(resp.Usage.OutputTokens),
		CachedInputTokens: int(resp.Usage.InputTokensDetails.CachedTokens),
	}
	finish := llm.FinishStop
	if len(msg.ToolCalls()) > 0 {
		finish = llm.FinishToolCalls
	} else if resp.Status == "incomplete" {
		finish = incompleteFinish(resp)
	}
	return msg, finish, usage
}

// incompleteFinish 映射不完整结束原因；未知原因按截断处理。
func incompleteFinish(resp *responses.Response) llm.FinishReason {
	switch resp.IncompleteDetails.Reason {
	case "content_filter":
		return llm.FinishContentFilter
	default:
		return llm.FinishLength
	}
}
