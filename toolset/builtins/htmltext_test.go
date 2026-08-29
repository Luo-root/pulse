package builtins

import "testing"

func TestHTMLToTextSkipsScriptAndBlocks(t *testing.T) {
	got := htmlToText(`<html><body><p>Hello</p><script>secret()</script><p>World</p></body></html>`)
	if got != "Hello\nWorld" {
		t.Fatalf("%q", got)
	}
}

func TestHTMLToTextCJKLastRune(t *testing.T) {
	got := htmlToText(`<html><body><span>中文</span><span>ok</span></body></html>`)
	if got != "中文 ok" {
		t.Fatalf("CJK last rune must not break spacing, got %q", got)
	}
}

func TestLooksLikeHTML(t *testing.T) {
	if !looksLikeHTML("<!DOCTYPE html><html>") || looksLikeHTML("plain text") {
		t.Fatal("looksLikeHTML")
	}
}
