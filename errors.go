package pulse

import (
	"errors"
	"fmt"
	"strings"
)

// ErrSkipped 表示等待的槽位以「跳过」到达。它不是工作流失败：
// Graph.Run / Execution.Err 不会把单纯的跳过当成 error 返回。
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
	// 自己的 Provides，写的允许集只有自己的 Provides）；宿主 ReadOutput 读取了
	// 本执行所属图没有声明为出口的同名同类型 Key 时也返回它。
	ErrUndeclared = errors.New("pulse: key not declared on this node")
	// ErrConflict：同一个槽位先 Set 后 Skip（或反过来）——槽位一旦以某种
	// 状态到达就不再改写，冲突一律报错而不是静默覆盖。
	ErrConflict = errors.New("pulse: slot already resolved with a conflicting state")
	// ErrGraphStarted 有两个触发点：
	//
	//	① 同一份 Execution 已成功 Start，再次 Start 被拒；
	//	② Graph 定义已封口（Validate 或某份 Execution.Start 成功，包含整棵子树），
	//	   Add / DeclareInput / DeclareOutput / Sub 修改该定义被拒。
	//
	// 封口不消耗定义：仍可创建新 Execution 运行；Seed 只写本轮 Inputs，不受定义封口限制。
	// 两种触发共用哨兵，具体原因见报文；一次性的是 Execution。
	ErrGraphStarted = errors.New("pulse: graph already started")
	// ErrGraphNotStarted：尚未成功 Start 的 Execution 调用 Wait。
	// Err 不要求先启动；ReadOutput 的未收尾错误也不使用本哨兵。
	ErrGraphNotStarted = errors.New("pulse: graph has not started")
	// ErrDuplicateSource：这个 Key 在**定义里**已经有来源了（某个节点 Provides 它，
	// 或挂载子图时绑定的那条键在子图里已经有来源）。一个 Key 至多一个来源。
	ErrDuplicateSource = errors.New("pulse: key already has a source")
	// ErrNextCalledTwice：**两个触发点**，都是「这一层不该再往下推进了」——
	//
	//	① **并发进入**：两个 goroutine 同时跑同一节点的体（`next` 重叠）。它们会抢同一批
	//	   槽位，语义上必然错——切面里的 `next` 不许并发调；
	//	② **无边界重入**：同一个节点执行上下文里推进过第二次，而这两次之间**没有新的尝试
	//	   边界**（`Retry` 每试都开一条新的写暂存，见 RunCtx.beginAdvance）。没有隔离的
	//	   第二次推进，第一次写出去的东西会被幂等首写静默吞掉，节点手里就成了两次的混装。
	//
	// 两者共用一个哨兵（冻结面里七条哨兵不新增、不改名），所以 `errors.Is` 分不出是哪一种；
	// 要区分就读报文。**限制的是「这一次执行里这个节点」的推进次数**，不是按节点 id 全局
	// 禁止：另一次 `Execution` / 另一次 `Run` 是全新的执行上下文，照常跑。
	ErrNextCalledTwice = errors.New("pulse: aspect next called concurrently/overlapping")
)
