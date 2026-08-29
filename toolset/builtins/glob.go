package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

func (e *env) regGlob() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name:        "glob",
			Description: "Find files by glob pattern under the workspace Root. Results are sorted. Does not apply .gitignore in P0 (explicit).",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "pattern":{"type":"string","description":"Glob pattern (e.g. **/*.go)"},
    "path":{"type":"string","description":"Search root (default workspace Root)"},
    "limit":{"type":"integer","description":"Max paths to return","minimum":1},
    "after":{"type":"string","description":"Exclusive start cursor (last path from previous page)"}
  },
  "required":["pattern"]
}`),
		},
		Fn:   e.glob,
		Risk: toolset.RiskReadonly,
	}
}

func (e *env) glob(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Limit   int    `json:"limit"`
		After   string `json:"after"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("builtins/glob: invalid args: %w", err)
	}
	if strings.TrimSpace(p.Pattern) == "" {
		return "", fmt.Errorf("builtins/glob: pattern is required")
	}
	if p.Path == "" {
		p.Path = "."
	}
	limit := p.Limit
	if limit <= 0 {
		limit = e.opt.GlobLimit
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
		return "", fmt.Errorf("builtins/glob: %w", err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("builtins/glob: path is not a directory")
	}

	var matches []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == "vendor" {
				if path != root {
					return fs.SkipDir
				}
			}
			if err := confineRead(e.opt.Root, e.opt.ForbidRead, path); err != nil {
				if path != root {
					return fs.SkipDir
				}
			}
			return nil
		}
		if err := confineRead(e.opt.Root, e.opt.ForbidRead, path); err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		ok, err := filepath.Match(filepath.ToSlash(p.Pattern), relSlash)
		if err != nil {
			return fmt.Errorf("builtins/glob: bad pattern: %w", err)
		}
		// 也支持 ** 简化：若 pattern 含 **/，用 pathMatch
		if !ok {
			ok = matchGlob(p.Pattern, relSlash)
		}
		if !ok {
			return nil
		}
		matches = append(matches, relSlash)
		return nil
	})
	if err != nil {
		return "", err
	}
	page, next, trunc := pageStrings(matches, p.After, limit)
	if len(page) == 0 {
		return "(no matches)", nil
	}
	out := formatLines(page)
	if trunc {
		out += truncatedTrailer("glob", "after", next, limit)
	}
	return out, nil
}

// matchGlob 支持简单的 ** 通配（非完整 gitignore 语义）。
func matchGlob(pattern, name string) bool {
	pattern = filepath.ToSlash(pattern)
	name = filepath.ToSlash(name)
	if !strings.Contains(pattern, "**") {
		ok, _ := filepath.Match(pattern, name)
		return ok
	}
	// **/*.ext → 任意深度
	parts := strings.Split(pattern, "**")
	if len(parts) != 2 {
		ok, _ := filepath.Match(pattern, name)
		return ok
	}
	prefix := strings.TrimSuffix(parts[0], "/")
	suffix := strings.TrimPrefix(parts[1], "/")
	if prefix != "" && !strings.HasPrefix(name, prefix+"/") && name != prefix {
		return false
	}
	rest := name
	if prefix != "" {
		rest = strings.TrimPrefix(name, prefix+"/")
	}
	if suffix == "" {
		return true
	}
	ok, _ := filepath.Match(suffix, filepath.Base(rest))
	if ok {
		return true
	}
	ok, _ = filepath.Match(suffix, rest)
	return ok
}
