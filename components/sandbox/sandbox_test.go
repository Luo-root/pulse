package sandbox

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ============================================================
// 辅助函数
// ============================================================

func newSandbox(t *testing.T) *ProcessSandbox {
	t.Helper()
	return NewProcessSandbox(ProcessConfig{})
}

// ============================================================
// 基础功能
// ============================================================

func TestSandbox_ListLangs(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	langs := s.ListLangs()
	t.Logf("available langs: %v", langs)

	if len(langs) == 0 {
		t.Fatal("expected at least 1 language")
	}

	mustHave := []string{"python", "node", "go", "shell"}
	for _, want := range mustHave {
		found := false
		for _, l := range langs {
			if l == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing language: %s", want)
		}
	}
}

func TestSandbox_CheckLang(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	// Go 应该可用
	if err := s.CheckLang("go"); err != nil {
		t.Errorf("go should be available: %v", err)
	}

	// 不存在的语言
	if err := s.CheckLang("fortran"); err == nil {
		t.Error("expected error for unsupported language")
	}
}

// ============================================================
// Go 执行（保证可用）
// ============================================================

func TestSandbox_ExecuteGo(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	if err := s.CheckLang("go"); err != nil {
		t.Skip("go not available:", err)
	}

	code := `package main

import "fmt"

func main() {
	fmt.Println("hello from go")
}
`
	result, err := s.Execute(context.Background(), ExecRequest{
		Language: "go",
		Code:     code,
		Timeout:  30 * time.Second,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	t.Logf("stdout: %q", result.Stdout)
	t.Logf("stderr: %q", result.Stderr)
	t.Logf("exit: %d, duration: %s", result.ExitCode, result.Duration)

	if result.ExitCode != 0 {
		t.Fatalf("non-zero exit: %d, stderr: %s", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "hello from go") {
		t.Errorf("unexpected output: %s", result.Stdout)
	}
}

// ============================================================
// Python 执行
// ============================================================

func TestSandbox_ExecutePython(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	if err := s.CheckLang("python"); err != nil {
		t.Skip("python not available:", err)
	}

	result, err := s.Execute(context.Background(), ExecRequest{
		Language: "python",
		Code:     "print('hello from python')",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	t.Logf("stdout: %q", result.Stdout)

	if result.ExitCode != 0 {
		t.Fatalf("non-zero exit: %d, stderr: %s", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "hello from python") {
		t.Errorf("unexpected output: %s", result.Stdout)
	}
}

// ============================================================
// Node 执行
// ============================================================

func TestSandbox_ExecuteNode(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	if err := s.CheckLang("node"); err != nil {
		t.Skip("node not available:", err)
	}

	result, err := s.Execute(context.Background(), ExecRequest{
		Language: "node",
		Code:     "console.log('hello from node')",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	t.Logf("stdout: %q", result.Stdout)

	if result.ExitCode != 0 {
		t.Fatalf("non-zero exit: %d, stderr: %s", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "hello from node") {
		t.Errorf("unexpected output: %s", result.Stdout)
	}
}

// ============================================================
// Shell 执行
// ============================================================

func TestSandbox_ExecuteShell(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	if err := s.CheckLang("shell"); err != nil {
		t.Skip("shell not available:", err)
	}

	var code string
	if runtime.GOOS == "windows" {
		code = "echo hello from shell"
	} else {
		code = "echo hello from shell"
	}

	result, err := s.Execute(context.Background(), ExecRequest{
		Language: "shell",
		Code:     code,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	t.Logf("stdout: %q", result.Stdout)

	if result.ExitCode != 0 {
		t.Fatalf("non-zero exit: %d, stderr: %s", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "hello from shell") {
		t.Errorf("unexpected output: %s", result.Stdout)
	}
}

// ============================================================
// 输入文件
// ============================================================

func TestSandbox_InputFiles(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	if err := s.CheckLang("go"); err != nil {
		t.Skip("go not available:", err)
	}

	code := `package main

import (
	"fmt"
	"os"
)

func main() {
	data, err := os.ReadFile("data.txt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "read error: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(string(data))
}
`
	result, err := s.Execute(context.Background(), ExecRequest{
		Language: "go",
		Code:     code,
		Timeout:  30 * time.Second,
		Files: []InputFile{
			{Path: "data.txt", Content: "hello from file"},
		},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	t.Logf("stdout: %q", result.Stdout)

	if result.ExitCode != 0 {
		t.Fatalf("exit code %d, stderr: %s", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "hello from file") {
		t.Errorf("expected file content, got: %s", result.Stdout)
	}
}

func TestSandbox_InputFilesNested(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	if err := s.CheckLang("go"); err != nil {
		t.Skip("go not available:", err)
	}

	code := `package main

import (
	"fmt"
	"os"
)

func main() {
	data, err := os.ReadFile("configs/app.json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(string(data))
}
`
	result, err := s.Execute(context.Background(), ExecRequest{
		Language: "go",
		Code:     code,
		Timeout:  30 * time.Second,
		Files: []InputFile{
			{Path: "configs/app.json", Content: `{"debug": true}`},
		},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.ExitCode != 0 {
		t.Fatalf("exit %d: %s", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "debug") {
		t.Errorf("expected nested file content, got: %s", result.Stdout)
	}
}

// ============================================================
// 超时处理
// ============================================================

func TestSandbox_Timeout(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	if err := s.CheckLang("go"); err != nil {
		t.Skip("go not available:", err)
	}

	// 死循环代码
	code := `package main

import "time"

func main() {
	time.Sleep(60 * time.Second)
}
`
	result, err := s.Execute(context.Background(), ExecRequest{
		Language: "go",
		Code:     code,
		Timeout:  2 * time.Second,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	t.Logf("timed_out: %v, exit: %d, duration: %s", result.TimedOut, result.ExitCode, result.Duration)

	if !result.TimedOut {
		t.Error("expected timeout, but got none")
	}
	if result.ExitCode != -1 {
		t.Errorf("expected exit code -1 for timeout, got %d", result.ExitCode)
	}
}

// ============================================================
// 错误处理
// ============================================================

func TestSandbox_NonZeroExit(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	if err := s.CheckLang("python"); err != nil {
		t.Skip("python not available:", err)
	}

	result, err := s.Execute(context.Background(), ExecRequest{
		Language: "python",
		Code:     "import sys; sys.exit(42)",
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	t.Logf("exit: %d", result.ExitCode)

	if result.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %d", result.ExitCode)
	}
}

func TestSandbox_EmptyCode(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	_, err := s.Execute(context.Background(), ExecRequest{
		Language: "go",
		Code:     "",
	})
	if err == nil {
		t.Error("expected error for empty code")
	}
	t.Logf("expected error: %v", err)
}

func TestSandbox_UnsupportedLang(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	_, err := s.Execute(context.Background(), ExecRequest{
		Language: "rust",
		Code:     "fn main() {}",
	})
	if err == nil {
		t.Error("expected error for unsupported language")
	}
	t.Logf("expected error: %v", err)
}

// ============================================================
// 输出截断
// ============================================================

func TestSandbox_OutputTruncation(t *testing.T) {
	s := NewProcessSandbox(ProcessConfig{
		MaxOutputBytes: 100, // 故意设很小
	})
	defer s.Close()

	if err := s.CheckLang("go"); err != nil {
		t.Skip("go not available:", err)
	}

	code := `package main

import (
	"fmt"
	"strings"
)

func main() {
	fmt.Println(strings.Repeat("A", 1000))
}
`
	result, err := s.Execute(context.Background(), ExecRequest{
		Language: "go",
		Code:     code,
		Timeout:  10 * time.Second,
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	t.Logf("stdout len: %d, truncated: %v", len(result.Stdout), result.Truncated)

	if !result.Truncated {
		t.Error("expected output to be truncated")
	}
	if len(result.Stdout) > 100 {
		t.Errorf("stdout too long: %d bytes", len(result.Stdout))
	}
}

// ============================================================
// 环境变量
// ============================================================

func TestSandbox_EnvVars(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	if err := s.CheckLang("go"); err != nil {
		t.Skip("go not available:", err)
	}

	code := `package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Print(os.Getenv("PULSE_TEST_VAR"))
}
`
	result, err := s.Execute(context.Background(), ExecRequest{
		Language: "go",
		Code:     code,
		Timeout:  10 * time.Second,
		Env:      map[string]string{"PULSE_TEST_VAR": "hello_env"},
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if result.ExitCode != 0 {
		t.Fatalf("exit %d: %s", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "hello_env") {
		t.Errorf("expected env var value, got: %s", result.Stdout)
	}
}

// ============================================================
// 默认超时配置
// ============================================================

func TestSandbox_DefaultTimeout(t *testing.T) {
	s := NewProcessSandbox(ProcessConfig{
		DefaultTimeout: 5 * time.Second,
	})
	defer s.Close()

	if err := s.CheckLang("go"); err != nil {
		t.Skip("go not available:", err)
	}

	code := `package main

import "time"

func main() {
	time.Sleep(60 * time.Second)
}
`
	start := time.Now()
	result, err := s.Execute(context.Background(), ExecRequest{
		Language: "go",
		Code:     code,
		// 不设 Timeout，应该使用默认 5s
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	t.Logf("timed_out: %v, elapsed: %s", result.TimedOut, elapsed)

	if !result.TimedOut {
		t.Error("expected timeout")
	}
	// 应该在 5s 左右返回，不会等到 60s
	if elapsed > 10*time.Second {
		t.Errorf("took too long: %s, default timeout may not be applied", elapsed)
	}
}

// ============================================================
// 动态添加语言
// ============================================================

func TestSandbox_AddLang(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	// 查找可用的解释器
	interpreter := ""
	for _, name := range []string{"bash", "sh"} {
		if err := s.CheckLang("shell"); err == nil {
			interpreter = name
			break
		}
	}
	if interpreter == "" {
		t.Skip("no shell interpreter found")
	}

	s.AddLang("echo", LangConfig{
		Command: interpreter,
		Args:    []string{"-c"},
	})

	if err := s.CheckLang("echo"); err != nil {
		t.Fatalf("echo should be available: %v", err)
	}
}

// ============================================================
// FormatResult 测试
// ============================================================

func TestFormatResult(t *testing.T) {
	tests := []struct {
		name   string
		result *ExecResult
		check  func(string) bool
	}{
		{
			name: "success",
			result: &ExecResult{
				Stdout:   "hello\n",
				ExitCode: 0,
				Duration: 100 * time.Millisecond,
			},
			check: func(s string) bool {
				return strings.Contains(s, "Exit Code: 0") && strings.Contains(s, "STDOUT")
			},
		},
		{
			name: "error",
			result: &ExecResult{
				Stderr:   "panic!\n",
				ExitCode: 1,
				Duration: 50 * time.Millisecond,
			},
			check: func(s string) bool {
				return strings.Contains(s, "Exit Code: 1") && strings.Contains(s, "STDERR")
			},
		},
		{
			name: "timeout",
			result: &ExecResult{
				TimedOut: true,
				ExitCode: -1,
				Duration: 30 * time.Second,
			},
			check: func(s string) bool {
				return strings.Contains(s, "TIMEOUT")
			},
		},
		{
			name: "truncated",
			result: &ExecResult{
				Stdout:    "partial...",
				Truncated: true,
				ExitCode:  0,
				Duration:  1 * time.Second,
			},
			check: func(s string) bool {
				return strings.Contains(s, "truncated")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output := FormatResult(tt.result)
			t.Logf("formatted:\n%s", output)
			if !tt.check(output) {
				t.Errorf("format check failed for %s", tt.name)
			}
		})
	}
}
