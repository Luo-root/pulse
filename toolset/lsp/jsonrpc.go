package lsp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// frameConn 是一帧一读写的 JSON-RPC 连接缝：真实现走 stdio 分帧，
// 测试注入内存实现钉死协议序（同 builtins lookupIPAddr 的缝模式）。
type frameConn interface {
	// Send 原子写一帧（含 header）。写端是管道：server 不读 stdin 时缓冲区会
	// 写满、写会一直挂着，因此由 ctx 兜底——ctx 结束即返回其错误。
	Send(ctx context.Context, body []byte) error
	// Recv 阻塞读一帧 body。
	Recv() ([]byte, error)
	Close() error
}

// stdioConn 按 LSP 规范在 io 流上做 Content-Length 分帧。
type stdioConn struct {
	mu sync.Mutex
	w  io.Writer
	rc io.ReadCloser // 读端可关时持有（进程退出后释放父进程的 fd）
	r  *bufio.Reader
}

func newStdioConn(w io.Writer, r io.Reader) *stdioConn {
	c := &stdioConn{w: w, r: bufio.NewReaderSize(r, 64*1024)}
	if rc, ok := r.(io.ReadCloser); ok {
		c.rc = rc
	}
	return c
}

// Send 把一帧交给写端。写落在独立 goroutine 里（并持 c.mu），调用方只等
// ctx 或写结果：notify / 收尾那几条路径没有请求帧可等，ctx 是唯一上限。
// ctx 结束后残写可能仍卡在管道上，但它持锁——后来的写不会与它交错成坏帧；
// 进程被树杀（写端破裂）时它自行返回。
func (c *stdioConn) Send(ctx context.Context, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	errCh := make(chan error, 1)
	go func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		errCh <- c.writeFrame(body)
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// writeFrame 是 Send 的实际写序：header + body（c.mu 由 Send 的写 goroutine 持）。
func (c *stdioConn) writeFrame(body []byte) error {
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

// Close 关掉读端（若可关）：readLoop 退出后父进程不再占管道 fd。写端与进程
// 句柄由 spawn 的 Wait 协程 / 进程树杀收尾。
func (c *stdioConn) Close() error {
	if c.rc == nil {
		return nil
	}
	return c.rc.Close()
}
