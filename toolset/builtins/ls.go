package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

func (e *env) regLS() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name:        "ls",
			Description: "List directory entries under the workspace Root (sorted). Directories end with /.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"Directory path (default Root)"},
    "depth":{"type":"integer","description":"Max recursion depth (default 1)","minimum":1},
    "limit":{"type":"integer","description":"Max entries to return","minimum":1}
  }
}`),
		},
		Fn:   e.ls,
		Risk: toolset.RiskReadonly,
	}
}

func (e *env) ls(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p struct {
		Path  string `json:"path"`
		Depth int    `json:"depth"`
		Limit int    `json:"limit"`
	}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("builtins/ls: invalid args: %w", err)
		}
	}
	if p.Path == "" {
		p.Path = "."
	}
	if p.Depth <= 0 {
		p.Depth = 1
	}
	limit := p.Limit
	if limit <= 0 {
		limit = e.opt.LSLimit
	}
	abs, err := resolveUnderRoot(e.opt.Root, p.Path)
	if err != nil {
		return "", err
	}
	if err := confineRead(e.opt.Root, e.opt.ForbidRead, abs); err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("builtins/ls: %w", err)
	}
	if !st.IsDir() {
		return "", fmt.Errorf("builtins/ls: not a directory: %s", abs)
	}

	var out []string
	truncated := false
	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(entries))
		for _, ent := range entries {
			names = append(names, ent.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			if truncated {
				return nil
			}
			full := filepath.Join(dir, name)
			if err := confineRead(e.opt.Root, e.opt.ForbidRead, full); err != nil {
				continue
			}
			rel, err := filepath.Rel(abs, full)
			if err != nil {
				rel = full
			}
			rel = filepath.ToSlash(rel)
			info, err := os.Stat(full)
			if err != nil {
				continue
			}
			if info.IsDir() {
				out = append(out, rel+"/")
				if len(out) >= limit {
					truncated = true
					return nil
				}
				if depth < p.Depth {
					if err := walk(full, depth+1); err != nil {
						return err
					}
				}
			} else {
				out = append(out, rel)
				if len(out) >= limit {
					truncated = true
					return nil
				}
			}
		}
		return nil
	}
	if err := walk(abs, 1); err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "(empty directory)", nil
	}
	var b strings.Builder
	for _, line := range out {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&b, "\n[truncated: entry limit %d reached]\n", limit)
	}
	return b.String(), nil
}
