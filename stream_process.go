package pulse

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"
)

// MessageOption 配置 HandleMessage 的有限原子尝试，不改变普通 Retry。
type MessageOption func(*messageConfig)

type messageConfig struct {
	attempts int
	delay    time.Duration
	limit    int
}

// WithMessageRetry 最多处理同一交付 attempts 次；非正次数按一次处理。
// delay 只在失败后的两次尝试之间等待，等待期间临时让出节点业务额度。
func WithMessageRetry(attempts int, delay time.Duration) MessageOption {
	if attempts <= 0 {
		attempts = 1
	}
	return func(c *messageConfig) {
		c.attempts, c.delay = attempts, delay
	}
}

// WithMessageLimit 限制一次消息尝试暂存的输出消息总数，默认 64；非正数不改变配置。
// 每个输出批次还不能超过其流的 WithBuffer 容量，超限的 Send 当场返回错误。
// 这里按消息条数计数，不深复制业务值，也不限制一条业务值的字节数。
func WithMessageLimit(n int) MessageOption {
	return func(c *messageConfig) {
		if n > 0 {
			c.limit = n
		}
	}
}

// HandleMessage 显式处理一次有效交付：失败只重做当前消息，成功才发布输出并 Ack。
// 默认只尝试一次；NoValue / Skip 是正常处理，零条输出也能成功确认。
// 耗尽预算或提交失败会锁存节点失败，即使外层忽略返回错误也不能把这一轮报成成功。
//
// 回调的 Set / Skip 和 StreamWriter.Send 暂存于本次尝试；失败成果对下游不可见。
// 输出写端须在回调之前由同一 RunCtx 打开。回调不得接收下一条消息、打开新流、
// 嵌套 HandleMessage 或套普通 Retry；确认由本函数完成，不在回调中手动 Ack / Nack。
// 渐进输出应在此边界外逐条 Send。检查点消息尝试尚未开放，明确拒绝。
// 闭包变量、业务值中的引用和外部副作用不回滚；不要并发使用同一 RunCtx（含 Fork）。
//
// 成功提交先等待每个批次的足够容量，再整批预检普通槽位、接纳全部输出并确认输入。
// 多流输出不承诺同一瞬间可见；取消发生在提交之前则不发布、不自动确认，已提交不改报失败。
func HandleMessage[T any](rc *RunCtx, delivery Delivery[T], fn func(*RunCtx, T) error, opts ...MessageOption) (err error) {
	defer func() {
		if err == nil || rc == nil {
			return
		}
		if rc.message != nil {
			rc.message.fail(err)
		} else if rc.streams != nil {
			rc.streams.failMessage(err)
		}
	}()
	if rc != nil && rc.exec != nil && rc.exec.checkpoint != nil {
		return fmt.Errorf("pulse: HandleMessage: persistent message attempts are not supported in this layer")
	}
	if err = checkManagedRunCtx(rc, "HandleMessage"); err != nil {
		return err
	}
	if rc.message != nil {
		return fmt.Errorf("pulse: HandleMessage: nested message attempts are not allowed")
	}
	if fn == nil {
		return fmt.Errorf("pulse: HandleMessage: nil callback")
	}
	if delivery.ack == nil {
		return fmt.Errorf("pulse: HandleMessage: zero stream delivery")
	}
	input := delivery.ack.stream
	if err = (Stream[T]{state: input}).checkReader(rc, "HandleMessage"); err != nil {
		return err
	}
	cfg := messageConfig{attempts: 1, limit: 64}
	for _, option := range opts {
		if option != nil {
			option(&cfg)
		}
	}
	for attempt := 0; attempt < cfg.attempts; attempt++ {
		if err = rc.Context().Err(); err != nil {
			return err
		}
		if !delivery.active() {
			return ErrDeliveryExpired
		}
		index := attempt + 1
		frame := &messageAttempt{
			writes: &writeSet{}, input: &input.mu, limit: cfg.limit,
			outputs: make(map[any]messageBatch),
		}
		err = runMessageAttempt(rc, frame, func() error { return fn(rc, delivery.Value) })
		if frame.err != nil {
			err = frame.err // 吞掉 Send 错误也不能确认这一条消息。
		}
		if failure := rc.exec.checkpointErr(); failure != nil {
			return failure
		}
		if frame.err == nil && (err == nil || isSkipped(err)) {
			return frame.commit(rc, delivery.ack)
		}
		if canceled := rc.exec.Context().Err(); canceled != nil {
			return canceled // 宿主中断保留持久 active 尝试，不消耗下一次预算。
		}
		if isCheckpointFailure(err) {
			return err
		}
		if index == cfg.attempts {
			return err
		}
		if canceled := rc.Context().Err(); canceled != nil {
			return canceled
		}
		if cfg.delay > 0 {
			if waitErr := rc.waitStream(func(ctx context.Context) error {
				timer := time.NewTimer(cfg.delay)
				defer timer.Stop()
				select {
				case <-timer.C:
					return ctx.Err()
				case <-ctx.Done():
					return ctx.Err()
				}
			}); waitErr != nil {
				return waitErr
			}
		}
	}
	return err
}

// messageAttempt 只由当前节点的一条调用栈使用；RunCtx.Fork 保留同一帧视图。
// writes 必须仍是当前帧，防止普通 Retry 的写暂存借本帧放行流 I/O。
type messageAttempt struct {
	writes  *writeSet
	input   *sync.Mutex
	limit   int
	count   int
	active  bool
	err     error
	outputs map[any]messageBatch
	batches []messageBatch
	// 仅显式广播使用：已 Stop 的分支不再有交付义务，其它分支继续。
	allowStoppedOutputs bool
}

func (m *messageAttempt) fail(err error) error {
	if m.err == nil {
		m.err = err
	}
	return m.err
}

func runMessageAttempt(rc *RunCtx, frame *messageAttempt, fn func() error) error {
	previousWrites, previousMessage := rc.writes, rc.message
	rc.writes, rc.message, frame.active = frame.writes, frame, true
	defer func() {
		frame.active = false
		rc.writes, rc.message = previousWrites, previousMessage
	}()
	return fn()
}

// stageMessageValue 在 Send 的普通写暂存检查之前调用。处理过的错误只归本消息帧，
// 不能写进真实流的 sendErr，否则失败后成功的重试仍会把整条流终结成失败。
func stageMessageValue[T any](rc *RunCtx, s *managedStream[T], value T) (bool, error) {
	if rc == nil || rc.message == nil {
		return false, nil
	}
	m := rc.message
	if m.err != nil {
		return true, m.err
	}
	if !m.active || rc.writes != m.writes {
		return true, m.fail(fmt.Errorf("pulse: Send: only the active message write set can stage stream output; ordinary Retry is not allowed"))
	}
	if err := rc.Context().Err(); err != nil {
		return true, m.fail(err)
	}
	if &s.mu == m.input {
		return true, m.fail(fmt.Errorf("pulse: Send: a message input cannot also be its output stream"))
	}
	if m.allowStoppedOutputs {
		s.mu.Lock()
		stopped := s.stopped && s.readerErr == nil && s.err == nil
		s.mu.Unlock()
		if stopped {
			return true, nil // 正常分支退出，不把丢弃算作消息确认。
		}
	}
	if m.count >= m.limit {
		return true, m.fail(fmt.Errorf("pulse: Send: message output limit %d exceeded", m.limit))
	}
	batch, exists := m.outputs[s]
	if !exists {
		batch = &messageOutput[T]{stream: s, ignoreStopped: m.allowStoppedOutputs}
	}
	output := batch.(*messageOutput[T])
	if len(output.values) >= s.capacity {
		return true, m.fail(fmt.Errorf("pulse: Send: message output batch exceeds stream capacity %d", s.capacity))
	}
	s.mu.Lock()
	err := output.productionErrorLocked()
	if err == nil && uint64(len(output.values)+1) > ^uint64(0)-s.last {
		err = fmt.Errorf("pulse: Send: message sequence exhausted")
	}
	s.mu.Unlock()
	if err != nil {
		return true, m.fail(err)
	}
	if !exists {
		m.outputs[s] = output
		m.batches = append(m.batches, output)
	}
	output.values = append(output.values, value)
	m.count++
	return true, nil
}

type messageBatch interface {
	mutex() *sync.Mutex
	operation() *streamOperation
	readyLocked() (<-chan struct{}, error)
	publishLocked()
}

type messageOutput[T any] struct {
	stream        *managedStream[T]
	values        []T
	ignoreStopped bool
}

func (b *messageOutput[T]) mutex() *sync.Mutex          { return &b.stream.mu }
func (b *messageOutput[T]) operation() *streamOperation { return &b.stream.operation }

func (b *messageOutput[T]) productionErrorLocked() error {
	s := b.stream
	if s.readerErr != nil {
		return s.readerErr
	}
	if s.stopped {
		if b.ignoreStopped && s.err == nil {
			return nil
		}
		return ErrStreamStopped
	}
	if s.finished {
		if s.err != nil {
			return s.err
		}
		return fmt.Errorf("pulse: Send: production already finished")
	}
	return nil
}

func (b *messageOutput[T]) readyLocked() (<-chan struct{}, error) {
	if err := b.productionErrorLocked(); err != nil {
		return nil, err
	}
	s := b.stream
	if b.ignoreStopped && s.stopped {
		return nil, nil
	}
	if len(b.values) > s.capacity {
		return nil, fmt.Errorf("pulse: Send: message output batch exceeds stream capacity %d", s.capacity)
	}
	if uint64(len(b.values)) > ^uint64(0)-s.last {
		return nil, fmt.Errorf("pulse: Send: message sequence exhausted")
	}
	if len(s.pending) > s.capacity-len(b.values) {
		return s.changed, nil
	}
	return nil, nil
}

func (b *messageOutput[T]) publishLocked() {
	s := b.stream
	if b.ignoreStopped && s.stopped {
		return
	}
	for _, value := range b.values {
		s.last++
		message := &managedMessage[T]{value: value, sequence: s.last}
		s.pending[message.sequence] = message
		s.enqueueLocked(message)
	}
}

// 不同泛型的输出统一按锁地址排序，含输入；不靠流类型/回调发送顺序决定锁序。
// 只在本地发布期间持锁，用户回调、容量等待与重试延迟都在锁外。
func (m *messageAttempt) locks() []*sync.Mutex {
	locks := make([]*sync.Mutex, 1, len(m.batches)+1)
	locks[0] = m.input
	for _, batch := range m.batches {
		locks = append(locks, batch.mutex())
	}
	sort.Slice(locks, func(i, j int) bool {
		return reflect.ValueOf(locks[i]).Pointer() < reflect.ValueOf(locks[j]).Pointer()
	})
	return locks
}

func waitMessageCapacity(rc *RunCtx, changed <-chan struct{}) error {
	return rc.waitStream(func(ctx context.Context) error {
		select {
		case <-changed:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

func (m *messageAttempt) waitCapacity(rc *RunCtx, changed <-chan struct{}, input messageConfirmation) error {
	m.input.Lock()
	if !input.activeLocked() {
		m.input.Unlock()
		return ErrDeliveryExpired
	}
	inputChanged := input.messageChangedLocked()
	m.input.Unlock()
	return rc.waitStream(func(ctx context.Context) error {
		select {
		case <-changed:
			return ctx.Err()
		case <-inputChanged:
			return ctx.Err() // 交付被 Ack / Nack / Stop 作废也会唤醒容量等待。
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

// 类型不同的输入交付由包内确认接口收口，提交期间不运行用户回调。
func (m *messageAttempt) commit(rc *RunCtx, input messageConfirmation) error {
	locks := m.locks()
	for {
		if err := rc.Context().Err(); err != nil {
			return err
		}
		for _, batch := range m.batches {
			mu := batch.mutex()
			mu.Lock()
			changed, err := batch.readyLocked()
			mu.Unlock()
			if err != nil {
				return err
			}
			if changed != nil {
				if err := m.waitCapacity(rc, changed, input); err != nil {
					return err
				}
			}
		}
		changed, err := m.commitWithOperations(rc, input, locks)
		if err != nil || changed == nil {
			return err
		}
		if err := m.waitCapacity(rc, changed, input); err != nil {
			return err
		}
	}
}

type messageConfirmation interface {
	activeLocked() bool
	messageChangedLocked() <-chan struct{}
	messageOperation() *streamOperation
	confirmMessageLocked()
}

func (a *managedAck[T]) messageChangedLocked() <-chan struct{} { return a.stream.changed }
func (a *managedAck[T]) messageOperation() *streamOperation    { return &a.stream.operation }

// operation 门与状态锁用同一地址顺序；容量不足时先退出本方法释放所有门，再等待。
func (m *messageAttempt) commitWithOperations(rc *RunCtx, input messageConfirmation, locks []*sync.Mutex) (<-chan struct{}, error) {
	operations := map[*sync.Mutex]*streamOperation{m.input: input.messageOperation()}
	for _, batch := range m.batches {
		operations[batch.mutex()] = batch.operation()
	}
	for i, mu := range locks {
		if err := operations[mu].lock(rc); err != nil {
			for j := i - 1; j >= 0; j-- {
				operations[locks[j]].unlock()
			}
			return nil, err
		}
	}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			operations[locks[i]].unlock()
		}
	}()
	for _, mu := range locks {
		mu.Lock()
	}
	changed, err := m.preflightLocked(rc, input)
	for i := len(locks) - 1; i >= 0; i-- {
		locks[i].Unlock()
	}
	if err != nil || changed != nil {
		return changed, err
	}
	// 持门但不持状态锁：同步确认期间 Nack / Stop / 可靠追加不能插入。
	if err := rc.Context().Err(); err != nil {
		return nil, err
	}
	for _, mu := range locks {
		mu.Lock()
	}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			locks[i].Unlock()
		}
	}()
	if err := rc.Context().Err(); err != nil {
		return nil, err
	}
	// checkpoint 写只合入完整节点外帧；单顺序写者保证预检之后目标不变。
	if err := m.writes.commit(rc.exec); err != nil {
		return nil, err
	}
	for _, batch := range m.batches {
		batch.publishLocked()
	}
	input.confirmMessageLocked()
	return nil, nil // 接纳已经完成，之后不再复查取消并改报失败。
}

func (m *messageAttempt) preflightLocked(rc *RunCtx, input messageConfirmation) (<-chan struct{}, error) {
	if err := rc.Context().Err(); err != nil {
		return nil, err
	}
	if !input.activeLocked() {
		return nil, ErrDeliveryExpired
	}
	for _, batch := range m.batches {
		if changed, err := batch.readyLocked(); changed != nil || err != nil {
			return changed, err
		}
	}
	m.writes.mu.Lock()
	entries := make([]stagedWrite, 0, len(m.writes.staged))
	for _, entry := range m.writes.staged {
		entries = append(entries, *entry)
	}
	m.writes.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].ref.name < entries[j].ref.name })
	for _, entry := range entries {
		if state, _ := entry.slot.snapshot(); state != slotPending {
			if err := checkCommitTarget(rc.exec, m.writes, entry.ref, state); err != nil {
				return nil, err
			}
		}
	}
	return nil, nil
}
