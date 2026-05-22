package tools

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ChromiumDataDir 下载的 Chromium 存放目录，可在 init 时修改
var ChromiumDataDir = "./data/chromium-browser"

// ChromiumManager 管理 Chrome/Chromium 二进制文件
// 查找优先级：系统已安装 > 已缓存下载 > 自动下载
type ChromiumManager struct {
	dataDir string

	mu      sync.Once
	binPath string
	initErr error
}

// 默认实例
var defaultMgr = NewChromiumManager(ChromiumDataDir)

func NewChromiumManager(dataDir string) *ChromiumManager {
	return &ChromiumManager{dataDir: dataDir}
}

// ChromePath 获取可用的 Chrome/Chromium 可执行文件路径
func (m *ChromiumManager) ChromePath() (string, error) {
	m.mu.Do(func() {
		m.binPath, m.initErr = m.resolve()
	})
	return m.binPath, m.initErr
}

func (m *ChromiumManager) resolve() (string, error) {
	// 1. 系统已安装
	if p := findSystemChrome(); p != "" {
		return p, nil
	}
	// 2. 已缓存的下载
	if p := m.findCached(); p != "" {
		return p, nil
	}
	// 3. 自动下载
	return m.download()
}

// ============================================================================
// 系统 Chrome 检测
// ============================================================================

func findSystemChrome() string {
	var candidates []string

	switch runtime.GOOS {
	case "windows":
		localApp := os.Getenv("LOCALAPPDATA")
		programFiles := os.Getenv("ProgramFiles")
		programFilesX86 := os.Getenv("ProgramFiles(x86)")
		candidates = []string{
			filepath.Join(programFiles, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(programFilesX86, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(localApp, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(programFiles, "Microsoft", "Edge", "Application", "msedge.exe"),
		}
	case "darwin":
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		}
	default: // linux
		candidates = []string{
			"google-chrome",
			"google-chrome-stable",
			"chromium-browser",
			"chromium",
		}
	}

	for _, name := range candidates {
		if filepath.IsAbs(name) {
			if info, err := os.Stat(name); err == nil && !info.IsDir() {
				return name
			}
			continue
		}
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// ============================================================================
// 已缓存下载检测
// ============================================================================

func (m *ChromiumManager) findCached() string {
	var path string
	switch runtime.GOOS {
	case "windows":
		path = filepath.Join(m.dataDir, "chrome-win64", "chrome.exe")
	case "darwin":
		if runtime.GOARCH == "arm64" {
			path = filepath.Join(m.dataDir, "chrome-mac-arm64",
				"Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing")
		} else {
			path = filepath.Join(m.dataDir, "chrome-mac-x64",
				"Google Chrome for Testing.app", "Contents", "MacOS", "Google Chrome for Testing")
		}
	default:
		path = filepath.Join(m.dataDir, "chrome-linux64", "chrome")
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path
	}
	return ""
}

// ============================================================================
// 自动下载
// ============================================================================

type chromeVersionInfo struct {
	Channels struct {
		Stable struct {
			Version   string `json:"version"`
			Downloads struct {
				Chrome []struct {
					Platform string `json:"platform"`
					URL      string `json:"url"`
				} `json:"chrome"`
			} `json:"downloads"`
		} `json:"Stable"`
	} `json:"channels"`
}

func (m *ChromiumManager) download() (string, error) {
	// 1. 获取最新版本信息
	apiURL := "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"
	resp, err := httpGet(apiURL, 15*time.Second)
	if err != nil {
		return "", fmt.Errorf("获取 Chrome 版本信息失败: %w", err)
	}
	defer resp.Body.Close()

	var info chromeVersionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("解析版本信息失败: %w", err)
	}

	// 2. 匹配当前平台的下载链接
	platform := chromePlatform()
	var downloadURL string
	for _, dl := range info.Channels.Stable.Downloads.Chrome {
		if dl.Platform == platform {
			downloadURL = dl.URL
			break
		}
	}
	if downloadURL == "" {
		return "", fmt.Errorf("没有适用于 %s/%s 的 Chrome 下载", runtime.GOOS, runtime.GOARCH)
	}

	// 3. 下载
	if err := os.MkdirAll(m.dataDir, 0755); err != nil {
		return "", fmt.Errorf("创建目录失败: %w", err)
	}

	zipPath := filepath.Join(m.dataDir, "chrome.zip")
	if err := downloadFile(downloadURL, zipPath, 5*time.Minute); err != nil {
		return "", fmt.Errorf("下载 Chrome 失败: %w", err)
	}
	defer os.Remove(zipPath)

	// 4. 解压
	if err := extractZip(zipPath, m.dataDir); err != nil {
		return "", fmt.Errorf("解压 Chrome 失败: %w", err)
	}

	// 5. 设置可执行权限 (Linux/macOS)
	chromePath := m.findCached()
	if chromePath == "" {
		return "", fmt.Errorf("下载完成但未找到 Chrome 二进制文件")
	}
	if runtime.GOOS != "windows" {
		os.Chmod(chromePath, 0755)
	}

	return chromePath, nil
}

func chromePlatform() string {
	os_ := runtime.GOOS
	arch := runtime.GOARCH

	switch os_ {
	case "windows":
		if arch == "arm64" {
			return "win-arm64"
		}
		return "win64"
	case "darwin":
		if arch == "arm64" {
			return "mac-arm64"
		}
		return "mac-x64"
	default: // linux
		if arch == "arm64" {
			return "linux-arm64"
		}
		return "linux64"
	}
}

// ============================================================================
// 工具函数
// ============================================================================

func httpGet(url string, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "PulseBot/1.0")
	return client.Do(req)
}

func downloadFile(url, dest string, timeout time.Duration) error {
	resp, err := httpGet(url, timeout)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// 先写临时文件，完成后再 rename，防止中断留下损坏文件
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	_, err = io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return err
	}

	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func extractZip(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)

	for _, f := range r.File {
		target := filepath.Join(destDir, filepath.FromSlash(f.Name))

		// 防止 zip slip
		if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), cleanDest) &&
			filepath.Clean(target) != cleanDest {
			return fmt.Errorf("非法文件路径: %s", f.Name)
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(target, 0755)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}

		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}

		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
