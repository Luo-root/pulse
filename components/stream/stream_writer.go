package stream

import (
	"sync"

	"github.com/Luo-root/pulse/components/schema"
)

// StreamWriter 流式消息写入器
// 与 StreamReader 配对使用，用于向流中发送消息
type StreamWriter struct {
	reader    *schema.StreamReader
	closeOnce sync.Once
	err       error
	errMu     sync.Mutex
}

// PipeStreamReader 创建一个配对的 StreamReader 和 StreamWriter
// 类似于 io.Pipe，但用于 Message 流
func PipeStreamReader() (*schema.StreamReader, *StreamWriter) {
	reader := schema.NewStreamReader()
	writer := &StreamWriter{
		reader: reader,
	}
	return reader, writer
}

// Send 向流中发送一条消息
// 如果流已关闭或发生错误，返回错误
func (sw *StreamWriter) Send(msg *schema.Message) error {
	sw.errMu.Lock()
	err := sw.err
	sw.errMu.Unlock()
	if err != nil {
		return err
	}
	sw.reader.Send(*msg)
	return nil
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
	sw.reader.SetError(err)
	sw.Close()
}
