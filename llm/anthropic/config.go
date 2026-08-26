package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// ProviderAnthropic 是 Registry.RegisterProvider 的登记键。
const ProviderAnthropic = "anthropic"

// Register 把 Messages 适配器登记到 reg。RegisterProvider 是可逆效应：
// adapter 插件在自己的 Apply 中调用并丢弃返回值即可，插件卸载时工厂
// 与其打开的实例随作用域自动收回。
func Register(scope *kernel.Context, reg *llm.Registry) error {
	_, err := reg.RegisterProvider(scope, ProviderAnthropic, New)
	return err
}

// adapterOptions 是 Config.Options 中本包认识的键。未知键忽略。
type adapterOptions struct {
	// TimeoutSeconds > 0 时为整个 HTTP 请求设客户端超时；默认 0 =
	// 不设超时，生命周期由 ctx 管控。
	TimeoutSeconds float64 `json:"timeout_seconds"`
	// MaxRetries 覆盖 SDK 传输层自动重试次数。默认显式置 0（关闭）：
	// 重试与 failover 属上层编排职责，adapter 不做静默重试。
	MaxRetries *int `json:"max_retries"`
	// Headers 附加到每个请求的自定义头（网关鉴权、追踪透传等）。
	Headers map[string]string `json:"headers"`
}

func decodeOptions(cfg llm.Config) (adapterOptions, error) {
	var opts adapterOptions
	if len(cfg.Options) == 0 {
		return opts, nil
	}
	raw, err := json.Marshal(cfg.Options)
	if err != nil {
		return opts, llm.NewError(llm.ErrBadRequest, cfg.Provider, 0, err, "options 序列化失败")
	}
	if err := json.Unmarshal(raw, &opts); err != nil {
		return opts, llm.NewError(llm.ErrBadRequest, cfg.Provider, 0, err, "options 类型不匹配: %v", err)
	}
	return opts, nil
}

// newClient 构造 SDK 客户端：认证、端点、重试策略、附加头。
// APIKey 必填——本包不读环境变量，配置显式优先。
func newClient(cfg llm.Config) (sdk.Client, error) {
	opts, err := decodeOptions(cfg)
	if err != nil {
		return sdk.Client{}, err
	}
	if cfg.APIKey == "" {
		return sdk.Client{}, llm.NewError(llm.ErrAuth, cfg.Provider, 0, nil, "api_key 为空")
	}
	retries := 0 // 默认关闭 SDK 自动重试，见 adapterOptions.MaxRetries
	if opts.MaxRetries != nil {
		retries = *opts.MaxRetries
	}
	o := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		option.WithMaxRetries(retries),
	}
	if cfg.BaseURL != "" {
		o = append(o, option.WithBaseURL(cfg.BaseURL))
	}
	if opts.TimeoutSeconds > 0 {
		o = append(o, option.WithHTTPClient(&http.Client{
			Timeout: time.Duration(opts.TimeoutSeconds * float64(time.Second)),
		}))
	}
	for k, v := range opts.Headers {
		o = append(o, option.WithHeader(k, v))
	}
	return sdk.NewClient(o...), nil
}

// mapError 把 SDK / 传输层错误统一翻译为 *llm.Error。
func mapError(provider string, err error) error {
	if err == nil {
		return nil
	}
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		kind := classifyStatus(apiErr.StatusCode, apiErr.Error())
		return llm.NewError(kind, provider, apiErr.StatusCode, err, "%s", apiErr.Error())
	}
	if errors.Is(err, context.Canceled) {
		return llm.NewError(llm.ErrCanceled, provider, 0, err, "调用方取消")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return llm.NewError(llm.ErrNetwork, provider, 0, err, "请求超时")
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return llm.NewError(llm.ErrNetwork, provider, 0, err, "传输层失败")
	}
	return llm.NewError(llm.ErrUnknown, provider, 0, err, "%v", err)
}

// classifyStatus 按 HTTP 状态码与错误文案分类。上下文超长在 Anthropic
// 侧以 400 + "prompt is too long" 形态出现，靠文案二次识别。
func classifyStatus(status int, msg string) llm.ErrKind {
	text := strings.ToLower(msg)
	switch {
	case status == 401 || status == 403:
		return llm.ErrAuth
	case status == 429 || strings.Contains(text, "rate limit"):
		return llm.ErrRateLimit
	case status == 404:
		return llm.ErrNoModel
	case status == 408:
		return llm.ErrNetwork
	case strings.Contains(text, "prompt is too long") ||
		strings.Contains(text, "context length") ||
		strings.Contains(text, "too many tokens"):
		return llm.ErrContextLength
	case strings.Contains(text, "content filter") || strings.Contains(text, "violat"):
		return llm.ErrContentFilter
	case status == 400 || status == 405 || status == 422:
		return llm.ErrBadRequest
	case status == 529 || strings.Contains(text, "overloaded"):
		return llm.ErrProvider
	case status >= 500:
		return llm.ErrProvider
	default:
		return llm.ErrUnknown
	}
}

// unsupportedPart 构造"词汇表块不被该线协议支持"的统一错误——
// 显式失败，不静默丢弃内容。
func unsupportedPart(provider string, role llm.Role, kind llm.PartKind) *llm.Error {
	return llm.NewError(llm.ErrBadRequest, provider, 0, nil,
		"角色 %s 的内容块 %s 不被 %s 线协议支持", role, kind, provider)
}
