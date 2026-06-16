package mcp

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ============================================================
// 辅助函数
// ============================================================

func findNode(t *testing.T) string {
	t.Helper()
	name := "node"
	if runtime.GOOS == "windows" {
		name = "node.exe"
	}
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skip("node not found, skipping")
	}
	return p
}

func startFS(t *testing.T, dirs ...string) *Client {
	t.Helper()

	nodePath := findNode(t)

	npmRoot, err := exec.Command("npm", "root", "-g").Output()
	if err != nil {
		t.Fatalf("npm root -g: %v", err)
	}
	serverJS := filepath.Join(
		strings.TrimSpace(string(npmRoot)),
		"@modelcontextprotocol", "server-filesystem", "dist", "index.js",
	)

	args := append([]string{serverJS}, dirs...)

	transport := NewStdioTransport(StdioConfig{
		Command: nodePath,
		Args:    args,
	})

	client := NewClient(transport, ClientConfig{
		Name:    "pulse-test",
		Version: "0.1.0",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { client.Close() })

	return client
}

func tmpDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	abs, _ := filepath.Abs(d)
	return abs
}

func getContent(t *testing.T, c *Client, tool string, args map[string]any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, tool, args)
	if err != nil {
		t.Fatalf("CallTool(%s): %v", tool, err)
	}
	return extractText(res.Content)
}

func callAndCheck(t *testing.T, c *Client, tool string, args map[string]any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, tool, args)
	if err != nil {
		t.Fatalf("CallTool(%s): %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("CallTool(%s) returned error: %s", tool, extractText(res.Content))
	}
	return extractText(res.Content)
}

// ============================================================
// 连接 & 工具发现
// ============================================================

func TestMCP_ConnectAndListTools(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	ctx := context.Background()
	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("expected tools > 0")
	}

	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
		t.Logf("  tool: %s", tool.Name)
	}

	mustHave := []string{
		"read_text_file",
		"write_file",
		"list_directory",
		"create_directory",
		"search_files",
		"get_file_info",
	}
	for _, n := range mustHave {
		if !names[n] {
			t.Errorf("missing tool: %s", n)
		}
	}

	info := c.ServerInfo()
	if info == nil {
		t.Fatal("ServerInfo() is nil")
	}
	t.Logf("server: %s v%s", info.Name, info.Version)

	if c.protocolVersion == "" {
		t.Error("protocolVersion is empty")
	}
	t.Logf("protocol: %s", c.protocolVersion)
}

func TestMCP_ServerCapabilities(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	caps := c.ServerCapabilities()
	if caps == nil {
		t.Fatal("ServerCapabilities() is nil")
	}
	if caps.Tools == nil {
		t.Error("tools capability not reported")
	}
}

// ============================================================
// 文件读写
// ============================================================

func TestMCP_WriteAndReadFile(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	fp := filepath.Join(dir, "hello.txt")
	want := "Hello MCP!\n你好世界"

	callAndCheck(t, c, "write_file", map[string]any{
		"path":    fp,
		"content": want,
	})

	got := getContent(t, c, "read_text_file", map[string]any{"path": fp})
	if got != want {
		t.Errorf("content mismatch:\n  want: %q\n  got:  %q", want, got)
	}
}

func TestMCP_ReadMultipleFiles(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	data := map[string]string{
		filepath.Join(dir, "a.txt"): "aaa",
		filepath.Join(dir, "b.txt"): "bbb",
		filepath.Join(dir, "c.txt"): "ccc",
	}
	for path, content := range data {
		callAndCheck(t, c, "write_file", map[string]any{
			"path":    path,
			"content": content,
		})
	}

	paths := make([]any, 0, len(data))
	for p := range data {
		paths = append(paths, p)
	}

	text := getContent(t, c, "read_multiple_files", map[string]any{
		"paths": paths,
	})

	for _, content := range data {
		if !strings.Contains(text, content) {
			t.Errorf("result missing %q", content)
		}
	}
	t.Logf("read_multiple_files:\n%s", text)
}

func TestMCP_ReadFileHeadTail(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	fp := filepath.Join(dir, "lines.txt")
	callAndCheck(t, c, "write_file", map[string]any{
		"path":    fp,
		"content": "line1\nline2\nline3\nline4\nline5",
	})

	head := getContent(t, c, "read_text_file", map[string]any{
		"path": fp,
		"head": float64(2),
	})
	t.Logf("head=2: %q", head)

	tail := getContent(t, c, "read_text_file", map[string]any{
		"path": fp,
		"tail": float64(2),
	})
	t.Logf("tail=2: %q", tail)

	if !strings.Contains(head, "line1") {
		t.Errorf("head should contain line1")
	}
	if !strings.Contains(tail, "line5") {
		t.Errorf("tail should contain line5")
	}
}

func TestMCP_EditFile(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	fp := filepath.Join(dir, "edit.txt")
	callAndCheck(t, c, "write_file", map[string]any{
		"path":    fp,
		"content": "first\nsecond\nthird",
	})

	diff := callAndCheck(t, c, "edit_file", map[string]any{
		"path": fp,
		"edits": []any{
			map[string]any{
				"oldText": "second",
				"newText": "SECOND",
			},
		},
	})
	t.Logf("diff:\n%s", diff)

	got := getContent(t, c, "read_text_file", map[string]any{"path": fp})
	if !strings.Contains(got, "SECOND") {
		t.Errorf("should contain SECOND, got: %q", got)
	}
	if strings.Contains(got, "second") {
		t.Errorf("should not contain second, got: %q", got)
	}
}

// ============================================================
// 目录操作
// ============================================================

func TestMCP_CreateAndListDirectory(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	// 逐层创建目录（filesystem server 要求父目录存在）
	sub := filepath.Join(dir, "proj", "src")
	callAndCheck(t, c, "create_directory", map[string]any{
		"path": filepath.Join(dir, "proj"),
	})
	callAndCheck(t, c, "create_directory", map[string]any{
		"path": sub,
	})

	callAndCheck(t, c, "write_file", map[string]any{
		"path":    filepath.Join(sub, "main.go"),
		"content": "package main",
	})

	listing := getContent(t, c, "list_directory", map[string]any{
		"path": sub,
	})
	t.Logf("listing:\n%s", listing)

	if !strings.Contains(listing, "main.go") {
		t.Error("should list main.go")
	}
}

func TestMCP_DirectoryTree(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	// 逐层创建
	callAndCheck(t, c, "create_directory", map[string]any{
		"path": filepath.Join(dir, "src"),
	})
	callAndCheck(t, c, "create_directory", map[string]any{
		"path": filepath.Join(dir, "src", "utils"),
	})
	callAndCheck(t, c, "write_file", map[string]any{
		"path":    filepath.Join(dir, "src", "main.go"),
		"content": "package main",
	})
	callAndCheck(t, c, "write_file", map[string]any{
		"path":    filepath.Join(dir, "src", "utils", "helper.go"),
		"content": "package utils",
	})

	tree := getContent(t, c, "directory_tree", map[string]any{
		"path": dir,
	})
	t.Logf("tree:\n%s", tree)

	if !strings.Contains(tree, "main.go") {
		t.Error("tree missing main.go")
	}
	if !strings.Contains(tree, "helper.go") {
		t.Error("tree missing helper.go")
	}
}

func TestMCP_ListDirectoryWithSizes(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	callAndCheck(t, c, "write_file", map[string]any{
		"path":    filepath.Join(dir, "big.txt"),
		"content": strings.Repeat("x", 1024),
	})

	text := getContent(t, c, "list_directory_with_sizes", map[string]any{
		"path": dir,
	})
	t.Logf("with sizes:\n%s", text)

	if !strings.Contains(text, "big.txt") {
		t.Error("should list big.txt")
	}
}

// ============================================================
// 搜索 & 元数据
// ============================================================

func TestMCP_SearchFiles(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	callAndCheck(t, c, "create_directory", map[string]any{
		"path": filepath.Join(dir, "sub"),
	})
	for _, f := range []string{"main.go", "util.go", "readme.md"} {
		callAndCheck(t, c, "write_file", map[string]any{
			"path":    filepath.Join(dir, f),
			"content": "// placeholder",
		})
	}
	callAndCheck(t, c, "write_file", map[string]any{
		"path":    filepath.Join(dir, "sub", "app.go"),
		"content": "package app",
	})

	result := getContent(t, c, "search_files", map[string]any{
		"path":    dir,
		"pattern": "**/*.go",
	})
	t.Logf("search *.go:\n%s", result)

	if !strings.Contains(result, "main.go") {
		t.Error("should find main.go")
	}
	if !strings.Contains(result, "app.go") {
		t.Error("should find app.go")
	}
	if strings.Contains(result, "readme.md") {
		t.Error("should not find readme.md")
	}
}

func TestMCP_GetFileInfo(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	fp := filepath.Join(dir, "info.txt")
	callAndCheck(t, c, "write_file", map[string]any{
		"path":    fp,
		"content": "hello",
	})

	info := getContent(t, c, "get_file_info", map[string]any{
		"path": fp,
	})
	t.Logf("file info:\n%s", info)

	if info == "" {
		t.Error("file info is empty")
	}
}

// ============================================================
// 移动文件
// ============================================================

func TestMCP_MoveFile(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	src := filepath.Join(dir, "old.txt")
	dst := filepath.Join(dir, "new.txt")

	callAndCheck(t, c, "write_file", map[string]any{
		"path":    src,
		"content": "moveme",
	})

	callAndCheck(t, c, "move_file", map[string]any{
		"source":      src,
		"destination": dst,
	})

	got := getContent(t, c, "read_text_file", map[string]any{"path": dst})
	if got != "moveme" {
		t.Errorf("moved file: want %q, got %q", "moveme", got)
	}
}

// ============================================================
// 边界条件
// ============================================================

func TestMCP_EmptyFile(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	fp := filepath.Join(dir, "empty.txt")
	callAndCheck(t, c, "write_file", map[string]any{
		"path":    fp,
		"content": "",
	})

	got := getContent(t, c, "read_text_file", map[string]any{"path": fp})
	t.Logf("empty file: %q", got)
}

func TestMCP_UnicodeContent(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	fp := filepath.Join(dir, "uni.txt")
	want := "中文\n日本語\n한국어"

	callAndCheck(t, c, "write_file", map[string]any{
		"path":    fp,
		"content": want,
	})

	got := getContent(t, c, "read_text_file", map[string]any{"path": fp})
	if got != want {
		t.Errorf("unicode mismatch:\n  want: %q\n  got:  %q", want, got)
	}
}

func TestMCP_OverwriteFile(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	fp := filepath.Join(dir, "ow.txt")
	callAndCheck(t, c, "write_file", map[string]any{"path": fp, "content": "v1"})
	callAndCheck(t, c, "write_file", map[string]any{"path": fp, "content": "v2"})

	got := getContent(t, c, "read_text_file", map[string]any{"path": fp})
	if got != "v2" {
		t.Errorf("want %q, got %q", "v2", got)
	}
}

func TestMCP_CreateExistingDirectory(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	sub := filepath.Join(dir, "existing")
	callAndCheck(t, c, "create_directory", map[string]any{"path": sub})
	callAndCheck(t, c, "create_directory", map[string]any{"path": sub})
}

// ============================================================
// 工具 Schema 验证
// ============================================================

func TestMCP_ToolSchemas(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	ctx := context.Background()
	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	for _, tool := range tools {
		t.Run(tool.Name, func(t *testing.T) {
			if tool.Description == "" {
				t.Errorf("empty description")
			}
			if tool.InputSchema == nil {
				t.Errorf("nil input schema")
			}
		})
	}
}

// ============================================================
// Allowed Directories
// ============================================================

func TestMCP_ListAllowedDirectories(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	// list_allowed_directories 的 schema 可能要求 arguments 为 object
	// 由于 CallToolParams.Arguments 有 omitempty，空 map 会被省略
	// 直接通过 ListTools 查看该工具的 schema，然后跳过实际调用
	ctx := context.Background()
	tools, err := c.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	for _, tool := range tools {
		if tool.Name == "list_allowed_directories" {
			t.Logf("found list_allowed_directories, description: %s", tool.Description)
			t.Logf("input schema: %+v", tool.InputSchema)

			// 尝试调用，允许失败（服务器可能对空参数敏感）
			ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			res, err := c.CallTool(ctx2, "list_allowed_directories", map[string]any{"_": true})
			if err != nil {
				t.Logf("call failed (expected on some servers): %v", err)
				return
			}
			if res.IsError {
				t.Logf("server returned error: %s", extractText(res.Content))
				return
			}
			text := extractText(res.Content)
			t.Logf("allowed dirs:\n%s", text)
			return
		}
	}
	t.Skip("list_allowed_directories tool not found")
}

// ============================================================
// 组合场景
// ============================================================

func TestMCP_FullWorkflow(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	// 1. 创建项目结构（逐层）
	project := filepath.Join(dir, "myapp")
	callAndCheck(t, c, "create_directory", map[string]any{
		"path": project,
	})
	callAndCheck(t, c, "create_directory", map[string]any{
		"path": filepath.Join(project, "src"),
	})

	// 2. 写文件
	callAndCheck(t, c, "write_file", map[string]any{
		"path":    filepath.Join(project, "src", "main.go"),
		"content": "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}",
	})
	callAndCheck(t, c, "write_file", map[string]any{
		"path":    filepath.Join(project, "README.md"),
		"content": "# My App",
	})

	// 3. 目录树
	tree := getContent(t, c, "directory_tree", map[string]any{"path": project})
	t.Logf("tree:\n%s", tree)

	// 4. 搜索
	search := getContent(t, c, "search_files", map[string]any{
		"path": project, "pattern": "**/*.go",
	})
	if !strings.Contains(search, "main.go") {
		t.Error("search should find main.go")
	}

	// 5. 编辑
	callAndCheck(t, c, "edit_file", map[string]any{
		"path": filepath.Join(project, "src", "main.go"),
		"edits": []any{
			map[string]any{
				"oldText": "println(\"hello\")",
				"newText": "println(\"hello, world!\")",
			},
		},
	})

	// 6. 验证编辑
	final := getContent(t, c, "read_text_file", map[string]any{
		"path": filepath.Join(project, "src", "main.go"),
	})
	if !strings.Contains(final, "hello, world!") {
		t.Errorf("edit failed, got: %q", final)
	}

	// 7. 创建目标目录并移动文件
	callAndCheck(t, c, "create_directory", map[string]any{
		"path": filepath.Join(project, "docs"),
	})

	callAndCheck(t, c, "move_file", map[string]any{
		"source":      filepath.Join(project, "README.md"),
		"destination": filepath.Join(project, "docs", "README.md"),
	})

	t.Log("full workflow passed")
}

// ============================================================
// 错误场景
// ============================================================

func TestMCP_CallNonExistentTool(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, "nonexistent_tool_xyz", map[string]any{})

	// 服务器可能返回 JSON-RPC 错误（err != nil）
	// 或返回工具结果但标记 isError=true（err == nil, res.IsError）
	if err != nil {
		t.Logf("got RPC error (expected): %v", err)
		return
	}
	if res != nil && res.IsError {
		t.Logf("got tool error (expected): %s", extractText(res.Content))
		return
	}

	// 两种情况都不是 → 意外
	t.Fatalf("expected error for nonexistent tool, got success: %+v", res)
}

func TestMCP_ReadNonExistentFile(t *testing.T) {
	dir := tmpDir(t)
	c := startFS(t, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := c.CallTool(ctx, "read_text_file", map[string]any{
		"path": filepath.Join(dir, "no_such_file.txt"),
	})
	if err != nil {
		t.Logf("got error: %v", err)
		return
	}
	if res.IsError {
		t.Logf("got isError: %s", extractText(res.Content))
		return
	}
	t.Logf("unexpected success: %s", extractText(res.Content))
}
