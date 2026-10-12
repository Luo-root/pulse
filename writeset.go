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
// 逐条写、按 **key 名排序**（map 的迭代顺序不许进任何可观察的行为）：多 Key 的成功提交
// **不承诺**严格原子可见（阶段零定案 (d)），窗口是「一部分输出已 `ready`、其余仍 `pending`」，
// 这些输出的下游可能被先到的那一条先唤醒。硬保证在别处，而且是清楚的：**失败的尝试根本
// 走不到这一步**——它的整份暂存被丢掉，下游从未见过。
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

	for _, en := range entries {
		st, v := en.slot.snapshot()
		if st == slotPending {
			// 条目只在写路径上建，理论上到不了这里；真到了也不该把 nil 当值提交出去。
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
