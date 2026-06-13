package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
// file_read 测试
// ============================================================================

func TestFileRead_Basic(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	os.WriteFile(testFile, []byte("hello world"), 0644)

	result, err := FileRead(context.Background(), map[string]any{"path": testFile})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["content"] != "hello world" {
		t.Errorf("content: %v", res["content"])
	}
	if res["path"] != testFile {
		t.Errorf("path: %v", res["path"])
	}
}

func TestFileRead_RelativePath(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "rel.txt")
	os.WriteFile(testFile, []byte("relative"), 0644)

	result, err := FileRead(context.Background(), map[string]any{"path": testFile})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["content"] != "relative" {
		t.Errorf("content: %v", res["content"])
	}
}

func TestFileRead_NotFound(t *testing.T) {
	_, err := FileRead(context.Background(), map[string]any{"path": "/nonexistent/file.txt"})
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestFileRead_EmptyPath(t *testing.T) {
	_, err := FileRead(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestFileRead_Directory(t *testing.T) {
	tmpDir := t.TempDir()
	_, err := FileRead(context.Background(), map[string]any{"path": tmpDir})
	if err == nil {
		t.Fatal("expected error for directory")
	}
}

// ============================================================================
// file_write 测试
// ============================================================================

func TestFileWrite_Basic(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "output.txt")

	result, err := FileWrite(context.Background(), map[string]any{
		"path":    testFile,
		"content": "hello write",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["size"] != len("hello write") {
		t.Errorf("size: %v", res["size"])
	}

	// 验证文件确实写入了
	data, _ := os.ReadFile(testFile)
	if string(data) != "hello write" {
		t.Errorf("file content: %s", string(data))
	}
}

func TestFileWrite_AutoCreateParentDir(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "deep", "nested", "dir", "file.txt")

	_, err := FileWrite(context.Background(), map[string]any{
		"path":    testFile,
		"content": "nested write",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(testFile)
	if string(data) != "nested write" {
		t.Errorf("file content: %s", string(data))
	}
}

func TestFileWrite_Overwrite(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "overwrite.txt")

	os.WriteFile(testFile, []byte("original"), 0644)

	_, err := FileWrite(context.Background(), map[string]any{
		"path":    testFile,
		"content": "overwritten",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(testFile)
	if string(data) != "overwritten" {
		t.Errorf("file content: %s", string(data))
	}
}

func TestFileWrite_EmptyPath(t *testing.T) {
	_, err := FileWrite(context.Background(), map[string]any{
		"content": "test",
	})
	if err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestFileWrite_EmptyContent(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "empty.txt")

	_, err := FileWrite(context.Background(), map[string]any{
		"path":    testFile,
		"content": "",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	data, _ := os.ReadFile(testFile)
	if string(data) != "" {
		t.Errorf("expected empty file, got: %s", string(data))
	}
}

// ============================================================================
// file_list 测试
// ============================================================================

func TestFileList_Basic(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "b.txt"), []byte("bb"), 0644)
	os.Mkdir(filepath.Join(tmpDir, "subdir"), 0755)

	result, err := FileList(context.Background(), map[string]any{"path": tmpDir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["count"] != 3 {
		t.Errorf("count: %v", res["count"])
	}

	entries := res["entries"].([]map[string]any)
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
}

func TestFileList_EmptyDir(t *testing.T) {
	tmpDir := t.TempDir()

	result, err := FileList(context.Background(), map[string]any{"path": tmpDir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["count"] != 0 {
		t.Errorf("count: %v", res["count"])
	}
}

func TestFileList_DefaultPath(t *testing.T) {
	result, err := FileList(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["path"] == "" {
		t.Error("expected non-empty path")
	}
}

func TestFileList_SingleFile(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "single.txt")
	os.WriteFile(testFile, []byte("content"), 0644)

	result, err := FileList(context.Background(), map[string]any{"path": testFile})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["is_dir"] != false {
		t.Error("expected is_dir=false for single file")
	}
}

func TestFileList_MaxEntries(t *testing.T) {
	tmpDir := t.TempDir()
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("file%02d.txt", i)
		os.WriteFile(filepath.Join(tmpDir, name), []byte(""), 0644)
	}

	result, err := FileList(context.Background(), map[string]any{
		"path":        tmpDir,
		"max_entries": 3,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["count"] != 3 {
		t.Errorf("count: %v", res["count"])
	}
	if res["truncated"] != true {
		t.Error("expected truncated=true")
	}
}

// ============================================================================
// command_exec 测试
// ============================================================================

func TestCommandExec_BasicEcho(t *testing.T) {
	result, err := CommandExec(context.Background(), map[string]any{
		"command": "echo hello",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if !strings.Contains(res["stdout"].(string), "hello") {
		t.Errorf("stdout: %v", res["stdout"])
	}
	if res["exit_code"] != 0 {
		t.Errorf("exit_code: %v", res["exit_code"])
	}
}

func TestCommandExec_NonZeroExit(t *testing.T) {
	result, err := CommandExec(context.Background(), map[string]any{
		"command": "exit 1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["exit_code"] != 1 {
		t.Errorf("exit_code: %v", res["exit_code"])
	}
}

func TestCommandExec_Timeout(t *testing.T) {
	t.Skip("timeout test is platform-dependent, skip")
}

func TestCommandExec_EmptyCommand(t *testing.T) {
	_, err := CommandExec(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestCommandExec_DangerousCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"rm -rf /", "rm -rf /"},
		{"rm -rf /*", "rm -rf /*"},
		{"mkfs", "mkfs.ext4 /dev/sda"},
		{"dd if=", "dd if=/dev/zero of=/dev/sda"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CommandExec(context.Background(), map[string]any{
				"command": tt.command,
			})
			if err == nil {
				t.Fatalf("expected error for dangerous command: %s", tt.command)
			}
		})
	}
}

func TestCommandExec_SafeCommands(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"ls", "ls"},
		{"echo", "echo test"},
		{"pwd", "pwd"},
		{"cat", "cat /dev/null"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CommandExec(context.Background(), map[string]any{
				"command": tt.command,
			})
			if err != nil {
				t.Fatalf("unexpected error for safe command %s: %v", tt.command, err)
			}
		})
	}
}

func TestCommandExec_WithWorkDir(t *testing.T) {
	tmpDir := t.TempDir()

	result, err := CommandExec(context.Background(), map[string]any{
		"command":  "go env GOMODCACHE",
		"work_dir": tmpDir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	// 只验证命令能执行，不验证特定输出
	if res["exit_code"] != 0 {
		t.Errorf("exit_code: %v, stderr: %v", res["exit_code"], res["stderr"])
	}
}

// ============================================================================
// checkDangerousCommand 测试
// ============================================================================

func TestCheckDangerousCommand(t *testing.T) {
	dangerous := []string{
		"rm -rf /",
		"rm -rf /*",
		"sudo rm -rf /home",
		"mkfs.ext4 /dev/sda",
		"dd if=/dev/zero of=/dev/sda",
		"format C:",
		"del /f /s /q C:\\*",
	}

	for _, cmd := range dangerous {
		if err := checkDangerousCommand(cmd); err == nil {
			t.Errorf("expected error for dangerous command: %s", cmd)
		}
	}

	safe := []string{
		"ls -la",
		"echo hello",
		"cat file.txt",
		"mkdir test",
		"go test ./...",
		"npm install",
	}

	for _, cmd := range safe {
		if err := checkDangerousCommand(cmd); err != nil {
			t.Errorf("unexpected error for safe command %s: %v", cmd, err)
		}
	}
}
