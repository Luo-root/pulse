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

// Error 实现 error：有 Key 名时列出它们，否则退化成 ErrSkipped 的文案。
func (e *SkipError) Error() string {
	if len(e.Keys) == 0 {
		return ErrSkipped.Error()
	}
	return fmt.Sprintf("pulse: skipped [%s]", strings.Join(e.Keys, ", "))
}

// Unwrap 让 errors.Is(err, ErrSkipped) 对 *SkipError 成立——判据只有一条，
// 调用方不需要认识本类型。
func (e *SkipError) Unwrap() error { return ErrSkipped }

func skipErr(names ...string) error {
	cp := append([]string(nil), names...)
	return &SkipError{Keys: cp}
}

// 声明期 / 契约错误，调用方写错 Key 或冲突写入时返回。
var (
	// ErrUndeclared：读写了本节点没声明过的 Key（读的允许集是 Requires ∪
	// 自己的 Provides，写的允许集只有自己的 Provides）。
	ErrUndeclared = errors.New("pulse: key not declared on this node")
	// ErrConflict：同一个槽位先 Set 后 Skip（或反过来）——槽位一旦以某种
	// 状态到达就不再改写，冲突一律报错而不是静默覆盖。
	ErrConflict = errors.New("pulse: slot already resolved with a conflicting state")
	// ErrGraphStarted：图已经启动过；启动后的 Add / Seed / Start 都被拒。
	// 一次性契约见 Graph。
	ErrGraphStarted = errors.New("pulse: graph already started")
	// ErrGraphNotStarted：在 Start 之前调用 Wait / Err（Err 例外：它先看
	// 首错与 ctx，再报这个）。
	ErrGraphNotStarted = errors.New("pulse: graph has not started")
	// ErrDuplicateSource：这个 Key 已经有来源了（外部 Seed/SkipSeed，或
	// 另一个节点的 Provides）。每个 Key 至多一种来源身份。
	ErrDuplicateSource = errors.New("pulse: key already has a source")
	// ErrNextCalledTwice：单节点的 Run 不得并发进入——两个 goroutine 同时
	// 跑同一节点会抢同一批槽位，语义上必然错。顺序重入是合法的，Retry
	// 正依赖它（1→0→1），所以判据是「重叠」而不是「多次」。
	ErrNextCalledTwice = errors.New("pulse: aspect next called concurrently/overlapping")
)
