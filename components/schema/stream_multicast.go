package schema

import (
	"context"
	"io"
	"sync"
	"time"
)

// MulticastController 多播控制器，管理源流到多个子流的分发
// 提供优雅关闭、错误传播、背压控制等生产级特性
type MulticastController struct {
	source     *StreamReader
	readers    []*StreamReader
	mu         sync.RWMutex
	closed     bool
	err        error
	wg         sync.WaitGroup
	cancel     context.CancelFunc
	bufferSize int
}

// NewMulticastController 创建多播控制器
func NewMulticastController(source *StreamReader, bufferSize int) *MulticastController {
	if bufferSize <= 0 {
		bufferSize = 16
	}
	return &MulticastController{
		source:     source,
		bufferSize: bufferSize,
	}
}

// Fork 创建 N 个子流，返回可独立读取的 StreamReader 列表
// 子流通过内部缓冲实现背压隔离，慢消费者不会影响其他消费者
func (mc *MulticastController) Fork(n int) []*StreamReader {
	if n <= 0 {
		return nil
	}

	mc.mu.Lock()
	defer mc.mu.Unlock()

	if mc.closed {
		// 已关闭时返回已关闭的reader
		readers := make([]*StreamReader, n)
		for i := range readers {
			readers[i] = NewStreamReaderWithBuffer(1)
			readers[i].Close()
		}
		return readers
	}

	readers := make([]*StreamReader, n)
	for i := range readers {
		readers[i] = NewStreamReaderWithBuffer(mc.bufferSize)
	}
	mc.readers = append(mc.readers, readers...)

	// 首次Fork时启动转发协程
	if len(mc.readers) == n {
		ctx, cancel := context.WithCancel(context.Background())
		mc.cancel = cancel
		mc.wg.Add(1)
		go mc.forwardLoop(ctx)
	}

	return readers
}

// forwardLoop 后台转发循环：从源流读取，广播到所有子流
func (mc *MulticastController) forwardLoop(ctx context.Context) {
	defer mc.wg.Done()

	for {
		select {
		case <-ctx.Done():
			mc.closeAllReaders(ctx.Err())
			return
		default:
		}

		msg, err := mc.source.Recv()
		if err != nil {
			if err == io.EOF {
				mc.closeAllReaders(nil)
			} else {
				mc.closeAllReaders(err)
			}
			return
		}

		// 获取当前所有reader的快照
		mc.mu.RLock()
		readers := make([]*StreamReader, len(mc.readers))
		copy(readers, mc.readers)
		mc.mu.RUnlock()

		if len(readers) == 0 {
			continue
		}

		// 克隆消息给每个子流
		cloned := make([]Message, len(readers))
		for i := range readers {
			cloned[i] = msg.Clone()
		}

		// 异步发送给所有子流，带超时保护
		mc.broadcast(readers, cloned, 5*time.Second)
	}
}

// broadcast 向所有子流广播消息，每个子流独立超时控制
// 超时的子流会被标记错误并关闭，不影响其他子流
func (mc *MulticastController) broadcast(readers []*StreamReader, msgs []Message, timeout time.Duration) {
	n := len(readers)
	if n == 0 {
		return
	}

	var wg sync.WaitGroup
	wg.Add(n)

	for i := range readers {
		go func(idx int) {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			select {
			case readers[idx].streamChan <- msgs[idx]:
				// 发送成功
			case <-ctx.Done():
				// 超时：设置错误并关闭该子流
				readers[idx].setError(context.DeadlineExceeded)
				readers[idx].Close()
				// 从控制器中移除该子流
				mc.removeReader(readers[idx])
			}
		}(i)
	}

	wg.Wait()
}

// removeReader 从控制器中移除指定子流
func (mc *MulticastController) removeReader(r *StreamReader) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	for i, reader := range mc.readers {
		if reader == r {
			// 交换删除
			mc.readers[i] = mc.readers[len(mc.readers)-1]
			mc.readers = mc.readers[:len(mc.readers)-1]
			break
		}
	}
}

// closeAllReaders 关闭所有子流并设置错误（如果err != nil）
func (mc *MulticastController) closeAllReaders(err error) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	if mc.closed {
		return
	}
	mc.closed = true
	mc.err = err

	for _, r := range mc.readers {
		if err != nil {
			r.setError(err)
		}
		r.Usage = mc.source.Usage
		r.Close()
	}
}

// Stop 停止多播，关闭所有子流
// 注意：Stop 不会等待转发协程结束，而是立即关闭所有子流
func (mc *MulticastController) Stop() {
	mc.mu.Lock()
	if mc.cancel != nil {
		mc.cancel()
	}
	// 直接关闭所有reader，不等待转发协程
	mc.closed = true
	for _, r := range mc.readers {
		r.Close()
	}
	mc.mu.Unlock()
}

// Err 返回多播过程中的错误
func (mc *MulticastController) Err() error {
	mc.mu.RLock()
	defer mc.mu.RUnlock()
	return mc.err
}
