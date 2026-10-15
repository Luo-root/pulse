package pulse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
)

// ErrStreamStopped 是读取组显式 Stop 后给生产者的正常停止信号。
// 生产者应明确处理它；直接作为节点错误返回仍会使这次执行失败。
var ErrStreamStopped = errors.New("pulse: stream reading group stopped")

// StreamTerminal 是实际生产节点完整链的终态；空值表示生产尚未结束。
type StreamTerminal string

const (
	StreamCompleted StreamTerminal = "completed"
	StreamFailed    StreamTerminal = "failed"
	StreamCanceled  StreamTerminal = "canceled"
	StreamStopped   StreamTerminal = "stopped"
)

// StreamSnapshot 是一眼取得的流事实副本，不包含业务值。
// Stopped 表示读取组已退出，与生产完整链是否结束分别记录。
// AckedThrough 只跨过连续确认的消息，Stop 清理义务不会把消息算作确认。
type StreamSnapshot struct {
	Terminal     StreamTerminal
	Err          error
	LastSequence uint64
	AckedThrough uint64
	InFlight     int
	Stopped      bool
}

// Stream 是一次执行里的受管流端点，以 Key[Stream[T]] 声明和传递。
// 值复制共享同一读取组；FanOut 的多个 worker 竞争读取，不隐式广播。
// T 中的指针、slice、map 等引用共享业务值，不做深复制。
// 零值未打开，Receive、Messages、Stop 会返回具体错误。
type Stream[T any] struct {
	state *managedStream[T]
}

func (Stream[T]) pulseStreamElement() reflect.Type { return reflect.TypeFor[T]() }

// StreamWriter 绑定打开它的 RunCtx，写入仍属于该节点的完整执行链。
// 不要把写端留到节点返回后的 goroutine；同一 RunCtx（含 Fork）的 I/O 串行使用。
type StreamWriter[T any] struct {
	state *managedStream[T]
	rc    *RunCtx
}

type managedStream[T any] struct {
	mu          sync.Mutex
	operation   streamOperation
	queue       chan Delivery[T]
	changed     chan struct{}
	done        chan struct{}
	capacity    int
	pending     map[uint64]*managedMessage[T]
	last        uint64
	frontier    uint64
	finished    bool
	closed      bool
	stopped     bool
	terminal    StreamTerminal
	err         error
	sendErr     error
	readerErr   error
	nativeOwner managedReaderID
	nativeCtx   *RunCtx
}

type managedReaderID struct {
	exec *Execution
	node string
}

// OpenStream 发布一个受管端点。每个输出只能打开一次。
// WithBuffer 控制总在途量（已排队 + 已读取未确认）；未给正缓冲时上限为 1。
// 普通整节点 Retry / checkpoint 写暂存不能包含流 I/O，拒绝发生在发布前。
// 切面属于打开它的 NewNode；WithStreamAspects 仅用于流节点构造糖，在此被拒绝。
func OpenStream[T any](rc *RunCtx, key Key[Stream[T]], opts ...StreamOption) (*StreamWriter[T], error) {
	if err := checkManagedRunCtx(rc, "OpenStream"); err != nil {
		return nil, err
	}
	ref := key.asRef()
	if err := rc.must(ref, true); err != nil {
		return nil, err
	}
	if err := rc.Context().Err(); err != nil {
		return nil, err
	}
	if st, _ := rc.exec.slotOf(ref).snapshot(); st != slotPending {
		return nil, fmt.Errorf("pulse: OpenStream %q: output already arrived; a stream has one writer: %w", ref.name, ErrConflict)
	}
	cfg := newStreamConfig(opts)
	if len(cfg.aspects) != 0 {
		return nil, fmt.Errorf("pulse: OpenStream: WithStreamAspects configures stream node constructors; attach aspects to the owning NewNode")
	}
	if cfg.buf <= 0 {
		cfg.buf = 1
	}
	state := &managedStream[T]{
		queue: make(chan Delivery[T], cfg.buf), changed: make(chan struct{}),
		done: make(chan struct{}), capacity: cfg.buf,
		pending: make(map[uint64]*managedMessage[T]),
	}
	// 本地流早发布端点；检查点模式尚未开放，不能借普通写暂存发布。
	if err := rc.exec.slotOf(ref).resolveValue(Stream[T]{state: state}); err != nil {
		return nil, err
	}
	rc.streams.add(state)
	return &StreamWriter[T]{state: state, rc: rc}, nil
}

// Send 成功只表示消息已被接纳；Receive 不确认消息，Ack 才释放容量。
// 满载等待通过 RunCtx 的受管等待临时让出额度，业务代码继续前重新取得。
// 显式 Stop 返回 ErrStreamStopped，其他错误不被转换成正常停止。
func (w *StreamWriter[T]) Send(value T) error {
	if w == nil || w.state == nil {
		return fmt.Errorf("pulse: Send: unopened stream writer")
	}
	if err := checkManagedRunCtx(w.rc, "Send"); err != nil {
		return w.state.recordFailure(err)
	}
	for {
		if err := w.rc.Context().Err(); err != nil {
			return w.state.recordFailure(err)
		}
		s := w.state
		s.mu.Lock()
		if s.readerErr != nil {
			err := s.readerErr
			s.mu.Unlock()
			return err
		}
		if s.stopped {
			s.mu.Unlock()
			return ErrStreamStopped
		}
		if s.finished {
			err := s.err
			s.mu.Unlock()
			if err != nil {
				return err
			}
			return fmt.Errorf("pulse: Send: production already finished")
		}
		if len(s.pending) < s.capacity {
			if s.last == ^uint64(0) {
				s.mu.Unlock()
				return s.recordFailure(fmt.Errorf("pulse: Send: message sequence exhausted"))
			}
			s.last++
			message := &managedMessage[T]{value: value, sequence: s.last}
			s.pending[message.sequence] = message
			s.enqueueLocked(message)
			s.mu.Unlock()
			// 接纳已发生，之后的取消不能把它改报为“没有发送”。
			return nil
		}
		changed := s.changed
		s.mu.Unlock()
		if err := w.rc.waitStream(func(ctx context.Context) error {
			select {
			case <-changed:
				return ctx.Err()
			case <-ctx.Done():
				return ctx.Err()
			}
		}); err != nil {
			return s.recordFailure(err)
		}
	}
}

// Receive 取得当前读取组的一次交付。成功后由调用方 Ack 或 Nack。
// 取消只打断尚未完成的等待，不确认消息。取得交付后即保留成功，
// 包括恢复额度期间取消；调用方可明确 Nack，不能将已取得改报成未取得。
// 仅生产正常结束且义务已确认，或显式 Stop 后返回 io.EOF；生产失败返回原因。
func (s Stream[T]) Receive(rc *RunCtx) (Delivery[T], error) {
	var delivery Delivery[T]
	if err := s.checkReader(rc, "Receive"); err != nil {
		return delivery, err
	}
	for {
		if err := rc.Context().Err(); err != nil {
			return delivery, s.state.receiveError(err)
		}
		var ok, received bool
		select {
		case delivery, ok = <-s.state.queue:
			received = true
		default:
		}
		var err error
		if !received {
			err = rc.waitStream(func(ctx context.Context) error {
				select {
				case delivery, ok = <-s.state.queue:
					received = true
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}
		if err != nil {
			return Delivery[T]{}, s.state.receiveError(err)
		}
		if !ok {
			return Delivery[T]{}, s.state.receiveError(io.EOF)
		}
		// Stop 可以与原生 channel 的读取相遇；已作废的交付不能再交给业务。
		if delivery.claim(managedReaderID{exec: rc.exec, node: rc.NodeID()}, rc) {
			return delivery, nil
		}
	}
}

// Snapshot 返回生产结论和交付进度副本；零端点的 Err 说明尚未打开。
func (s Stream[T]) Snapshot() StreamSnapshot {
	if s.state == nil {
		return StreamSnapshot{Err: fmt.Errorf("pulse: Snapshot: unopened stream")}
	}
	state := s.state
	state.mu.Lock()
	defer state.mu.Unlock()
	return StreamSnapshot{
		Terminal:     state.terminal,
		Err:          state.err,
		LastSequence: state.last,
		AckedThrough: state.frontier,
		InFlight:     len(state.pending),
		Stopped:      state.stopped,
	}
}

// Done 在实际生产节点的完整链结束后关闭；与消息是否已经确认分别记录。
// 零端点返回 nil。生产正常结束时本地 RunCtx 清理取消不会撤销已接纳消息。
func (s Stream[T]) Done() <-chan struct{} {
	if s.state == nil {
		return nil
	}
	return s.state.done
}

func checkManagedRunCtx(rc *RunCtx, operation string) error {
	if rc == nil || rc.exec == nil || rc.node == nil || rc.ctx == nil {
		return fmt.Errorf("pulse: %s: a node RunCtx is required", operation)
	}
	if rc.writes != nil {
		return fmt.Errorf("pulse: %s on node %q: managed stream I/O cannot run inside a per-attempt write set "+
			"(ordinary whole-node Retry or checkpoint staging); use an explicit message attempt instead", operation, rc.NodeID())
	}
	return nil
}

func (s Stream[T]) checkReader(rc *RunCtx, operation string) error {
	if s.state == nil {
		return fmt.Errorf("pulse: %s: unopened stream", operation)
	}
	if err := checkManagedRunCtx(rc, operation); err != nil {
		return err
	}
	// 挂载接线可改键名，权限依本 RunCtx 声明的真实槽来源判断；
	// 不能凭名字放行别次 Execution 的同名端点。
	for _, ref := range rc.allowed {
		if ref.typ != reflect.TypeFor[Stream[T]]() {
			continue
		}
		st, raw := rc.exec.slotOf(ref).snapshot()
		if endpoint, ok := raw.(Stream[T]); st == slotReady && ok && endpoint.state == s.state {
			if err := rc.must(ref, false); err != nil {
				return err
			}
			return nil
		}
	}
	return fmt.Errorf("pulse: %s on node %q: stream is not a declared endpoint of this execution: %w", operation, rc.NodeID(), ErrUndeclared)
}

func (s *managedStream[T]) signalLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *managedStream[T]) closeQueueLocked() {
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
}

func (s *managedStream[T]) recordFailure(err error) error {
	if err != nil && err != ErrStreamStopped {
		s.mu.Lock()
		if s.sendErr == nil {
			s.sendErr = err
		}
		s.mu.Unlock()
	}
	return err
}

func (s *managedStream[T]) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sendErr
}

func (s *managedStream[T]) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	if err == nil || isSkipped(err) {
		err = s.sendErr
	}
	s.err = err
	s.finished = true
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		s.terminal = StreamCanceled
	case err != nil:
		s.terminal = StreamFailed
	case s.stopped:
		s.terminal = StreamStopped
	default:
		s.terminal = StreamCompleted
	}
	// 先锁存生产原因，再开放终态通知。仍有义务时保留唯一队列，
	// 这样生产结束后的 Nack 仍可重投；最后一次 Ack 才关闭它。
	close(s.done)
	if len(s.pending) == 0 {
		s.closeQueueLocked()
	}
	s.signalLocked()
}

func (s *managedStream[T]) receiveError(fallback error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	return fallback
}
