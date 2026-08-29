package builtins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/toolset"
)

// jobKillWait 是 job_kill 发出信号后等进程退出的上限。
const jobKillWait = 5 * time.Second

// job 是一个后台命令：合流输出环形缓冲 + 退出状态。
type job struct {
	id       string
	command  string
	cmd      *exec.Cmd
	cancel   context.CancelFunc
	waitCh   chan struct{} // 关闭表示 Wait 已返回

	mu       sync.Mutex
	buf      []byte
	dropped  int // 环形丢头的字节数（全局偏移基准）
	done     bool
	killed   bool
	exitCode int
}

func (j *job) running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return !j.done
}

// append 追加输出；超环形上限丢头（对齐 rune 起点），dropped 记全局基准。
func (j *job) append(p []byte, bufMax int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.buf = append(j.buf, p...)
	if bufMax > 0 && len(j.buf) > bufMax {
		cut := len(j.buf) - bufMax
		for cut < len(j.buf) && !utf8.RuneStart(j.buf[cut]) {
			cut++
		}
		j.dropped += cut
		j.buf = j.buf[cut:]
	}
}

// read 返回 [offset, offset+limit) 的输出窗口。offset 是全局字节偏移；
// 落在丢弃窗口内时从可用处继续。返回 clipped=offset 被窗口截断。
func (j *job) read(offset, limit int) (data []byte, clipped bool, total int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	total = j.dropped + len(j.buf)
	start := offset
	if start < j.dropped {
		start = j.dropped
	}
	clipped = start != offset
	rel := start - j.dropped
	if rel > len(j.buf) {
		rel = len(j.buf)
	}
	end := rel + limit
	if end > len(j.buf) {
		end = len(j.buf)
	}
	return j.buf[rel:end], clipped, total
}

// status 返回状态快照。
func (j *job) status() (running bool, killed bool, exitCode int, total int, dropped int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return !j.done, j.killed, j.exitCode, j.dropped + len(j.buf), j.dropped
}

// jobWriter 把进程输出写进 job 环形缓冲（stdout/stderr 共用，交错了不保证）。
type jobWriter struct {
	j     *job
	bufMax int
}

func (w jobWriter) Write(p []byte) (int, error) {
	w.j.append(p, w.bufMax)
	return len(p), nil
}

// jobTable 是 env 内的后台 job 注册表。done job 超过 2*max 时按创建序淘汰最旧。
type jobTable struct {
	mu     sync.Mutex
	seq    int
	max    int
	bufMax int
	jobs   map[string]*job
	order  []string // 创建序（含已 remove 的残留，reap 时跳过）
}

func newJobTable(max, bufMax int) *jobTable {
	if max <= 0 {
		max = DefaultMaxJobs
	}
	if bufMax <= 0 {
		bufMax = DefaultMaxExecBytes
	}
	return &jobTable{max: max, bufMax: bufMax, jobs: make(map[string]*job)}
}

func (t *jobTable) get(id string) (*job, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, ok := t.jobs[id]
	return j, ok
}

func (t *jobTable) remove(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.jobs, id)
	for i, x := range t.order {
		if x == id {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
}

// reapDoneLocked 在 t.mu 内调用（锁序 t.mu → j.mu）：done job 超过
// 2*max 时按创建序淘汰最旧，防止长命宿主下 job 表无界增长。
func (t *jobTable) reapDoneLocked() {
	limit := 2 * t.max
	for {
		doneCount := 0
		oldest := ""
		for _, id := range t.order {
			j, ok := t.jobs[id]
			if !ok {
				continue
			}
			if !j.running() {
				doneCount++
				if oldest == "" {
					oldest = id
				}
			}
		}
		if doneCount <= limit {
			return
		}
		delete(t.jobs, oldest)
		for i, id := range t.order {
			if id == oldest {
				t.order = append(t.order[:i], t.order[i+1:]...)
				break
			}
		}
	}
}

// launch 启动后台命令：生命周期不绑请求 ctx（WithoutCancel），绑 kernel dispose。
func (t *jobTable) launch(ctx context.Context, command, cwd string) (*job, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	cmd := buildShellCommand(runCtx, command)
	cmd.Dir = cwd
	setupBackgroundProcess(cmd)

	t.mu.Lock()
	running := 0
	for _, j := range t.jobs {
		if j.running() {
			running++
		}
	}
	if running >= t.max {
		t.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("builtins/exec: too many running background jobs (max=%d); kill or wait for one", t.max)
	}
	t.seq++
	j := &job{id: "j" + strconv.Itoa(t.seq), command: command, cmd: cmd, cancel: cancel, waitCh: make(chan struct{})}
	t.jobs[j.id] = j
	t.order = append(t.order, j.id)
	t.reapDoneLocked()
	t.mu.Unlock()

	w := jobWriter{j: j, bufMax: t.bufMax}
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Start(); err != nil {
		cancel()
		t.remove(j.id)
		return nil, fmt.Errorf("builtins/exec: start background: %w", err)
	}
	go func() {
		err := cmd.Wait()
		j.mu.Lock()
		j.done = true
		switch {
		case j.killed:
			j.exitCode = -1
		case err == nil:
			j.exitCode = 0
		default:
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				j.exitCode = ee.ExitCode()
			} else {
				j.exitCode = -1
			}
		}
		close(j.waitCh)
		j.mu.Unlock()
	}()
	return j, nil
}

// kill 整树杀并等待进程真正退出；已退出的 job 报错。
func (t *jobTable) kill(id string) (*job, error) {
	j, ok := t.get(id)
	if !ok {
		return nil, fmt.Errorf("builtins/job_kill: no such job: %s", id)
	}
	j.mu.Lock()
	if j.done {
		code := j.exitCode
		j.mu.Unlock()
		return nil, fmt.Errorf("builtins/job_kill: job %s already exited (exit_code=%d)", id, code)
	}
	j.killed = true
	pid := j.cmd.Process.Pid
	j.mu.Unlock()

	if err := killTree(pid); err != nil {
		j.cancel() // 树杀失败兜底：只杀包装 shell
	}
	select {
	case <-j.waitCh:
	case <-time.After(jobKillWait):
		return nil, fmt.Errorf("builtins/job_kill: kill signal sent to %s but process did not exit within %s", id, jobKillWait)
	}
	return j, nil
}

// killAll 杀掉全部活 job（dispose / scope Dispose 用）；不等退出，watcher 自行收尾。
func (t *jobTable) killAll() {
	t.mu.Lock()
	alive := make([]*job, 0, len(t.jobs))
	for _, j := range t.jobs {
		if !j.done {
			alive = append(alive, j)
		}
	}
	t.mu.Unlock()
	for _, j := range alive {
		j.mu.Lock()
		j.killed = true
		pid := j.cmd.Process.Pid
		j.mu.Unlock()
		if err := killTree(pid); err != nil {
			j.cancel()
		}
	}
}

func (e *env) regJobOutput() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name:        "job_output",
			Description: "Read incremental combined output of a background job (exec with background=true). offset is a global byte offset — continue from the trailer. Reports running/exited/killed status and exit code.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "id":{"type":"string","description":"Job id returned by exec (background=true)"},
    "offset":{"type":"integer","description":"Global byte offset into the job output (default 0)","minimum":0},
    "limit":{"type":"integer","description":"Max bytes to return (default from Options)","minimum":1}
  },
  "required":["id"]
}`),
		},
		Fn:        e.jobOutput,
		Risk:      toolset.RiskReadonly,
		PreviewFn: e.previewJobOutput,
	}
}

func (e *env) previewJobOutput(_ context.Context, args json.RawMessage) (toolset.Preview, error) {
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolset.Preview{}, fmt.Errorf("builtins/job_output: invalid args: %w", err)
	}
	j, ok := e.jobs.get(p.ID)
	if !ok {
		return toolset.Preview{}, fmt.Errorf("builtins/job_output: no such job: %s", p.ID)
	}
	running, killed, code, total, _ := j.status()
	state := "running"
	if !running {
		if killed {
			state = "killed"
		} else {
			state = fmt.Sprintf("exited exit_code=%d", code)
		}
	}
	return toolset.Preview{
		Kind:    toolset.KindOpaque,
		Action:  toolset.ActionRead,
		Subject: p.ID,
		Opaque: &toolset.OpaqueChange{
			Summary:     fmt.Sprintf("read output of job %s (%s, %d bytes so far)", j.id, state, total),
			ArgsExcerpt: j.command,
		},
	}, nil
}

func (e *env) jobOutput(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p struct {
		ID     string `json:"id"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("builtins/job_output: invalid args: %w", err)
	}
	if p.ID == "" {
		return "", fmt.Errorf("builtins/job_output: id is required")
	}
	j, ok := e.jobs.get(p.ID)
	if !ok {
		return "", fmt.Errorf("builtins/job_output: no such job: %s", p.ID)
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	limit := p.Limit
	if limit <= 0 {
		limit = e.opt.MaxExecBytes
	}
	if limit > e.opt.MaxExecBytes {
		limit = e.opt.MaxExecBytes
	}

	running, killed, code, total, dropped := j.status()
	data, clipped, _ := j.read(p.Offset, limit)

	state := "running"
	if !running {
		switch {
		case killed:
			state = "killed"
		default:
			state = "exited"
		}
	}
	var b strings.Builder
	if !running {
		fmt.Fprintf(&b, "job=%s status=%s exit_code=%d bytes_total=%d dropped=%d offset=%d\n",
			j.id, state, code, total, dropped, p.Offset)
	} else {
		fmt.Fprintf(&b, "job=%s status=running bytes_total=%d dropped=%d offset=%d\n",
			j.id, total, dropped, p.Offset)
	}
	b.WriteString("---\n")
	if len(data) == 0 {
		if p.Offset >= total {
			if running {
				b.WriteString("(no new output yet)\n")
			} else {
				b.WriteString("(job finished; no more output)\n")
			}
		} else {
			b.WriteString("(output already dropped from ring buffer)\n")
		}
		return b.String(), nil
	}
	b.Write(data)
	if !strings.HasSuffix(string(data), "\n") {
		b.WriteByte('\n')
	}
	if clipped {
		fmt.Fprintf(&b, "\n[requested offset %d is inside the dropped window (dropped=%d); resumed at byte %d]\n",
			p.Offset, dropped, j.droppedAt())
	}
	if next := p.Offset + len(data); next < total {
		fmt.Fprintf(&b, "\n[truncated: more output below; pass offset=%d to continue]\n", next)
	}
	return b.String(), nil
}

func (j *job) droppedAt() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.dropped
}

func (e *env) regJobKill() toolset.Registration {
	return toolset.Registration{
		Def: llm.ToolDef{
			Name:        "job_kill",
			Description: "Kill a background job (exec with background=true) and wait for its process tree to exit. Windows uses taskkill /T /F; Unix kills the process group. Errors if the job already exited.",
			Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "id":{"type":"string","description":"Job id returned by exec (background=true)"}
  },
  "required":["id"]
}`),
		},
		Fn:        e.jobKill,
		Risk:      toolset.RiskDangerous,
		PreviewFn: e.previewJobKill,
	}
}

func (e *env) previewJobKill(_ context.Context, args json.RawMessage) (toolset.Preview, error) {
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolset.Preview{}, fmt.Errorf("builtins/job_kill: invalid args: %w", err)
	}
	j, ok := e.jobs.get(p.ID)
	if !ok {
		return toolset.Preview{}, fmt.Errorf("builtins/job_kill: no such job: %s", p.ID)
	}
	return toolset.Preview{
		Kind:    toolset.KindCommand,
		Action:  toolset.ActionExecute,
		Subject: j.command,
		Command: &toolset.CommandChange{Command: fmt.Sprintf("kill background job %s: %s", j.id, j.command)},
	}, nil
}

func (e *env) jobKill(ctx context.Context, args json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("builtins/job_kill: invalid args: %w", err)
	}
	if p.ID == "" {
		return "", fmt.Errorf("builtins/job_kill: id is required")
	}
	j, err := e.jobs.kill(p.ID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("killed %s (command: %s)", j.id, j.command), nil
}
