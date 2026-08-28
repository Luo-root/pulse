package builtins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

func (e *env) regExec() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name:        "exec",
			Description: "Run a shell command in the workspace. Windows uses PowerShell; Unix uses sh -c. Returns exit_code, duration, and truncated combined output. Dangerous — prefer dedicated tools for file edits.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "command":{"type":"string","description":"Command string"},
    "cwd":{"type":"string","description":"Working directory (default Root; must stay under Root)"},
    "timeout_seconds":{"type":"integer","description":"Timeout seconds (default from Options)","minimum":1}
  },
  "required":["command"]
}`),
		},
		Fn:   e.execCmd,
		Risk: toolset.RiskDangerous,
	}
}

func (e *env) execCmd(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p struct {
		Command        string `json:"command"`
		Cwd            string `json:"cwd"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("builtins/exec: invalid args: %w", err)
	}
	if strings.TrimSpace(p.Command) == "" {
		return "", fmt.Errorf("builtins/exec: command is required")
	}
	cwd := e.opt.Root
	if p.Cwd != "" {
		abs, err := resolveUnderRoot(e.opt.Root, p.Cwd)
		if err != nil {
			return "", err
		}
		if err := confineRead(e.opt.Root, nil, abs); err != nil {
			return "", fmt.Errorf("builtins/exec: cwd %w", err)
		}
		cwd = abs
	}
	timeout := e.opt.ExecTimeout
	if p.TimeoutSeconds > 0 {
		timeout = time.Duration(p.TimeoutSeconds) * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := buildShellCommand(runCtx, p.Command)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	dur := time.Since(start)

	exitCode := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else if runCtx.Err() != nil {
			return "", fmt.Errorf("builtins/exec: timeout after %s: %w", timeout, runCtx.Err())
		} else {
			return "", fmt.Errorf("builtins/exec: %w", err)
		}
	}

	out := stdout.String()
	errOut := stderr.String()
	combined := out
	if errOut != "" {
		if combined != "" {
			combined += "\n"
		}
		combined += errOut
	}
	combined, trunc := truncateHeadTail(combined, e.opt.MaxExecBytes)

	var b strings.Builder
	fmt.Fprintf(&b, "exit_code=%d duration=%s cwd=%s\n", exitCode, dur.Round(time.Millisecond), cwd)
	if trunc {
		fmt.Fprintf(&b, "truncated=true max_bytes=%d\n", e.opt.MaxExecBytes)
	}
	b.WriteString("---\n")
	b.WriteString(combined)
	if combined != "" && !strings.HasSuffix(combined, "\n") {
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func truncateHeadTail(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	half := max / 2
	// 尽量按 rune 边界切
	head := s
	if len(head) > half {
		head = s[:half]
		for len(head) > 0 && !utf8.ValidString(head) {
			head = head[:len(head)-1]
		}
	}
	tail := s[len(s)-half:]
	for len(tail) > 0 && !utf8.ValidString(tail) {
		tail = tail[1:]
	}
	return head + "\n\n...[truncated]...\n\n" + tail, true
}
