package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Luo-root/pulse/components/schema"
	"github.com/chromedp/chromedp"
)

// ============================================================================
// web_fetch — 轻量 HTTP 抓取，不启动浏览器
// ============================================================================

func WebFetch(ctx context.Context, args map[string]any) (any, error) {
	url, _ := args["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("url is required")
	}

	format := "text"
	if f, ok := args["format"].(string); ok {
		format = f
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; PulseBot/1.0)")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return nil, err
	}

	result := map[string]any{
		"url":         url,
		"status":      resp.StatusCode,
		"body_length": len(body),
	}

	switch format {
	case "html":
		result["content"] = string(body)
	case "headers":
		h := make(map[string]string)
		for k, v := range resp.Header {
			h[k] = strings.Join(v, ", ")
		}
		result["headers"] = h
	default:
		result["content"] = htmlToTextV2(string(body))
	}

	return result, nil
}

// htmlToText 使用正则表达式和字符串操作将 HTML 转换为纯文本
// 已弃用：请使用 htmlToTextV2（基于 golang.org/x/net/html 解析器）
func htmlToText(s string) string {
	// 去掉 script / style / noscript 块
	lower := strings.ToLower(s)
	for _, tag := range []string{"script", "style", "noscript"} {
		for {
			start := strings.Index(lower, "<"+tag)
			if start == -1 {
				break
			}
			end := strings.Index(lower[start:], "</"+tag+">")
			if end == -1 {
				s = s[:start]
				lower = strings.ToLower(s)
				break
			}
			s = s[:start] + s[start+end+len("</"+tag+">"):]
			lower = strings.ToLower(s)
		}
	}

	// 去标签
	var buf strings.Builder
	inTag := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '<':
			inTag = true
		case '>':
			inTag = false
			buf.WriteByte(' ')
		default:
			if !inTag {
				buf.WriteByte(s[i])
			}
		}
	}

	// 合并多余空白行
	var lines []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			lines = append(lines, s)
		}
	}
	return strings.Join(lines, "\n")
}

// ============================================================================
// web_browse — chromedp 真实浏览器
// ============================================================================

func WebBrowse(ctx context.Context, args map[string]any) (any, error) {
	action := "text"
	if a, ok := args["action"].(string); ok {
		action = a
	}

	if action == "close" {
		sid, _ := args["session_id"].(string)
		if sid == "" {
			return nil, fmt.Errorf("session_id is required for close action")
		}
		closeSession(sid)
		return map[string]any{"status": "closed", "session_id": sid}, nil
	}

	url, _ := args["url"].(string)
	if url == "" {
		return nil, fmt.Errorf("url is required")
	}

	// 参数校验
	switch action {
	case "click":
		if selector, _ := args["selector"].(string); selector == "" {
			return nil, fmt.Errorf("selector is required for click")
		}
	case "type":
		selector, _ := args["selector"].(string)
		inputText, _ := args["text"].(string)
		if selector == "" || inputText == "" {
			return nil, fmt.Errorf("selector and text are required for type")
		}
	case "evaluate":
		if expression, _ := args["expression"].(string); expression == "" {
			return nil, fmt.Errorf("expression is required for evaluate")
		}
	case "wait":
		if selector, _ := args["selector"].(string); selector == "" {
			return nil, fmt.Errorf("selector is required for wait")
		}
	case "text", "screenshot":
	default:
		return nil, fmt.Errorf("unknown action: %s (available: text, screenshot, click, type, evaluate, wait, close)", action)
	}

	chromePath, err := defaultMgr.ChromePath()
	if err != nil {
		return nil, fmt.Errorf("初始化浏览器失败: %w", err)
	}

	sessionID, _ := args["session_id"].(string)
	sess, sid, err := getOrCreateSession(sessionID, chromePath, ctx)
	if err != nil {
		return nil, err
	}

	timeout := 30 * time.Second
	if t, ok := args["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t * float64(time.Second))
	}

	// 导航（换了页面才导航）
	if url != sess.url {
		if err := sess.runWithTimeout(timeout,
			chromedp.Navigate(url),
			chromedp.WaitReady("body"),
		); err != nil {
			closeSession(sid)
			return nil, fmt.Errorf("navigate: %w", err)
		}
		sess.url = url
	}

	result := map[string]any{
		"url":        url,
		"session_id": sid,
	}

	switch action {

	case "text":
		var text string
		if err := sess.runWithTimeout(timeout,
			chromedp.Text("body", &text, chromedp.ByQuery),
		); err != nil {
			return nil, err
		}
		if len(text) > 100_000 {
			text = text[:100_000] + "\n...(truncated)"
		}
		result["content"] = text

	case "screenshot":
		var buf []byte
		fullPage := false
		if fp, ok := args["full_page"].(bool); ok {
			fullPage = fp
		}
		if fullPage {
			err = sess.runWithTimeout(timeout, chromedp.FullScreenshot(&buf, 90))
		} else {
			err = sess.runWithTimeout(timeout, chromedp.CaptureScreenshot(&buf))
		}
		if err != nil {
			return nil, err
		}

		outputPath, hasOutputPath := args["output_path"].(string)

		// 保存到文件（仅指定了 output_path 时）
		if hasOutputPath && outputPath != "" {
			if err := os.WriteFile(outputPath, buf, 0644); err != nil {
				return nil, fmt.Errorf("save screenshot: %w", err)
			}
			result["screenshot_path"] = outputPath
		}

		result["size_bytes"] = len(buf)

		// 返回 base64 图片作为 ContentPart
		b64 := base64.StdEncoding.EncodeToString(buf)
		return &schema.ToolResultContent{
			Content: fmt.Sprintf("截图完成 (%d bytes)", len(buf)),
			ContentParts: []schema.ContentPart{
				schema.TextPart(fmt.Sprintf("截图完成 (%d bytes)", len(buf))),
				schema.ImagePartBase64("image/png", b64),
			},
		}, nil

	case "click":
		selector := args["selector"].(string)
		if err := sess.runWithTimeout(timeout,
			chromedp.Click(selector, chromedp.ByQuery),
			chromedp.Sleep(500*time.Millisecond),
		); err != nil {
			return nil, fmt.Errorf("click %q: %w", selector, err)
		}
		result["status"] = "clicked"
		result["selector"] = selector

	case "type":
		selector := args["selector"].(string)
		inputText := args["text"].(string)
		actions := []chromedp.Action{
			chromedp.Click(selector, chromedp.ByQuery),
			chromedp.SendKeys(selector, inputText, chromedp.ByQuery),
		}
		if submit, _ := args["submit"].(bool); submit {
			actions = append(actions, chromedp.SendKeys(selector, "\n", chromedp.ByQuery))
		}
		if err := sess.runWithTimeout(timeout, actions...); err != nil {
			return nil, fmt.Errorf("type: %w", err)
		}
		chromedp.Run(sess.browserCtx, chromedp.Sleep(500*time.Millisecond))
		result["status"] = "typed"
		result["selector"] = selector

	case "evaluate":
		expression := args["expression"].(string)
		var jsResult interface{}
		if err := sess.runWithTimeout(timeout,
			chromedp.Evaluate(expression, &jsResult),
		); err != nil {
			return nil, fmt.Errorf("evaluate: %w", err)
		}
		result["result"] = jsResult

	case "wait":
		selector := args["selector"].(string)
		if err := sess.runWithTimeout(timeout,
			chromedp.WaitVisible(selector, chromedp.ByQuery),
		); err != nil {
			return nil, fmt.Errorf("wait %q: %w", selector, err)
		}
		result["status"] = "visible"
		result["selector"] = selector
	}

	return result, nil
}

// ============================================================================
// 参数定义
// ============================================================================

var webFetchParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"url": map[string]any{
			"type":        "string",
			"description": "要抓取的 URL",
		},
		"format": map[string]any{
			"type":        "string",
			"enum":        []string{"text", "html", "headers"},
			"description": "返回格式：text=纯文本（默认），html=原始HTML，headers=响应头",
		},
	},
	"required": []string{"url"},
}

var webBrowseParams = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"url": map[string]any{
			"type":        "string",
			"description": "要访问的 URL（首次调用必填，复用 session 时可省略省略则在当前页面操作）",
		},
		"action": map[string]any{
			"type":        "string",
			"enum":        []string{"text", "screenshot", "click", "type", "evaluate", "wait", "close"},
			"description": "操作类型：text/screenshot/click/type/evaluate/wait/close",
		},
		"session_id": map[string]any{
			"type":        "string",
			"description": "会话 ID。不传则创建新会话，传已有 ID 则复用。用于多步交互操作",
		},
		"selector": map[string]any{
			"type":        "string",
			"description": "CSS 选择器（click/type/wait 需要）",
		},
		"text": map[string]any{
			"type":        "string",
			"description": "要输入的文本（type 操作需要）",
		},
		"expression": map[string]any{
			"type":        "string",
			"description": "JavaScript 表达式（evaluate 操作需要）",
		},
		"full_page": map[string]any{
			"type":        "boolean",
			"description": "screenshot 是否截取全页（默认 false）",
		},
		"output_path": map[string]any{
			"type":        "string",
			"description": "screenshot 输出文件路径（可选，不指定则自动生成临时文件）",
		},
		"submit": map[string]any{
			"type":        "boolean",
			"description": "type 后是否回车提交（默认 false）",
		},
		"timeout": map[string]any{
			"type":        "number",
			"description": "超时秒数，默认 30",
		},
	},
	"required": []string{"action"},
}

// ============================================================================
// 注册
// ============================================================================

func RegisterWebTools(registry *ToolRegistry) {
	registry.MustRegister(ToolMetadata{
		Name: "web_fetch",
		Description: "通过 HTTP 抓取网页内容（纯文本/HTML/响应头）。" +
			"不需要浏览器，速度快，适合静态页面和 API 请求。",
		Parameters: webFetchParams,
		Permission: PermReadOnly,
		Category:   "web",
		Version:    "1.0.0",
		Tags:       []string{"web", "fetch", "http", "safe"},
		Timeout:    15 * time.Second,
	}, WebFetch)

	registry.MustRegister(ToolMetadata{
		Name: "web_browse",
		Description: "用真实浏览器访问网页，支持文本提取、截图、点击、输入、执行JS、等待元素。" +
			"适合需要 JavaScript 渲染的动态页面、登录交互、表单填写等复杂操作。" +
			"首次使用会自动下载 Chromium。",
		Parameters: webBrowseParams,
		Permission: PermReadOnly,
		Category:   "web",
		Version:    "1.0.0",
		Tags:       []string{"web", "browser", "chromium", "screenshot"},
		Timeout:    30 * time.Second,
	}, WebBrowse)
}

// ============================================================================
// session 管理
// ============================================================================

// browserSession 浏览器会话，支持上下文取消和超时控制
type browserSession struct {
	allocCtx    context.Context
	allocCancel context.CancelFunc
	browserCtx  context.Context
	browCancel  context.CancelFunc
	lastUsed    time.Time
	url         string

	// 会话级别的互斥锁，防止并发操作同一个 session
	mu sync.Mutex
}

var (
	sessions   = make(map[string]*browserSession)
	sessionsMu sync.Mutex
	sessionTTL = 3 * time.Minute // 超过 3 分钟不用自动回收
)

func init() {
	go func() {
		for {
			time.Sleep(30 * time.Second)
			sessionsMu.Lock()
			for id, s := range sessions {
				if time.Since(s.lastUsed) > sessionTTL {
					s.close()
					delete(sessions, id)
				}
			}
			sessionsMu.Unlock()
		}
	}()
}

// close 安全关闭会话的所有资源
func (s *browserSession) close() {
	if s.browCancel != nil {
		s.browCancel()
	}
	if s.allocCancel != nil {
		s.allocCancel()
	}
}

// runWithTimeout 在指定的超时时间内执行 chromedp 操作
// 修复：使用 chromedp 内置的超时机制，避免 goroutine 泄漏
func (s *browserSession) runWithTimeout(timeout time.Duration, actions ...chromedp.Action) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 使用 chromedp 的 RunResponse 配合 context 超时
	// 这样超时后 chromedp 会正确清理内部资源
	ctx, cancel := context.WithTimeout(s.browserCtx, timeout)
	defer cancel()

	// 包装 actions 为任务列表
	tasks := chromedp.Tasks(actions)

	// 直接运行，context 超时后会自动取消
	err := chromedp.Run(ctx, tasks)
	if err != nil {
		// 检查是否是超时错误
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("browser operation timed out after %v", timeout)
		}
		return err
	}
	return nil
}

func getOrCreateSession(sessionID string, chromePath string, parentCtx context.Context) (*browserSession, string, error) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()

	if sessionID != "" {
		if s, ok := sessions[sessionID]; ok {
			s.lastUsed = time.Now()
			return s, sessionID, nil
		}
		return nil, "", fmt.Errorf("session %s 已过期或不存在", sessionID)
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.ExecPath(chromePath),
			chromedp.Flag("headless", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("disable-dev-shm-usage", true),
			chromedp.Flag("disable-extensions", true),
			chromedp.Flag("no-first-run", true),
			chromedp.Flag("no-default-browser-check", true),
			chromedp.Flag("disable-background-networking", true),
			chromedp.WindowSize(1280, 800),
			chromedp.Flag("no-sandbox", runtime.GOOS == "linux"),
		)...,
	)

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)

	if err := chromedp.Run(browserCtx); err != nil {
		cancelBrowser()
		cancelAlloc()
		return nil, "", fmt.Errorf("启动浏览器失败 (path=%s): %w", chromePath, err)
	}

	id := fmt.Sprintf("sess_%d", time.Now().UnixNano())
	s := &browserSession{
		allocCtx:    allocCtx,
		allocCancel: cancelAlloc,
		browserCtx:  browserCtx,
		browCancel:  cancelBrowser,
		lastUsed:    time.Now(),
	}
	sessions[id] = s
	return s, id, nil
}

func closeSession(sessionID string) {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()

	if s, ok := sessions[sessionID]; ok {
		s.close()
		delete(sessions, sessionID)
	}
}
