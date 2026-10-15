package pulse

import (
	"context"
	"errors"
	"fmt"
)

// ErrDeliveryExpired 表示交付已被 Nack 或 Stop 作废，不能再推进确认前沿。
var ErrDeliveryExpired = errors.New("pulse: stream delivery expired")

// Delivery 是一条消息的当前交付。Sequence 从 1 起，Nack 重投保留原序号。
// 复制 Delivery 共享同一确认凭据；修改 Value / Sequence 不会改写引擎的消息身份。
type Delivery[T any] struct {
	Value    T
	Sequence uint64
	ack      *managedAck[T]
}

type managedDeliveryState uint8

const (
	deliveryActive managedDeliveryState = iota
	deliveryAcknowledged
	deliveryExpired
)

type managedMessage[T any] struct {
	value    T
	sequence uint64
	current  *managedAck[T]
}

type managedAck[T any] struct {
	stream  *managedStream[T]
	message *managedMessage[T]
	state   managedDeliveryState // 受 stream.mu 保护
	owner   managedReaderID
	rc      *RunCtx // 读取者的额度账；宿主等待上下文不控制后续显式确认。
}

// enqueueLocked 总在途不超过通道容量；Nack 重投沿用原在途义务。
// 所以这次发送一定有位置，不等待，也不会新增一份消息保留。
func (s *managedStream[T]) enqueueLocked(message *managedMessage[T]) {
	ack := &managedAck[T]{stream: s, message: message, owner: s.nativeOwner, rc: s.nativeCtx}
	message.current = ack
	s.queue <- Delivery[T]{Value: message.value, Sequence: message.sequence, ack: ack}
}

// Ack 确认本次有效交付并释放一个在途位置；重复 Ack 幂等成功。
// 已 Nack / Stop 的旧交付返回 ErrDeliveryExpired，不影响后来那次交付。
// 可靠模式先同步确认，再推进本地前沿；钩子不在流状态锁中运行。
func (d Delivery[T]) Ack() error {
	if d.ack == nil {
		return fmt.Errorf("pulse: Ack: zero stream delivery")
	}
	ack, s := d.ack, d.ack.stream
	s.mu.Lock()
	if ack.state == deliveryAcknowledged {
		s.mu.Unlock()
		return nil
	}
	active := ack.activeLocked()
	rc := ack.operationContextLocked()
	s.mu.Unlock()
	if !active {
		return ErrDeliveryExpired
	}
	if err := s.operation.lock(rc); err != nil {
		return err
	}
	defer s.operation.unlock()
	s.mu.Lock()
	if ack.state == deliveryAcknowledged {
		s.mu.Unlock()
		return nil
	}
	active = ack.activeLocked()
	s.mu.Unlock()
	if !active {
		return ErrDeliveryExpired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ack.confirmMessageLocked()
	return nil
}

// Nack 作废本次交付，立即把同一条消息重新排入同一读取组。
// 它不释放容量、不推进确认前沿，也不等待一个已经由自己占据的容量位置。
func (d Delivery[T]) Nack() error {
	if d.ack == nil {
		return fmt.Errorf("pulse: Nack: zero stream delivery")
	}
	ack, s := d.ack, d.ack.stream
	s.mu.Lock()
	rc := ack.operationContextLocked()
	s.mu.Unlock()
	if err := s.operation.lock(rc); err != nil {
		return err
	}
	defer s.operation.unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ack.activeLocked() {
		return ErrDeliveryExpired
	}
	ack.state = deliveryExpired
	s.enqueueLocked(ack.message)
	s.signalLocked()
	return nil
}

func (a *managedAck[T]) activeLocked() bool {
	return a.state == deliveryActive && a.message.current == a &&
		a.stream.pending[a.message.sequence] == a.message
}

func (a *managedAck[T]) operationContextLocked() *RunCtx {
	if a.rc == nil {
		return nil
	}
	copy := *a.rc
	// 显式确认与取消不自动确认是两件事；本地交付仍可由业务明确处置。
	copy.ctx = context.WithoutCancel(copy.ctx)
	return &copy
}

func (d Delivery[T]) claim(owner managedReaderID, contexts ...*RunCtx) bool {
	if d.ack == nil {
		return false
	}
	s := d.ack.stream
	s.mu.Lock()
	defer s.mu.Unlock()
	if !d.ack.activeLocked() {
		return false
	}
	d.ack.owner = owner
	if len(contexts) != 0 {
		d.ack.rc = contexts[0]
	}
	return true
}

func (d Delivery[T]) active() bool {
	if d.ack == nil {
		return false
	}
	s := d.ack.stream
	s.mu.Lock()
	defer s.mu.Unlock()
	return d.ack.activeLocked()
}

// 与 Delivery.Ack 相同的连续前沿规则。
func (a *managedAck[T]) confirmMessageLocked() {
	s := a.stream
	a.state = deliveryAcknowledged
	delete(s.pending, a.message.sequence)
	var zero T
	a.message.value = zero
	s.frontier = s.last
	for sequence := range s.pending {
		if sequence <= s.frontier {
			s.frontier = sequence - 1
		}
	}
	if s.finished && len(s.pending) == 0 {
		s.closeQueueLocked()
	}
	s.signalLocked()
}
