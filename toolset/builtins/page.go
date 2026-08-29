package builtins

import (
	"fmt"
	"sort"
	"strings"
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
