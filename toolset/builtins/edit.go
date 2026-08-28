package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

func (e *env) regEdit() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name:        "edit",
			Description: "Replace text in an existing file. Default requires a unique old_string match. You must read the file first in this process. Set replace_all to replace every occurrence.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string"},
    "old_string":{"type":"string"},
    "new_string":{"type":"string"},
    "replace_all":{"type":"boolean"}
  },
  "required":["path","old_string","new_string"]
}`),
		},
		Fn:   e.edit,
		Risk: toolset.RiskReadWrite,
	}
}

func (e *env) edit(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p struct {
		Path       string `json:"path"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("builtins/edit: invalid args: %w", err)
	}
	if p.Path == "" {
		return "", fmt.Errorf("builtins/edit: path is required")
	}
	if p.OldString == p.NewString {
		return "", fmt.Errorf("builtins/edit: old_string and new_string are identical")
	}
	abs, err := resolveUnderRoot(e.opt.Root, p.Path)
	if err != nil {
		return "", err
	}
	if err := confineWrite(e.opt.WriteRoots, abs); err != nil {
		return "", err
	}
	if err := e.requireFreshRead(abs); err != nil {
		return "", err
	}

	raw, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("builtins/edit: %w", err)
	}
	content, isCRLF := toUnix(string(raw))
	old := p.OldString
	neu := p.NewString
	if isCRLF {
		old = strings.ReplaceAll(old, "\r\n", "\n")
		neu = strings.ReplaceAll(neu, "\r\n", "\n")
	}

	var next string
	if p.ReplaceAll {
		if !strings.Contains(content, old) {
			return "", fmt.Errorf("builtins/edit: old_string not found")
		}
		next = strings.ReplaceAll(content, old, neu)
	} else {
		idx := strings.Index(content, old)
		if idx < 0 {
			return "", fmt.Errorf("builtins/edit: old_string not found — read again and provide more unique context")
		}
		if strings.LastIndex(content, old) != idx {
			return "", fmt.Errorf("builtins/edit: old_string matches multiple times; provide more context or set replace_all=true")
		}
		next = content[:idx] + neu + content[idx+len(old):]
	}
	if next == content {
		return "", fmt.Errorf("builtins/edit: no changes")
	}
	out := next
	if isCRLF {
		out = strings.ReplaceAll(next, "\n", "\r\n")
	}
	if err := os.WriteFile(abs, []byte(out), 0o644); err != nil {
		return "", fmt.Errorf("builtins/edit: write: %w", err)
	}
	e.tracker.mark(abs, time.Now())
	return fmt.Sprintf("edited %s", abs), nil
}

func (e *env) requireFreshRead(abs string) error {
	last, ok := e.tracker.last(abs)
	if !ok {
		return fmt.Errorf("builtins: file must be read before edit/write: %s", abs)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("builtins: %w", err)
	}
	mod := st.ModTime().Truncate(time.Second)
	if mod.After(last.Truncate(time.Second)) {
		return fmt.Errorf("builtins: file modified since last read (mtime=%s last_read=%s): %s",
			mod.Format(time.RFC3339), last.Format(time.RFC3339), abs)
	}
	return nil
}

func toUnix(s string) (string, bool) {
	if strings.Contains(s, "\r\n") {
		return strings.ReplaceAll(s, "\r\n", "\n"), true
	}
	return s, false
}
