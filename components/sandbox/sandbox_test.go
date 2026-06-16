package sandbox

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

func newSandbox(t *testing.T) *ProcessSandbox {
	t.Helper()
	return NewProcessSandbox(ProcessConfig{})
}

// ============================================================
// 基础功能
// ============================================================

func TestSandbox_ExecuteEcho(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	shell := "sh"
	args := []string{"-c", "echo hello"}
	if runtime.GOOS == "windows" {
		shell = "cmd"
		args = []string{"/C", "echo hello"}
	}

	result, err := s.Execute(context.Background(), ExecRequest{
		Command: shell,
		Args:    args,
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", result.ExitCode, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "hello") {
		t.Fatalf("expected 'hello' in stdout, got: %s", result.Stdout)
	}
}

func TestSandbox_ExecuteWithFiles(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	shell := "sh"
	catCmd := []string{"-c", "cat data.txt"}
	if runtime.GOOS == "windows" {
		shell = "cmd"
		catCmd = []string{"/C", "type data.txt"}
	}

	result, err := s.Execute(context.Background(), ExecRequest{
		Command: shell,
		Args:    catCmd,
		Files: []InputFile{
			{Path: "data.txt", Content: "file content here"},
		},
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if !strings.Contains(result.Stdout, "file content here") {
		t.Fatalf("expected file content in stdout, got: %s", result.Stdout)
	}
}

func TestSandbox_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("timeout test unreliable on Windows due to cmd.exe signal handling")
	}
	s := newSandbox(t)
	defer s.Close()

	result, err := s.Execute(context.Background(), ExecRequest{
		Command: "sh",
		Args:    []string{"-c", "sleep 10"},
		Timeout: 500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if !result.TimedOut {
		t.Fatal("expected timeout")
	}
}

func TestSandbox_ExitCode(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	shell := "sh"
	args := []string{"-c", "exit 42"}
	if runtime.GOOS == "windows" {
		shell = "cmd"
		args = []string{"/C", "exit /b 42"}
	}

	result, err := s.Execute(context.Background(), ExecRequest{
		Command: shell,
		Args:    args,
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if result.ExitCode != 42 {
		t.Fatalf("expected exit code 42, got %d", result.ExitCode)
	}
}

func TestSandbox_EmptyCommand(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	_, err := s.Execute(context.Background(), ExecRequest{})
	if err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestSandbox_OutputTruncation(t *testing.T) {
	s := NewProcessSandbox(ProcessConfig{MaxOutputBytes: 100})
	defer s.Close()

	shell := "sh"
	args := []string{"-c", "for i in $(seq 1 1000); do echo line_$i; done"}
	if runtime.GOOS == "windows" {
		shell = "cmd"
		args = []string{"/C", "for /L %i in (1,1,1000) do @echo line_%i"}
	}

	result, err := s.Execute(context.Background(), ExecRequest{
		Command: shell,
		Args:    args,
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if !result.Truncated {
		t.Fatal("expected truncation")
	}
}

func TestSandbox_WorkDir(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	shell := "sh"
	args := []string{"-c", "pwd"}
	if runtime.GOOS == "windows" {
		shell = "cmd"
		args = []string{"/C", "cd"}
	}

	result, err := s.Execute(context.Background(), ExecRequest{
		Command: shell,
		Args:    args,
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", result.ExitCode)
	}
	if strings.TrimSpace(result.Stdout) == "" {
		t.Fatal("expected work dir in stdout")
	}
}

func TestSandbox_CustomWorkDir(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	dir := t.TempDir()
	shell := "sh"
	args := []string{"-c", "pwd"}
	if runtime.GOOS == "windows" {
		shell = "cmd"
		args = []string{"/C", "cd"}
	}

	result, err := s.Execute(context.Background(), ExecRequest{
		Command: shell,
		Args:    args,
		WorkDir: dir,
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d", result.ExitCode)
	}
}

// ============================================================
// 环境变量
// ============================================================

func TestSandbox_EnvBlacklist(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	shell := "sh"
	args := []string{"-c", "env"}
	if runtime.GOOS == "windows" {
		shell = "cmd"
		args = []string{"/C", "set"}
	}

	result, err := s.Execute(context.Background(), ExecRequest{
		Command: shell,
		Args:    args,
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if strings.Contains(result.Stdout, "AWS_SECRET") {
		t.Fatal("AWS_SECRET should be blocked")
	}
}

func TestSandbox_EnvPassthrough(t *testing.T) {
	s := NewProcessSandbox(ProcessConfig{EnvMode: EnvModePassthrough})
	defer s.Close()

	shell := "sh"
	args := []string{"-c", "env"}
	if runtime.GOOS == "windows" {
		shell = "cmd"
		args = []string{"/C", "set"}
	}

	result, err := s.Execute(context.Background(), ExecRequest{
		Command: shell,
		Args:    args,
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	// passthrough should have PATH
	if !strings.Contains(result.Stdout, "PATH") {
		t.Fatal("PATH should be present in passthrough mode")
	}
}

func TestSandbox_CustomEnv(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	shell := "sh"
	args := []string{"-c", "echo $MY_VAR"}
	if runtime.GOOS == "windows" {
		shell = "cmd"
		args = []string{"/C", "echo %MY_VAR%"}
	}

	result, err := s.Execute(context.Background(), ExecRequest{
		Command: shell,
		Args:    args,
		Env:     map[string]string{"MY_VAR": "hello_world"},
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if !strings.Contains(result.Stdout, "hello_world") {
		t.Fatalf("expected MY_VAR in output, got: %s", result.Stdout)
	}
}

// ============================================================
// 路径安全
// ============================================================

func TestSandbox_PathEscape(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	_, err := s.Execute(context.Background(), ExecRequest{
		Command: "echo",
		Files: []InputFile{
			{Path: "../../../etc/passwd", Content: "bad"},
		},
	})
	if err == nil {
		t.Fatal("expected error for path escape")
	}
}

func TestSandbox_FileInjection(t *testing.T) {
	s := newSandbox(t)
	defer s.Close()

	shell := "sh"
	args := []string{"-c", "cat subdir/data.txt"}
	if runtime.GOOS == "windows" {
		shell = "cmd"
		args = []string{"/C", "type subdir\\data.txt"}
	}

	result, err := s.Execute(context.Background(), ExecRequest{
		Command: shell,
		Args:    args,
		Files: []InputFile{
			{Path: "subdir/data.txt", Content: "nested content"},
		},
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if !strings.Contains(result.Stdout, "nested content") {
		t.Fatalf("expected nested content, got: %s", result.Stdout)
	}
}

// ============================================================
// FormatResult
// ============================================================

func TestFormatResult(t *testing.T) {
	r := &ExecResult{
		Stdout:   "output\n",
		Stderr:   "warning\n",
		ExitCode: 0,
		Duration: 100 * time.Millisecond,
	}
	out := FormatResult(r)
	if !strings.Contains(out, "Exit Code: 0") {
		t.Fatal("missing exit code")
	}
	if !strings.Contains(out, "output") {
		t.Fatal("missing stdout")
	}
	if !strings.Contains(out, "warning") {
		t.Fatal("missing stderr")
	}
}

func TestFormatResult_Timeout(t *testing.T) {
	r := &ExecResult{
		TimedOut: true,
		Stderr:   "killed",
		Duration: 5 * time.Second,
	}
	out := FormatResult(r)
	if !strings.Contains(out, "TIMEOUT") {
		t.Fatal("missing timeout indicator")
	}
}
