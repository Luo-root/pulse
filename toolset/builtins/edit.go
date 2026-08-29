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
		Fn:        e.edit,
		Risk:      toolset.RiskReadWrite,
		PreviewFn: e.previewEdit,
	}
}

func (e *env) previewEdit(ctx context.Context, args json.RawMessage) (toolset.Preview, error) {
	if err := ctx.Err(); err != nil {
		return toolset.Preview{}, err
	}
	p, abs, err := e.parseEdit(args)
	if err != nil {
		return toolset.Preview{}, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return toolset.Preview{}, err
	}
	next, err := applyEdit(string(raw), p.OldString, p.NewString, p.ReplaceAll)
	if err != nil {
		return toolset.Preview{}, err
	}
	return toolset.Preview{
		Kind:    toolset.KindFile,
		Action:  toolset.ActionWrite,
		Subject: abs,
		File:    buildFileChange("modify", abs, string(raw), next),
	}, nil
}

type editArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (e *env) parseEdit(args json.RawMessage) (editArgs, string, error) {
	var p editArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return editArgs{}, "", fmt.Errorf("builtins/edit: invalid args: %w", err)
	}
	if p.Path == "" {
		return editArgs{}, "", fmt.Errorf("builtins/edit: path is required")
	}
	if p.OldString == p.NewString {
		return editArgs{}, "", fmt.Errorf("builtins/edit: old_string and new_string are identical")
	}
	abs, err := resolveUnderRoot(e.opt.Root, p.Path)
	if err != nil {
		return editArgs{}, "", err
	}
	if err := confineWrite(e.opt.WriteRoots, abs); err != nil {
		return editArgs{}, "", err
	}
	return p, abs, nil
}

func applyEdit(raw, oldString, newString string, replaceAll bool) (string, error) {
	content, isCRLF := toUnix(raw)
	old, neu := oldString, newString
	if isCRLF {
		old = strings.ReplaceAll(old, "\r\n", "\n")
		neu = strings.ReplaceAll(neu, "\r\n", "\n")
	}
	var next string
	if replaceAll {
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
	if isCRLF {
		return strings.ReplaceAll(next, "\n", "\r\n"), nil
	}
	return next, nil
}

func (e *env) edit(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p, abs, err := e.parseEdit(args)
	if err != nil {
		return "", err
	}
	if err := e.requireFreshRead(abs); err != nil {
		return "", err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("builtins/edit: %w", err)
	}
	out, err := applyEdit(string(raw), p.OldString, p.NewString, p.ReplaceAll)
	if err != nil {
		return "", err
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
