package builtins

import (
	"strings"
	"unicode/utf8"

	"github.com/Luo-root/pulse/toolset"
)

const maxDiffBytes = 8192
const maxDiffLines = 400

func buildFileChange(op, path, oldText, newText string) *toolset.FileChange {
	fc := &toolset.FileChange{Op: op, Path: path}
	if strings.IndexByte(oldText, 0) >= 0 || strings.IndexByte(newText, 0) >= 0 {
		fc.Binary = true
		return fc
	}
	fc.Added, fc.Removed, fc.Diff, fc.Truncated = unifiedDiff(oldText, newText)
	return fc
}

func unifiedDiff(oldText, newText string) (added, removed int, diff string, truncated bool) {
	if oldText == newText {
		return 0, 0, "", false
	}
	oldL := splitLines(oldText)
	newL := splitLines(newText)
	if len(oldL)+len(newL) > maxDiffLines {
		a, r := tally(oldL, newL)
		return a, r, "(diff omitted: change too large)\n", true
	}
	pre := 0
	for pre < len(oldL) && pre < len(newL) && oldL[pre] == newL[pre] {
		pre++
	}
	oldEnd, newEnd := len(oldL), len(newL)
	for oldEnd > pre && newEnd > pre && oldL[oldEnd-1] == newL[newEnd-1] {
		oldEnd--
		newEnd--
	}
	var b strings.Builder
	b.WriteString("@@\n")
	for i := pre; i < oldEnd; i++ {
		removed++
		b.WriteString("-")
		b.WriteString(oldL[i])
		b.WriteByte('\n')
	}
	for i := pre; i < newEnd; i++ {
		added++
		b.WriteString("+")
		b.WriteString(newL[i])
		b.WriteByte('\n')
	}
	out := b.String()
	if len(out) > maxDiffBytes {
		head := out[:maxDiffBytes]
		for len(head) > 0 && !utf8.ValidString(head) {
			head = head[:len(head)-1]
		}
		return added, removed, head + "\n(truncated)\n", true
	}
	return added, removed, out, false
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if strings.HasSuffix(s, "\n") {
		s = s[:len(s)-1]
	}
	return strings.Split(s, "\n")
}

func tally(oldL, newL []string) (added, removed int) {
	counts := make(map[string]int, len(oldL))
	for _, l := range oldL {
		counts[l]++
	}
	for _, l := range newL {
		if counts[l] > 0 {
			counts[l]--
		} else {
			added++
		}
	}
	for _, c := range counts {
		removed += c
	}
	return added, removed
}
