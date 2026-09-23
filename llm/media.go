package llm

import "strings"

// MediaFamily 是 MIME 家族分类结果：adapter 用它把开放模态块
// （[PartCustom] + [MediaContent]）路由到 provider 的线格式分支。
//
// 家族判定是**词汇表层**的规则而非 provider 细节——同一份前缀定义
// 若两个 adapter 各写一遍，新增家族或调整 application/pdf 这类判定
// 时改漏一侧不会红。provider 支持与否仍归 adapter：不认的家族显式
// 报 [ErrBadRequest]，不静默丢弃。
type MediaFamily string

const (
	MediaImage MediaFamily = "image"
	MediaVideo MediaFamily = "video"
	MediaAudio MediaFamily = "audio"
	MediaPDF   MediaFamily = "pdf"
)

// ClassifyMIME 把 MIME 类型归入家族（大小写与首尾空白不敏感）；
// 不认识的返回空串——调用方据此显式报 ErrBadRequest。
func ClassifyMIME(mediaType string) MediaFamily {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	switch {
	case strings.HasPrefix(mt, "image/"):
		return MediaImage
	case strings.HasPrefix(mt, "video/"):
		return MediaVideo
	case strings.HasPrefix(mt, "audio/"):
		return MediaAudio
	case mt == "application/pdf":
		return MediaPDF
	default:
		return ""
	}
}
