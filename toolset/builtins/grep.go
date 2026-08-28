package builtins

import (
	"bufio"
	"context"
	"encoding/json"
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
			Description: "Search file contents with a Go regexp under the workspace Root. Invalid patterns return an error (not empty matches). P0 does not apply .gitignore.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "pattern":{"type":"string","description":"Go regexp"},
    "path":{"type":"string","description":"File or directory to search (default Root)"},
    "glob":{"type":"string","description":"Optional filename filter (e.g. *.go)"},
    "limit":{"type":"integer","description":"Max matches","minimum":1},
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

	type hit struct {
		file string
		line int
		text string
	}
	var hits []hit
	truncated := false

	searchFile := func(path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := confineRead(e.opt.Root, e.opt.ForbidRead, path); err != nil {
			return nil
		}
		if p.Glob != "" {
			base := filepath.Base(path)
			ok, err := filepath.Match(p.Glob, base)
			if err != nil {
				return fmt.Errorf("builtins/grep: bad glob filter: %w", err)
			}
			if !ok {
				return nil
			}
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			text := sc.Text()
			if !re.MatchString(text) {
				continue
			}
			rel, err := filepath.Rel(e.opt.Root, path)
			if err != nil {
				rel = path
			}
			hits = append(hits, hit{file: filepath.ToSlash(rel), line: lineNo, text: text})
			if len(hits) >= limit {
				truncated = true
				return fs.SkipAll
			}
		}
		return nil
	}

	if !st.IsDir() {
		_ = searchFile(root)
	} else {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				base := d.Name()
				if path != root && (base == ".git" || base == "node_modules" || base == "vendor") {
					return fs.SkipDir
				}
				return nil
			}
			return searchFile(path)
		})
	}

	if len(hits) == 0 {
		return "(no matches)", nil
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "%s:%d:%s\n", h.file, h.line, h.text)
	}
	if truncated {
		fmt.Fprintf(&b, "\n[truncated: match limit %d]\n", limit)
	}
	return b.String(), nil
}
