package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/components/schema"
)

// ============================================================================
// WebFetch
// ============================================================================

func TestWebFetch_Text(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><body><h1>Hello</h1><p>World</p></body></html>`))
	}))
	defer srv.Close()

	result, err := WebFetch(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}

	res := result.(map[string]any)
	assertIntEqual(t, res["status"].(int), 200)

	content := res["content"].(string)
	if !strings.Contains(content, "Hello") || !strings.Contains(content, "World") {
		t.Errorf("缺少预期文本: %q", content)
	}
	if strings.Contains(content, "<h1>") {
		t.Errorf("text 格式不应包含 HTML 标签: %q", content)
	}
}

func TestWebFetch_HTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><p>原始HTML</p></body></html>`))
	}))
	defer srv.Close()

	result, err := WebFetch(context.Background(), map[string]any{
		"url":    srv.URL,
		"format": "html",
	})
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}

	res := result.(map[string]any)
	content := res["content"].(string)
	if !strings.Contains(content, "<p>原始HTML</p>") {
		t.Errorf("html 格式应保留标签: %q", content)
	}
}

func TestWebFetch_Headers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "test-value")
		w.Write([]byte("OK"))
	}))
	defer srv.Close()

	result, err := WebFetch(context.Background(), map[string]any{
		"url":    srv.URL,
		"format": "headers",
	})
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}

	res := result.(map[string]any)
	headers := res["headers"].(map[string]string)
	assertStrEqual(t, headers["X-Custom"], "test-value")
}

func TestWebFetch_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("Not Found"))
	}))
	defer srv.Close()

	result, err := WebFetch(context.Background(), map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}

	res := result.(map[string]any)
	assertIntEqual(t, res["status"].(int), 404)
}

func TestWebFetch_MissingURL(t *testing.T) {
	_, err := WebFetch(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("缺少 URL 应该报错")
	}
}

func TestWebFetch_ConnectionRefused(t *testing.T) {
	_, err := WebFetch(context.Background(), map[string]any{
		"url": "http://127.0.0.1:1",
	})
	if err == nil {
		t.Skip("端口 1 竟然可连")
	}
}

func TestWebFetch_UserAgent(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Write([]byte("OK"))
	}))
	defer srv.Close()

	WebFetch(context.Background(), map[string]any{"url": srv.URL})

	if !strings.Contains(gotUA, "PulseBot") {
		t.Errorf("User-Agent 应包含 PulseBot, 实际: %q", gotUA)
	}
}

func TestWebFetch_LargeBody(t *testing.T) {
	big := strings.Repeat("A", 3*1024*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	}))
	defer srv.Close()

	result, err := WebFetch(context.Background(), map[string]any{
		"url":    srv.URL,
		"format": "html",
	})
	if err != nil {
		t.Fatalf("WebFetch: %v", err)
	}

	res := result.(map[string]any)
	content := res["content"].(string)
	if len(content) > 2*1024*1024+100 {
		t.Errorf("内容应该被截断到 2MB, 实际: %d bytes", len(content))
	}
	t.Logf("截断后大小: %d bytes", len(content))
}

// ============================================================================
// WebBrowse
// ============================================================================

func TestWebBrowse(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过浏览器测试")
	}
	if _, err := defaultMgr.ChromePath(); err != nil {
		t.Skipf("Chrome 不可用: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html>
<html><head><title>Test Page</title></head>
<body>
<h1 id="title">Hello Browser</h1>
<p class="desc">This is a test page.</p>
<input id="search" type="text" placeholder="Search..." />
<button id="btn" onclick="document.getElementById('result').innerText='clicked'">Click Me</button>
<div id="result"></div>
</body></html>`))
	}))
	defer srv.Close()

	// browse 接收子测试的 t，创建的 session 在子测试结束时自动关闭
	browse := func(t *testing.T, action string, extra map[string]any) (map[string]any, error) {
		t.Helper()
		args := map[string]any{
			"url":     srv.URL,
			"action":  action,
			"timeout": float64(20),
		}
		for k, v := range extra {
			args[k] = v
		}
		result, err := WebBrowse(context.Background(), args)
		if err != nil {
			return nil, err
		}
		res := result.(map[string]any)

		// 没传 session_id = 新建的 session，子测试结束时自动关闭
		if _, hasSession := extra["session_id"]; !hasSession {
			if sid, ok := res["session_id"].(string); ok {
				t.Cleanup(func() {
					WebBrowse(context.Background(), map[string]any{
						"action":     "close",
						"session_id": sid,
					})
				})
			}
		}
		return res, nil
	}

	t.Run("text", func(t *testing.T) {
		res, err := browse(t, "text", nil)
		if err != nil {
			t.Fatalf("text: %v", err)
		}
		content, ok := res["content"].(string)
		if !ok || content == "" {
			t.Fatal("应返回非空文本")
		}
		if !strings.Contains(content, "Hello Browser") {
			t.Errorf("缺少预期文本: %q", content)
		}
		t.Logf("页面文本: %s", truncate(content, 200))
	})

	t.Run("screenshot", func(t *testing.T) {
		result, err := WebBrowse(context.Background(), map[string]any{
			"url":     srv.URL,
			"action":  "screenshot",
			"timeout": float64(20),
		})
		if err != nil {
			t.Fatalf("screenshot: %v", err)
		}

		// screenshot 现在返回 *schema.ToolResultContent
		tc, ok := result.(*schema.ToolResultContent)
		if !ok {
			t.Fatalf("expected *schema.ToolResultContent, got %T", result)
		}

		if tc.Content == "" {
			t.Error("应返回非空文本内容")
		}

		if len(tc.ContentParts) != 2 {
			t.Fatalf("expected 2 content parts (text + image), got %d", len(tc.ContentParts))
		}

		if tc.ContentParts[0].Type != schema.ContentTypeText {
			t.Errorf("part 0 type: %s", tc.ContentParts[0].Type)
		}
		if tc.ContentParts[1].Type != schema.ContentTypeImageURL {
			t.Errorf("part 1 type: %s", tc.ContentParts[1].Type)
		}
		if tc.ContentParts[1].ImageURL == nil || tc.ContentParts[1].ImageURL.URL == "" {
			t.Error("image URL should not be empty")
		}

		t.Logf("截图完成: %s, content_parts=%d", tc.Content, len(tc.ContentParts))
	})

	t.Run("screenshot_full_page", func(t *testing.T) {
		result, err := WebBrowse(context.Background(), map[string]any{
			"url":       srv.URL,
			"action":    "screenshot",
			"full_page": true,
			"timeout":   float64(20),
		})
		if err != nil {
			t.Fatalf("full screenshot: %v", err)
		}

		tc, ok := result.(*schema.ToolResultContent)
		if !ok {
			t.Fatalf("expected *schema.ToolResultContent, got %T", result)
		}

		if len(tc.ContentParts) != 2 {
			t.Fatalf("expected 2 content parts, got %d", len(tc.ContentParts))
		}

		t.Logf("全页截图完成: %s", tc.Content)
	})

	t.Run("screenshot_with_output_path", func(t *testing.T) {
		tmpPath := filepath.Join(t.TempDir(), "screenshot.png")
		result, err := WebBrowse(context.Background(), map[string]any{
			"url":         srv.URL,
			"action":      "screenshot",
			"output_path": tmpPath,
			"timeout":     float64(20),
		})
		if err != nil {
			t.Fatalf("screenshot with output_path: %v", err)
		}

		tc, ok := result.(*schema.ToolResultContent)
		if !ok {
			t.Fatalf("expected *schema.ToolResultContent, got %T", result)
		}

		if len(tc.ContentParts) != 2 {
			t.Fatalf("expected 2 content parts, got %d", len(tc.ContentParts))
		}

		// 验证文件已保存
		data, err := os.ReadFile(tmpPath)
		if err != nil {
			t.Fatalf("读取截图文件失败: %v", err)
		}
		if len(data) < 100 {
			t.Error("截图文件太小")
		}

		t.Logf("截图已保存到: %s, 大小: %d bytes", tmpPath, len(data))
	})

	t.Run("click", func(t *testing.T) {
		res, err := browse(t, "click", map[string]any{"selector": "#btn"})
		if err != nil {
			t.Fatalf("click: %v", err)
		}
		assertStrEqual(t, res["status"].(string), "clicked")
		assertStrEqual(t, res["selector"].(string), "#btn")
	})

	t.Run("type", func(t *testing.T) {
		res, err := browse(t, "type", map[string]any{
			"selector": "#search",
			"text":     "hello world",
		})
		if err != nil {
			t.Fatalf("type: %v", err)
		}
		assertStrEqual(t, res["status"].(string), "typed")
		assertStrEqual(t, res["selector"].(string), "#search")
	})

	t.Run("evaluate", func(t *testing.T) {
		res, err := browse(t, "evaluate", map[string]any{
			"expression": "document.title",
		})
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		assertStrEqual(t, res["result"].(string), "Test Page")
	})

	t.Run("evaluate_complex", func(t *testing.T) {
		res, err := browse(t, "evaluate", map[string]any{
			"expression": `document.querySelectorAll("p").length`,
		})
		if err != nil {
			t.Fatalf("evaluate complex: %v", err)
		}
		count, ok := res["result"].(float64)
		if !ok {
			t.Fatalf("result 类型: %T, 值: %v", res["result"], res["result"])
		}
		if count < 1 {
			t.Errorf("p 标签数量 = %v, 期望 >= 1", count)
		}
	})

	t.Run("wait", func(t *testing.T) {
		res, err := browse(t, "wait", map[string]any{"selector": "#title"})
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		assertStrEqual(t, res["status"].(string), "visible")
	})

	t.Run("click_and_read", func(t *testing.T) {
		// 创建 session
		sess1, err := browse(t, "text", nil)
		if err != nil {
			t.Fatalf("创建 session: %v", err)
		}
		sid := sess1["session_id"].(string)

		// 点击（复用 session，传 session_id）
		_, err = browse(t, "click", map[string]any{
			"selector":   "#btn",
			"session_id": sid,
		})
		if err != nil {
			t.Fatalf("click: %v", err)
		}

		// 读取（复用 session）
		sess3, err := browse(t, "evaluate", map[string]any{
			"expression": `document.getElementById("result").innerText`,
			"session_id": sid,
		})
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		assertStrEqual(t, sess3["result"].(string), "clicked")

		// session 会在 click_and_read 子测试结束时自动关闭
	})

	t.Run("screenshot_real_page", func(t *testing.T) {
		if testing.Short() {
			t.Skip("short 模式跳过")
		}

		result, err := WebBrowse(context.Background(), map[string]any{
			"url":       "https://www.baidu.com",
			"action":    "screenshot",
			"full_page": true,
			"timeout":   float64(30),
		})
		if err != nil {
			t.Fatalf("real page screenshot: %v", err)
		}

		tc, ok := result.(*schema.ToolResultContent)
		if !ok {
			t.Fatalf("expected *schema.ToolResultContent, got %T", result)
		}

		if len(tc.ContentParts) != 2 {
			t.Fatalf("expected 2 content parts, got %d", len(tc.ContentParts))
		}

		t.Logf("百度首页截图完成: %s", tc.Content)
	})
	t.Run("screenshot_with_custom_path", func(t *testing.T) {
		tmpDir := t.TempDir()
		customPath := tmpDir + "/custom-screenshot.png"

		result, err := WebBrowse(context.Background(), map[string]any{
			"url":         srv.URL,
			"action":      "screenshot",
			"output_path": customPath,
			"timeout":     float64(20),
		})
		if err != nil {
			t.Fatalf("screenshot with custom path: %v", err)
		}

		tc, ok := result.(*schema.ToolResultContent)
		if !ok {
			t.Fatalf("expected *schema.ToolResultContent, got %T", result)
		}

		if len(tc.ContentParts) != 2 {
			t.Fatalf("expected 2 content parts, got %d", len(tc.ContentParts))
		}

		// 验证文件已保存到自定义路径
		data, err := os.ReadFile(customPath)
		if err != nil {
			t.Fatalf("读取截图文件失败: %v", err)
		}
		if len(data) < 100 {
			t.Error("截图文件太小")
		}

		t.Logf("自定义路径截图: %s, 大小: %d bytes", customPath, len(data))
	})

	t.Run("screenshot_real_page", func(t *testing.T) {
		if testing.Short() {
			t.Skip("short 模式跳过")
		}

		result, err := WebBrowse(context.Background(), map[string]any{
			"url":       "https://www.baidu.com",
			"action":    "screenshot",
			"full_page": true,
			"timeout":   float64(30),
		})
		if err != nil {
			t.Fatalf("real page screenshot: %v", err)
		}

		tc, ok := result.(*schema.ToolResultContent)
		if !ok {
			t.Fatalf("expected *schema.ToolResultContent, got %T", result)
		}

		if len(tc.ContentParts) != 2 {
			t.Fatalf("expected 2 content parts, got %d", len(tc.ContentParts))
		}

		t.Logf("百度首页截图完成: %s", tc.Content)
	})
}

func TestWebBrowse_ArgErrors(t *testing.T) {
	// 不需要 Chrome，因为参数校验在启动浏览器之前
	tests := []struct {
		name string
		args map[string]any
	}{
		{"缺少 url", map[string]any{"action": "text"}},
		{"无效 action", map[string]any{"url": "http://example.com", "action": "nope"}},
		{"click 缺 selector", map[string]any{"url": "http://example.com", "action": "click"}},
		{"type 缺 selector", map[string]any{"url": "http://example.com", "action": "type", "text": "hi"}},
		{"type 缺 text", map[string]any{"url": "http://example.com", "action": "type", "selector": "#x"}},
		{"evaluate 缺 expression", map[string]any{"url": "http://example.com", "action": "evaluate"}},
		{"wait 缺 selector", map[string]any{"url": "http://example.com", "action": "wait"}},
		{"close 缺 session_id", map[string]any{"action": "close"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := WebBrowse(context.Background(), tt.args)
			if err == nil {
				t.Fatal("应该返回参数错误")
			}
			t.Logf("正确报错: %v", err)
		})
	}
}

func TestWebBrowse_Timeout(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过")
	}
	if _, err := defaultMgr.ChromePath(); err != nil {
		t.Skipf("Chrome 不可用: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 等请求取消时自动退出，不会阻塞 srv.Close()
		<-r.Context().Done()
	}))
	defer srv.Close()

	_, err := WebBrowse(context.Background(), map[string]any{
		"url":     srv.URL,
		"action":  "text",
		"timeout": float64(3),
	})
	if err == nil {
		t.Fatal("应该超时报错")
	}
	t.Logf("超时正确触发: %v", err)
}

// ============================================================================
// helpers
// ============================================================================

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
