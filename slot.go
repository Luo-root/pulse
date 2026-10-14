package pulse

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// slotState 是槽位三态。pending 不是到达；ready 与 skipped 都是到达。
type slotState int

const (
	slotPending slotState = iota
	slotReady
	slotSkipped
)

// slot 是一次运行里的一个数据槽。
type slot struct {
	mu    sync.Mutex
	state slotState
	value any
	done  chan struct{} // 到达（值或跳过）时 close
}

func newSlot() *slot {
	return &slot{done: make(chan struct{})}
}

func (s *slot) snapshot() (slotState, any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.value
}

// isReady 报槽位是否已经就绪（有值）。只读一眼，不参与任何序列化。
func (s *slot) isReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == slotReady
}

// resolveValue 幂等首写为就绪。已就绪：忽略。已跳过：冲突。
func (s *slot) resolveValue(v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case slotReady:
		return nil
	case slotSkipped:
		return ErrConflict
	default:
		s.value = v
		s.state = slotReady
		close(s.done)
		return nil
	}
}

// resolveSkip 标记跳过。已跳过：忽略。已就绪：冲突。
func (s *slot) resolveSkip() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case slotSkipped:
		return nil
	case slotReady:
		return ErrConflict
	default:
		s.state = slotSkipped
		close(s.done)
		return nil
	}
}

// wait 阻塞到到达。值为 (v, nil)；跳过为 (zero, ErrSkipped)。
//
// **取消优先**：ctx 已取消时一律返回 ctx.Err()，「到达」与「取消」同时就绪
// 时也以取消为准。否则首错取消图之后，下游的终态会在 skipped（先看到跳过）
// 与 canceled（先看到取消）之间随调度抖——而两者的成因都是那次取消。
//
// 这条判据要在**每个**「准备报到达」的出口都复查一遍，不能只在 select 的
// done 分支查：入口那次 `ctx.Err()` 与 `snapshot()` 之间也有窗口——槽位在
// 窗口里到达、ctx 在窗口里被取消，漏了复查就会把「被取消」报成 skipped
// （首错取消时 cancel 先于那条「解阻塞用的跳过」，所以窗口里到达的一定是
// 跳过；值那条也一样让位给取消，与 done 分支同口径）。
func (s *slot) wait(ctx context.Context) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st, v := s.snapshot()
	if st != slotPending {
		if err := ctx.Err(); err != nil { // 快照与入口检查之间被取消
			return nil, err
		}
		if st == slotSkipped {
			return nil, ErrSkipped
		}
		return v, nil
	}
	select {
	case <-s.done:
		if err := ctx.Err(); err != nil { // done 与取消同时就绪：取消优先
			return nil, err
		}
		st, v = s.snapshot()
		if st == slotSkipped {
			return nil, ErrSkipped
		}
		return v, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// 以下 writeSet 把尝试或完整节点链的写入与执行槽位隔开：Retry 每次尝试各有一层，
// 成功或正常跳过才交出去，失败整层丢弃（见 object-model 第 5 节）。
//
// [#307] 的检查点模式已共用这份实现：runCheckpointChain 在完整节点链外开暂存，
// 链内 Retry 的成功写先交给这一层；完整检查点经 Commit 确认后，引擎才发布执行槽位。
// 一份暂存的提交目标始终是**父写入目标**：有外层暂存就交给外层，否则写进执行槽位。
// 这份写入机制是**包内**实现，公开桥接面见 checkpoint.go 与 docs/design/checkpoint.md。
//
// [#307]: https://github.com/Luo-root/pulse/issues/307

// writeSet 是一次尝试的写暂存。
//
// 它跟执行槽位共用同一个 `slot` 类型——「幂等首写」「就绪与跳过互斥」「阻塞等待」这几条
// 语义因此逐条一致：写暂存换的是**落地位置**，不是语义。于是「attempt 内读回自己写的值」
// 天然带上了与槽位一致的阻塞与跳过语义，不需要第二套规则。
type writeSet struct {
	// outer 是**外层写暂存**：可能属于外层 Retry 尝试，也可能属于检查点完整节点链。
	// nil 表示提交目标是执行槽位；检查点节点帧由 runCheckpointChain 先确认存储再提交。
	outer *writeSet

	mu     sync.Mutex
	staged map[string]*stagedWrite
}

// stagedWrite 是暂存里的一条：键 + 那条槽（就绪或跳过）。
//
// 留 `ref` 是为了提交时能定位执行槽位、也让报文能点名——`slotOf` 虽然只按名字查表，
// 但报一条「提交失败」时手里得有键的全名（名字 + 类型）。
type stagedWrite struct {
	ref  keyRef
	slot *slot
}

// slotOf 取（必要时建）这条键在**本暂存**里的槽。**写路径**走它。
//
// 「落暂存就建条目」是必要的：`Set` 一条还没写过的键要能在暂存里留下痕迹，提交时
// 才知道这一次尝试到底写过谁。
func (ws *writeSet) slotOf(ref keyRef) *slot {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.staged == nil {
		ws.staged = make(map[string]*stagedWrite, 2)
	}
	if e, ok := ws.staged[ref.name]; ok {
		return e.slot
	}
	s := newSlot()
	ws.staged[ref.name] = &stagedWrite{ref: ref, slot: s}
	return s
}

// peek 只读地看一眼这条键在本暂存里写过没有。**读路径**走它——读一条本 attempt 没写过的
// 键**不建条目**：建了就会留下一个永远 `pending` 的空位，把这条键的读取整个遮住
// （它本来有别的来源，该读到那条来源的值）。
func (ws *writeSet) peek(ref keyRef) *slot {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if e, ok := ws.staged[ref.name]; ok {
		return e.slot
	}
	return nil
}

// commit 把这份暂存交给**父写入目标**：外层还有一层暂存就交给外层（嵌套 `Retry` 时
// 内层的结果属于外层那一次尝试），否则逐条写进执行槽位。
//
// **先整批判过，再逐条写**：判据是「对面有没有已经以另一个状态到达」——值写进已跳过的槽、
// 跳过写进已就绪的槽都是 `ErrConflict`（同状态是幂等首写，放行）。判不过就**一条都不发布**，
// 整批以失败告终；这样调用方看不到「一半发布了、一半没发」这种没有可解释语义的中间态
// （逐条写中途才发现冲突时，前面那几条已经落地、下游可能已经被唤醒——那是评审点名的一种
// 失败形状，而不是契约允许的窗口）。
//
// 判过之后的逐条写**不承诺严格原子可见**（阶段零定案 (d)）：窗口是「一部分输出已 `ready`、
// 其余仍 `pending`」，这些输出的下游可能被先到的那一条先唤醒。硬保证在别处，而且是清楚的：
// **失败的尝试根本走不到这一步**——它的整份暂存被丢掉，下游从未见过。
//
// 逐条写的那一支仍然带错误返回：当前设计里它**没有可达形状**（见下面那条不变量），留着是防御性的。
//
// **它依赖一条不变量：一条输出只有一个顺序写入者。** 引擎里能写某个执行槽位的路径只有三类，
// 而且都在同一个 goroutine 上顺序执行：
//
//  1. 建执行时种输入（`prepareExecution`，发生在 `Start` 之前；「图自己产出的键」也不许当输入
//     喂进来，静态就拒了）；
//  2. 这个节点自己的**尝试链**——体、切面、子图桥回都在这一条栈上（`TestNodeWritesFinishBeforeTheChainReturns`
//     钉着「链返回时这个节点已经没有在飞的写」：成功那次的提交已经落地，超时也不在体退出前返回）；
//  3. 它的**收尾路径**（`settleUnwritten`，同样是体的那一条栈）。
//
// 别的节点写不了这条键：一条键只有一个来源（`Add` 第二次提供撞 `ErrDuplicateSource`）。于是
// 「整批判过」与「逐条写」之间没有第三方能插进来——这正是那句「判不过就一条都不发布」成立的理由。
//
// **不在契约内的用法**：从自己起的 goroutine 里**并发** `Set` 同一个 `Provides`。单条 `Set` 自身
// 是安全的（槽位带自己的锁），但「整批」这条保证以单写者为前提——引擎没有为多写者准备提交协议。
func (ws *writeSet) commit(e *Execution) error {
	ws.mu.Lock()
	names := make([]string, 0, len(ws.staged))
	for name := range ws.staged {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]stagedWrite, 0, len(names))
	for _, name := range names {
		entries = append(entries, *ws.staged[name])
	}
	ws.mu.Unlock()

	// —— 第一趟：全判。任何一条落不下去就整批不发布 ——
	for _, en := range entries {
		st, _ := en.slot.snapshot()
		if st == slotPending {
			// 条目只在写路径上建，理论上到不了这里；真到了也不该把 nil 当值提交出去。
			continue
		}
		if err := checkCommitTarget(e, ws, en.ref, st); err != nil {
			return err
		}
	}

	// —— 第二趟：逐条写（顺序见上面那段 godoc） ——
	for _, en := range entries {
		st, v := en.slot.snapshot()
		if st == slotPending {
			continue
		}
		var err error
		switch {
		case ws.outer != nil && st == slotSkipped:
			err = ws.outer.slotOf(en.ref).resolveSkip()
		case ws.outer != nil:
			err = ws.outer.slotOf(en.ref).resolveValue(v)
		case st == slotSkipped:
			err = e.slotOf(en.ref).resolveSkip()
		default:
			err = e.slotOf(en.ref).resolveValue(v)
		}
		if err != nil {
			return fmt.Errorf("pulse: committing attempt output %s failed: %w", en.ref, err)
		}
	}
	return nil
}

// checkCommitTarget 判这一条交出去会不会撞上「已经以另一个状态到达」。
//
// 它**不改任何状态**：外层暂存那条走 `peek`，**绝不建条目**——给外层 `slotOf` 会在那里留下一个
// 永远 `pending` 的空位，而那个空位随后会把这条键真正的来源整个遮住（一次没能提交的尝试不该在
// 外层留下任何痕迹）；执行槽位那条读的是这条键**本来就有**的槽（`prepareExecution` 已经为全图
// 的 requires / provides 建过表，而能走到这里的键必然是这个节点的 Provide）。
func checkCommitTarget(e *Execution, ws *writeSet, ref keyRef, st slotState) error {
	var cur slotState
	if ws.outer != nil {
		s := ws.outer.peek(ref)
		if s == nil {
			return nil // 外层没写过这条：这一次就是首写
		}
		cur, _ = s.snapshot()
	} else {
		cur, _ = e.slotOf(ref).snapshot()
	}
	switch {
	case cur == slotPending, cur == st:
		return nil // 没到过 → 首写；同状态 → 幂等首写
	default:
		return fmt.Errorf("pulse: committing attempt output %s failed: %w", ref, ErrConflict)
	}
}
