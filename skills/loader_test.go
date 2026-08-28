package skills_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/skills"
)

func writeSkill(t *testing.T, root, dir, frontmatter, body string) {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\n" + frontmatter + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOpenListLoadSorted(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "zeta", "name: zeta\ndescription: Z skill for sorting tests", "# Zeta\n")
	writeSkill(t, root, "alpha", "name: alpha\ndescription: A skill for sorting tests", "# Alpha\nhello\n")
	// subdir without SKILL.md ignored
	_ = os.MkdirAll(filepath.Join(root, "empty"), 0o755)

	l, err := skills.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	list, err := l.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "zeta" {
		t.Fatalf("list=%v", list)
	}
	body, err := l.Load(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "# Alpha") || strings.Contains(body, "description:") {
		t.Fatalf("body should be markdown without frontmatter: %q", body)
	}
}

func TestRejectInvalidFrontmatter(t *testing.T) {
	cases := []struct {
		dir, fm, sub string
	}{
		{"badname", "name: Bad_Name\ndescription: x", "lowercase"},
		{"mismatch", "name: other\ndescription: enough text here", "must match directory"},
		{"nodesc", "name: nodesc\ndescription: ", "description is required"},
		{"noname", "description: some description text here", "name is required"},
	}
	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			root := t.TempDir()
			// directory name for mismatch case
			dir := tc.dir
			if tc.dir == "badname" {
				dir = "bad-name"
				// but frontmatter has Bad_Name — use dir bad-name with invalid name field
				writeSkill(t, root, dir, tc.fm, "# x\n")
			} else if tc.dir == "mismatch" {
				writeSkill(t, root, "mismatch", tc.fm, "# x\n")
			} else {
				writeSkill(t, root, dir, tc.fm, "# x\n")
			}
			_, err := skills.Open(root)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.sub)) {
				t.Fatalf("err=%v want substring %q", err, tc.sub)
			}
		})
	}
}

func TestIgnoresPrivateKeys(t *testing.T) {
	root := t.TempDir()
	fm := "name: demo\ndescription: Demo skill with private keys ignored\ncategory: research\ntimeout: 30\nparameters:\n  type: object\n"
	writeSkill(t, root, "demo", fm, "# Demo\n")
	l, err := skills.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := l.List(context.Background())
	if len(list) != 1 || list[0].Name != "demo" {
		t.Fatalf("%v", list)
	}
}

func TestReadFileSafe(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "demo", "name: demo\ndescription: Demo skill for ReadFile safety tests", "# Demo\n")
	ref := filepath.Join(root, "demo", "references")
	if err := os.MkdirAll(ref, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ref, "a.md"), []byte("ref-body"), 0o644); err != nil {
		t.Fatal(err)
	}
	// outside file
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	l, err := skills.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.ReadFile(context.Background(), "demo", "references/a.md")
	if err != nil || string(b) != "ref-body" {
		t.Fatalf("%q %v", b, err)
	}
	for _, bad := range []string{"../secret.txt", "..\\secret.txt", filepath.Join(root, "secret.txt")} {
		_, err := l.ReadFile(context.Background(), "demo", bad)
		if err == nil {
			t.Fatalf("expected reject for %q", bad)
		}
	}
}

func TestLoadUsesOpenSnapshot(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "demo", "name: demo\ndescription: Original description for snapshot test", "# Original body\n")
	l, err := skills.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	// 改磁盘后不重新 Open：List/Load 仍应是快照
	writeSkill(t, root, "demo", "name: demo\ndescription: Changed description after open scan", "# Changed body\n")
	list, err := l.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Description != "Original description for snapshot test" {
		t.Fatalf("List should keep snapshot meta: %q", list[0].Description)
	}
	body, err := l.Load(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "Original body") || strings.Contains(body, "Changed body") {
		t.Fatalf("Load should keep snapshot body: %q", body)
	}
}

func TestOpenFailsOnBadSkillDir(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "good", "name: good\ndescription: A valid skill that should not matter", "# Good\n")
	writeSkill(t, root, "bad", "name: bad\ndescription: ", "# Bad\n")
	_, err := skills.Open(root)
	if err == nil || !strings.Contains(err.Error(), "description") {
		t.Fatalf("Open should fail entire root on bad skill: %v", err)
	}
}

func TestRejectConsecutiveHyphens(t *testing.T) {
	root := t.TempDir()
	dir := "bad--name"
	if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: bad--name\ndescription: consecutive hyphens should fail validation\n---\n# X\n"
	if err := os.WriteFile(filepath.Join(root, dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := skills.Open(root)
	if err == nil || !strings.Contains(err.Error(), "consecutive") {
		t.Fatalf("%v", err)
	}
}

func TestListRespectsContextCancel(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "demo", "name: demo\ndescription: Demo skill for context cancel test", "# D\n")
	l, err := skills.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.List(ctx); err == nil {
		t.Fatal("want canceled")
	}
	if _, err := l.Load(ctx, "demo"); err == nil {
		t.Fatal("want canceled")
	}
}

func TestUnknownSkill(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "demo", "name: demo\ndescription: Demo skill for unknown name tests", "# D\n")
	l, err := skills.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.Load(context.Background(), "nope")
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("%v", err)
	}
}

func TestMetadataAndAllowedToolsOpaque(t *testing.T) {
	root := t.TempDir()
	fm := "name: demo\ndescription: Demo skill with metadata and allowed-tools\nlicense: MIT\nmetadata:\n  author: pulse\n  version: \"1\"\nallowed-tools: Bash(git:*) Read\n"
	writeSkill(t, root, "demo", fm, "# D\n")
	l, err := skills.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := l.List(context.Background())
	if list[0].License != "MIT" || list[0].Metadata["author"] != "pulse" {
		t.Fatalf("%+v", list[0])
	}
	if list[0].AllowedTools != "Bash(git:*) Read" {
		t.Fatalf("allowed-tools should stay opaque: %q", list[0].AllowedTools)
	}
}
