package flow

import (
	"errors"
	"fmt"
	"strings"
)

// ErrSkipped 表示等待的槽位以「跳过」到达。它不是工作流失败：
// Graph.Run / Graph.Err 不会把单纯的跳过当成 error 返回。
var ErrSkipped = errors.New("flow: skipped")

// SkipError 携带被跳过的 Key 名，便于 WaitAll 的调用方区分哪些输入
// 走了跳过。errors.Is(err, ErrSkipped) 仍成立。
type SkipError struct {
	Keys []string
}

func (e *SkipError) Error() string {
	if len(e.Keys) == 0 {
		return ErrSkipped.Error()
	}
	return fmt.Sprintf("flow: skipped [%s]", strings.Join(e.Keys, ", "))
}

func (e *SkipError) Unwrap() error { return ErrSkipped }

func skipErr(names ...string) error {
	cp := append([]string(nil), names...)
	return &SkipError{Keys: cp}
}

// 声明期 / 契约错误，调用方写错 Key 或冲突写入时返回。
var (
	ErrUndeclared      = errors.New("flow: key not declared on this node")
	ErrConflict        = errors.New("flow: slot already resolved with a conflicting state")
	ErrGraphStarted    = errors.New("flow: graph already started")
	ErrGraphNotStarted = errors.New("flow: graph has not started")
	ErrDuplicateSource = errors.New("flow: key already has a source")
	ErrNextCalledTwice = errors.New("flow: aspect next called more than once")
)
