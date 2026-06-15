package mcp

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SSEConfig SSE 传输配置
type SSEConfig struct {
	// URL SSE 端点地址，如 http://localhost:8080/sse
	URL string
	// Headers 自定义 HTTP 请求头（如认证头）
	Headers map[string]string
	// HTTPClient 自定义 HTTP 客户端（可选）
	HTTPClient *http.Client
}

// SSETransport 基于 HTTP + Server-Sent Events 的传输层
// 协议流程：
//  1. 客户端 GET 连接 SSE 端点
//  2. 服务端发送 endpoint 事件，告知 POST 地址
//  3. 客户端 POST JSON-RPC 消息到该地址
//  4. 服务端通过 SSE message 事件返回响应
type SSETransport struct {
	config     SSEConfig
	httpClient *http.Client

	endpointURL string             // 从 endpoint 事件获取的 POST 地址
	messageCh   chan []byte        // 接收消息队列
	errCh       chan error         // 错误通知
	cancelSSE   context.CancelFunc // 取消 SSE 连接

	mu     sync.Mutex
	closed bool
}

func NewSSETransport(config SSEConfig) *SSETransport {
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 0} // SSE 需要长连接，不设超时
	}
	return &SSETransport{
		config:     config,
		httpClient: client,
		messageCh:  make(chan []byte, 64),
		errCh:      make(chan error, 1),
	}
}

func (t *SSETransport) Connect(ctx context.Context) error {
	sseCtx, cancel := context.WithCancel(ctx)
	t.cancelSSE = cancel

	req, err := http.NewRequestWithContext(sseCtx, "GET", t.config.URL, nil)
	if err != nil {
		return fmt.Errorf("mcp sse: create request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")
	for k, v := range t.config.Headers {
		req.Header.Set(k, v)
	}

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mcp sse: connect: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("mcp sse: unexpected status %d", resp.StatusCode)
	}

	// 启动 SSE 事件循环
	go t.readSSELoop(resp.Body)

	// 等待 endpoint 事件或超时
	select {
	case err := <-t.errCh:
		return fmt.Errorf("mcp sse: %w", err)
	case <-time.After(10 * time.Second):
		return fmt.Errorf("mcp sse: timeout waiting for endpoint event")
	case <-ctx.Done():
		return ctx.Err()
	default:
		// 检查 endpoint 是否已设置
		t.mu.Lock()
		hasEndpoint := t.endpointURL != ""
		t.mu.Unlock()
		if !hasEndpoint {
			// endpoint 还没到，再等一下
			select {
			case err := <-t.errCh:
				return fmt.Errorf("mcp sse: %w", err)
			case <-time.After(10 * time.Second):
				return fmt.Errorf("mcp sse: timeout waiting for endpoint event")
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	return nil
}

// readSSELoop 解析 SSE 流，分发事件
func (t *SSETransport) readSSELoop(body io.ReadCloser) {
	defer body.Close()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var eventType string
	var dataLines []string

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// 空行 = 事件结束，分发
			if len(dataLines) > 0 {
				data := strings.Join(dataLines, "\n")
				t.handleEvent(eventType, data)
			}
			eventType = ""
			dataLines = nil
			continue
		}

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		// 忽略 id: 和 retry: 行
	}

	if err := scanner.Err(); err != nil {
		t.errCh <- fmt.Errorf("sse read: %w", err)
	} else {
		t.errCh <- fmt.Errorf("sse connection closed")
	}
}

// handleEvent 处理单个 SSE 事件
func (t *SSETransport) handleEvent(eventType, data string) {
	switch eventType {
	case "endpoint":
		// endpoint 事件：更新 POST 地址
		t.mu.Lock()
		// 如果是相对路径，基于 SSE URL 拼接
		if strings.HasPrefix(data, "/") {
			// 从 SSE URL 提取 scheme + host
			idx := strings.Index(t.config.URL[8:], "/") // 跳过 https://
			if idx >= 0 {
				t.endpointURL = t.config.URL[:8+idx] + data
			} else {
				t.endpointURL = t.config.URL + data
			}
		} else {
			t.endpointURL = data
		}
		t.mu.Unlock()

	case "message":
		// message 事件：JSON-RPC 消息
		t.messageCh <- []byte(data)

	default:
		// 忽略未知事件类型
	}
}

func (t *SSETransport) Send(data []byte) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return fmt.Errorf("mcp sse: transport closed")
	}
	url := t.endpointURL
	t.mu.Unlock()

	if url == "" {
		return fmt.Errorf("mcp sse: endpoint not available yet")
	}

	req, err := http.NewRequestWithContext(context.Background(), "POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("mcp sse: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range t.config.Headers {
		req.Header.Set(k, v)
	}

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("mcp sse: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mcp sse: send failed %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (t *SSETransport) Recv() ([]byte, error) {
	select {
	case msg, ok := <-t.messageCh:
		if !ok {
			return nil, fmt.Errorf("mcp sse: connection closed")
		}
		return msg, nil
	case err := <-t.errCh:
		return nil, err
	}
}

func (t *SSETransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true

	if t.cancelSSE != nil {
		t.cancelSSE()
	}
	close(t.messageCh)

	return nil
}
