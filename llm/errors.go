package llm

import (
	"errors"
	"fmt"
)

// ErrKind 是 provider 无关的错误分类，供上层做退避与 failover
// 决策——分类是接口契约的一部分，不是日志细节。
type ErrKind string

const (
	ErrAuth           ErrKind = "auth"            // 凭据缺失/无效，不可重试
	ErrRateLimit      ErrKind = "rate_limit"      // 限流，可重试（宜退避）
	ErrContextLength  ErrKind = "context_length"  // 上下文超长，重试前须压缩
	ErrContentFilter  ErrKind = "content_filter"  // 安全策略拦截，通常不可重试
	ErrBadRequest     ErrKind = "bad_request"     // 参数错误，不可重试
	ErrNetwork        ErrKind = "network"         // 网络层失败，可重试
	ErrProvider       ErrKind = "provider"        // 上游 5xx，可重试
	ErrNoModel        ErrKind = "no_model"        // 注册中心无此实例/提供方
	ErrCanceled       ErrKind = "canceled"        // 调用方取消
	ErrUnknown        ErrKind = "unknown"
)

// Error 是本包所有对外错误的统一形态。
type Error struct {
	Kind       ErrKind
	Provider   string // 出错来源的 provider 名；注册中心自身错误为 ""
	Retryable  bool
	StatusCode int    // HTTP 状态码；非 HTTP 错误为 0
	Detail     string
	Err        error  // 底层错误，可为 nil
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("llm: %s (%s%s): %v", e.Kind, e.providerPart(), e.Detail, e.Err)
	}
	return fmt.Sprintf("llm: %s (%s%s)", e.Kind, e.providerPart(), e.Detail)
}

func (e *Error) Unwrap() error { return e.Err }

func (e *Error) providerPart() string {
	if e.Provider == "" {
		return ""
	}
	return e.Provider + ", "
}

// NewError 构造分类错误。
func NewError(kind ErrKind, provider string, statusCode int, err error, detailFormat string, args ...any) *Error {
	return &Error{
		Kind:       kind,
		Provider:   provider,
		Retryable:  defaultRetryable(kind),
		StatusCode: statusCode,
		Detail:     fmt.Sprintf(detailFormat, args...),
		Err:        err,
	}
}

func defaultRetryable(kind ErrKind) bool {
	switch kind {
	case ErrRateLimit, ErrNetwork, ErrProvider:
		return true
	default:
		return false
	}
}

// KindOf 从错误链中提取分类；非本包错误返回 ErrUnknown。
func KindOf(err error) ErrKind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ErrUnknown
}

// IsRetryable 报告错误链是否标记为可重试。
// 非本包错误一律不可重试（保守默认）。
func IsRetryable(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Retryable
	}
	return false
}

// 预置构造器：覆盖 adapter 最常见的几类场景。

func errAuth(provider string, status int, err error) error {
	return NewError(ErrAuth, provider, status, err, "authentication failed")
}

func errRateLimit(provider string, status int, err error) error {
	return NewError(ErrRateLimit, provider, status, err, "rate limited")
}

func errContextLength(provider string, status int, err error) error {
	return NewError(ErrContextLength, provider, status, err, "context length exceeded")
}
