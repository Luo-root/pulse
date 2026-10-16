package pulse

import (
	"context"
	"reflect"
	"sync"
)

// 受管流类型的标记由包内方法提供；普通 channel 仍可作本地实时端口，
// 但不会因此获得消息确认和持久恢复能力。
type managedStreamDeclaration interface {
	pulseStreamElement() reflect.Type
}

var managedStreamDeclarationType = reflect.TypeOf((*managedStreamDeclaration)(nil)).Elem()

func isManagedStreamType(t reflect.Type) bool {
	return t != nil && t.Implements(managedStreamDeclarationType)
}

func isLiveStreamType(t reflect.Type) bool {
	return t != nil && (t.Kind() == reflect.Chan || isManagedStreamType(t))
}

// 每个实际写端节点在完整 Aspect 链结束后终结它拥有的流。
// 这份账与 RunCtx 一起属于一次运行，Fork 共享；祖先 Sub 不属于写端链。
type streamLifecycle interface {
	failure() error
	finish(error)
}

type managedReaderLifecycle interface {
	pulseReaderFinished(*RunCtx, error) error
}

type nodeStreams struct {
	// 归因用实际节点的取消域；Fork 共享此账，它的局部取消不等于节点取消。
	exec       *Execution
	ctx        context.Context
	mu         sync.Mutex
	outputs    []streamLifecycle
	messageErr error
}

func (s *nodeStreams) classifyFailure(err error) error {
	if s == nil || !isCanceled(err) || isExecutionCancellation(err) {
		return err
	}
	if s.ctx != nil && s.ctx.Err() != nil {
		return &executionCancellation{cause: err}
	}
	if s.exec != nil {
		return s.exec.classifyFailure(err)
	}
	return err
}

func (s *nodeStreams) failMessage(err error) {
	if err == nil || isSkipped(err) {
		return
	}
	err = s.classifyFailure(err)
	s.mu.Lock()
	if s.messageErr == nil {
		s.messageErr = err
	}
	s.mu.Unlock()
}

func (s *nodeStreams) add(output streamLifecycle) {
	s.mu.Lock()
	s.outputs = append(s.outputs, output)
	s.mu.Unlock()
}

func (rc *RunCtx) finishStreams(err error) error {
	if rc.streams == nil {
		return err
	}
	err = rc.streams.classifyFailure(err)
	rc.streams.mu.Lock()
	outputs := append([]streamLifecycle(nil), rc.streams.outputs...)
	messageErr := rc.streams.messageErr
	rc.streams.mu.Unlock()
	if (err == nil || isSkipped(err)) && messageErr != nil {
		err = rc.streams.classifyFailure(messageErr)
	}
	if err == nil || isSkipped(err) {
		for _, output := range outputs {
			if cause := output.failure(); cause != nil {
				err = rc.streams.classifyFailure(cause)
				break
			}
		}
	}
	// Sub 只是接线代理，不是读取者；只有声明 Requires 的实际业务节点
	// 履行读取义务。同一端点可经不同名称出现，按端点幂等收尾。
	if rc.node != nil && !rc.node.sub {
		seen := make(map[managedReaderLifecycle]bool)
		for _, ref := range rc.node.requires {
			if !isManagedStreamType(ref.typ) {
				continue
			}
			input := rc.exec.slotOf(ref)
			st, value := input.snapshot()
			// 切面可以不推进 next 就成功或跳过。此时真实读取者已经退出，
			// 但端点可能尚未发布；在本栈等待到达后记退出账，不能漏掉义务。
			// 正常缺失不产生端点，也不产生读取义务；失败时不再等新输入。
			if st == slotPending && (err == nil || isSkipped(err)) {
				cause := rc.waitStream(func(ctx context.Context) error {
					_, waitErr := input.wait(ctx)
					return waitErr
				})
				if cause != nil && !isSkipped(cause) {
					err = rc.streams.classifyFailure(cause)
				}
				st, value = input.snapshot()
			}
			reader, ok := value.(managedReaderLifecycle)
			if st != slotReady || !ok || seen[reader] {
				continue
			}
			seen[reader] = true
			if cause := reader.pulseReaderFinished(rc, err); cause != nil && (err == nil || isSkipped(err)) {
				err = rc.streams.classifyFailure(cause)
			}
		}
	}
	// 先确定完整节点的共同结论，再终结各出口；不能先把第一条流关成成功，
	// 随后才从另一条流发现这个实际写端早已失败。
	for _, output := range outputs {
		output.finish(err)
	}
	return err
}
