package builtins

import (
	"strings"
	"testing"
)

func TestApplyBlocksSequentialNoOverlap(t *testing.T) {
	old := []string{"x", "m", "x"}
	blocks := [][]patchLine{
		{{op: '-', text: "x"}, {op: '+', text: "y"}},
		{{op: '-', text: "x"}, {op: '+', text: "z"}},
	}
	out, added, removed, err := applyBlocks(old, blocks, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(out, "\n") != "y\nm\nz" {
		t.Fatalf("out=%v", out)
	}
	if added != 2 || removed != 2 {
		t.Fatalf("added=%d removed=%d", added, removed)
	}
}

func TestApplyBlocksContextAndKeeps(t *testing.T) {
	old := []string{"a", "b", "c", "d"}
	blocks := [][]patchLine{
		{{op: ' ', text: "b"}, {op: '-', text: "c"}, {op: '+', text: "C"}, {op: '+', text: "C2"}},
	}
	out, added, removed, err := applyBlocks(old, blocks, "f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(out, "\n") != "a\nb\nC\nC2\nd" {
		t.Fatalf("out=%v", out)
	}
	if added != 2 || removed != 1 {
		t.Fatalf("added=%d removed=%d", added, removed)
	}
}

func TestApplyBlocksNotFound(t *testing.T) {
	_, _, _, err := applyBlocks([]string{"a"}, [][]patchLine{
		{{op: '-', text: "zzz"}},
	}, "f.txt")
	if err == nil || !strings.Contains(err.Error(), "context not found") {
		t.Fatalf("err=%v", err)
	}
}

func TestParseV4AErrors(t *testing.T) {
	cases := []struct {
		name  string
		patch string
		want  string
	}{
		{"no begin", "*** Add File: a\n+x", "*** Begin Patch"},
		{"no end", "*** Begin Patch\n*** Add File: a\n+x", "*** End Patch"},
		{"unknown directive", "*** Begin Patch\n*** Foo\n*** End Patch", "unsupported directive"},
		{"move to", "*** Begin Patch\n*** Update File: a\n*** Move to: b\n*** End Patch", "Move to"},
		{"add without lines", "*** Begin Patch\n*** Add File: a\n*** End Patch", "no lines"},
		{"add bare line", "*** Begin Patch\n*** Add File: a\nx\n*** End Patch", "must be lines prefixed"},
		{"update without blocks", "*** Begin Patch\n*** Update File: a\n*** End Patch", "no change blocks"},
		{"block without anchor", "*** Begin Patch\n*** Update File: a\n+only add\n*** End Patch", "no context anchor"},
		{"no sections", "*** Begin Patch\n*** End Patch", "no file sections"},
		{"directive without path", "*** Begin Patch\n*** Add File:\n*** End Patch", "needs a path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseV4A(c.patch)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q in error, got %v", c.want, err)
			}
		})
	}
}

func TestParseV4AMinimalAdd(t *testing.T) {
	ops, err := parseV4A("*** Begin Patch\n*** Add File: a/b.txt\n+hi\n+  spaced\n*** End Patch")
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].kind != "add" || ops[0].path != "a/b.txt" {
		t.Fatalf("ops=%+v", ops)
	}
	if strings.Join(ops[0].added, "\n") != "hi\n  spaced" {
		t.Fatalf("added=%q", ops[0].added)
	}
}
