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
			Description: "Run a shell command in the workspace. Windows uses PowerShell; Unix uses sh -c. Returns exit_code, duration, and truncated combined output. With background=true the command starts a long-running job: returns job_id for job_output / job_kill and is not subject to timeout. Dangerous — prefer dedicated tools for file edits.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "command":{"type":"string","description":"Command string"},
    "cwd":{"type":"string","description":"Working directory (default Root; must stay under Root)"},
    "timeout_seconds":{"type":"integer","description":"Timeout seconds (default from Options)","minimum":1},
    "background":{"type":"boolean","description":"Run as background job (no timeout); returns job_id"}
  },
  "required":["command"]
}`),
		},
		Fn:        e.execCmd,
		Risk:      toolset.RiskDangerous,
		PreviewFn: e.previewExec,
	}
}

// execArgs 是 exec 的统一参数（preview 与 execute 共用一份解析）。
type execArgs struct {
	Command        string `json:"command"`
	Cwd            string `json:"cwd"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Background     bool   `json:"background"`
}

func parseExecArgs(args json.RawMessage) (execArgs, error) {
	var p execArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return execArgs{}, fmt.Errorf("builtins/exec: invalid args: %w", err)
	}
	if strings.TrimSpace(p.Command) == "" {
		return execArgs{}, fmt.Errorf("builtins/exec: command is required")
	}
	return p, nil
}

func (e *env) resolveExecCwd(p execArgs) (string, error) {
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
	return cwd, nil
}

func (p execArgs) timeout(defaultTimeout time.Duration) time.Duration {
	if p.TimeoutSeconds > 0 {
		return time.Duration(p.TimeoutSeconds) * time.Second
	}
	return defaultTimeout
}

func (e *env) previewExec(ctx context.Context, args json.RawMessage) (toolset.Preview, error) {
	if err := ctx.Err(); err != nil {
		return toolset.Preview{}, err
	}
	p, err := parseExecArgs(args)
	if err != nil {
		return toolset.Preview{}, err
	}
	cwd, err := e.resolveExecCwd(p)
	if err != nil {
		return toolset.Preview{}, err
	}
	timeoutText := p.timeout(e.opt.ExecTimeout).String()
	if p.Background {
		timeoutText = "background (no timeout)"
	}
	return toolset.Preview{
		Kind:    toolset.KindCommand,
		Action:  toolset.ActionExecute,
		Subject: p.Command,
		Command: &toolset.CommandChange{Command: p.Command, Cwd: cwd, Timeout: timeoutText},
	}, nil
}

func (e *env) execCmd(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p, err := parseExecArgs(args)
	if err != nil {
		return "", err
	}
	cwd, err := e.resolveExecCwd(p)
	if err != nil {
		return "", err
	}
	if p.Background {
		j, err := e.jobs.launch(ctx, p.Command, cwd)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("job_id=%s started (background; read output with job_output, stop with job_kill)\ncommand: %s\ncwd: %s\n",
			j.id, p.Command, cwd), nil
	}
	timeout := p.timeout(e.opt.ExecTimeout)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := buildShellCommand(runCtx, p.Command)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err = cmd.Run()
	dur := time.Since(start)

	exitCode := 0
	if err != nil {
		if runCtx.Err() != nil {
			return "", fmt.Errorf("builtins/exec: timeout after %s: %w", timeout, runCtx.Err())
		}
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
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
