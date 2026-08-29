package builtins

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// pageStrings 对已收集集合：稳定排序 → after 游标之后 → 切 limit。
// next 为当前页最后一项（供 after）；空表示没有更多。
func pageStrings(all []string, after string, limit int) (page []string, next string, truncated bool) {
	sort.Strings(all)
	start := 0
	if after != "" {
		for start < len(all) && all[start] <= after {
			start++
		}
	}
	if start >= len(all) {
		return nil, "", false
	}
	end := start + limit
	if end < len(all) {
		page = append([]string(nil), all[start:end]...)
		return page, page[len(page)-1], true
	}
	page = append([]string(nil), all[start:]...)
	return page, "", false
}

func truncatedTrailer(kind, afterKey, next string, limit int) string {
	if next == "" {
		return ""
	}
	return fmt.Sprintf("\n[truncated: %s limit %d; pass %s=%q to continue]\n",
		kind, limit, afterKey, next)
}

func formatLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// pageByOffset 对已切好的行：从 offset 起最多 limit 条。more 表示还有后续行。
func pageByOffset(lines []string, offset, limit int) (page []string, more bool) {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || offset >= len(lines) {
		return nil, false
	}
	end := offset + limit
	if end < len(lines) {
		return lines[offset:end], true
	}
	return lines[offset:], false
}

// clipLine 超 MaxLineRunes 时截断并加省略号，与 read 同口径。
func clipLine(s string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "…"
}

func clipLines(lines []string, maxRunes int) {
	for i, s := range lines {
		lines[i] = clipLine(s, maxRunes)
	}
}
