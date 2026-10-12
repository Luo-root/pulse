package pulse

import (
	"fmt"
	"sort"
	"sync"
)

// 本文件是**一次尝试的写暂存**：`Retry` 用它把「这一试写了什么」跟执行槽位隔开——
// 成功才交出去、失败整份丢掉（见 object-model 第 5 节）。
//
// 它同时是给 [#307]（Durable 的检查点提交）留的那条缝：一份暂存的提交目标是**父写入目标**
// ——外层还有一层暂存就交给外层，没有就写进执行槽位。Durable 的节点暂存区将来作为
// 「这个节点的写入目标」挂在链上，最外层什么时候发布仍然由引擎决定。这条缝是**包内**的，
// 不公开成扩展点（见 Sub / Retry 的 godoc 与 docs/design/object-model.md）。
//
// [#307]: https://github.com/Luo-root/pulse/issues/307

// writeSet 是一次尝试的写暂存。
//
// 它跟执行槽位共用同一个 `slot` 类型——「幂等首写」「就绪与跳过互斥」「阻塞等待」这几条
// 语义因此逐条一致：写暂存换的是**落地位置**，不是语义。于是「attempt 内读回自己写的值」
// 天然带上了与槽位一致的阻塞与跳过语义，不需要第二套规则。
type writeSet struct {
	// outer 是**外层尝试**的暂存：嵌套 `Retry` 时内层的结果属于外层那一次尝试。
	// nil 表示这份暂存的提交目标就是执行槽位（最外层）。
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
// 逐条写的那一支仍然带错误返回：当前设计里它**没有可达形状**（这些键的写入者只有这个节点
// 自己的尝试链，而尝试链是顺序的，判过与写之间没有第三方能插进来），留着是防御性的。
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
