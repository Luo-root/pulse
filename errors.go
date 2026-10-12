package pulse

import (
	"errors"
	"fmt"
	"strings"
)

// ErrSkipped 表示等待的槽位以「跳过」到达。它不是工作流失败：
// Graph.Run / Graph.Err 不会把单纯的跳过当成 error 返回。
var ErrSkipped = errors.New("pulse: skipped")

// SkipError 携带被跳过的 Key 名：读一条没值的输入（Get）或要求「全到齐」
// （WaitAll）时，调用方据此知道是哪几路输入没值。errors.Is(err, ErrSkipped)
// 仍成立。
//
// 它同时是**显式的 fan-in 策略**载体：节点把 WaitAll 的返回值直接返回，
// 就是声明「缺一条就别跑我」，引擎按「本节点以跳过收尾」处理——尚未发布的
// 输出被跳过、已经发布的槽位不回滚，且不是失败。
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
	// ErrGraphStarted：**两个触发点**，两个都是「你不能再改这张图了」但原因不同——
	//
	//	① 这张图已经启动过（`Start` 成功过一次）：再 `Start` / `Add` / `Seed` 都被拒；
	//	② 这张**定义**已经封口（`Validate` 通过过一次，连同它挂着的子图整棵一起封，
	//	   见 Graph.sealed）：`Add` / `DeclareInput` / `DeclareOutput` / 挂子图被拒，
	//	   但 `Start` / `Seed` **照旧可以**——`Seed` 是这一轮运行的值，不属于定义。
	//
	// 两者共用一个哨兵（冻结面里七条哨兵不新增、不改名），所以 `errors.Is` 分不出是哪一种；
	// 要区分就读报文（封口那句写着 sealed），别把「定义冻了」当成「已经跑过」。
	// 一次性契约见 Graph。
	ErrGraphStarted = errors.New("pulse: graph already started")
	// ErrGraphNotStarted：在 Start 之前调用 Wait / Err（Err 例外：它先看
	// 首错与 ctx，再报这个）。
	ErrGraphNotStarted = errors.New("pulse: graph has not started")
	// ErrDuplicateSource：这个 Key 在**定义里**已经有来源了（某个节点 Provides 它，
	// 或挂载子图时绑定的那条键在子图里已经有来源）。一个 Key 至多一个来源。
	ErrDuplicateSource = errors.New("pulse: key already has a source")
	// ErrNextCalledTwice：单节点的 Run 不得并发进入——两个 goroutine 同时
	// 跑同一节点会抢同一批槽位，语义上必然错。顺序重入是合法的，Retry
	// 正依赖它（1→0→1），所以判据是「重叠」而不是「多次」。
	ErrNextCalledTwice = errors.New("pulse: aspect next called concurrently/overlapping")
)
