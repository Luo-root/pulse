package pulse

import (
	"context"
	"fmt"
)

type hostStreamCompletion interface {
	pulseWaitReadCompletion(context.Context) error
}

// 只等待必需读取义务，不从消息队列取走数据，也不制造节点生命周期事件。
func (e *Execution) runHostStreamReader(ref keyRef) {
	defer e.wg.Done()
	value, err := e.slotOf(ref).wait(e.ctx)
	if isSkipped(err) {
		return
	}
	if err == nil {
		stream, ok := value.(hostStreamCompletion)
		if !ok {
			err = fmt.Errorf("pulse: host stream output %q did not provide a managed endpoint", ref.name)
		} else {
			err = stream.pulseWaitReadCompletion(e.ctx)
		}
	}
	if err != nil {
		e.fail(err)
	}
}

func (s Stream[T]) pulseWaitReadCompletion(ctx context.Context) error {
	if s.state == nil {
		return fmt.Errorf("pulse: host output contains an unopened stream")
	}
	if err := s.state.ensureCheckpoint(); err != nil {
		return err
	}
	for {
		state := s.state
		state.mu.Lock()
		err := state.err
		complete := state.stopped || (state.finished && len(state.pending) == 0)
		changed := state.changed
		state.mu.Unlock()
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// StreamSubscription 是本次执行的一个真实宿主读取者。必须在 Start 前登记；
// 它与图内读取者参与同一条流的单读取组检查，不把输出声明当作有人消费。
// 宿主应持续 Receive 并确认，显式 Stop 截取，或 Cancel 执行；Wait 不替宿主排空。
// 与 ReadOutput 不同，订阅在运行中读取，ReadOutput 仍只读 Wait 后的普通结果。
type StreamSubscription[T any] struct {
	exec *Execution
	key  Key[Stream[T]]
}

// SubscribeStream 为一个已声明的根出口登记本次执行的实时读取者。
// 相同执行、相同出口只能登记一次；图内读取者与本订阅不能瓜分同一读取组。
// 在 NewExecution 后、Start 前调用；错误不改变图或执行的注册状态。
func SubscribeStream[T any](e *Execution, key Key[Stream[T]]) (*StreamSubscription[T], error) {
	if e == nil || e.graph == nil {
		return nil, fmt.Errorf("pulse: SubscribeStream requires an execution")
	}
	ref := key.asRef()
	assemblyMu.Lock()
	defer assemblyMu.Unlock()
	e.graph.mu.Lock()
	defer e.graph.mu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return nil, ErrGraphStarted
	}
	declared, ok := e.graph.outPorts[ref.name]
	if !ref.valid() || !ok || declared != ref {
		return nil, fmt.Errorf("pulse: %s is not a declared stream output of this execution", ref)
	}
	if _, exists := e.hostReaders[ref.name]; exists {
		return nil, fmt.Errorf("pulse: stream output %q already has a host subscriber", ref.name)
	}
	if e.hostReaders == nil {
		e.hostReaders = make(map[string]keyRef)
	}
	e.hostReaders[ref.name] = ref
	return &StreamSubscription[T]{exec: e, key: key}, nil
}

// 调用方持装配锁与 Graph.mu；订阅使原本可选的透传进口成为本次必需读取。
func (e *Execution) checkHostStreamInputsLocked() error {
	for _, ref := range e.graph.outPortOrder {
		name := ref.name
		if _, subscribed := e.hostReaders[name]; !subscribed {
			continue
		}
		if _, inlet := e.graph.inPorts[name]; !inlet {
			continue
		}
		if _, supplied := e.seeded[name]; !supplied {
			return fmt.Errorf("pulse: host stream output %q is a pass-through inlet with no value for this run; seed or skip-seed it before subscribing", name)
		}
	}
	return nil
}

func (e *Execution) hostStreamReads() map[string]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	reads := make(map[string]bool, len(e.hostReaders))
	for name := range e.hostReaders {
		reads[name] = true
	}
	return reads
}

// Receive 取一条可确认交付；只有 Ack 才算本读取组处理完成。
// ctx 仅控制本次等待，取消不会自动确认消息或停止整个执行。
// 已成立的执行取消会阻止新读取；正常 Wait 收尾后仍可读取 EOF。
// 取消不撤销已经取得的交付，调用方仍须显式处置。
func (s *StreamSubscription[T]) Receive(ctx context.Context) (Delivery[T], error) {
	var zero Delivery[T]
	rc, release, err := s.readContext(ctx)
	if err != nil {
		return zero, err
	}
	defer release()
	stream, err := Get(rc, s.key)
	if err != nil {
		return zero, err
	}
	return stream.Receive(rc)
}

// Stop 明确结束本读取组，不把正常截取伪装成失败。未确认的消息不再属于
// 这个已结束读取组的义务；生产者会收到 ErrStreamStopped 并应明确结束生产。
func (s *StreamSubscription[T]) Stop(ctx context.Context) error {
	rc, release, err := s.readContext(ctx)
	if err != nil {
		return err
	}
	defer release()
	stream, err := Get(rc, s.key)
	if err != nil {
		return err
	}
	return stream.Stop(rc)
}

func (s *StreamSubscription[T]) readContext(ctx context.Context) (*RunCtx, func(), error) {
	if s == nil || s.exec == nil {
		return nil, nil, fmt.Errorf("pulse: nil stream subscription")
	}
	e := s.exec
	e.mu.Lock()
	started := e.started
	// 与 Wait 的终态同眼采样，不能只等异步 AfterFunc 转发既有取消。
	// 仅成功 Wait 的清理取消豁免；生产 completed 本身仍可能欠宿主确认。
	var interrupted error
	if started && !(e.done && e.err == nil) {
		interrupted = e.ctx.Err()
	}
	e.mu.Unlock()
	if !started {
		return nil, nil, ErrGraphNotStarted
	}
	if interrupted != nil {
		return nil, nil, interrupted
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	// 继续转发同步采样之后的执行取消；成功 Wait 的清理取消仍保留正常 EOF。
	stop := context.AfterFunc(e.ctx, func() {
		e.mu.Lock()
		clean := e.done && e.err == nil
		e.mu.Unlock()
		if !clean {
			cancel()
		}
	})
	ref := s.key.asRef()
	rc := &RunCtx{
		exec:    e,
		host:    true,
		node:    &Node{id: "<host>", requires: []keyRef{ref}},
		ctx:     ctx,
		allowed: map[string]keyRef{ref.name: ref},
	}
	return rc, func() { stop(); cancel() }, nil
}
