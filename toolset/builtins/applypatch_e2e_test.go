package builtins_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse/toolset/builtins"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func patchArgs(patch string) map[string]any {
	return map[string]any{"patch": patch}
}

func TestApplyPatchAddUpdateDelete(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.go"), "package a\n\nfunc A() {}\n")
	mustWrite(t, filepath.Join(root, "gone.txt"), "l1\nl2\nl3\n")

	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	// Delete 目标也要求先 read。
	call(t, reg, "read", map[string]any{"path": "gone.txt"})
	call(t, reg, "read", map[string]any{"path": "a.go"})

	patch := `*** Begin Patch
*** Add File: sub/new.txt
+hello
+world
*** Update File: a.go
@@
 package a

-func A() {}
+func B() {}
*** Delete File: gone.txt
*** End Patch`
	out := call(t, reg, "apply_patch", patchArgs(patch))
	if !strings.Contains(out, "3 file(s)") ||
		!strings.Contains(out, "create") ||
		!strings.Contains(out, "modify") ||
		!strings.Contains(out, "delete") {
		t.Fatalf("out=%q", out)
	}

	b, err := os.ReadFile(filepath.Join(root, "sub", "new.txt"))
	if err != nil || string(b) != "hello\nworld" {
		t.Fatalf("new.txt=%q err=%v", b, err)
	}
	b, err = os.ReadFile(filepath.Join(root, "a.go"))
	if err != nil || string(b) != "package a\n\nfunc B() {}\n" {
		t.Fatalf("a.go=%q err=%v", b, err)
	}
	if _, err := os.Stat(filepath.Join(root, "gone.txt")); !os.IsNotExist(err) {
		t.Fatalf("gone.txt should be removed, err=%v", err)
	}
	if !strings.Contains(out, "(+2/-0)") || !strings.Contains(out, "(+1/-1)") || !strings.Contains(out, "(+0/-3)") {
		t.Fatalf("line counts missing: %q", out)
	}
}

func TestApplyPatchUpdateRequiresRead(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "b.txt"), "one\ntwo\n")

	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	patch := `*** Begin Patch
*** Update File: b.txt
@@
-one
+ONE
*** End Patch`
	msg := callErr(t, reg, "apply_patch", patchArgs(patch))
	if !strings.Contains(msg, "must be read") {
		t.Fatalf("msg=%s", msg)
	}
}

func TestApplyPatchStaleRead(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "c.txt")
	mustWrite(t, path, "one\ntwo\n")

	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	call(t, reg, "read", map[string]any{"path": "c.txt"})
	mustWrite(t, path, "one\ntwo\nCHANGED\n")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	patch := `*** Begin Patch
*** Update File: c.txt
@@
-one
+ONE
*** End Patch`
	msg := callErr(t, reg, "apply_patch", patchArgs(patch))
	if !strings.Contains(msg, "modified since last read") {
		t.Fatalf("msg=%s", msg)
	}
}

func TestApplyPatchAllOrNothing(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "keep.txt"), "a\nb\n")

	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	call(t, reg, "read", map[string]any{"path": "keep.txt"})

	patch := `*** Begin Patch
*** Add File: fresh.txt
+x
*** Update File: keep.txt
@@
-context that does not exist
+nope
*** End Patch`
	msg := callErr(t, reg, "apply_patch", patchArgs(patch))
	if !strings.Contains(msg, "context not found") {
		t.Fatalf("msg=%s", msg)
	}
	if _, err := os.Stat(filepath.Join(root, "fresh.txt")); !os.IsNotExist(err) {
		t.Fatal("verify failed: fresh.txt must not be written")
	}
}

func TestApplyPatchEscapeAndNoopAndAddExists(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "exist.txt"), "x\n")
	mustWrite(t, filepath.Join(root, "noop.txt"), "a\nb\n")

	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	msg := callErr(t, reg, "apply_patch", patchArgs(`*** Begin Patch
*** Add File: ../evil.txt
+x
*** End Patch`))
	if !strings.Contains(msg, "outside WriteRoots") {
		t.Fatalf("escape: %s", msg)
	}

	call(t, reg, "read", map[string]any{"path": "noop.txt"})
	msg = callErr(t, reg, "apply_patch", patchArgs(`*** Begin Patch
*** Update File: noop.txt
@@
 a
*** End Patch`))
	if !strings.Contains(msg, "makes no changes") {
		t.Fatalf("noop: %s", msg)
	}

	msg = callErr(t, reg, "apply_patch", patchArgs(`*** Begin Patch
*** Add File: exist.txt
+x
*** End Patch`))
	if !strings.Contains(msg, "already exists") {
		t.Fatalf("add exists: %s", msg)
	}
}

func TestApplyPatchCRLFPreserved(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "crlf.txt"), "a\r\nb\r\n")

	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	call(t, reg, "read", map[string]any{"path": "crlf.txt"})

	patch := `*** Begin Patch
*** Update File: crlf.txt
@@
-a
+A
*** End Patch`
	call(t, reg, "apply_patch", patchArgs(patch))
	b, err := os.ReadFile(filepath.Join(root, "crlf.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "A\r\nb\r\n" {
		t.Fatalf("CRLF must be preserved, got %q", b)
	}
}

func TestApplyPatchDuplicateTargetAndDeleteNeedsRead(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "dup.txt"), "x\ny\n")
	mustWrite(t, filepath.Join(root, "unread.txt"), "z\n")

	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	call(t, reg, "read", map[string]any{"path": "dup.txt"})
	msg := callErr(t, reg, "apply_patch", patchArgs(`*** Begin Patch
*** Update File: dup.txt
@@
-x
+X
*** Update File: dup.txt
@@
-y
+Y
*** End Patch`))
	if !strings.Contains(msg, "duplicate target") {
		t.Fatalf("duplicate: %s", msg)
	}

	msg = callErr(t, reg, "apply_patch", patchArgs(`*** Begin Patch
*** Delete File: unread.txt
*** End Patch`))
	if !strings.Contains(msg, "must be read") {
		t.Fatalf("delete unread: %s", msg)
	}
}

func TestApplyPatchPreviewListsFiles(t *testing.T) {
	root := t.TempDir()
	_, reg, cleanup := setup(t, builtins.Options{Root: root})
	defer cleanup()

	p, ok, err := reg.Preview(context.Background(), "apply_patch", json.RawMessage(
		`{"patch":"*** Begin Patch\n*** Add File: p.txt\n+hi\n*** End Patch"}`))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if p.Opaque == nil || !strings.Contains(p.Opaque.Summary, "1 file(s)") ||
		!strings.Contains(p.Opaque.Summary, "create") || !strings.Contains(p.Opaque.Summary, "p.txt") {
		t.Fatalf("preview=%+v", p)
	}
}
