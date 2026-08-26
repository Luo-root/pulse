package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/Luo-root/pulse/llm"
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

// New 构造 Messages 线协议的模型实例（provider 名 "anthropic"）。
func New(cfg llm.Config) (llm.ChatModel, error) {
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	return &messagesModel{client: client, provider: cfg.Provider, model: cfg.Model}, nil
}

// messagesModel 是无状态的 ChatModel 实现：并发使用安全。
type messagesModel struct {
	client   sdk.Client
	provider string
	model    string
}

// 编译期断言：满足 ChatModel 契约。
var _ llm.ChatModel = (*messagesModel)(nil)

func (m *messagesModel) Generate(ctx context.Context, req *llm.GenerateRequest) (*llm.Response, error) {
	params, err := m.buildParams(req)
	if err != nil {
		return nil, err
	}
	msg, err := m.client.Messages.New(ctx, params)
	if err != nil {
		return nil, mapError(m.provider, err)
	}
	return mapResponse(m.provider, msg)
}

func (m *messagesModel) Stream(ctx context.Context, req *llm.GenerateRequest) (<-chan llm.StreamEvent, error) {
	params, err := m.buildParams(req)
	if err != nil {
		return nil, err
	}
	stream := m.client.Messages.NewStreaming(ctx, params)
	ch := make(chan llm.StreamEvent, 16)
	go m.pump(ctx, stream, req, ch)
	return ch, nil
}

// buildParams 把 llm.GenerateRequest 翻译为 Messages 请求。
func (m *messagesModel) buildParams(req *llm.GenerateRequest) (sdk.MessageNewParams, error) {
	params := sdk.MessageNewParams{
		Model: sdk.Model(m.model),
	}
	// Anthropic 的 max_tokens 是必填参数，provider 没有默认值——
	// 不设魔法默认值，缺了就显式报错。
	if req.MaxTokens == nil {
		return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
			"anthropic 线格式要求 max_tokens：请设置 GenerateRequest.MaxTokens")
	}
	params.MaxTokens = int64(*req.MaxTokens)

	// system 不是消息角色，是顶层参数；多条合并。
	var system []string
	msgs := make([]sdk.MessageParam, 0, len(req.Messages))
	for _, msg := range req.Messages {
		switch msg.Role {
		case llm.RoleSystem:
			system = append(system, joinText(msg.Parts))
		default:
			mp, err := m.convertMessage(msg)
			if err != nil {
				return params, err
			}
			msgs = append(msgs, mp)
		}
	}
	params.Messages = msgs
	if len(system) > 0 {
		params.System = []sdk.TextBlockParam{{Text: strings.Join(system, "\n\n")}}
	}

	if req.Temperature != nil {
		params.Temperature = param.NewOpt(*req.Temperature)
	}
	if req.TopP != nil {
		params.TopP = param.NewOpt(*req.TopP)
	}
	if len(req.StopSequences) > 0 {
		params.StopSequences = req.StopSequences
	}
	for _, t := range req.Tools {
		tool := sdk.ToolParam{Name: t.Name}
		if t.Description != "" {
			tool.Description = param.NewOpt(t.Description)
		}
		if len(t.Parameters) > 0 {
			var schema map[string]any
			if err := json.Unmarshal(t.Parameters, &schema); err != nil {
				return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, err,
					"工具 %s 的参数 Schema 不是合法 JSON 对象", t.Name)
			}
			tool.InputSchema.Properties = schema
		}
		params.Tools = append(params.Tools, sdk.ToolUnionParam{OfTool: &tool})
	}
	if req.ToolChoice != nil {
		switch req.ToolChoice.Mode {
		case llm.ToolAuto:
			params.ToolChoice.OfAuto = &sdk.ToolChoiceAutoParam{}
		case llm.ToolNone:
			params.ToolChoice.OfNone = &sdk.ToolChoiceNoneParam{}
		case llm.ToolAny:
			params.ToolChoice.OfAny = &sdk.ToolChoiceAnyParam{}
		case llm.ToolSpecific:
			params.ToolChoice.OfTool = &sdk.ToolChoiceToolParam{Name: req.ToolChoice.Name}
		default:
			return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
				"未知 ToolChoiceMode %q", req.ToolChoice.Mode)
		}
	}
	if req.ResponseFormat != nil {
		switch req.ResponseFormat.Type {
		case llm.FormatText, "":
			// 纯文本是默认行为，不下发。
		default:
			// Anthropic Messages 没有结构化输出参数；json_schema 靠
			// 提示词工程是上层的事，这里显式拒绝而不是静默忽略。
			return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
				"anthropic 线格式不支持 ResponseFormat：%s", req.ResponseFormat.Type)
		}
	}
	if req.Audio != nil {
		return params, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
			"anthropic 线格式不支持 Audio 输出")
	}
	if len(req.Metadata) > 0 {
		md := sdk.MetadataParam{}
		if id, ok := req.Metadata["user_id"].(string); ok {
			md.UserID = param.NewOpt(id)
		}
		if md.UserID.Valid() {
			params.Metadata = md
		}
	}
	return params, nil
}

// convertMessage 翻译 user / assistant / tool 消息。Anthropic 只认
// user / assistant 两种角色；tool 结果是 user 消息里的 tool_result 块。
func (m *messagesModel) convertMessage(msg *llm.Message) (sdk.MessageParam, error) {
	var blocks []sdk.ContentBlockParamUnion
	switch msg.Role {
	case llm.RoleAssistant:
		for i := range msg.Parts {
			p := &msg.Parts[i]
			switch p.Kind {
			case llm.PartText:
				if p.Text != "" {
					blocks = append(blocks, sdk.NewTextBlock(p.Text))
				}
			case llm.PartToolCall:
				tc := p.ToolCallValue
				if tc == nil {
					return sdk.MessageParam{}, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
						"assistant 消息的 ToolCallValue 为空")
				}
				input := tc.Arguments
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				blocks = append(blocks, sdk.ContentBlockParamUnion{
					OfToolUse: &sdk.ToolUseBlockParam{ID: tc.ID, Name: tc.Name, Input: input},
				})
			case llm.PartReasoning:
				// 不回传：thinking 块回传需要 signature 原样配对，
				// 词汇表不承载签名。
			default:
				return sdk.MessageParam{}, unsupportedPart(m.provider, msg.Role, p.Kind)
			}
		}
		return sdk.NewAssistantMessage(blocks...), nil

	case llm.RoleUser, llm.RoleTool:
		// Anthropic 要求 tool_result 位于 user 轮的首位：先收
		// tool_result，再收其余内容。
		var results, rest []sdk.ContentBlockParamUnion
		for i := range msg.Parts {
			p := &msg.Parts[i]
			switch p.Kind {
			case llm.PartText:
				if p.Text != "" {
					rest = append(rest, sdk.NewTextBlock(p.Text))
				}
			case llm.PartImage:
				block, err := m.imageBlock(p.Image)
				if err != nil {
					return sdk.MessageParam{}, err
				}
				rest = append(rest, block)
			case llm.PartToolResult:
				tr := p.ToolResultValue
				if tr == nil {
					return sdk.MessageParam{}, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil,
						"%s 消息的 ToolResultValue 为空", msg.Role)
				}
				// 结果内容只映射文本块；错误以 IsError 标记。
				isError := param.NewOpt(tr.IsError)
				if !tr.IsError {
					isError = param.Opt[bool]{} // false 是默认值，不下发
				}
				results = append(results, sdk.ContentBlockParamUnion{
					OfToolResult: &sdk.ToolResultBlockParam{
						ToolUseID: tr.ToolCallID,
						IsError:   isError,
						Content: []sdk.ToolResultBlockParamContentUnion{
							{OfText: &sdk.TextBlockParam{Text: joinText(tr.Content)}},
						},
					},
				})
			case llm.PartCustom:
				block, err := m.customBlock(msg.Role, p)
				if err != nil {
					return sdk.MessageParam{}, err
				}
				rest = append(rest, block)
			case llm.PartReasoning:
				// 不回传。
			default:
				return sdk.MessageParam{}, unsupportedPart(m.provider, msg.Role, p.Kind)
			}
		}
		blocks = append(results, rest...)
		return sdk.MessageParam{Role: sdk.MessageParamRoleUser, Content: blocks}, nil

	default:
		return sdk.MessageParam{}, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil, "未知消息角色 %q", msg.Role)
	}
}

// imageBlock 映射图像：内联字节走 base64 source，URL 走 url source。
func (m *messagesModel) imageBlock(src *llm.ImageSource) (sdk.ContentBlockParamUnion, error) {
	var empty sdk.ContentBlockParamUnion
	if src == nil {
		return empty, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil, "图像块缺少 ImageSource")
	}
	switch {
	case len(src.Data) > 0:
		if src.MediaType == "" {
			return empty, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil, "内联图像缺少 MediaType")
		}
		return sdk.ContentBlockParamUnion{
			OfImage: &sdk.ImageBlockParam{
				Source: sdk.ImageBlockParamSourceUnion{
					OfBase64: &sdk.Base64ImageSourceParam{
						Data:      base64.StdEncoding.EncodeToString(src.Data),
						MediaType: sdk.Base64ImageSourceMediaType(src.MediaType),
					},
				},
			},
		}, nil
	case src.URL != "":
		return sdk.ContentBlockParamUnion{
			OfImage: &sdk.ImageBlockParam{
				Source: sdk.ImageBlockParamSourceUnion{
					OfURL: &sdk.URLImageSourceParam{URL: src.URL},
				},
			},
		}, nil
	default:
		return empty, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil, "图像块既无 Data 也无 URL")
	}
}

// MIME 家族分类：Anthropic 支持的只有 image 与 PDF；audio / video
// 显式报错，不静默丢弃。
const (
	mediaImage = "image"
	mediaVideo = "video"
	mediaAudio = "audio"
	mediaPDF   = "pdf"
)

func classifyMIME(mediaType string) string {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	switch {
	case strings.HasPrefix(mt, "image/"):
		return mediaImage
	case strings.HasPrefix(mt, "video/"):
		return mediaVideo
	case strings.HasPrefix(mt, "audio/"):
		return mediaAudio
	case mt == "application/pdf":
		return mediaPDF
	default:
		return ""
	}
}

// customBlock 映射开放模态：PDF → document 块（base64 / URL）；
// audio / video / 其他显式报错——Anthropic 不支持，不静默丢弃。
func (m *messagesModel) customBlock(role llm.Role, p *llm.Part) (sdk.ContentBlockParamUnion, error) {
	var empty sdk.ContentBlockParamUnion
	if p.Media == nil {
		return empty, unsupportedPart(m.provider, role, p.Kind)
	}
	switch classifyMIME(p.Media.MediaType) {
	case mediaPDF:
		doc := sdk.DocumentBlockParam{}
		switch {
		case len(p.Media.Data) > 0:
			doc.Source.OfBase64 = &sdk.Base64PDFSourceParam{
				Data: base64.StdEncoding.EncodeToString(p.Media.Data),
			}
		case p.Media.URL != "":
			doc.Source.OfURL = &sdk.URLPDFSourceParam{URL: p.Media.URL}
		default:
			return empty, llm.NewError(llm.ErrBadRequest, m.provider, 0, nil, "PDF 块既无 Data 也无 URL")
		}
		return sdk.ContentBlockParamUnion{OfDocument: &doc}, nil
	default:
		return empty, unsupportedPart(m.provider, role, p.Kind)
	}
}

// mapResponse 翻译非流式响应。
func mapResponse(provider string, msg *sdk.Message) (*llm.Response, error) {
	parts := make([]llm.Part, 0, len(msg.Content))
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, llm.Text(block.Text))
			}
		case "thinking":
			if block.Thinking != "" {
				parts = append(parts, llm.Reasoning(block.Thinking))
			}
		case "redacted_thinking":
			// 加密思维链：无签名无法回传，跳过。
		case "tool_use":
			args := block.Input
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			parts = append(parts, llm.Call(llm.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: args,
			}))
		}
	}
	usage := llm.TokenUsage{
		InputTokens:       int(msg.Usage.InputTokens),
		OutputTokens:      int(msg.Usage.OutputTokens),
		CachedInputTokens: int(msg.Usage.CacheReadInputTokens),
	}
	return &llm.Response{
		Message:      &llm.Message{Role: llm.RoleAssistant, Parts: parts},
		FinishReason: mapFinishReason(msg.StopReason),
		Usage:        usage,
	}, nil
}

// mapFinishReason 映射结束原因；pause_turn 是服务端暂停续传标记，
// 对调用方等价于本轮自然结束。
func mapFinishReason(s sdk.StopReason) llm.FinishReason {
	switch s {
	case sdk.StopReasonToolUse:
		return llm.FinishToolCalls
	case sdk.StopReasonMaxTokens, sdk.StopReasonModelContextWindowExceeded:
		return llm.FinishLength
	case sdk.StopReasonRefusal:
		return llm.FinishContentFilter
	case "", sdk.StopReasonEndTurn, sdk.StopReasonStopSequence, sdk.StopReasonPauseTurn:
		return llm.FinishStop
	default:
		return llm.FinishError
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

// pump 消费 SDK 流并翻译为 llm.StreamEvent。
//
// 事件映射：content_block_delta.text_delta → text_delta；
// thinking_delta → reasoning_delta；input_json_delta → tool_call_delta
// （其 content_block_start 已发过 begin）；message_delta → 记录
// stop_reason 与输出用量；message_stop → 聚合 done。
func (m *messagesModel) pump(ctx context.Context, stream *ssestream.Stream[sdk.MessageStreamEventUnion], req *llm.GenerateRequest, ch chan<- llm.StreamEvent) {
	defer close(ch)
	send := func(ev llm.StreamEvent) bool {
		select {
		case ch <- ev:
			return true
		case <-ctx.Done():
			select {
			case ch <- llm.StreamEvent{Kind: llm.EventError, Err: mapError(m.provider, ctx.Err())}:
			default:
			}
			return false
		}
	}

	var (
		text      strings.Builder
		reasoning strings.Builder
		calls     []*tcAcc
		finish    sdk.StopReason
		usage     llm.TokenUsage
	)
	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		case "message_start":
			usage.InputTokens = int(ev.Message.Usage.InputTokens)
			usage.CachedInputTokens = int(ev.Message.Usage.CacheReadInputTokens)

		case "content_block_start":
			cb := ev.ContentBlock
			if cb.Type != "tool_use" {
				continue
			}
			// llm 契约：Index 0 = 文本/思维链，1 起按到达顺序编号工具
			// 调用（线格式的 block index 只用于关联后续参数分片）。
			acc := &tcAcc{index: len(calls) + 1, srcIndex: int(ev.Index), id: cb.ID, name: cb.Name, begun: true}
			calls = append(calls, acc)
			if !send(llm.StreamEvent{Kind: llm.EventToolCallBegin, Index: acc.index,
				CallID: acc.id, ToolName: acc.name}) {
				return
			}

		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				text.WriteString(ev.Delta.Text)
				if !send(llm.StreamEvent{Kind: llm.EventTextDelta, Index: 0, Text: ev.Delta.Text}) {
					return
				}
			case "thinking_delta":
				reasoning.WriteString(ev.Delta.Thinking)
				if !send(llm.StreamEvent{Kind: llm.EventReasoningDelta, Index: 0, Text: ev.Delta.Thinking}) {
					return
				}
			case "input_json_delta":
				idx := int(ev.Index)
				var acc *tcAcc
				for _, a := range calls {
					if a.srcIndex == idx {
						acc = a
						break
					}
				}
				if acc == nil {
					// 未见过 content_block_start（异常服务端）：兜底建账。
					acc = &tcAcc{index: len(calls) + 1, srcIndex: idx}
					calls = append(calls, acc)
					if !send(llm.StreamEvent{Kind: llm.EventToolCallBegin, Index: acc.index}) {
						return
					}
				}
				acc.args.WriteString(ev.Delta.PartialJSON)
				if !send(llm.StreamEvent{Kind: llm.EventToolCallDelta, Index: acc.index,
					Text: ev.Delta.PartialJSON}) {
					return
				}
			case "signature_delta":
				// 签名增量：词汇表不承载，跳过。
			}

		case "message_delta":
			if ev.Delta.StopReason != "" {
				finish = ev.Delta.StopReason
			}
			usage.OutputTokens = int(ev.Usage.OutputTokens)
			usage.InputTokens = int(ev.Usage.InputTokens)
			usage.CachedInputTokens = int(ev.Usage.CacheReadInputTokens)

		case "message_stop":
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
					args = "{}" // 无参调用：词汇表要求合法 JSON
				}
				msg.Parts = append(msg.Parts, llm.Call(llm.ToolCall{
					ID: acc.id, Name: acc.name, Arguments: json.RawMessage(args)}))
			}
			if !send(llm.StreamEvent{Kind: llm.EventDone, Response: &llm.Response{
				Message:      msg,
				FinishReason: mapFinishReason(finish),
				Usage:        usage,
			}}) {
				return
			}
			// message_stop 是 Anthropic 流的最后一条事件：done 后
			// 直接返回，避免把连接关闭误判为异常终止。
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
	// message_stop 缺失：流在完成事件前静默结束，对消费方是基础设施失败。
	send(llm.StreamEvent{Kind: llm.EventError, Err: llm.NewError(
		llm.ErrProvider, m.provider, 0, nil, "流在 message_stop 事件前结束")})
}

// tcAcc 累积一个流式工具调用（Anthropic 按 content block index 分片）。
type tcAcc struct {
	index    int // 对外事件序号：block index+1（0 保留给文本/思维链）
	srcIndex int // 线格式里的 content block index
	id       string
	name     string
	args     strings.Builder
	begun    bool
}
