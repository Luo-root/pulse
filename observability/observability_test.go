package observability

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
)

// recorder 是带断言辅助的 Sink 收集器。
type recorder struct {
	sink *MemorySink
}

func newRecorder() *recorder { return &recorder{sink: &MemorySink{}} }

func (r *recorder) fiberEvents() []Record {
	var out []Record
	for _, rec := range r.sink.Snapshot() {
		if rec.Event == EventFiberState {
			out = append(out, rec)
		}
	}
	return out
}

func (r *recorder) transitionsOf(name string) [][2]string {
	var out [][2]string
	for _, ev := range r.fiberEvents() {
		if ev.FiberName == name {
			out = append(out, [2]string{ev.From, ev.To})
		}
	}
	return out
}

type nopPlugin struct{}

func (nopPlugin) Inject() []kernel.Dependency { return nil }
func (nopPlugin) Apply(*kernel.Context) error { return nil }

type failPlugin struct{}

func (failPlugin) Inject() []kernel.Dependency { return nil }
func (failPlugin) Apply(*kernel.Context) error { return errors.New("apply boom") }

// depPlugin 声明对 key 的依赖；满足则 Active，否则 Inactive 挂起。
type depPlugin struct {
	key kernel.ServiceKey[string]
}

func (p depPlugin) Inject() []kernel.Dependency {
	return []kernel.Dependency{kernel.Require(p.key)}
}
func (p depPlugin) Apply(c *kernel.Context) error {
	_, _ = kernel.Get(c, p.key)
	return nil
}

const traceHost = "host-test"

// newTracedHost 返回已装 Bootstrap 的 host（横幅与后续轨迹都进 sink）。
func newTracedHost(t *testing.T, sink *MemorySink) *kernel.Context {
	t.Helper()
	host := kernel.New()
	f, err := kernel.Use(host, Bootstrap(traceHost, sink))
	if err != nil {
		t.Fatal(err)
	}
	if f.State() != kernel.StateActive {
		t.Fatalf("bootstrap not active: %v", f.State())
	}
	return host
}

// T1/T5：首次 Use 的完整装载序列 inactive→loading→active，
// 且 Bootstrap 自身也走同样的序列（观测最先 Use 时可见自身）。
func TestFirstUseFullTrajectory(t *testing.T) {
	r := newRecorder()
	host := newTracedHost(t, r.sink)
	defer host.Dispose()

	if _, err := kernel.Use(host, nopPlugin{}); err != nil {
		t.Fatal(err)
	}

	got := r.transitionsOf("nopPlugin#2")
	want := [][2]string{{"inactive", "loading"}, {"loading", "active"}}
	if len(got) != len(want) {
		t.Fatalf("transitions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("transition[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// flakyPlugin 首次 Apply 失败（若 fail 置位），之后成功——用于制造
// 真实的 failed→loading（T2）重试路径。失败必须发生在有依赖声明的
// 插件上：Failed 态只有依赖视图变化才会重新收敛。
type flakyPlugin struct {
	key  kernel.ServiceKey[string]
	fail atomic.Bool
}

func (p *flakyPlugin) Inject() []kernel.Dependency {
	return []kernel.Dependency{kernel.Require(p.key)}
}

func (p *flakyPlugin) Apply(c *kernel.Context) error {
	_, _ = kernel.Get(c, p.key)
	if p.fail.Swap(false) {
		return errors.New("first apply fails")
	}
	return nil
}

// T1/T4/T2/T5 完整链：依赖先满足但首次 Apply 失败（failed），
// 服务更新触发重收敛 → failed→loading（T2）→ active（T5）。
func TestFailedThenRetryUsesT2(t *testing.T) {
	r := newRecorder()
	host := newTracedHost(t, r.sink)
	defer host.Dispose()

	key := kernel.NewServiceKey[string]("obs.test.flaky")
	if _, err := kernel.Provide(host, key, "v1"); err != nil {
		t.Fatal(err)
	}
	fp := &flakyPlugin{key: key, fail: atomic.Bool{}}
	fp.fail.Store(true)
	f, err := kernel.Use(host, fp)
	if err == nil {
		t.Fatal("expected first apply to fail")
	}
	if f.State() != kernel.StateFailed {
		t.Fatalf("want failed, got %v", f.State())
	}
	// 断言 loading→failed（T4）已发生
	trans := r.transitionsOf(f.Name())
	if len(trans) == 0 || trans[len(trans)-1] != [2]string{"loading", "failed"} {
		t.Fatalf("want T4 loading->failed, got %v", trans)
	}
	nAfterFail := len(trans)

	// 更新服务（同 key 重写即广播变更）→ Failed 重试
	if _, err := kernel.Provide(host, key, "v2"); err != nil {
		t.Fatal(err)
	}
	if err := f.WaitState(5*time.Second, kernel.StateActive); err != nil {
		t.Fatalf("retry did not reach active: %v (state=%v)", err, f.State())
	}

	all := r.transitionsOf(f.Name())
	additional := all[nAfterFail:]
	var sawT2, sawActive bool
	for _, tr := range additional {
		if tr == [2]string{"failed", "loading"} {
			sawT2 = true
		}
		if tr[1] == "active" {
			sawActive = true
		}
	}
	if !sawT2 || !sawActive {
		t.Fatalf("missing T2(failed->loading)=%v or active=%v; additional=%v", sawT2, sawActive, additional)
	}
}

// T6：依赖消失驱动卸载 active→unloading→inactive。
func TestDepLossUnloadsT6(t *testing.T) {
	r := newRecorder()
	host := kernel.New()
	kernel.Use(host, Bootstrap(traceHost, r.sink))
	key := kernel.NewServiceKey[string]("obs.test.t6")
	dispose, err := kernel.Provide(host, key, "v")
	if err != nil {
		t.Fatal(err)
	}
	f, err := kernel.Use(host, depPlugin{key: key})
	if err != nil || f.State() != kernel.StateActive {
		t.Fatalf("use state=%v err=%v", f.State(), err)
	}
	dispose() // 服务消失 → 自动卸载
	if err := f.WaitState(2*time.Second, kernel.StateInactive); err != nil {
		t.Fatalf("fiber did not settle: %v", err)
	}

	trans := r.transitionsOf(f.Name())
	if len(trans) < 3 ||
		trans[len(trans)-3] != [2]string{"loading", "active"} ||
		trans[len(trans)-2] != [2]string{"active", "unloading"} ||
		trans[len(trans)-1] != [2]string{"unloading", "inactive"} {
		t.Fatalf("T6 sequence wrong: %v", trans)
	}
}

// WaitingFor：未满足依赖在快照中输出拷贝。
func TestWaitingForInSnapshots(t *testing.T) {
	r := newRecorder()
	host := newTracedHost(t, r.sink)
	defer host.Dispose()

	key := kernel.NewServiceKey[string]("obs.test.waiting")
	kernel.Use(host, depPlugin{key: key})

	found := false
	for _, s := range host.FiberSnapshots() {
		for _, w := range s.WaitingFor {
			if w == key.Name() {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("waiting dependency missing from snapshots")
	}
}

// 树销毁的可观测契约：dispose 完成后零残留写入；
// 每 fiber 的树销毁迁移（T7）当前不经过事件总线（kernel dispose
// 先 clear 自身总线，挂监听位置无法可靠存活）——终态一致性由
// FiberSnapshots 快照验证（横幅语义本就基于快照）。
func TestTreeDisposeZeroResidual(t *testing.T) {
	r := newRecorder()
	host := newTracedHost(t, r.sink)
	defer host.Dispose()

	kernel.Use(host, nopPlugin{})
	before := r.sink.Len()
	if before == 0 {
		t.Fatal("expected some records")
	}
	host.Dispose()
	host.Dispose() // 幂等
	time.Sleep(50 * time.Millisecond)
	after := r.sink.Len()
	if after != before {
		t.Fatalf("residual writes after dispose: %d -> %d", before, after)
	}
}


// T8a：手动 Close 的两段卸载序列。
func TestCloseEmitsT8a(t *testing.T) {
	r := newRecorder()
	host := kernel.New()
	kernel.Use(host, Bootstrap(traceHost, r.sink))
	f, err := kernel.Use(host, nopPlugin{})
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	got := r.transitionsOf(f.Name())
	tail := got[len(got)-2:]
	if tail[0] != [2]string{"active", "unloading"} || tail[1] != [2]string{"unloading", "inactive"} {
		t.Fatalf("T8a sequence = %v", tail)
	}

	// 幂等 Close 不再发事件
	n := r.sink.Len()
	f.Close()
	if r.sink.Len() != n {
		t.Fatal("idempotent close emitted extra records")
	}
}

// 快照横幅（评审修正版）：Bootstrap **后装**也能扫出已存在的
// Inactive(waiting)/Failed/Active 全体实例——横幅是快照不是事件流。
func TestBannerSummaryShape(t *testing.T) {
	sink := &MemorySink{}
	host := kernel.New()

	// 先装业务插件（无观测）
	kernel.Use(host, nopPlugin{})
	kernel.Use(host, failPlugin{})
	kernel.Use(host, depPlugin{key: kernel.NewServiceKey[string]("obs.missing")})

	// 后装 Bootstrap：快照横幅必须反映全部三类状态
	if _, err := kernel.Use(host, Bootstrap(traceHost+"-late", sink)); err != nil {
		t.Fatal(err)
	}

	var summary string
	for _, rec := range sink.Snapshot() {
		if rec.Event == EventHostReady && rec.FiberName == "" &&
			strings.Contains(rec.Status, "active=") {
			summary = rec.Status
			break
		}
	}
	if summary == "" {
		t.Fatal("no banner summary record found")
	}
	for _, want := range []string{"active=1", "failed=1", "waiting=1"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("banner summary missing %q: %q", want, summary)
		}
	}
}


func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
