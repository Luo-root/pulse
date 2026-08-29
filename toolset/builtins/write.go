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
		Fn:        e.write,
		Risk:      toolset.RiskReadWrite,
		PreviewFn: e.previewWrite,
	}
}

func (e *env) previewWrite(ctx context.Context, args json.RawMessage) (toolset.Preview, error) {
	if err := ctx.Err(); err != nil {
		return toolset.Preview{}, err
	}
	p, abs, err := e.parseWrite(args)
	if err != nil {
		return toolset.Preview{}, err
	}
	oldText := ""
	op := "create"
	raw, err := os.ReadFile(abs)
	if err == nil {
		oldText = string(raw)
		op = "modify"
	} else if !os.IsNotExist(err) {
		return toolset.Preview{}, err
	}
	return toolset.Preview{
		Kind:    toolset.KindFile,
		Action:  toolset.ActionWrite,
		Subject: abs,
		File:    buildFileChange(op, abs, oldText, p.Content),
	}, nil
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (e *env) parseWrite(args json.RawMessage) (writeArgs, string, error) {
	var p writeArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return writeArgs{}, "", fmt.Errorf("builtins/write: invalid args: %w", err)
	}
	if p.Path == "" {
		return writeArgs{}, "", fmt.Errorf("builtins/write: path is required")
	}
	abs, err := resolveUnderRoot(e.opt.Root, p.Path)
	if err != nil {
		return writeArgs{}, "", err
	}
	if err := confineWrite(e.opt.WriteRoots, abs); err != nil {
		return writeArgs{}, "", err
	}
	return p, abs, nil
}

func (e *env) write(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p, abs, err := e.parseWrite(args)
	if err != nil {
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
