package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
// file_edit 测试
// ============================================================================

func TestFileEdit_BasicReplace(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "edit.txt")
	os.WriteFile(testFile, []byte("hello world\nfoo bar\nhello again"), 0644)

	result, err := FileEdit(context.Background(), map[string]any{
		"path":       testFile,
		"old_string": "hello world",
		"new_string": "hi world",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["replaced"] != 1 {
		t.Errorf("replaced: %v", res["replaced"])
	}

	data, _ := os.ReadFile(testFile)
	if !strings.Contains(string(data), "hi world") {
		t.Error("expected 'hi world' in file")
	}
	if !strings.Contains(string(data), "hello again") {
		t.Error("expected 'hello again' preserved")
	}
}

func TestFileEdit_ReplaceAll(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "edit_all.txt")
	os.WriteFile(testFile, []byte("aaa bbb aaa ccc aaa"), 0644)

	result, err := FileEdit(context.Background(), map[string]any{
		"path":        testFile,
		"old_string":  "aaa",
		"new_string":  "xxx",
		"replace_all": true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["replaced"] != 3 {
		t.Errorf("replaced: %v", res["replaced"])
	}

	data, _ := os.ReadFile(testFile)
	expected := "xxx bbb xxx ccc xxx"
	if string(data) != expected {
		t.Errorf("content: %q, expected: %q", string(data), expected)
	}
}

func TestFileEdit_ReplaceFirstOnly(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "edit_first.txt")
	os.WriteFile(testFile, []byte("aaa bbb aaa ccc aaa"), 0644)

	result, err := FileEdit(context.Background(), map[string]any{
		"path":       testFile,
		"old_string": "aaa",
		"new_string": "xxx",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["replaced"] != 1 {
		t.Errorf("replaced: %v", res["replaced"])
	}

	data, _ := os.ReadFile(testFile)
	expected := "xxx bbb aaa ccc aaa"
	if string(data) != expected {
		t.Errorf("content: %q, expected: %q", string(data), expected)
	}
}

func TestFileEdit_MultilineReplace(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "edit_multi.txt")
	content := "func main() {\n\t// TODO\n\tfmt.Println(\"hello\")\n}"
	os.WriteFile(testFile, []byte(content), 0644)

	result, err := FileEdit(context.Background(), map[string]any{
		"path":       testFile,
		"old_string": "// TODO\n\tfmt.Println(\"hello\")",
		"new_string": "fmt.Println(\"world\")",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["replaced"] != 1 {
		t.Errorf("replaced: %v", res["replaced"])
	}

	data, _ := os.ReadFile(testFile)
	if strings.Contains(string(data), "TODO") {
		t.Error("TODO should be removed")
	}
	if !strings.Contains(string(data), "world") {
		t.Error("expected 'world'")
	}
}

func TestFileEdit_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "edit.txt")
	os.WriteFile(testFile, []byte("hello world"), 0644)

	_, err := FileEdit(context.Background(), map[string]any{
		"path":       testFile,
		"old_string": "nonexistent text",
		"new_string": "replacement",
	})
	if err == nil {
		t.Fatal("expected error for non-matching old_string")
	}
}

func TestFileEdit_FileNotFound(t *testing.T) {
	_, err := FileEdit(context.Background(), map[string]any{
		"path":       "/nonexistent/file.txt",
		"old_string": "old",
		"new_string": "new",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestFileEdit_EmptyOldString(t *testing.T) {
	_, err := FileEdit(context.Background(), map[string]any{
		"path":       "/tmp/test",
		"old_string": "",
		"new_string": "new",
	})
	if err == nil {
		t.Fatal("expected error for empty old_string")
	}
}

// ============================================================================
// file_search 文件名匹配测试
// ============================================================================

func TestFileSearch_FilesGlob(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "a.go"), []byte("package a"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "b.go"), []byte("package b"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "c.txt"), []byte("text"), 0644)

	result, err := FileSearch(context.Background(), map[string]any{
		"pattern": "*.go",
		"path":    tmpDir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["count"] != 2 {
		t.Errorf("count: %v", res["count"])
	}
}

func TestFileSearch_FilesGlob_Recursive(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "sub")
	os.Mkdir(subDir, 0755)
	os.WriteFile(filepath.Join(tmpDir, "root.go"), []byte("package root"), 0644)
	os.WriteFile(filepath.Join(subDir, "sub.go"), []byte("package sub"), 0644)

	result, err := FileSearch(context.Background(), map[string]any{
		"pattern": "*.go",
		"path":    tmpDir,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["count"].(int) < 2 {
		t.Errorf("count: %v (expected >= 2)", res["count"])
	}
}

func TestFileSearch_ContentSearch(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "a.go"), []byte("func Hello() {\n\tfmt.Println(\"hello\")\n}"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "b.go"), []byte("func World() {\n\tfmt.Println(\"world\")\n}"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "c.txt"), []byte("no match here"), 0644)

	result, err := FileSearch(context.Background(), map[string]any{
		"pattern": "Println",
		"path":    tmpDir,
		"mode":    "content",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["count"] != 2 {
		t.Errorf("count: %v (expected 2)", res["count"])
	}

	results := res["results"].([]map[string]any)
	for _, r := range results {
		if !strings.Contains(r["content"].(string), "Println") {
			t.Errorf("expected Println in match: %v", r["content"])
		}
	}
}

func TestFileSearch_ContentSearch_SkipsBinary(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "code.go"), []byte("func test() {}"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "image.png"), []byte("\x89PNG\x0d\x0a\x1a\x0a"), 0644)

	result, err := FileSearch(context.Background(), map[string]any{
		"pattern": "test",
		"path":    tmpDir,
		"mode":    "content",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["count"] != 1 {
		t.Errorf("count: %v (expected 1, binary should be skipped)", res["count"])
	}
}

func TestFileSearch_ContentSearch_SkipsHiddenDirs(t *testing.T) {
	tmpDir := t.TempDir()
	hiddenDir := filepath.Join(tmpDir, ".hidden")
	os.Mkdir(hiddenDir, 0755)
	os.WriteFile(filepath.Join(hiddenDir, "secret.go"), []byte("secret content"), 0644)
	os.WriteFile(filepath.Join(tmpDir, "visible.go"), []byte("visible content"), 0644)

	result, err := FileSearch(context.Background(), map[string]any{
		"pattern": "content",
		"path":    tmpDir,
		"mode":    "content",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["count"] != 1 {
		t.Errorf("count: %v (expected 1, hidden dir should be skipped)", res["count"])
	}
}

func TestFileSearch_ContentSearch_LineNumber(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "test.go"), []byte("line1\nline2\nTARGET\nline4"), 0644)

	result, err := FileSearch(context.Background(), map[string]any{
		"pattern": "TARGET",
		"path":    tmpDir,
		"mode":    "content",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	results := res["results"].([]map[string]any)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0]["line"] != 3 {
		t.Errorf("line: %v (expected 3)", results[0]["line"])
	}
}

func TestFileSearch_MaxResults(t *testing.T) {
	tmpDir := t.TempDir()
	for i := 0; i < 10; i++ {
		os.WriteFile(filepath.Join(tmpDir, "match.go"), []byte("keyword"), 0644)
	}
	// 用不同文件名确保有多个匹配
	os.Mkdir(filepath.Join(tmpDir, "sub"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "sub", "match.go"), []byte("keyword"), 0644)

	result, err := FileSearch(context.Background(), map[string]any{
		"pattern":     "keyword",
		"path":        tmpDir,
		"mode":        "content",
		"max_results": 3,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["count"].(int) > 3 {
		t.Errorf("count: %v (expected <= 3)", res["count"])
	}
}

func TestFileSearch_NoResults(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "test.go"), []byte("hello"), 0644)

	result, err := FileSearch(context.Background(), map[string]any{
		"pattern": "nonexistent_xyz",
		"path":    tmpDir,
		"mode":    "content",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := result.(map[string]any)
	if res["count"] != 0 {
		t.Errorf("count: %v (expected 0)", res["count"])
	}
}
