package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

func (e *env) regWrite() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name:        "write",
			Description: "Create a new file or overwrite an entire file with content. Overwriting an existing file requires a prior read in this process (stale-read guard).",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string"},
    "content":{"type":"string"}
  },
  "required":["path","content"]
}`),
		},
		Fn:   e.write,
		Risk: toolset.RiskReadWrite,
	}
}

func (e *env) write(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("builtins/write: invalid args: %w", err)
	}
	if p.Path == "" {
		return "", fmt.Errorf("builtins/write: path is required")
	}
	abs, err := resolveUnderRoot(e.opt.Root, p.Path)
	if err != nil {
		return "", err
	}
	if err := confineWrite(e.opt.WriteRoots, abs); err != nil {
		return "", err
	}

	_, err = os.Stat(abs)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("builtins/write: %w", err)
	}
	if exists {
		if err := e.requireFreshRead(abs); err != nil {
			return "", err
		}
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("builtins/write: mkdir: %w", err)
	}
	if err := os.WriteFile(abs, []byte(p.Content), 0o644); err != nil {
		return "", fmt.Errorf("builtins/write: %w", err)
	}
	e.tracker.mark(abs, time.Now())
	if exists {
		return fmt.Sprintf("overwrote %s (%d bytes)", abs, len(p.Content)), nil
	}
	return fmt.Sprintf("created %s (%d bytes)", abs, len(p.Content)), nil
}
