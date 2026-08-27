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

// 树销毁的可观测契约：dispose 完成后零残留写入。
// T7 裁决：forceUnload 静默，不发逐 Fiber fiber_state；验收是
// Dispose 后 Sink 零增量（见 observability-v1-design.md §4）。
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

// 内置 Sink 在 Time 为零时补 wall clock，避免 Record.Time 成死字段。
func TestSinkStampsZeroTime(t *testing.T) {
	sink := &MemorySink{}
	before := time.Now()
	sink.Write(Record{HostID: "h", Source: SourceKernel, Event: EventHostReady})
	recs := sink.Snapshot()
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].Time.IsZero() {
		t.Fatal("MemorySink must stamp Time when caller leaves it zero")
	}
	if recs[0].Time.Before(before) {
		t.Fatalf("stamped time %v before write started %v", recs[0].Time, before)
	}
}


// blockingDepPlugin 可在 Apply 中阻塞，用于制造 Close 与 doLoad 竞态（T8b）。
type blockingDepPlugin struct {
	key     kernel.ServiceKey[string]
	block   *atomic.Bool
	entered chan struct{}
	gate    chan struct{}
}

func (p *blockingDepPlugin) Inject() []kernel.Dependency {
	return []kernel.Dependency{kernel.Require(p.key)}
}

func (p *blockingDepPlugin) Apply(c *kernel.Context) error {
	_, _ = kernel.Get(c, p.key)
	if p.block != nil && p.block.Load() {
		p.entered <- struct{}{}
		<-p.gate
		return errors.New("boom after close")
	}
	return nil
}

// T8b：Apply 进行中 Close → loading→inactive，且进入 Sink。
func TestCloseDuringLoadingEmitsT8b(t *testing.T) {
	r := newRecorder()
	host := newTracedHost(t, r.sink)
	defer host.Dispose()

	key := kernel.NewServiceKey[string]("obs.test.t8b")
	disposeDep, err := kernel.Provide(host, key, "dep")
	if err != nil {
		t.Fatal(err)
	}
	block := &atomic.Bool{}
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	p := &blockingDepPlugin{key: key, block: block, entered: entered, gate: gate}

	f, err := kernel.Use(host, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.WaitState(2*time.Second, kernel.StateActive); err != nil {
		t.Fatal(err)
	}

	disposeDep()
	if err := f.WaitState(2*time.Second, kernel.StateInactive); err != nil {
		t.Fatal(err)
	}
	nBefore := len(r.transitionsOf(f.Name()))

	block.Store(true)
	if _, err := kernel.Provide(host, key, "dep-v2"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("doLoad did not enter Apply")
	}
	f.Close()
	close(gate)
	if err := f.WaitState(2*time.Second, kernel.StateInactive); err != nil {
		t.Fatal(err)
	}

	additional := r.transitionsOf(f.Name())[nBefore:]
	saw := false
	for _, tr := range additional {
		if tr == [2]string{"loading", "inactive"} {
			saw = true
			break
		}
	}
	if !saw {
		t.Fatalf("missing T8b loading->inactive in Sink; additional=%v", additional)
	}
}

func loaderKinds(sink *MemorySink) []string {
	var out []string
	for _, rec := range sink.Snapshot() {
		if rec.Event == EventLoaderAction {
			out = append(out, rec.LoaderKind)
		}
	}
	return out
}

func countKind(kinds []string, want string) int {
	n := 0
	for _, k := range kinds {
		if k == want {
			n++
		}
	}
	return n
}

// LoaderAction 四分支：mount / recreate / disable / unmount；noop 静默。
func TestLoaderActionFourKinds(t *testing.T) {
	r := newRecorder()
	host := newTracedHost(t, r.sink)
	defer host.Dispose()

	l := kernel.NewLoader(host)
	l.MustRegister("nop", func() kernel.Plugin { return nopPlugin{} })

	if err := l.Reconcile([]kernel.Entry{
		{ID: "a", Name: "nop", Config: map[string]any{"v": 1}},
	}); err != nil {
		t.Fatal(err)
	}
	kinds := loaderKinds(r.sink)
	if countKind(kinds, string(kernel.ActionMount)) < 1 {
		t.Fatalf("want mount, got %v", kinds)
	}
	nAfterMount := len(kinds)

	// 无变化：noop 静默
	if err := l.Reconcile([]kernel.Entry{
		{ID: "a", Name: "nop", Config: map[string]any{"v": 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(loaderKinds(r.sink)) != nAfterMount {
		t.Fatalf("noop must be silent; kinds grew %v", loaderKinds(r.sink)[nAfterMount:])
	}

	// Config 变 → recreate（另有后续 mount）
	if err := l.Reconcile([]kernel.Entry{
		{ID: "a", Name: "nop", Config: map[string]any{"v": 2}},
	}); err != nil {
		t.Fatal(err)
	}
	kinds = loaderKinds(r.sink)
	if countKind(kinds, string(kernel.ActionRecreate)) < 1 {
		t.Fatalf("want recreate, got %v", kinds)
	}

	// Disabled → disable
	if err := l.Reconcile([]kernel.Entry{
		{ID: "a", Name: "nop", Disabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	kinds = loaderKinds(r.sink)
	if countKind(kinds, string(kernel.ActionDisable)) < 1 {
		t.Fatalf("want disable, got %v", kinds)
	}

	// 先恢复再移除 → unmount
	if err := l.Reconcile([]kernel.Entry{
		{ID: "a", Name: "nop"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
	kinds = loaderKinds(r.sink)
	if countKind(kinds, string(kernel.ActionUnmount)) < 1 {
		t.Fatalf("want unmount, got %v", kinds)
	}

	// 四动作都必须进正式 Record（SourceKernel + LoaderKind）
	for _, want := range []string{
		string(kernel.ActionMount),
		string(kernel.ActionRecreate),
		string(kernel.ActionDisable),
		string(kernel.ActionUnmount),
	} {
		found := false
		for _, rec := range r.sink.Snapshot() {
			if rec.Event == EventLoaderAction && rec.LoaderKind == want && rec.Source == SourceKernel {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Record missing loader_action kind %s", want)
		}
	}
}
