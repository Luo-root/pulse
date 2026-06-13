package tools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// RegisterAll 一键注册所有内置工具
// 自动检测环境，跳过不可用的工具
func RegisterAll(registry *ToolRegistry) []string {
	var registered []string

	// 文件工具（始终可用）
	RegisterFileTools(registry)
	registered = append(registered, "file_read", "file_write", "file_list")

	// 文件编辑和搜索工具（始终可用）
	RegisterFileEditTools(registry)
	registered = append(registered, "file_edit", "file_search")

	// 命令执行工具（始终可用）
	RegisterCommandTools(registry)
	registered = append(registered, "command_exec")

	// 环境工具（始终可用）
	RegisterEnvTools(registry)
	registered = append(registered, "get_work_dir")

	// 用户配置工具（始终可用）
	RegisterUserConfigTools(registry)
	registered = append(registered, "user_config")

	// Web 工具
	RegisterWebTools(registry)
	registered = append(registered, "web_fetch")

	// 浏览器工具 — 检查 Chrome 是否可用
	if chromeAvailable() {
		registered = append(registered, "web_browse")
	} else {
		// 浏览器不可用，注销 web_browse
		registry.Unregister("web_browse")
		fmt.Println("[tools] web_browse: Chrome not found, skipped (will auto-download on first use if registered)")
	}

	return registered
}

// RegisterAllWithBrowser 强制注册所有工具（包括浏览器工具）
// 即使 Chrome 不可用也注册，首次使用时会自动下载
func RegisterAllWithBrowser(registry *ToolRegistry) []string {
	var registered []string

	RegisterFileTools(registry)
	registered = append(registered, "file_read", "file_write", "file_list")

	RegisterFileEditTools(registry)
	registered = append(registered, "file_edit", "file_search")

	RegisterCommandTools(registry)
	registered = append(registered, "command_exec")

	RegisterEnvTools(registry)
	registered = append(registered, "get_work_dir")

	RegisterUserConfigTools(registry)
	registered = append(registered, "user_config")

	RegisterWebTools(registry)
	registered = append(registered, "web_fetch", "web_browse")

	return registered
}

// chromeAvailable 检查系统是否有可用的 Chrome/Chromium
func chromeAvailable() bool {
	candidates := chromeCandidates()
	for _, name := range candidates {
		if filepath.IsAbs(name) {
			if _, err := os.Stat(name); err == nil {
				return true
			}
		} else {
			if _, err := execLookPath(name); err == nil {
				return true
			}
		}
	}
	return false
}

func chromeCandidates() []string {
	switch runtime.GOOS {
	case "windows":
		localApp := os.Getenv("LOCALAPPDATA")
		programFiles := os.Getenv("ProgramFiles")
		programFilesX86 := os.Getenv("ProgramFiles(x86)")
		return []string{
			filepath.Join(programFiles, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(programFilesX86, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(localApp, "Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join(programFiles, "Microsoft", "Edge", "Application", "msedge.exe"),
		}
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		}
	default:
		return []string{
			"google-chrome",
			"google-chrome-stable",
			"chromium-browser",
			"chromium",
		}
	}
}

// execLookPath 封装 exec.LookPath 便于测试
var execLookPath = defaultLookPath

func defaultLookPath(name string) (string, error) {
	return exec.LookPath(name)
}
