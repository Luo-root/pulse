package pulse

import (
	"errors"
	"fmt"
	"strings"
)

// ErrSkipped 表示等待的槽位以「跳过」到达。它不是工作流失败：
// Graph.Run / Graph.Err 不会把单纯的跳过当成 error 返回。
var ErrSkipped = errors.New("pulse: skipped")

// SkipError 携带被跳过的 Key 名，便于 WaitAll 的调用方区分哪些输入
// 走了跳过。errors.Is(err, ErrSkipped) 仍成立。
type SkipError struct {
	Keys []string
}

func (e *SkipError) Error() string {
	if len(e.Keys) == 0 {
		return ErrSkipped.Error()
	}
	return fmt.Sprintf("pulse: skipped [%s]", strings.Join(e.Keys, ", "))
}

func (e *SkipError) Unwrap() error { return ErrSkipped }

func skipErr(names ...string) error {
	cp := append([]string(nil), names...)
	return &SkipError{Keys: cp}
}

// 声明期 / 契约错误，调用方写错 Key 或冲突写入时返回。
var (
	ErrUndeclared      = errors.New("pulse: key not declared on this node")
	ErrConflict        = errors.New("pulse: slot already resolved with a conflicting state")
	ErrGraphStarted    = errors.New("pulse: graph already started")
	ErrGraphNotStarted = errors.New("pulse: graph has not started")
	ErrDuplicateSource = errors.New("pulse: key already has a source")
	// ErrNextCalledTwice：单节点的 Run 不得并发进入——两个 goroutine 同时
	// 跑同一节点会抢同一批槽位，语义上必然错。顺序重入是合法的，Retry
	// 正依赖它（1→0→1），所以判据是「重叠」而不是「多次」。
	ErrNextCalledTwice = errors.New("pulse: aspect next called concurrently/overlapping")
)
