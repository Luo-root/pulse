package builtins

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

func (e *env) regGrep() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name:        "grep",
			Description: "Search file contents with a Go regexp under the workspace Root. Invalid patterns return an error (not empty matches). Skips .git / node_modules / vendor directories by name; does not otherwise apply .gitignore (P0). Use after to page.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "pattern":{"type":"string","description":"Go regexp"},
    "path":{"type":"string","description":"File or directory to search (default Root)"},
    "glob":{"type":"string","description":"Optional filename filter (e.g. *.go)"},
    "limit":{"type":"integer","description":"Max matches","minimum":1},
    "after":{"type":"string","description":"Exclusive start cursor (last file:line from previous page)"},
    "case_insensitive":{"type":"boolean"}
  },
  "required":["pattern"]
}`),
		},
		Fn:   e.grep,
		Risk: toolset.RiskReadonly,
	}
}

func (e *env) grep(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p struct {
		Pattern         string `json:"pattern"`
		Path            string `json:"path"`
		Glob            string `json:"glob"`
		Limit           int    `json:"limit"`
		After           string `json:"after"`
		CaseInsensitive bool   `json:"case_insensitive"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("builtins/grep: invalid args: %w", err)
	}
	if strings.TrimSpace(p.Pattern) == "" {
		return "", fmt.Errorf("builtins/grep: pattern is required")
	}
	pat := p.Pattern
	if p.CaseInsensitive {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return "", fmt.Errorf("builtins/grep: invalid regexp: %w", err)
	}
	if p.Path == "" {
		p.Path = "."
	}
	// glob 过滤器前置校验：非法 pattern 必须报错（工具描述承诺），不能等到
	// 遍历某个文件时让错误被吞成 (no matches)（与 glob 工具行为对齐）。
	if p.Glob != "" {
		if _, err := filepath.Match(p.Glob, ""); err != nil {
			return "", fmt.Errorf("builtins/grep: bad glob filter: %w", err)
		}
	}
	limit := p.Limit
	if limit <= 0 {
		limit = e.opt.GrepLimit
	}
	root, err := resolveUnderRoot(e.opt.Root, p.Path)
	if err != nil {
		return "", err
	}
	if err := confineRead(e.opt.Root, e.opt.ForbidRead, root); err != nil {
		return "", err
	}

	st, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("builtins/grep: %w", err)
	}

	// relOf 输出用相对 Root 的斜杠路径（命中行前缀与跳过清单同口径）。
	relOf := func(path string) string {
		rel, err := filepath.Rel(e.opt.Root, path)
		if err != nil {
			rel = path
		}
		return filepath.ToSlash(rel)
	}

	var keys []string
	var skipped []skippedPath
	searchFile := func(path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := confineRead(e.opt.Root, e.opt.ForbidRead, path); err != nil {
			// 策略排除（Root 外 / forbid-read）：静默跳过，与 glob 同口径。
			return nil
		}
		if p.Glob != "" {
			// pattern 已在前置校验里确认合法。
			if ok, _ := filepath.Match(p.Glob, filepath.Base(path)); !ok {
				return nil
			}
		}
		relSlash := relOf(path)
		f, err := os.Open(path)
		if err != nil {
			// 打不开（权限 / 半删除 / 特殊文件）也是「没搜」，如实记下。
			skipped = append(skipped, skippedPath{rel: relSlash, err: err})
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), grepScanMax)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			text := sc.Text()
			if !re.MatchString(text) {
				continue
			}
			// 命中行按单行上限截断（与 read / web_fetch 同口径）：单条命中可达
			// 1 MiB，默认 100 条上限足以把 MB 级文本塞进上下文。
			keys = append(keys, fmt.Sprintf("%s:%d:%s", relSlash, lineNo, clipLine(text, e.opt.MaxLineRunes)))
		}
		if err := sc.Err(); err != nil {
			// 超长行会让 Scanner 在此停止：**该行之后的匹配全部消失**。不能静默
			// （模型会把「没搜到」当成「不存在」），也不吃掉整次搜索——归入跳过
			// 清单，由尾部汇总。
			if errors.Is(err, bufio.ErrTooLong) {
				err = fmt.Errorf("a line exceeds %d bytes; matches after it were not searched", grepScanMax)
			}
			skipped = append(skipped, skippedPath{rel: relSlash, err: err})
		}
		return nil
	}

	if !st.IsDir() {
		if err := searchFile(root); err != nil {
			return "", err
		}
	} else {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// 目录不可读等：如实记下（否则其中的命中会静默消失），继续遍历。
				skipped = append(skipped, skippedPath{rel: relOf(path), err: err})
				return nil
			}
			if d.IsDir() {
				base := d.Name()
				if path != root && skippedDirNames[base] {
					return fs.SkipDir
				}
				return nil
			}
			return searchFile(path)
		})
		if err != nil {
			return "", err
		}
	}

	page, next, trunc := pageStrings(keys, p.After, limit)
	if len(page) == 0 {
		return "(no matches)" + skippedTrailer("grep", skipped), nil
	}
	out := formatLines(page)
	if trunc {
		out += truncatedTrailer("grep", "after", next, limit)
	}
	return out + skippedTrailer("grep", skipped), nil
}

// grepScanMax 是逐行扫描的单行上限：超过它的行会让 bufio.Scanner 停止扫描，
// 该行之后的匹配全部消失，因此必须按「跳过该文件」如实上报。
const grepScanMax = 1024 * 1024

// maxSkippedDetail 是尾部跳过清单的明细条数上限，超出只报数量。
const maxSkippedDetail = 5

// skippedPath 记录本次搜索被跳过的路径（文件打不开 / 单行超长 / 目录不可读）
// 与原因：个别路径不终止整次搜索，但必须在结果尾部报出——「搜不到」和「没搜」
// 对模型是两回事。
type skippedPath struct {
	rel string
	err error
}

// skippedTrailer 汇总被跳过的路径；无跳过时返回空串。
func skippedTrailer(kind string, skipped []skippedPath) string {
	if len(skipped) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n[%s: skipped %d path(s) it could not search; matches inside them are unknown]\n",
		kind, len(skipped))
	for i, sk := range skipped {
		if i == maxSkippedDetail {
			fmt.Fprintf(&b, "  … and %d more\n", len(skipped)-maxSkippedDetail)
			break
		}
		fmt.Fprintf(&b, "  %s: %v\n", sk.rel, sk.err)
	}
	return b.String()
}
