package builtins_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/toolset"
	"github.com/Luo-root/pulse/toolset/builtins"
)

// bgTwoLines 是跨平台的「输出两行、中间停顿」后台命令。
func bgTwoLines() string {
	if runtime.GOOS == "windows" {
		return "Write-Output line1; Start-Sleep -Milliseconds 400; Write-Output line2"
	}
	return "echo line1; sleep 0.4; echo line2"
}

// bgLongSleep 是跨平台的 30 秒长跑命令。
func bgLongSleep() string {
	if runtime.GOOS == "windows" {
		return "Start-Sleep -Seconds 30"
	}
	return "sleep 30"
}

// bgSleepWrite 是「睡 3 秒后写 done.txt」的命令，用于验证 dispose 真的杀掉进程。
func bgSleepWrite() string {
	if runtime.GOOS == "windows" {
		return "Start-Sleep -Seconds 3; Set-Content -Path done.txt -Value done"
	}
	return "sleep 3; echo done > done.txt"
}

func bgQuick() string {
	if runtime.GOOS == "windows" {
		return "Write-Output hi"
	}
	return "echo hi"
}

func extractJobID(t *testing.T, out string) string {
	t.Helper()
	marker := "job_id="
	i := strings.Index(out, marker)
	if i < 0 {
		t.Fatalf("no job_id in %q", out)
	}
	id := out[i+len(marker):]
	if j := strings.IndexAny(id, " \r\n"); j >= 0 {
		id = id[:j]
	}
	return id
}

func extractContinueOffset(t *testing.T, out string) int {
	t.Helper()
	marker := "pass offset="
	i := strings.Index(out, marker)
	if i < 0 {
		t.Fatalf("no continuation trailer in %q", out)
	}
	rest := out[i+len(marker):]
	if j := strings.IndexAny(rest, " ]"); j >= 0 {
		rest = rest[:j]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		t.Fatalf("bad offset %q: %v", rest, err)
	}
	return n
}

func waitForJob(t *testing.T, reg *toolset.Registry, id, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		last = call(t, reg, "job_output", map[string]any{"id": id})
		if strings.Contains(last, want) {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s: %q never reached %q", id, last, want)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func TestExecBackgroundOutputAndExit(t *testing.T) {
	_, reg, cleanup := setup(t, builtins.Options{Root: t.TempDir()})
	defer cleanup()

	out := call(t, reg, "exec", map[string]any{"command": bgTwoLines(), "background": true})
	if !strings.Contains(out, "started") {
		t.Fatalf("out=%q", out)
	}
	id := extractJobID(t, out)

	final := waitForJob(t, reg, id, "status=exited", 10*time.Second)
	if !strings.Contains(final, "line1") || !strings.Contains(final, "line2") || !strings.Contains(final, "exit_code=0") {
		t.Fatalf("final=%q", final)
	}

	// 小页续读：limit=6（"line1\n"）拼回全量。
	page0 := call(t, reg, "job_output", map[string]any{"id": id, "offset": 0, "limit": 6})
	if !strings.Contains(page0, "line1") || strings.Contains(page0, "line2") {
		t.Fatalf("page0=%q", page0)
	}
	next := extractContinueOffset(t, page0)
	page1 := call(t, reg, "job_output", map[string]any{"id": id, "offset": next, "limit": 6})
	if !strings.Contains(page1, "line2") {
		t.Fatalf("page1=%q", page1)
	}

	// 越过末尾：空且明示结束。
	tail := call(t, reg, "job_output", map[string]any{"id": id, "offset": 9999})
	if !strings.Contains(tail, "no more output") {
		t.Fatalf("tail=%q", tail)
	}
}

func TestExecJobKill(t *testing.T) {
	_, reg, cleanup := setup(t, builtins.Options{Root: t.TempDir()})
	defer cleanup()

	out := call(t, reg, "exec", map[string]any{"command": bgLongSleep(), "background": true})
	id := extractJobID(t, out)

	killed := call(t, reg, "job_kill", map[string]any{"id": id})
	if !strings.Contains(killed, "killed "+id) {
		t.Fatalf("kill=%q", killed)
	}
	status := call(t, reg, "job_output", map[string]any{"id": id})
	if !strings.Contains(status, "status=killed") {
		t.Fatalf("status=%q", status)
	}
	msg := callErr(t, reg, "job_kill", map[string]any{"id": id})
	if !strings.Contains(msg, "already exited") {
		t.Fatalf("re-kill: %s", msg)
	}
}

func TestJobsUnknownAndErrors(t *testing.T) {
	_, reg, cleanup := setup(t, builtins.Options{Root: t.TempDir()})
	defer cleanup()

	msg := callErr(t, reg, "job_output", map[string]any{"id": "nope"})
	if !strings.Contains(msg, "no such job") {
		t.Fatalf("output: %s", msg)
	}
	msg = callErr(t, reg, "job_kill", map[string]any{"id": "nope"})
	if !strings.Contains(msg, "no such job") {
		t.Fatalf("kill: %s", msg)
	}

	out := call(t, reg, "exec", map[string]any{"command": bgQuick(), "background": true})
	id := extractJobID(t, out)
	waitForJob(t, reg, id, "status=exited", 10*time.Second)
	msg = callErr(t, reg, "job_kill", map[string]any{"id": id})
	if !strings.Contains(msg, "already exited") {
		t.Fatalf("quick kill: %s", msg)
	}
}

func TestDisposeKillsJobs(t *testing.T) {
	root := t.TempDir()
	_, reg, cleanup := setup(t, builtins.Options{Root: root})

	out := call(t, reg, "exec", map[string]any{"command": bgSleepWrite(), "background": true})
	_ = extractJobID(t, out)

	// cleanup 内含 dispose → killAll。
	cleanup()

	// 进程应在 dispose 后 2 秒内被杀；3 秒后本该写出的 done.txt 不该出现。
	time.Sleep(2 * time.Second)
	if _, err := os.Stat(filepath.Join(root, "done.txt")); !os.IsNotExist(err) {
		t.Fatalf("dispose must kill the job so done.txt is never written, err=%v", err)
	}
}

func TestScopeDisposeKillsJobs(t *testing.T) {
	root := t.TempDir()
	host := kernel.New()
	if _, err := kernel.Use(host, toolset.Plugin()); err != nil {
		t.Fatal(err)
	}
	reg, ok := kernel.Get(host, toolset.ServiceKey)
	if !ok {
		t.Fatal("no registry")
	}
	dispose, err := builtins.Register(host, reg, builtins.Options{Root: root})
	if err != nil {
		host.Dispose()
		t.Fatal(err)
	}
	// 故意不调 dispose：模拟宿主只走 kernel Effect 栈（host.Dispose()）。
	_ = dispose

	out := call(t, reg, "exec", map[string]any{"command": bgSleepWrite(), "background": true})
	_ = extractJobID(t, out)
	host.Dispose()

	time.Sleep(2 * time.Second)
	if _, err := os.Stat(filepath.Join(root, "done.txt")); !os.IsNotExist(err) {
		t.Fatalf("scope dispose must kill the job so done.txt is never written, err=%v", err)
	}
}
