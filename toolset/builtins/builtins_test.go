package builtins_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
	"github.com/Luo-root/pulse/toolset/builtins"
)

func setup(t *testing.T, opt builtins.Options) (*kernel.Context, *toolset.Registry, func()) {
	t.Helper()
	host := kernel.New()
	if _, err := kernel.Use(host, toolset.Plugin()); err != nil {
		t.Fatal(err)
	}
	reg, ok := kernel.Get(host, toolset.ServiceKey)
	if !ok {
		t.Fatal("no registry")
	}
	if opt.Root == "" {
		opt.Root = t.TempDir()
	}
	dispose, err := builtins.Register(host, reg, opt)
	if err != nil {
		host.Dispose()
		t.Fatal(err)
	}
	return host, reg, func() {
		dispose()
		host.Dispose()
	}
}

func call(t *testing.T, reg *toolset.Registry, name string, args any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := reg.AsToolSet().Execute(context.Background(), llm.ToolCall{
		ID: "t1", Name: name, Arguments: b,
	})
	if err != nil {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
	return out
}

func callErr(t *testing.T, reg *toolset.Registry, name string, args any) string {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := reg.AsToolSet().Execute(context.Background(), llm.ToolCall{
		ID: "t1", Name: name, Arguments: b,
	})
	if err == nil {
		t.Fatalf("%s: want error, got %q", name, out)
	}
	return err.Error()
}

func TestRegisterDefinitionsAndDispose(t *testing.T) {
	host := kernel.New()
	defer host.Dispose()
	if _, err := kernel.Use(host, toolset.Plugin()); err != nil {
		t.Fatal(err)
	}
	reg, ok := kernel.Get(host, toolset.ServiceKey)
	if !ok {
		t.Fatal("no registry")
	}
	dispose, err := builtins.Register(host, reg, builtins.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, d := range reg.AsToolSet().Definitions() {
		names[d.Name] = true
	}
	for _, want := range []string{"read", "ls", "glob", "grep", "exec", "edit", "write", "apply_patch", "web_fetch", "web_search", "question", "job_output", "job_kill"} {
		if !names[want] {
			t.Fatalf("missing %s", want)
		}
	}
	dispose()
	for _, d := range reg.AsToolSet().Definitions() {
		switch d.Name {
		case "read", "ls", "glob", "grep", "exec", "edit", "write", "apply_patch", "web_fetch", "web_search", "question", "job_output", "job_kill":
			t.Fatalf("dispose() left %s registered", d.Name)
		}
	}
}

func TestReadEditWriteStaleAndEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	out := call(t, reg, "read", map[string]any{"path": "a.txt"})
	if !strings.Contains(out, "1|hello world") {
		t.Fatalf("read=%q", out)
	}

	// edit without unique context / after read
	out = call(t, reg, "edit", map[string]any{
		"path": "a.txt", "old_string": "hello", "new_string": "hi",
	})
	if !strings.Contains(out, "edited") {
		t.Fatalf("edit=%q", out)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if !strings.Contains(string(raw), "hi world") {
		t.Fatalf("content=%q", raw)
	}

	// escape write roots
	msg := callErr(t, reg, "write", map[string]any{
		"path": filepath.Join(outside, "x.txt"), "content": "x",
	})
	if !strings.Contains(msg, "WriteRoots") && !strings.Contains(msg, "escapes") && !strings.Contains(msg, "outside") {
		t.Fatalf("escape write: %s", msg)
	}

	// read escape
	msg = callErr(t, reg, "read", map[string]any{"path": filepath.Join(outside, "secret.txt")})
	if !strings.Contains(msg, "escapes") && !strings.Contains(msg, "Root") {
		t.Fatalf("escape read: %s", msg)
	}
}

func TestEditRequiresReadAndRejectsStale(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "b.txt")
	if err := os.WriteFile(path, []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	msg := callErr(t, reg, "edit", map[string]any{
		"path": "b.txt", "old_string": "alpha", "new_string": "beta",
	})
	if !strings.Contains(msg, "must be read") {
		t.Fatalf("%s", msg)
	}

	_ = call(t, reg, "read", map[string]any{"path": "b.txt"})
	// bump mtime into the future relative to last read
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	msg = callErr(t, reg, "edit", map[string]any{
		"path": "b.txt", "old_string": "alpha", "new_string": "beta",
	})
	if !strings.Contains(msg, "modified since last read") {
		t.Fatalf("%s", msg)
	}
}

func TestEditUniqueMatch(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "c.txt"), []byte("x\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()
	_ = call(t, reg, "read", map[string]any{"path": "c.txt"})
	msg := callErr(t, reg, "edit", map[string]any{
		"path": "c.txt", "old_string": "x", "new_string": "y",
	})
	if !strings.Contains(msg, "multiple") {
		t.Fatalf("%s", msg)
	}
	_ = call(t, reg, "edit", map[string]any{
		"path": "c.txt", "old_string": "x", "new_string": "y", "replace_all": true,
	})
}

func TestReadPagination(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 50; i++ {
		b.WriteString("line\n")
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	_, reg, cleanup := setup(t, builtins.Options{Root: root, ReadLimit: 10})
	defer cleanup()
	out := call(t, reg, "read", map[string]any{"path": "big.txt"})
	if !strings.Contains(out, "truncated") {
		t.Fatalf("want truncated: %q", out)
	}
	out2 := call(t, reg, "read", map[string]any{"path": "big.txt", "offset": 10, "limit": 10})
	if !strings.Contains(out2, "11|") {
		t.Fatalf("%q", out2)
	}
}

func TestGlobGrepLS(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "pkg"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "pkg", "a.go"), []byte("package pkg\nfunc Hello() {}\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "pkg", "b.txt"), []byte("ignore\n"), 0o644)

	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	g := call(t, reg, "glob", map[string]any{"pattern": "**/*.go"})
	if !strings.Contains(g, "pkg/a.go") {
		t.Fatalf("glob=%q", g)
	}
	gr := call(t, reg, "grep", map[string]any{"pattern": "Hello", "glob": "*.go"})
	if !strings.Contains(gr, "Hello") {
		t.Fatalf("grep=%q", gr)
	}
	msg := callErr(t, reg, "grep", map[string]any{"pattern": "("})
	if !strings.Contains(msg, "invalid regexp") {
		t.Fatalf("%s", msg)
	}
	ls := call(t, reg, "ls", map[string]any{"path": "pkg"})
	if !strings.Contains(ls, "a.go") {
		t.Fatalf("ls=%q", ls)
	}
}

func TestExecPlatform(t *testing.T) {
	root := t.TempDir()
	_, reg, cleanup := setup(t, builtins.Options{Root: root, ExecTimeout: 15 * time.Second})
	defer cleanup()

	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "Write-Output 'hello-builtins'"
	} else {
		cmd = "echo hello-builtins"
	}
	out := call(t, reg, "exec", map[string]any{"command": cmd})
	if !strings.Contains(out, "exit_code=0") || !strings.Contains(out, "hello-builtins") {
		t.Fatalf("exec=%q", out)
	}
}

func TestEnabledSubset(t *testing.T) {
	_, reg, cleanup := setup(t, builtins.Options{Enabled: []string{"read", "ls"}})
	defer cleanup()
	names := map[string]bool{}
	for _, d := range reg.AsToolSet().Definitions() {
		names[d.Name] = true
	}
	if !names["read"] || !names["ls"] || names["exec"] {
		t.Fatalf("%v", names)
	}
}

func TestExecTimeout(t *testing.T) {
	root := t.TempDir()
	_, reg, cleanup := setup(t, builtins.Options{Root: root, ExecTimeout: 15 * time.Second})
	defer cleanup()
	var cmd string
	if runtime.GOOS == "windows" {
		cmd = "Start-Sleep -Seconds 8"
	} else {
		cmd = "sleep 8"
	}
	msg := callErr(t, reg, "exec", map[string]any{"command": cmd, "timeout_seconds": 1})
	if !strings.Contains(msg, "timeout") {
		t.Fatalf("want timeout, got %s", msg)
	}
}

func TestListPaginationAndForbidRead(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "secret")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secret, "s.txt"), []byte("nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := filepath.Join(root, "files")
	if err := os.MkdirAll(files, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("f%02d.txt", i)
		if err := os.WriteFile(filepath.Join(files, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, reg, cleanup := setup(t, builtins.Options{Root: root, GlobLimit: 2, LSLimit: 2, GrepLimit: 2, ForbidRead: []string{secret}})
	defer cleanup()

	g1 := call(t, reg, "glob", map[string]any{"pattern": "files/*.txt", "limit": 2})
	if !strings.Contains(g1, "pass after=") {
		t.Fatalf("glob page1=%q", g1)
	}
	if strings.Contains(g1, "secret") {
		t.Fatalf("glob leaked forbid-read: %q", g1)
	}
	// 从 trailer 抽 after 太脆，直接传已知第二项
	g2 := call(t, reg, "glob", map[string]any{"pattern": "files/*.txt", "limit": 2, "after": "files/f01.txt"})
	if !strings.Contains(g2, "files/f02.txt") {
		t.Fatalf("glob page2=%q", g2)
	}

	ls1 := call(t, reg, "ls", map[string]any{"path": "files", "limit": 2})
	if !strings.Contains(ls1, "pass after=") {
		t.Fatalf("ls page1=%q", ls1)
	}

	msg := callErr(t, reg, "read", map[string]any{"path": "secret/s.txt"})
	if !strings.Contains(msg, "forbid-read") {
		t.Fatalf("forbid-read: %s", msg)
	}
}

func TestWriteSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink not permitted: %v", err)
	}
	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()
	msg := callErr(t, reg, "write", map[string]any{
		"path": "link/escaped.txt", "content": "nope",
	})
	if !strings.Contains(msg, "WriteRoots") && !strings.Contains(msg, "outside") && !strings.Contains(msg, "escapes") {
		t.Fatalf("symlink write: %s", msg)
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped.txt")); err == nil {
		t.Fatal("wrote through symlink to outside root")
	}
}

// TestWriteDanglingSymlinkEscape：#213——root/link 指向 Root 外、且目标
// **尚不存在**时，write 与 apply_patch 都必须拒绝，且根外文件不得被创建。
// 对照（已覆盖）：目标已存在的链接、普通 ../ 越界都会被拒 —— 恰好漏的是
// 「目标不存在」这一档（悬空链接的两次 EvalSymlinks 都失败）。
func TestWriteDanglingSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	target := filepath.Join(outside, "x.txt") // 目标不存在：悬空链接
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not permitted: %v", err)
	}
	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	rejected := func(msg string) bool {
		return strings.Contains(msg, "WriteRoots") || strings.Contains(msg, "outside") ||
			strings.Contains(msg, "symlink") || strings.Contains(msg, "escapes")
	}

	msg := callErr(t, reg, "write", map[string]any{"path": "link", "content": "PWNED"})
	if !rejected(msg) {
		t.Fatalf("dangling symlink write must be rejected, got: %s", msg)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("write followed a dangling symlink out of WriteRoots")
	}

	// apply_patch 走同一 resolveUnderRoot + confineWrite（同缝）。
	patch := "*** Begin Patch\n*** Add File: link\n+PWNED\n*** End Patch\n"
	msg = callErr(t, reg, "apply_patch", map[string]any{"patch": patch})
	if !rejected(msg) {
		t.Fatalf("dangling symlink apply_patch must be rejected, got: %s", msg)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("apply_patch followed a dangling symlink out of WriteRoots")
	}

	// 反向对照：不悬空（目标已存在）时，写出 Root 外同样被拒。
	if err := os.WriteFile(target, []byte("seed"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg = callErr(t, reg, "write", map[string]any{"path": "link", "content": "PWNED"})
	if !rejected(msg) {
		t.Fatalf("resolved symlink write must be rejected, got: %s", msg)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "seed" {
		t.Fatalf("outside file changed: %q err=%v", b, err)
	}
}

// TestRegisterEnabledValidation：#214-1——Enabled 只登记列出的工具；出现未
// 实现的名字必须 fail-loud（原先静默注册 0 个工具、err=nil，错误要么拖到
// 运行期才以模型可见的 unknown tool 露出，要么永不暴露）。
func TestRegisterEnabledValidation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, reg, cleanup := setup(t, builtins.Options{Root: root, Enabled: []string{"read", "write"}})
	defer cleanup()
	if defs := reg.AsToolSet().Definitions(); len(defs) != 2 {
		t.Fatalf("Enabled subset registered %d tools, want 2", len(defs))
	}
	if out := call(t, reg, "read", map[string]any{"path": "a.txt"}); !strings.Contains(out, "hi") {
		t.Fatalf("read output = %q", out)
	}
	if msg := callErr(t, reg, "ls", map[string]any{"path": "."}); !strings.Contains(msg, "unknown tool") {
		t.Fatalf("a tool outside Enabled must not be registered: %s", msg)
	}

	// 未知名：报错并列出合法名字；失败的 Register 不留半个登记。
	host := kernel.New()
	defer host.Dispose()
	if _, err := kernel.Use(host, toolset.Plugin()); err != nil {
		t.Fatal(err)
	}
	reg2, ok := kernel.Get(host, toolset.ServiceKey)
	if !ok {
		t.Fatal("no registry")
	}
	_, err := builtins.Register(host, reg2, builtins.Options{Root: root, Enabled: []string{"reed", "writ"}})
	if err == nil {
		t.Fatal("unknown names in Options.Enabled must fail (they used to register zero tools silently)")
	}
	for _, want := range []string{"reed", "writ", "read", "write"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must mention %q: %v", want, err)
		}
	}
	if n := len(reg2.AsToolSet().Definitions()); n != 0 {
		t.Fatalf("a failed Register must leave nothing behind, got %d tools", n)
	}
}

// TestSearchToolsDeclareSkippedDirs：#214-2——glob/grep 按目录名跳过 .git /
// node_modules / vendor：行为侧钉住「确实跳过且只跳这些」，描述侧钉住
// 「模型被告知了」——否则「文件明明存在却 no matches」会被误判成 pattern
// 写错，而宿主也没有开关。
func TestSearchToolsDeclareSkippedDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "visible.js"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(root, "node_modules", "pkg")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "x.js"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitdir := filepath.Join(root, "vendor", ".git")
	if err := os.MkdirAll(gitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitdir, "z.js"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	for _, name := range []string{"glob", "grep"} {
		desc := ""
		for _, d := range reg.AsToolSet().Definitions() {
			if d.Name == name {
				desc = d.Description
			}
		}
		if desc == "" {
			t.Fatalf("%s: no tool definition", name)
		}
		for _, dir := range []string{".git", "node_modules", "vendor"} {
			if !strings.Contains(desc, dir) {
				t.Fatalf("%s description must name the skipped dir %q: %s", name, dir, desc)
			}
		}
	}

	g := call(t, reg, "glob", map[string]any{"pattern": "**/*.js"})
	if !strings.Contains(g, "visible.js") || strings.Contains(g, "x.js") || strings.Contains(g, "z.js") {
		t.Fatalf("glob must return visible.js and skip the three dirs, got %q", g)
	}
	gr := call(t, reg, "grep", map[string]any{"pattern": "needle", "glob": "*.js"})
	if !strings.Contains(gr, "visible.js") || strings.Contains(gr, "x.js") || strings.Contains(gr, "z.js") {
		t.Fatalf("grep must match visible.js and skip the three dirs, got %q", gr)
	}
}

func TestPreviewWriteEditExecDoesNotWrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.txt")
	if err := os.WriteFile(path, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	p, ok, err := reg.Preview(context.Background(), "write", json.RawMessage(`{"path":"b.txt","content":"new\n"}`))
	if err != nil || !ok || p.File == nil || p.File.Op != "create" {
		t.Fatalf("write create preview %+v ok=%v err=%v", p, ok, err)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); err == nil {
		t.Fatal("preview must not create file")
	}

	p, ok, err = reg.Preview(context.Background(), "edit", json.RawMessage(`{"path":"a.txt","old_string":"hello","new_string":"hi"}`))
	if err != nil || !ok || p.File == nil || p.File.Op != "modify" {
		t.Fatalf("edit preview %+v ok=%v err=%v", p, ok, err)
	}
	if p.File.Added < 1 && !strings.Contains(p.File.Diff, "+hi") && !strings.Contains(p.File.Diff, "-hello") {
		// 中间整段替换：-hello world / +hi world
		if !strings.Contains(p.File.Diff, "+") {
			t.Fatalf("diff=%q", p.File.Diff)
		}
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "hello world\n" {
		t.Fatalf("preview mutated file: %q", raw)
	}

	p, ok, err = reg.Preview(context.Background(), "exec", json.RawMessage(`{"command":"echo x"}`))
	if err != nil || !ok || p.Kind != "command" || p.Command == nil || p.Command.Command != "echo x" {
		t.Fatalf("exec preview %+v ok=%v err=%v", p, ok, err)
	}

	if _, ok := reg.LookupPreview("read"); ok {
		t.Fatal("read must not register PreviewFn")
	}
}
