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

// unifiedDiff 同时产出计数与 diff 文本；两者共用同一份口径（由 trimCommon
// 给出的位置区间），大文件只是不生成文本。
//
// 计数口径 = 共同前缀 / 共同后缀之外的位置区间：旧行计 removed、新行计
// added。**不按多重集计数**——位移、重排这类变更的行集合前后相同，多重集
// 会算出 0/0，而 Added/Removed 是 HITL 卡片上的 `+N/-M`（toolset.Preview
// 的 FileChange），任何「按改动量升级审批」的策略（if card.File.Added > 50
// { 问人 }）在**恰好大改动**时读到 0 就成了 fail-open。
func unifiedDiff(oldText, newText string) (added, removed int, diff string, truncated bool) {
	if oldText == newText {
		return 0, 0, "", false
	}
	oldL := splitLines(oldText)
	newL := splitLines(newText)
	pre, oldEnd, newEnd := trimCommon(oldL, newL)
	removed = oldEnd - pre
	added = newEnd - pre
	if len(oldL)+len(newL) > maxDiffLines {
		// 只截断文本，不换算法：换算法会让「整体位移」报成 0。
		return added, removed, "(diff omitted: change too large)\n", true
	}
	var b strings.Builder
	b.WriteString("@@\n")
	for i := pre; i < oldEnd; i++ {
		b.WriteString("-")
		b.WriteString(oldL[i])
		b.WriteByte('\n')
	}
	for i := pre; i < newEnd; i++ {
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

// trimCommon 返回共同前缀的结束位置与两侧各自的结束位置：区间
// [pre, oldEnd) / [pre, newEnd) 之外的行两侧完全相同（含中间那段共同后缀）。
func trimCommon(oldL, newL []string) (pre, oldEnd, newEnd int) {
	for pre < len(oldL) && pre < len(newL) && oldL[pre] == newL[pre] {
		pre++
	}
	oldEnd, newEnd = len(oldL), len(newL)
	for oldEnd > pre && newEnd > pre && oldL[oldEnd-1] == newL[newEnd-1] {
		oldEnd--
		newEnd--
	}
	return pre, oldEnd, newEnd
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
