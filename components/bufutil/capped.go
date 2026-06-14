// Package bufutil 提供共享的缓冲区工具
package bufutil

import "bytes"

// CappedBuffer 有容量上限的输出缓冲区
// 写入超过上限后静默丢弃多余数据，不报错
type CappedBuffer struct {
	buf bytes.Buffer
	max int
}

// NewCappedBuffer 创建指定容量上限的缓冲区
func NewCappedBuffer(maxBytes int) *CappedBuffer {
	return &CappedBuffer{max: maxBytes}
}

func (b *CappedBuffer) Write(p []byte) (int, error) {
	origLen := len(p)
	if b.buf.Len() >= b.max {
		return origLen, nil
	}
	remaining := b.max - b.buf.Len()
	if len(p) > remaining {
		p = p[:remaining]
	}
	_, err := b.buf.Write(p)
	if err != nil {
		return 0, err
	}
	return origLen, nil
}

func (b *CappedBuffer) String() string {
	return b.buf.String()
}

// Truncated 返回是否发生过截断
func (b *CappedBuffer) Truncated() bool {
	return b.buf.Len() >= b.max
}
