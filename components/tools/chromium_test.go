package tools

import (
	"archive/zip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// ============================================================================
// chromePlatform
// ============================================================================

func TestChromePlatform(t *testing.T) {
	p := chromePlatform()
	if p == "" {
		t.Fatal("chromePlatform 返回了空字符串")
	}

	switch runtime.GOOS {
	case "windows":
		switch runtime.GOARCH {
		case "arm64":
			assertStrEqual(t, p, "win-arm64")
		default:
			assertStrEqual(t, p, "win64")
		}
	case "darwin":
		switch runtime.GOARCH {
		case "arm64":
			assertStrEqual(t, p, "mac-arm64")
		default:
			assertStrEqual(t, p, "mac-x64")
		}
	case "linux":
		switch runtime.GOARCH {
		case "arm64":
			assertStrEqual(t, p, "linux-arm64")
		default:
			assertStrEqual(t, p, "linux64")
		}
	}

	t.Logf("platform: %s (os=%s arch=%s)", p, runtime.GOOS, runtime.GOARCH)
}

// ============================================================================
// findSystemChrome
// ============================================================================

func TestFindSystemChrome(t *testing.T) {
	p := findSystemChrome()
	t.Logf("system chrome: %q", p)
	// 不管找不找得到，都不能 panic
}

// ============================================================================
// extractZip
// ============================================================================

func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "test.zip")

	createTestZip(t, zipPath, map[string]string{
		"hello.txt":      "hello world",
		"sub/nested.txt": "nested content",
	})

	dest := filepath.Join(dir, "out")
	if err := extractZip(zipPath, dest); err != nil {
		t.Fatalf("extractZip: %v", err)
	}

	assertFileContent(t, filepath.Join(dest, "hello.txt"), "hello world")
	assertFileContent(t, filepath.Join(dest, "sub", "nested.txt"), "nested content")
}

func TestExtractZipEmpty(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "empty.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	w.Close()
	f.Close()

	dest := filepath.Join(dir, "out")
	if err := extractZip(zipPath, dest); err != nil {
		t.Fatalf("extractZip on empty zip: %v", err)
	}
}

func TestExtractZipSlip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "evil.zip")

	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	_, _ = w.Create("../../evil.txt") // 路径穿越
	w.Close()
	f.Close()

	dest := filepath.Join(dir, "out")
	err = extractZip(zipPath, dest)
	if err == nil {
		t.Fatal("zip slip 应该被拦截，但没报错")
	}
	t.Logf("正确拦截 zip slip: %v", err)
}

// ============================================================================
// findCached
// ============================================================================

func TestFindCached_NotFound(t *testing.T) {
	dir := t.TempDir()
	mgr := NewChromiumManager(dir)

	if p := mgr.findCached(); p != "" {
		t.Errorf("期望空路径，实际得到 %q", p)
	}
}

func TestFindCached_Found(t *testing.T) {
	dir := t.TempDir()
	mgr := NewChromiumManager(dir)

	chromePath := expectedCachedPath(dir)
	if err := os.MkdirAll(filepath.Dir(chromePath), 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(chromePath, []byte("fake"), 0755)

	got := mgr.findCached()
	if got != chromePath {
		t.Errorf("findCached() = %q, 期望 %q", got, chromePath)
	}
}

// ============================================================================
// downloadFile
// ============================================================================

func TestDownloadFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fake binary content"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "download.zip")

	if err := downloadFile(srv.URL, dest, 10*time.Second); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}

	assertFileContent(t, dest, "fake binary content")

	// 临时文件应该被清理
	if _, err := os.Stat(dest + ".tmp"); !os.IsNotExist(err) {
		t.Error("临时文件应该已被清理")
	}
}

func TestDownloadFile_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "download.zip")

	err := downloadFile(srv.URL, dest, 10*time.Second)
	if err == nil {
		t.Fatal("404 应该返回错误")
	}
}

func TestDownloadFile_ConnectionRefused(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "download.zip")

	// 端口 1 几乎不可能有服务在监听
	err := downloadFile("http://127.0.0.1:1", dest, 2*time.Second)
	if err == nil {
		t.Skip("端口 1 竟然可连，跳过")
	}
}

// ============================================================================
// httpGet
// ============================================================================

func TestHTTPGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "hello")
		w.Write([]byte("OK"))
	}))
	defer srv.Close()

	resp, err := httpGet(srv.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("httpGet: %v", err)
	}
	defer resp.Body.Close()

	assertIntEqual(t, resp.StatusCode, 200)
	assertStrEqual(t, resp.Header.Get("X-Test"), "hello")
}

// ============================================================================
// ChromiumManager.resolve (集成测试，可能下载 Chromium)
// ============================================================================

func TestChromiumManagerResolve(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过集成测试")
	}

	dir := t.TempDir()
	mgr := NewChromiumManager(dir)

	path, err := mgr.resolve()
	if err != nil {
		t.Skipf("resolve 失败 (无系统 Chrome, 下载可能也失败): %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.IsDir() {
		t.Fatalf("解析结果是个目录: %s", path)
	}

	t.Logf("resolved chrome: %s", path)
}

func TestChromiumManagerPathConsistency(t *testing.T) {
	dir := t.TempDir()
	mgr := NewChromiumManager(dir)

	// 多次调用应该返回相同结果（sync.Once）
	p1, err1 := mgr.ChromePath()
	p2, err2 := mgr.ChromePath()

	if err1 != nil {
		t.Skipf("Chrome 不可用: %v", err1)
	}

	assertStrEqual(t, p1, p2)
	if err2 != nil {
		t.Errorf("第二次调用不应返回错误: %v", err2)
	}
}

// ============================================================================
// helpers
// ============================================================================

func expectedCachedPath(dir string) string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(dir, "chrome-win64", "chrome.exe")
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return filepath.Join(dir, "chrome-mac-arm64",
				"Google Chrome for Testing.app", "Contents", "MacOS",
				"Google Chrome for Testing")
		}
		return filepath.Join(dir, "chrome-mac-x64",
			"Google Chrome for Testing.app", "Contents", "MacOS",
			"Google Chrome for Testing")
	default:
		return filepath.Join(dir, "chrome-linux64", "chrome")
	}
}

func createTestZip(t *testing.T, zipPath string, files map[string]string) {
	t.Helper()
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := zip.NewWriter(f)
	defer w.Close()

	for name, content := range files {
		fw, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		fw.Write([]byte(content))
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	if string(data) != want {
		t.Errorf("%s 内容 = %q, 期望 %q", path, string(data), want)
	}
}

func assertStrEqual(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func assertIntEqual(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("got %d, want %d", got, want)
	}
}
