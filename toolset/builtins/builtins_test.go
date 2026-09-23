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
	_, err := builtins.Register(host, reg2, builtins.Options{Root: root, Enabled: []string{"reed", "writ", "reed"}})
	if err == nil {
		t.Fatal("unknown names in Options.Enabled must fail (they used to register zero tools silently)")
	}
	for _, want := range []string{"reed", "writ", "read", "write"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must mention %q: %v", want, err)
		}
	}
	if n := strings.Count(err.Error(), "reed"); n != 1 {
		t.Fatalf("a repeated unknown name must be reported once, got %d occurrences: %v", n, err)
	}
	if n := len(reg2.AsToolSet().Definitions()); n != 0 {
		t.Fatalf("a failed Register must leave nothing behind, got %d tools", n)
	}
}

// TestWriteDanglingSymlinkRelativeTarget：悬空链接的**相对目标**（`../outside/x.txt`）
// 走的是 `filepath.Join(filepath.Dir(exist), target)` 这条独立分支，修复前
// 同样会逃逸；这里与绝对目标档一一对应地钉住。
func TestWriteDanglingSymlinkRelativeTarget(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "rel")
	if err := os.Symlink(filepath.Join("..", "outside", "rel.txt"), link); err != nil {
		t.Skipf("symlink not permitted: %v", err)
	}
	target := filepath.Join(outside, "rel.txt")
	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	msg := callErr(t, reg, "write", map[string]any{"path": "rel", "content": "PWNED"})
	if !strings.Contains(msg, "WriteRoots") && !strings.Contains(msg, "outside") {
		t.Fatalf("relative dangling symlink must be rejected with the real target, got: %s", msg)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("write followed a relative dangling symlink out of WriteRoots")
	}
}

// TestSymlinkCycleFailsFast：自指 / 互指链接必须**快速**报错。修复的第一版
// 在成环路径上每轮都跑一次注定失败的 filepath.EvalSymlinks（它自己要走满
// 255 步，实测单次约 150ms），255 轮 ≈ 38s——一次系统调用级的快失败被放大
// 成几十秒卡顿（#213 review 实测 37.98s；对照：修复前 OS 层 294ms 返回）。
func TestSymlinkCycleFailsFast(t *testing.T) {
	root := t.TempDir()
	loop := filepath.Join(root, "loop")
	if err := os.Symlink("loop", loop); err != nil { // 自指：相对目标 → 解析回自身
		t.Skipf("symlink not permitted: %v", err)
	}
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	if err := os.Symlink("b", a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a", b); err != nil {
		t.Fatal(err)
	}
	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	for _, name := range []string{"loop", "loop/child.txt", "a"} {
		start := time.Now()
		msg := callErr(t, reg, "write", map[string]any{"path": name, "content": "x"})
		elapsed := time.Since(start)
		if !strings.Contains(msg, "cycle") && !strings.Contains(msg, "symbolic links") {
			t.Fatalf("%s: want a cycle error, got %s", name, msg)
		}
		if elapsed > time.Second {
			t.Fatalf("%s: a symlink cycle must fail fast, took %s (was ~38s before the fix)", name, elapsed)
		}
	}
}

// TestRootItselfSymlinked：Root / WriteRoots / ForbidRead 自身是 symlink 时
// 必须解析到真实落点——只做 Abs 的话，解析后的真实路径与未解析前缀永远对
// 不上（withinRoot 必然失败），读写全被判越界。macOS 的 /tmp、/var 都是
// 链接（t.TempDir() 落在 /var/folders/…），CI 只跑 ubuntu 看不见这一档。
//
// 「解析 Root」与「守住牢笼」是一对：解析只许把口径对齐到真实落点，不许
// 把边界放宽——所以这里同时钉住越界仍被拒（`..` 逃逸、Root 内链接指向
// Root 外），否则「让读写能用」会被做成「不再 confined」。
func TestRootItselfSymlinked(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "secret"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "secret", "s.txt"), []byte("s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Root 之外的对照物：`..` 逃逸与链接逃逸都要落到这里，但都不许落成。
	if err := os.WriteFile(filepath.Join(base, "outside.txt"), []byte("outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideDir := filepath.Join(base, "outside-dir")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 本用例**第一次**建链接必须是这一处：没有 symlink 权限的平台
	// （Windows 非管理员 / 未开开发者模式）要在这里整条跳过。后面的
	// os.Symlink 都排在它之后，走 t.Fatal 是对的——到那一步说明平台支持。
	rootLink := filepath.Join(base, "root-link")
	if err := os.Symlink(real, rootLink); err != nil {
		t.Skipf("symlink not permitted: %v", err)
	}
	// Root 内部的链接指向 Root 外：confine* 判的必须是解析后的落点。
	if err := os.Symlink(outsideDir, filepath.Join(real, "escape")); err != nil {
		t.Fatal(err)
	}
	// ForbidRead 也用链接路径给出：canonRoot 不解析它的话，禁读前缀与解析后
	// 的真实路径对不上，secret 会被读出来。
	secretLink := filepath.Join(base, "secret-link")
	if err := os.Symlink(filepath.Join(real, "secret"), secretLink); err != nil {
		t.Fatal(err)
	}

	_, reg, cleanup := setup(t, builtins.Options{Root: rootLink, ForbidRead: []string{secretLink}})
	defer cleanup()

	if out := call(t, reg, "read", map[string]any{"path": "a.txt"}); !strings.Contains(out, "hi") {
		t.Fatalf("read through a symlinked Root must work, got %q", out)
	}
	if out := call(t, reg, "write", map[string]any{"path": "new.txt", "content": "written\n"}); !strings.Contains(out, "created") {
		t.Fatalf("write through a symlinked Root must work, got %q", out)
	}
	if b, err := os.ReadFile(filepath.Join(real, "new.txt")); err != nil || string(b) != "written\n" {
		t.Fatalf("file must land in the real directory: %q err=%v", b, err)
	}
	if msg := callErr(t, reg, "read", map[string]any{"path": "secret/s.txt"}); !strings.Contains(msg, "forbid-read") {
		t.Fatalf("a symlinked ForbidRead prefix must still forbid, got %s", msg)
	}

	// 边界仍要守住：解析 Root 只对齐口径，不放宽牢笼。
	if msg := callErr(t, reg, "read", map[string]any{"path": "../outside.txt"}); !strings.Contains(msg, "escapes Root") {
		t.Fatalf("read must still escape-check against the resolved Root, got %s", msg)
	}
	if msg := callErr(t, reg, "write", map[string]any{"path": "../new-outside.txt", "content": "x"}); !strings.Contains(msg, "outside WriteRoots") {
		t.Fatalf("write must still refuse targets outside the resolved Root, got %s", msg)
	}
	if msg := callErr(t, reg, "write", map[string]any{"path": "escape/new.txt", "content": "x"}); !strings.Contains(msg, "outside WriteRoots") {
		t.Fatalf("a link inside Root pointing outside must be refused, got %s", msg)
	}
	if _, err := os.Stat(filepath.Join(base, "new-outside.txt")); !os.IsNotExist(err) {
		t.Fatalf("escape via .. must not create files outside Root: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("escape via an inner link must not create files outside Root: err=%v", err)
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

// TestPreviewLargeFileCountsNonZero：#215-1——大文件与常规文件共用同一套计数
// 口径（共同前缀/后缀之外的位置区间），大文件只是不生成 diff 文本。
//
// 曾经这一档改用多重集计数，整体位移的行集合前后完全相同 → 算出 0/0；而
// Added/Removed 是卡片上的 `+N/-M`，按改动量升级审批的策略（if Added > 50
// { 问人 }）在**恰好大改动**时读到 0，等于 fail-open。
func TestPreviewLargeFileCountsNonZero(t *testing.T) {
	root := t.TempDir()
	const lines = 250 // 250 + 250 = 500 > maxDiffLines(400)，必走大文件档
	oldL := make([]string, 0, lines)
	for i := 0; i < lines; i++ {
		oldL = append(oldL, fmt.Sprintf("line %03d\n", i))
	}
	// 整体位移：首行挪到末尾——多重集视角下两版完全相同。
	rotated := append(append([]string{}, oldL[1:]...), oldL[0])
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(strings.Join(oldL, "")), 0o644); err != nil {
		t.Fatal(err)
	}
	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	args, err := json.Marshal(map[string]any{"path": "big.txt", "content": strings.Join(rotated, "")})
	if err != nil {
		t.Fatal(err)
	}
	p, ok, err := reg.Preview(context.Background(), "write", args)
	if err != nil || !ok || p.File == nil {
		t.Fatalf("write preview %+v ok=%v err=%v", p, ok, err)
	}
	if p.File.Added == 0 || p.File.Removed == 0 {
		t.Fatalf("a %d-line rotation must report non-zero counts, got +%d/-%d (truncated=%v)",
			lines, p.File.Added, p.File.Removed, p.File.Truncated)
	}
	// 位置区间口径下，整体位移就是全量替换；这个数字也钉住「大文件不换算法」。
	if p.File.Added != lines || p.File.Removed != lines {
		t.Fatalf("whole-file rotation must count all lines as rewritten: want +%d/-%d, got +%d/-%d",
			lines, lines, p.File.Added, p.File.Removed)
	}
	if !p.File.Truncated || !strings.Contains(p.File.Diff, "diff omitted") {
		t.Fatalf("a large diff must be marked truncated with an omitted-text note: truncated=%v diff=%q",
			p.File.Truncated, p.File.Diff)
	}
}
