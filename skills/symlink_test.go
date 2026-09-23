package skills_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Luo-root/pulse/skills"
)

// TestReadFileRejectsSymlinkEscape：skill 目录内的符号链接不得成为读出目录外
// 文件的通道（词法校验挡不住链接）。覆盖两档——链接 → 目录外文件、链接 →
// 目录外目录；并带一条正例钉住「两侧都解析」：skills 根目录路径本身含链接段
// 时，正常文件不得被误判成逃逸。
func TestReadFileRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "demo", "name: demo\ndescription: Demo skill for symlink escape tests", "# Demo\n")
	skillDir := filepath.Join(root, "demo")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET-OUTSIDE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "ok.md"), []byte("inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 第一处 symlink 调用：平台/权限不支持时 skip（CI 的 Linux 档会真跑）。
	if err := os.Symlink(secret, filepath.Join(skillDir, "leak.txt")); err != nil {
		t.Skipf("symlink not permitted: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(skillDir, "leakdir")); err != nil {
		t.Fatalf("symlink to dir failed after a link already succeeded: %v", err)
	}

	l, err := skills.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, rel := range []string{"leak.txt", "leakdir/secret.txt"} {
		b, err := l.ReadFile(ctx, "demo", rel)
		if err == nil {
			t.Fatalf("symlink escape must be rejected: %q -> %q (err=nil)", rel, b)
		}
	}
	if b, err := l.ReadFile(ctx, "demo", "ok.md"); err != nil || string(b) != "inside\n" {
		t.Fatalf("in-dir file read = %q, %v", b, err)
	}

	// 正例：skills 根目录是链接（宿主把 skills 树投放到别处）时，正常文件必须
	// 仍可读——只解析请求侧、不解析目录侧的实现会在这里误报逃逸。
	linkBase := t.TempDir()
	realRoot := filepath.Join(linkBase, "real-skills")
	writeSkill(t, realRoot, "demo", "name: demo\ndescription: Demo skill behind a linked root", "# D\n")
	if err := os.WriteFile(filepath.Join(realRoot, "demo", "note.md"), []byte("linked-root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(linkBase, "linked-skills")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatalf("symlink root failed after a link already succeeded: %v", err)
	}
	l2, err := skills.Open(linkedRoot)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := l2.ReadFile(ctx, "demo", "note.md"); err != nil || string(b) != "linked-root\n" {
		t.Fatalf("linked skills root must still read in-dir files: %q, %v", b, err)
	}
}
