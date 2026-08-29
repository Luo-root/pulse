package lsp

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// frameConn 是一帧一读写的 JSON-RPC 连接缝：真实现走 stdio 分帧，
// 测试注入内存实现钉死协议序（同 builtins lookupIPAddr 的缝模式）。
type frameConn interface {
	// Send 原子写一帧（含 header）。
	Send(body []byte) error
	// Recv 阻塞读一帧 body。
	Recv() ([]byte, error)
	Close() error
}

// stdioConn 按 LSP 规范在 io 流上做 Content-Length 分帧。
type stdioConn struct {
	mu sync.Mutex
	w  io.Writer
	r  *bufio.Reader
}

func newStdioConn(w io.Writer, r io.Reader) *stdioConn {
	return &stdioConn{w: w, r: bufio.NewReaderSize(r, 64*1024)}
}

func (c *stdioConn) Send(body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err := c.w.Write(body)
	return err
}

func (c *stdioConn) Recv() ([]byte, error) {
	clen := -1
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue
		}
		if strings.ToLower(strings.TrimSpace(line[:idx])) == "content-length" {
			clen, err = strconv.Atoi(strings.TrimSpace(line[idx+1:]))
			if err != nil {
				return nil, fmt.Errorf("lsp: bad content-length: %w", err)
			}
		}
	}
	if clen < 0 {
		return nil, fmt.Errorf("lsp: missing content-length header")
	}
	if clen > maxFrameBytes {
		return nil, fmt.Errorf("lsp: frame too large (%d bytes > %d)", clen, maxFrameBytes)
	}
	body := make([]byte, clen)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return nil, err
	}
	return body, nil
}

func (c *stdioConn) Close() error { return nil } // stdio 生命周期由进程树杀负责
