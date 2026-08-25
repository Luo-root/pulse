package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/Luo-root/pulse/llm"
	sdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

// 适配层一次性打通的 MIME 家族：调用方只给 PartImage / PartCustom，
// 不再在使用处按供应商手写线格式。官方不认的块（video_url /
// input_video）用 Override 发出，兼容网关能吃就吃，官方端点
// 会 bad_request——不静默丢弃。
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

// mediaRef 把内联字节或 URL 归一为线格式引用（data URI 或原 URL）。
func mediaRef(provider string, data []byte, mediaType, rawURL string) (string, error) {
	switch {
	case len(data) > 0:
		if mediaType == "" {
			return "", llm.NewError(llm.ErrBadRequest, provider, 0, nil, "内联媒体缺少 MediaType")
		}
		return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
	case rawURL != "":
		return rawURL, nil
	default:
		return "", llm.NewError(llm.ErrBadRequest, provider, 0, nil, "媒体块既无 Data 也无 URL")
	}
}

func imageRef(provider string, src *llm.ImageSource) (string, error) {
	if src == nil {
		return "", llm.NewError(llm.ErrBadRequest, provider, 0, nil, "图像块缺少 ImageSource")
	}
	return mediaRef(provider, src.Data, src.MediaType, src.URL)
}

func audioFormat(mediaType string) string {
	mt := strings.ToLower(mediaType)
	if strings.Contains(mt, "mpeg") || strings.HasSuffix(mt, "/mp3") {
		return "mp3"
	}
	return "wav"
}

func mediaFilename(media *llm.MediaContent, fallback string) string {
	if media != nil && media.Metadata != nil {
		if s, ok := media.Metadata["filename"].(string); ok && s != "" {
			return s
		}
	}
	return fallback
}

// overridePart 用原始 JSON 发出 SDK 联合类型没有的内容块（video_url 等）。
func overrideCompletionsPart(raw map[string]any) (sdk.ChatCompletionContentPartUnionParam, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return sdk.ChatCompletionContentPartUnionParam{}, err
	}
	return param.Override[sdk.ChatCompletionContentPartUnionParam](json.RawMessage(b)), nil
}

func overrideResponsesPart(raw map[string]any) (responses.ResponseInputContentUnionParam, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return responses.ResponseInputContentUnionParam{}, err
	}
	return param.Override[responses.ResponseInputContentUnionParam](json.RawMessage(b)), nil
}

// sendEvent 把事件写入 channel；ctx 取消时先发 EventError 再返回 false，
// 保证消费方 range 看到的最后一条是 Error 而不是静默关闭。
func sendEvent(ctx context.Context, ch chan<- llm.StreamEvent, provider string, ev llm.StreamEvent) bool {
	select {
	case ch <- ev:
		return true
	case <-ctx.Done():
		select {
		case ch <- llm.StreamEvent{Kind: llm.EventError, Err: mapError(provider, ctx.Err())}:
		default:
		}
		return false
	}
}
