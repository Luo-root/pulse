package builtins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

// execIOWait 是命令退出后等 I/O 管道收尾的上限（exec.Cmd.WaitDelay）：脱离进程
// 组的后代若顶着 stdout/stderr 不退，Wait 不再无限期挂着。
const execIOWait = 5 * time.Second

// defaultChildEnvKeys 是 exec / 后台 job 子进程的默认继承白名单：常见命令
// 运行必需的平台键（产品参数，不是行业标准）。宿主用 Options.ExecEnv 追加，
// 用 Options.ExecEnvInheritAll 显式恢复全量继承。
var defaultChildEnvKeys = []string{
	"PATH",
	"HOME",
	"USER",
	"USERNAME",
	"LANG",
	"LC_ALL",
	"TMPDIR",
	"TEMP",
	"TMP",
	// Windows 常用键（go / git 等工具链依赖）。
	"SYSTEMROOT",
	"SYSTEMDRIVE",
	"COMSPEC",
	"PATHEXT",
	"APPDATA",
	"LOCALAPPDATA",
	"PROGRAMFILES",
	"PROGRAMFILES(X86)",
}

// environ 是 os.Environ 的包级别名（测试注入点）。
var environ = os.Environ

// childEnv 构造子进程环境。白名单模式（默认）：只继承
// defaultChildEnvKeys + Options.ExecEnv 列出的键，值取自宿主当前环境；
// 返回值恒非 nil（白名单一个键都不命中时是空环境，绝不静默退回继承
// 全量）。ExecEnvInheritAll 时返回 nil——os/exec 的 nil Env 语义即继承
// 全量。exec 是命令逃逸口（shell 命令的文件访问不受 confineRead 管），
// 默认白名单保证宿主 secret 不随环境泄漏进模型驱动的命令。
func childEnv(opt Options) []string {
	if opt.ExecEnvInheritAll {
		return nil
	}
	allow := make(map[string]struct{}, len(defaultChildEnvKeys)+len(opt.ExecEnv))
	for _, k := range defaultChildEnvKeys {
		allow[k] = struct{}{}
	}
	for _, k := range opt.ExecEnv {
		if k == "" || strings.Contains(k, "=") {
			continue
		}
		allow[k] = struct{}{}
	}
	env := []string{}
	for _, kv := range environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := allow[k]; ok {
			env = append(env, kv)
		}
	}
	return env
}

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
		j, err := e.jobs.launch(ctx, p.Command, cwd, childEnv(e.opt))
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
	cmd.Env = childEnv(e.opt)
	// 前台命令也要整树收尾：默认 Cancel 只杀包装 shell（powershell / sh），它拉起
	// 的子进程会成孤儿，继续占端口 / 工作区。整树杀不可用的平台（killTree 报错）
	// 退回默认的只杀直接子进程。
	cmd.Cancel = func() error {
		if err := killTree(cmd.Process.Pid); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	// 杀完之后不再无限等 I/O：孙进程若顶着管道赖着不退，Wait 在 execIOWait 后
	// 关闭管道并返回 ErrWaitDelay，工具调用不会挂在超时之后。
	cmd.WaitDelay = execIOWait
	setupProcessTree(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err = cmd.Run()
	dur := time.Since(start)

	exitCode := 0
	stalledIO := false
	if err != nil {
		if ctxErr := runCtx.Err(); ctxErr != nil {
			// 取消与超时是两回事：取消来自宿主（用户按停 / 上层放弃），超时才是
			// 我们打的 deadline。分类只看 ctx 错误链，别用「err 是否匹配 ctxErr」
			// 反推——两种情形的错误链都可能带上 ctxErr。
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				return "", fmt.Errorf("builtins/exec: timeout after %s (process tree killed): %w", timeout, ctxErr)
			}
			return "", fmt.Errorf("builtins/exec: canceled (process tree killed): %w", ctxErr)
		}
		var ee *exec.ExitError
		stalledIO = errors.Is(err, exec.ErrWaitDelay)
		switch {
		case errors.As(err, &ee):
			exitCode = ee.ExitCode()
		case stalledIO:
			// 进程已退出、没拿到退出码：只是有子进程顶着 I/O 管道不放。
		default:
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
	if stalledIO {
		fmt.Fprintf(&b, "\n[note: the command exited but its I/O pipes stayed open for %s — a detached child may still be running]\n", execIOWait)
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
