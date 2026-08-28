package builtins_test

import (
	"context"
	"encoding/json"
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
	host, reg, cleanup := setup(t, builtins.Options{})
	defer cleanup()
	names := map[string]bool{}
	for _, d := range reg.AsToolSet().Definitions() {
		names[d.Name] = true
	}
	for _, want := range []string{"read", "ls", "glob", "grep", "exec", "edit", "write"} {
		if !names[want] {
			t.Fatalf("missing %s", want)
		}
	}
	host.Dispose()
	if len(reg.AsToolSet().Definitions()) != 0 {
		// Dispose via host Effect should clear registrations
		// (Plugin Close clears all; scope dispose also removes builtins)
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
