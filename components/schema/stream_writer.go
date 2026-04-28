package schema

import (
	"io"
	"sync"
)

// StreamWriter 流式消息写入器
// 与 StreamReader 配对使用，用于向流中发送消息
type StreamWriter struct {
	reader    *StreamReader
	closeOnce sync.Once
	err       error
	errMu     sync.Mutex
}

// PipeStreamReader 创建一个配对的 StreamReader 和 StreamWriter
// 类似于 io.Pipe，但用于 Message 流
func PipeStreamReader() (*StreamReader, *StreamWriter) {
	reader := NewStreamReader()
	writer := &StreamWriter{
		reader: reader,
	}
	return reader, writer
}

// Send 向流中发送一条消息
// 如果流已关闭或发生错误，返回错误
func (sw *StreamWriter) Send(msg *Message) error {
	sw.errMu.Lock()
	err := sw.err
	sw.errMu.Unlock()

	if err != nil {
		return err
	}

	// 检查 reader 是否已关闭
	select {
	case sw.reader.streamChan <- *msg:
		return nil
	default:
		// 通道可能已关闭或已满
		select {
		case sw.reader.streamChan <- *msg:
			return nil
		case <-func() chan struct{} {
			ch := make(chan struct{})
			go func() {
				// 尝试检测 reader 是否已关闭
				// 由于无法直接检测，我们尝试非阻塞发送
				close(ch)
			}()
			return ch
		}():
			return io.ErrClosedPipe
		}
	}
}

// Close 关闭写入器，同时关闭关联的读取器
func (sw *StreamWriter) Close() {
	sw.closeOnce.Do(func() {
		sw.reader.Close()
	})
}

// CloseWithError 关闭写入器并设置错误
func (sw *StreamWriter) CloseWithError(err error) {
	sw.errMu.Lock()
	sw.err = err
	sw.errMu.Unlock()
	sw.reader.setError(err)
	sw.Close()
}
