package pulse

import "fmt"

// ReadOutput 读取这一次执行的声明出口。k 必须与本图 DeclareOutput 声明的键同名同类型；
// Key 不携带图来源，同名同类型的键等价。零 Key、内部键、未声明出口和错误类型均返回
// 包装 ErrUndeclared 的错误。nil 或零值 Execution 返回普通契约错误。
//
// 结果以成功 Wait 的收尾为边界：尚未收尾时立即返回普通生命周期错误，不等待，
// 即使节点已经 Set 或全部节点已结束也不返回部分值。OnGraphFinished 回调中可以读取。
// 已捕获的失败或真实 ctx 取消优先返回原始原因，保留 errors.Is；正常 Wait 收尾释放 ctx
// 所产生的取消不算错误。任何错误返回的 value 都是零值，state 都是空串，调用方应先判 err。
//
// 成功收尾后，ready 返回业务值，skipped 返回零值且不报错。无人 Requires、未喂值的
// 可选透传出口保持 pending，返回零值且不报错。ready 的 0、空串、nil 接口和 typed nil
// 都保留 ready，不会被当作缺值。
//
// 读取不创建槽位、不改变状态、不发 Observer 事件，也不把业务值写进快照或错误。
// slice、map、指针等返回原业务值，不深拷贝；宿主对值的并发修改，以及节点自己启动的
// 后台写入，不在本函数的并发保证内。
func ReadOutput[T any](e *Execution, k Key[T]) (value T, state SlotState, err error) {
	if e == nil || e.graph == nil || e.ctx == nil {
		return value, "", fmt.Errorf("pulse: ReadOutput: nil or zero Execution: create it with NewExecution")
	}

	ref := k.asRef()
	g := e.graph
	g.mu.Lock()
	port, declared := g.outPorts[ref.name]
	g.mu.Unlock()
	if ref.name == "" || !declared || port.typ != ref.typ {
		return value, "", fmt.Errorf("pulse: ReadOutput: %s is not a declared output of graph %q (%w)",
			ref, g.id, ErrUndeclared)
	}

	// 分段读锁：终态错误优先于槽位状态，done 后只认 Wait 锁存的结果。
	e.mu.Lock()
	done, cause := e.done, e.err
	if cause == nil && !done {
		cause = e.ctx.Err()
	}
	e.mu.Unlock()
	if cause != nil {
		return value, "", cause
	}
	if !done {
		return value, "", fmt.Errorf("pulse: ReadOutput: execution %q has no final result: call Start and Wait before reading outputs", e.ID())
	}

	// 只查已有槽位：晚声明的可选透传出口可能没有槽，仍然是真实的 pending。
	e.slotsMu.Lock()
	s := e.slots[ref.name]
	e.slotsMu.Unlock()
	if s == nil {
		return value, SlotStatePending, nil
	}
	st, raw := s.snapshot()
	if st != slotReady {
		return value, publicSlotState(st), nil
	}
	// nil 接口没有动态类型，不能用 raw.(T) 判为类型错误；端口类型已在上面核对。
	if raw == nil {
		return value, SlotStateReady, nil
	}
	out, ok := raw.(T)
	if !ok {
		return value, "", fmt.Errorf("pulse: ReadOutput: key %q type assertion failed", ref.name)
	}
	return out, SlotStateReady, nil
}
