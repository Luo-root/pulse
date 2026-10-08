package pulse

import (
	"context"
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
func (s *slot) wait(ctx context.Context) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st, v := s.snapshot()
	if st == slotReady {
		return v, nil
	}
	if st == slotSkipped {
		return nil, ErrSkipped
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
