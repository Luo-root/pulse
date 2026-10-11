package pulse

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNodeStatusMatchesFinishReason 钉住两套词表的对应关系：快照的**终态**与
// `Observer` 的 `NodeFinishReason` 是同一件事实的两个出口，字符串必须逐字相同。
func TestNodeStatusMatchesFinishReason(t *testing.T) {
	pairs := []struct {
		reason NodeFinishReason
		status NodeStatus
	}{
		{NodeCompleted, NodeStatusCompleted},
		{NodeSkipped, NodeStatusSkipped},
		{NodeFailed, NodeStatusFailed},
		{NodeCanceled, NodeStatusCanceled},
	}
	for _, p := range pairs {
		if string(p.reason) != string(p.status) {
			t.Errorf("终态词表漂了：NodeFinishReason %q vs NodeStatus %q", p.reason, p.status)
		}
		if got := nodeStatusOf(p.reason); got != p.status {
			t.Errorf("nodeStatusOf(%q) = %q，期望 %q", p.reason, got, p.status)
		}
	}
	if got := slotStateOf(slotPending); got != SlotPending {
		t.Errorf("slotStateOf(pending) = %q", got)
	}
	if got := slotStateOf(slotReady); got != SlotReady {
		t.Errorf("slotStateOf(ready) = %q", got)
	}
	if got := slotStateOf(slotSkipped); got != SlotSkipped {
		t.Errorf("slotStateOf(skipped) = %q", got)
	}
}

// nodeStatus 从快照里取某个节点的状态（找不到就 Fatal）。
func nodeStatus(t *testing.T, s *Snapshot, id string) NodeState {
	t.Helper()
	for _, n := range s.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("快照里没有节点 %q", id)
	return NodeState{}
}

// keyState 取某条键的读数。
func keyState(t *testing.T, ks []KeyState, name string) KeyState {
	t.Helper()
	for _, k := range ks {
		if k.Name == name {
			return k
		}
	}
	t.Fatalf("快照里没有键 %q（有 %d 条）", name, len(ks))
	return KeyState{}
}

// TestSnapshotBeforeStart：`Start` 之前先拍一张，正是这套 API 的主要用例——
// 图 `pending`、每个节点 `pending`、没碰过的键 `pending`。
func TestSnapshotBeforeStart(t *testing.T) {
	ctx := context.Background()
	in := NewKey[int]("snap.in")
	out := NewKey[int]("snap.out")

	g, err := New(ctx, "snap-before", "启动前")
	if err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("src", "来源", nil, Provides(out), func(rc *RunCtx) error {
		return Set(rc, out, 1)
	}))
	mustAdd(t, g, NewNode("sink", "汇聚", Requires(in), nil, func(rc *RunCtx) error { return nil }))
	if err := Seed(g, in, 42); err != nil {
		t.Fatal(err)
	}

	s := g.Snapshot()
	if s.Graph.Status != GraphPending {
		t.Errorf("图状态 %q，期望 pending", s.Graph.Status)
	}
	if s.Graph.ID != "snap-before" || s.Graph.Description != "启动前" || s.Graph.Path != "" {
		t.Errorf("图元数据不对：%+v", s.Graph)
	}
	if s.Graph.Err != nil {
		t.Errorf("还没跑就有首错：%v", s.Graph.Err)
	}

	// 与 Topology 同序、同 id（宿主 join 一列就得到度数那些形状）。
	topo := g.Topology()
	if len(topo.Nodes) != len(s.Nodes) {
		t.Fatalf("快照 %d 行、拓扑 %d 行，应当同序同长", len(s.Nodes), len(topo.Nodes))
	}
	for i := range topo.Nodes {
		if topo.Nodes[i].ID != s.Nodes[i].ID {
			t.Fatalf("第 %d 行 ID 对不上：拓扑 %q、快照 %q", i, topo.Nodes[i].ID, s.Nodes[i].ID)
		}
	}

	for _, n := range s.Nodes {
		if n.Status != NodeStatusPending {
			t.Errorf("节点 %q 在 Start 之前是 %q，期望 pending", n.ID, n.Status)
		}
		if n.Child != nil {
			t.Errorf("节点 %q 还没跑就有子图句柄", n.ID)
		}
	}

	sink := nodeStatus(t, s, "sink")
	inState := keyState(t, sink.Inputs, "snap.in")
	if inState.State != SlotReady {
		t.Errorf("外部种子喂过的输入应当是 ready，实际 %q", inState.State)
	}
	if inState.From != (Source{Kind: SourceSeed}) {
		t.Errorf("输入来源应当是 seed，实际 %+v", inState.From)
	}
	if inState.Type != "int" {
		t.Errorf("类型记号 = %q，期望 int（与 Topology 同一套）", inState.Type)
	}
	if len(sink.Outputs) != 0 {
		t.Errorf("sink 没声明 Provides，输出不该有行：%+v", sink.Outputs)
	}

	// 没写过的输出：pending；输出侧的 From 恒为零值（来源就是它自己那个节点，
	// 要看那个走 Topology().Edges）。
	src := nodeStatus(t, s, "src")
	outState := keyState(t, src.Outputs, "snap.out")
	if outState.State != SlotPending {
		t.Errorf("还没写的输出应当是 pending，实际 %q", outState.State)
	}
	if outState.From != (Source{}) {
		t.Errorf("输出侧的 From 应当恒为零值，实际 %+v", outState.From)
	}
	if len(src.Inputs) != 0 {
		t.Errorf("src 没声明 Requires，输入不该有行：%+v", src.Inputs)
	}
}

// snapProbe 在回调里拍快照，钉「状态写入**先于**回调」这条线性化点。
type snapProbe struct {
	g    *Graph
	mu   sync.Mutex
	wait map[string]NodeStatus
	run  map[string]NodeStatus
	fin  map[string]NodeStatus
	gFin GraphStatus
}

func newSnapProbe(g *Graph) *snapProbe {
	return &snapProbe{
		g:    g,
		wait: map[string]NodeStatus{},
		run:  map[string]NodeStatus{},
		fin:  map[string]NodeStatus{},
	}
}

func (p *snapProbe) statusOf(id string) NodeStatus {
	s := p.g.Snapshot()
	for _, n := range s.Nodes {
		if n.ID == id {
			return n.Status
		}
	}
	return NodeStatus("<missing>")
}

func (p *snapProbe) OnGraphStarted(string) {}

func (p *snapProbe) OnGraphFinished(string, NodeFinishReason, error) {
	p.mu.Lock()
	p.gFin = p.g.Snapshot().Graph.Status
	p.mu.Unlock()
}

func (p *snapProbe) OnNodeWaiting(_, id string) {
	st := p.statusOf(id)
	p.mu.Lock()
	p.wait[id] = st
	p.mu.Unlock()
}

func (p *snapProbe) OnNodeRunning(_, id string) {
	st := p.statusOf(id)
	p.mu.Lock()
	p.run[id] = st
	p.mu.Unlock()
}

func (p *snapProbe) OnNodeFinished(_, id string, _ NodeFinishReason, _ error) {
	st := p.statusOf(id)
	p.mu.Lock()
	p.fin[id] = st
	p.mu.Unlock()
}

// TestSnapshotInCallbackSeesTransition：在回调里拍的快照**一定包含这次迁移**
// （状态先写、再发回调），图级那条同理。
func TestSnapshotInCallbackSeesTransition(t *testing.T) {
	k := NewKey[int]("cb.out")
	// 观察者要拿到图才能拍快照，而图要先有观察者才能建——所以分两步：
	// 先拿一张空壳探针过 `WithObserver`，建完再把图塞回去（回调都在 `Start` 之后）。
	probe := newSnapProbe(nil)
	g, err := New(context.Background(), "cb", "回调里拍", WithObserver(probe))
	if err != nil {
		t.Fatal(err)
	}
	probe.g = g

	mustAdd(t, g, NewNode("only", "唯一节点", nil, Provides(k), func(rc *RunCtx) error {
		return Set(rc, k, 7)
	}))

	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	probe.mu.Lock()
	defer probe.mu.Unlock()
	if got := probe.wait["only"]; got != NodeStatusWaiting {
		t.Errorf("OnNodeWaiting 里拍到 %q，期望 waiting", got)
	}
	if got := probe.run["only"]; got != NodeStatusRunning {
		t.Errorf("OnNodeRunning 里拍到 %q，期望 running（不是 waiting）", got)
	}
	if got := probe.fin["only"]; got != NodeStatusCompleted {
		t.Errorf("OnNodeFinished 里拍到 %q，期望 completed", got)
	}
	if probe.gFin != GraphCompleted {
		t.Errorf("OnGraphFinished 里图读到 %q，期望 completed（同一时刻、同一判据）", probe.gFin)
	}
}

// TestSnapshotMirrorsEventStream：跑完之后的快照与事件流折出的终态**逐节点相等**，
// 图级同理。对账范围是**一张图**——根图快照里没有子图节点。
func TestSnapshotMirrorsEventStream(t *testing.T) {
	ctx := context.Background()
	a := NewKey[int]("mir.a")
	b := NewKey[int]("mir.b")
	obs := &recordingObserver{}
	g, err := New(ctx, "mirror", "对账", WithObserver(obs))
	if err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("one", "第一个", nil, Provides(a), func(rc *RunCtx) error {
		return Set(rc, a, 1)
	}))
	mustAdd(t, g, NewNode("two", "第二个", Requires(a), Provides(b), func(rc *RunCtx) error {
		v, err := Get(rc, a)
		if err != nil {
			return err
		}
		return Set(rc, b, v+1)
	}))
	mustAdd(t, g, NewNode("three", "跳过自己", Requires(a), nil, func(rc *RunCtx) error {
		return NoValue()
	}))
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}

	s := g.Snapshot()
	fromStream := map[string]NodeFinishReason{}
	for _, line := range obs.snapshot() {
		if len(line) > 2 && line[0] == 'F' {
			// "F:<id>:<reason>"
			rest := line[2:]
			for i := 0; i < len(rest); i++ {
				if rest[i] == ':' {
					fromStream[rest[:i]] = NodeFinishReason(rest[i+1:])
					break
				}
			}
		}
	}
	if len(fromStream) != len(s.Nodes) {
		t.Fatalf("事件流里 %d 个节点、快照 %d 个节点", len(fromStream), len(s.Nodes))
	}
	for _, n := range s.Nodes {
		if want := string(fromStream[n.ID]); string(n.Status) != want {
			t.Errorf("节点 %q：快照 %q、事件流 %q", n.ID, n.Status, want)
		}
	}
	if s.Graph.Status != GraphCompleted {
		t.Errorf("图状态 %q，期望 completed", s.Graph.Status)
	}
}

// TestSnapshotWaitingSaysWhatItWaitsFor：`waiting` 覆盖两件事，快照靠槽位分得出。
//
// 判据（见 NodeStatus）：还有输入是 pending ⇒ 在等这些 Key；输入全部到达 ⇒
// 门已经过了，它在等进 `Run` 的机会。两个节点都卡在 `waiting`，靠输入读数分开。
func TestSnapshotWaitingSaysWhatItWaitsFor(t *testing.T) {
	ctx := context.Background()
	gate := make(chan struct{})
	started := make(chan struct{})
	a := NewKey[int]("wait.a")
	g1 := NewKey[int]("wait.gate")

	g, err := New(ctx, "wait", "等谁", WithMaxRunning(1))
	if err != nil {
		t.Fatal(err)
	}
	// slow 先占住唯一的名额：发文凭（g1）再堵在门口，所以 queued 一定卡在等名额上。
	mustAdd(t, g, NewNode("slow", "占住名额", nil, Provides(a, g1), func(rc *RunCtx) error {
		if err := Set(rc, g1, 1); err != nil {
			return err
		}
		close(started)
		<-gate
		return Set(rc, a, 1)
	}))
	// 输入还没到：在等 wait.a
	mustAdd(t, g, NewNode("needs", "等输入", Requires(a), nil, func(rc *RunCtx) error { return nil }))
	// 输入已到、门已过：在等**名额**
	mustAdd(t, g, NewNode("queued", "等名额", Requires(g1), nil, func(rc *RunCtx) error { return nil }))

	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	<-started
	// 两个节点各自还得被调度到才会写下 waiting，所以等到「两边都在 waiting、
	// 且 queued 的输入已到」为止。这不是竞态窗口：名额在 slow 手里、wait.a 也
	// 要等 slow 放行，在此之前两边都不可能离开 waiting。
	settled := func() bool {
		s := g.Snapshot()
		needs := nodeStatus(t, s, "needs")
		queued := nodeStatus(t, s, "queued")
		return needs.Status == NodeStatusWaiting &&
			queued.Status == NodeStatusWaiting &&
			keyState(t, queued.Inputs, "wait.gate").State == SlotReady
	}
	deadline := time.Now().Add(2 * time.Second)
	for !settled() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	s := g.Snapshot()
	needs := nodeStatus(t, s, "needs")
	if needs.Status != NodeStatusWaiting {
		t.Errorf("needs 状态 %q，期望 waiting", needs.Status)
	}
	if got := keyState(t, needs.Inputs, "wait.a"); got.State != SlotPending {
		t.Errorf("needs 的输入应当是 pending（它在等这条），实际 %q", got.State)
	}
	queued := nodeStatus(t, s, "queued")
	if queued.Status != NodeStatusWaiting {
		t.Errorf("queued 状态 %q，期望 waiting（名额被 slow 占着）", queued.Status)
	}
	// 与 needs 的分界就在这一行：输入全是 ready，说明门已经过了。
	if got := keyState(t, queued.Inputs, "wait.gate"); got.State != SlotReady {
		t.Errorf("queued 的输入应当已经 ready，实际 %q", got.State)
	}
	if s.Graph.Status != GraphRunning {
		t.Errorf("图状态 %q，期望 running", s.Graph.Status)
	}

	close(gate)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := nodeStatus(t, g.Snapshot(), "queued").Status; got != NodeStatusCompleted {
		t.Errorf("放行之后 queued 状态 %q，期望 completed", got)
	}
}

// TestSnapshotTerminalStates：五种终态各一条，外加「普通输出调用 Skip 不改节点状态」。
func TestSnapshotTerminalStates(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		k := NewKey[int]("term.ok")
		g, _ := New(context.Background(), "t1", "")
		mustAdd(t, g, NewNode("ok", "", nil, Provides(k), func(rc *RunCtx) error { return Set(rc, k, 1) }))
		if err := g.Run(); err != nil {
			t.Fatal(err)
		}
		n := nodeStatus(t, g.Snapshot(), "ok")
		if n.Status != NodeStatusCompleted || n.Err != nil {
			t.Fatalf("状态 %q / 错误 %v，期望 completed / nil", n.Status, n.Err)
		}
	})

	t.Run("skipped-no-input-arrived", func(t *testing.T) {
		in := NewKey[int]("term.skip.in")
		obs := &recordingObserver{}
		g, _ := New(context.Background(), "t2", "", WithObserver(obs))
		mustAdd(t, g, NewNode("skipme", "", Requires(in), nil, func(rc *RunCtx) error { return nil }))
		if err := SkipSeed(g, in); err != nil {
			t.Fatal(err)
		}
		if err := g.Run(); err != nil {
			t.Fatal(err)
		}
		n := nodeStatus(t, g.Snapshot(), "skipme")
		if n.Status != NodeStatusSkipped {
			t.Fatalf("状态 %q，期望 skipped", n.Status)
		}
		if c := countPref(obs.snapshot(), "R:skipme"); c != 0 {
			t.Errorf("没进 Run 的节点不该有 running 事件，实际 %d 条", c)
		}
	})

	t.Run("skipped-by-novalue", func(t *testing.T) {
		obs := &recordingObserver{}
		g, _ := New(context.Background(), "t3", "", WithObserver(obs))
		mustAdd(t, g, NewNode("self", "", nil, nil, func(rc *RunCtx) error { return NoValue() }))
		if err := g.Run(); err != nil {
			t.Fatal(err)
		}
		n := nodeStatus(t, g.Snapshot(), "self")
		if n.Status != NodeStatusSkipped || n.Err != nil {
			t.Fatalf("状态 %q / 错误 %v，期望 skipped / nil", n.Status, n.Err)
		}
		if c := countPref(obs.snapshot(), "R:self"); c != 1 {
			t.Errorf("自己跳过的节点进过 Run，应当有 1 条 running，实际 %d", c)
		}
	})

	t.Run("failed-with-err", func(t *testing.T) {
		boom := errors.New("boom")
		g, _ := New(context.Background(), "t4", "")
		mustAdd(t, g, NewNode("bad", "", nil, nil, func(rc *RunCtx) error { return boom }))
		if err := g.Run(); !errors.Is(err, boom) {
			t.Fatalf("Run 返回 %v，期望带 boom", err)
		}
		s := g.Snapshot()
		n := nodeStatus(t, s, "bad")
		if n.Status != NodeStatusFailed || !errors.Is(n.Err, boom) {
			t.Fatalf("状态 %q / 错误 %v，期望 failed / boom", n.Status, n.Err)
		}
		if s.Graph.Status != GraphFailed || !errors.Is(s.Graph.Err, boom) {
			t.Fatalf("图状态 %q / 错误 %v，期望 failed / boom", s.Graph.Status, s.Graph.Err)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		k := NewKey[int]("term.cancel")
		g, _ := New(ctx, "t5", "")
		mustAdd(t, g, NewNode("waits", "", Requires(k), nil, func(rc *RunCtx) error { return nil }))
		// 节点等**自己那把** ctx（rc.Context()）：它与引擎取消域同一条链。
		//
		// 别写成「等外层 ctx 的 Done、再读 rc.Context().Err()」——取消是**逐级**
		// 传播的：context 的 cancelCtx.cancel 先关自己的 done、再遍历 children
		// 逐个 cancel，所以外层的 done 一关，内层（子图 / 节点那两级）的 Err()
		// 还可能读到 nil。那条路测出来的是这个窗口，不是引擎行为。
		mustAdd(t, g, NewNode("src", "", nil, Provides(k), func(rc *RunCtx) error {
			<-rc.Context().Done()
			return rc.Context().Err()
		}))
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()
		_ = g.Run()
		s := g.Snapshot()
		if s.Graph.Status != GraphCanceled {
			t.Errorf("图状态 %q，期望 canceled", s.Graph.Status)
		}
		for _, n := range s.Nodes {
			if n.Status != NodeStatusCanceled {
				t.Errorf("节点 %q 状态 %q，期望 canceled", n.ID, n.Status)
			}
		}
	})

	t.Run("waiting-then-canceled-while-queued", func(t *testing.T) {
		// 事实 5 的第二条序列：**等名额**期间被取消 = `waiting → canceled`（中间没有 running）。
		// 与上面那条 canceled 的分工：那条是等**输入**时被拆，这条是门已过、在排队等名额时被拆。
		ctx, cancel := context.WithCancel(context.Background())
		k := NewKey[int]("term.queued")
		entered := make(chan struct{})
		g, _ := New(ctx, "t7", "", WithMaxRunning(1))
		mustAdd(t, g, NewNode("holder", "占住唯一名额", nil, Provides(k), func(rc *RunCtx) error {
			if err := Set(rc, k, 1); err != nil {
				return err
			}
			close(entered)
			// 堵在自己那把 ctx 上：父侧取消要能把它拆掉（普通 channel 上堵着的话
			// 这一轮就等不到头，用例会挂死）。
			<-rc.Context().Done()
			return rc.Context().Err()
		}))
		mustAdd(t, g, NewNode("queued", "排队等名额", Requires(k), nil, func(rc *RunCtx) error { return nil }))

		if err := g.Start(); err != nil {
			t.Fatal(err)
		}
		<-entered
		// 名额在 holder 手里，queued 出不了 waiting——所以这里是等它**到**，不是等它变。
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if nodeStatus(t, g.Snapshot(), "queued").Status == NodeStatusWaiting {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if got := nodeStatus(t, g.Snapshot(), "queued"); got.Status != NodeStatusWaiting {
			t.Fatalf("queued 状态 %q，期望 waiting（它在排队等名额）", got.Status)
		}
		cancel()
		if err := g.Wait(); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait = %v，期望 context.Canceled", err)
		}
		s := g.Snapshot()
		if got := nodeStatus(t, s, "queued"); got.Status != NodeStatusCanceled {
			t.Errorf("排队时被拆的节点状态 %q，期望 canceled", got.Status)
		}
		if got := nodeStatus(t, s, "holder"); got.Status != NodeStatusCanceled {
			t.Errorf("占着名额那个节点状态 %q，期望 canceled", got.Status)
		}
	})

	t.Run("skip-output-keeps-node-completed", func(t *testing.T) {
		k := NewKey[int]("term.skipout")
		g, _ := New(context.Background(), "t6", "")
		mustAdd(t, g, NewNode("half", "", nil, Provides(k), func(rc *RunCtx) error {
			return Skip(rc, k)
		}))
		if err := g.Run(); err != nil {
			t.Fatal(err)
		}
		n := nodeStatus(t, g.Snapshot(), "half")
		if n.Status != NodeStatusCompleted {
			t.Fatalf("普通输出调用 Skip 不应把节点标成 skipped，实际 %q", n.Status)
		}
		if got := keyState(t, n.Outputs, "term.skipout"); got.State != SlotSkipped {
			t.Fatalf("输出槽应当是 skipped，实际 %q", got.State)
		}
	})
}

// TestSnapshotRetryKeepsOneLifecycle：`Retry` 不重走状态——状态是**一次节点
// 生命周期**，重跑期间一直是 `running`，最终只写一次终态；事件流也是各一次。
func TestSnapshotRetryKeepsOneLifecycle(t *testing.T) {
	obs := &recordingObserver{}
	k := NewKey[int]("retry.out")
	g, err := New(context.Background(), "retry", "", WithObserver(obs))
	if err != nil {
		t.Fatal(err)
	}
	var attempts int32
	mustAdd(t, g, NewNode("flaky", "先失败再成功", nil, Provides(k), func(rc *RunCtx) error {
		if atomic.AddInt32(&attempts, 1) == 1 {
			return errors.New("first attempt fails")
		}
		return Set(rc, k, 1)
	}, Retry(2, time.Millisecond)))

	stop := make(chan struct{})
	var mu sync.Mutex
	var seq []NodeStatus
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			s := g.Snapshot()
			for _, n := range s.Nodes {
				if n.ID == "flaky" {
					mu.Lock()
					seq = append(seq, n.Status)
					mu.Unlock()
				}
			}
		}
	}()

	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	<-done

	mu.Lock()
	defer mu.Unlock()
	// 单调：不许从 running 退回 waiting/pending，也不许终态之后再变。
	rank := map[NodeStatus]int{
		NodeStatusPending: 0, NodeStatusWaiting: 1, NodeStatusRunning: 2,
	}
	last := NodeStatusPending
	terminal := false
	for _, st := range seq {
		if terminal && st != last {
			t.Fatalf("终态之后状态又变了：%q → %q（序列 %v）", last, st, seq)
		}
		if r, ok := rank[st]; ok {
			if prev, ok2 := rank[last]; ok2 && r < prev {
				t.Fatalf("状态回退：%q → %q（序列 %v）", last, st, seq)
			}
		} else {
			terminal = true
		}
		last = st
	}
	if len(seq) == 0 || seq[len(seq)-1] != NodeStatusCompleted {
		t.Fatalf("序列结尾应当是 completed，实际 %v", seq)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Fatalf("应当重试一次（共两次 attempt），实际 %d", attempts)
	}

	log := obs.snapshot()
	if c := countPref(log, "W:flaky"); c != 1 {
		t.Errorf("waiting 事件应当只有一次（不按 attempt 重发），实际 %d", c)
	}
	if c := countPref(log, "R:flaky"); c != 1 {
		t.Errorf("running 事件应当只有一次，实际 %d", c)
	}
	if c := countPref(log, "F:flaky:completed"); c != 1 {
		t.Errorf("finished 事件应当只有一次，实际 %d", c)
	}
}

// TestSnapshotCarriesNoValues：快照默认**不含任何真实值**——用反射钉住
// 「结构里根本没有这个字段」，而不是靠一句注释。
func TestSnapshotCarriesNoValues(t *testing.T) {
	anyType := reflect.TypeOf((*any)(nil)).Elem()
	graphPtr := reflect.TypeOf((*Graph)(nil))
	seen := map[reflect.Type]bool{}

	var walk func(rt reflect.Type)
	walk = func(rt reflect.Type) {
		for rt.Kind() == reflect.Ptr || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Array {
			rt = rt.Elem()
		}
		if rt == graphPtr || rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if f.Type == anyType {
				t.Errorf("快照结构 %s 里的字段 %s 是 any —— 快照不该带真实值", rt, f.Name)
			}
			walk(f.Type)
		}
	}
	for _, rt := range []reflect.Type{
		reflect.TypeOf(Snapshot{}),
		reflect.TypeOf(GraphState{}),
		reflect.TypeOf(NodeState{}),
		reflect.TypeOf(KeyState{}),
	} {
		walk(rt)
	}
	if len(seen) < 4 {
		t.Fatalf("只走到 %d 个结构，反射遍历没干活", len(seen))
	}
}

// startedSnap 在 `OnGraphStarted` 里拍一张快照：那是「图已经 `running`、而每个
// 节点仍是 `pending`」**唯一确定的时刻**——引擎先发这一发，再起节点 goroutine
// （见 start），所以这里不需要靠调度去凑，也没有竞态窗口。
type startedSnap struct {
	g    *Graph
	snap *Snapshot
}

func (s *startedSnap) OnGraphStarted(string)                           { s.snap = s.g.Snapshot() }
func (s *startedSnap) OnGraphFinished(string, NodeFinishReason, error) {}
func (s *startedSnap) OnNodeWaiting(_, _ string)                       {}
func (s *startedSnap) OnNodeRunning(_, _ string)                       {}
func (s *startedSnap) OnNodeFinished(_, _ string, _ NodeFinishReason, _ error) {
}

// TestSnapshotStartBeforeNodesRun：`Start` 之后、节点还没被提交时拍的快照——
// 图 `running`、每个节点 `pending`、没有子图句柄。
func TestSnapshotStartBeforeNodesRun(t *testing.T) {
	k := NewKey[int]("gate.out")
	probe := &startedSnap{}
	g, err := New(context.Background(), "gate", "点名时刻", WithObserver(probe))
	if err != nil {
		t.Fatal(err)
	}
	probe.g = g
	mustAdd(t, g, NewNode("a", "第一个", nil, Provides(k), func(rc *RunCtx) error { return Set(rc, k, 1) }))
	mustAdd(t, g, NewNode("b", "第二个", Requires(k), nil, func(rc *RunCtx) error { return nil }))

	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if probe.snap == nil {
		t.Fatal("GraphStarted 那一发没拍到快照")
	}
	s := probe.snap
	if s.Graph.Status != GraphRunning {
		t.Errorf("图状态 %q，期望 running（已经 Start 了）", s.Graph.Status)
	}
	if len(s.Nodes) != 2 {
		t.Fatalf("快照 %d 行，期望 2 行", len(s.Nodes))
	}
	for _, n := range s.Nodes {
		if n.Status != NodeStatusPending {
			t.Errorf("节点 %q 在 GraphStarted 那一刻是 %q，期望 pending（goroutine 还没起）", n.ID, n.Status)
		}
		if n.Child != nil {
			t.Errorf("节点 %q 还没跑就有子图句柄", n.ID)
		}
	}
	// 启动前种子喂过的那条：那一行读数是到达，不是状态。
	if got := keyState(t, nodeStatus(t, s, "b").Inputs, "gate.out"); got.State != SlotPending {
		t.Errorf("输入槽 %q，期望 pending（生产者还没跑）", got.State)
	}
}

// TestSnapshotOutputReadyWhileRunning：生产者写完第一条输出之后**还在跑**——
// 快照里它仍是 `running`，而那条输出槽已经 `ready`。「槽位就绪」与「这个节点
// 结束了」是两件事，别拿前者当后者。
func TestSnapshotOutputReadyWhileRunning(t *testing.T) {
	k := NewKey[int]("ready.out")
	wrote := make(chan struct{})
	gate := make(chan struct{})
	g, err := New(context.Background(), "ready", "")
	if err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("producer", "写完还堵着", nil, Provides(k), func(rc *RunCtx) error {
		if err := Set(rc, k, 7); err != nil {
			return err
		}
		close(wrote)
		<-gate
		return nil
	}))

	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	<-wrote
	s := g.Snapshot()
	n := nodeStatus(t, s, "producer")
	if n.Status != NodeStatusRunning {
		t.Errorf("节点状态 %q，期望 running（它还堵在 Run 里）", n.Status)
	}
	if got := keyState(t, n.Outputs, "ready.out"); got.State != SlotReady {
		t.Errorf("输出槽 %q，期望 ready（值已经写进去了）", got.State)
	}
	if s.Graph.Status != GraphRunning {
		t.Errorf("图状态 %q，期望 running", s.Graph.Status)
	}

	close(gate)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := nodeStatus(t, g.Snapshot(), "producer").Status; got != NodeStatusCompleted {
		t.Errorf("放行之后状态 %q，期望 completed", got)
	}
}

// TestSnapshotKeyStatesAndFrom：四条输入把三种到达形态与「谁喂的」各读一遍——
// 外部种子的 `ready`、上游节点喂的 `ready`（`From` 是那个节点）、以跳过到达的
// `skipped`，外加一条还没到的 `pending`。
func TestSnapshotKeyStatesAndFrom(t *testing.T) {
	seeded := NewKey[int]("from.seeded")
	up := NewKey[int]("from.up")
	none := NewKey[int]("from.none")
	slow := NewKey[int]("from.slow")
	gate := make(chan struct{})
	slowReady := make(chan struct{})

	g, err := New(context.Background(), "from", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := Seed(g, seeded, 1); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("producer", "", nil, Provides(up), func(rc *RunCtx) error { return Set(rc, up, 2) }))
	mustAdd(t, g, NewNode("quiet", "", nil, Provides(none), func(rc *RunCtx) error { return Skip(rc, none) }))
	mustAdd(t, g, NewNode("slow", "", nil, Provides(slow), func(rc *RunCtx) error {
		close(slowReady)
		<-gate
		return Set(rc, slow, 3)
	}))
	mustAdd(t, g, NewNode("consumer", "", Deps(Requires(seeded), Requires(up), Requires(none), Requires(slow)),
		nil, func(rc *RunCtx) error { return nil }))

	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	<-slowReady // slow 已经进了 Run、还没写：它那条输入一定还是 pending

	// 等前三条各就各位——**三条都要等**：它们各由一个 goroutine 产出，谁先到不保证
	// （只等其中一条的话，另一条可能还没写，读出来是 pending）；consumer 那条也要等，
	// 它自己的 goroutine 得先走到门才轮到它读 `waiting`。slow 那条不会到位，所以
	// consumer 出不了 waiting——这个判据是稳的。
	settled := func() bool {
		cur := nodeStatus(t, g.Snapshot(), "consumer")
		inputs := cur.Inputs
		return cur.Status == NodeStatusWaiting &&
			keyState(t, inputs, "from.seeded").State == SlotReady &&
			keyState(t, inputs, "from.up").State == SlotReady &&
			keyState(t, inputs, "from.none").State == SlotSkipped
	}
	deadline := time.Now().Add(2 * time.Second)
	for !settled() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	s := g.Snapshot()
	inputs := nodeStatus(t, s, "consumer").Inputs

	got := keyState(t, inputs, "from.seeded")
	if got.State != SlotReady || got.From != (Source{Kind: SourceSeed}) {
		t.Errorf("外部种子那条：%q / %+v，期望 ready / seed", got.State, got.From)
	}
	got = keyState(t, inputs, "from.up")
	if got.State != SlotReady || got.From != (Source{Kind: SourceNode, NodeID: "producer"}) {
		t.Errorf("上游喂的那条：%q / %+v，期望 ready / node producer", got.State, got.From)
	}
	got = keyState(t, inputs, "from.none")
	if got.State != SlotSkipped || got.From != (Source{Kind: SourceNode, NodeID: "quiet"}) {
		t.Errorf("以跳过到达那条：%q / %+v，期望 skipped / node quiet", got.State, got.From)
	}
	got = keyState(t, inputs, "from.slow")
	if got.State != SlotPending {
		t.Errorf("还没到那条：%q，期望 pending", got.State)
	}
	if consumer := nodeStatus(t, s, "consumer"); consumer.Status != NodeStatusWaiting {
		t.Errorf("consumer 状态 %q，期望 waiting（还差一条输入）", consumer.Status)
	}

	close(gate)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotConcurrentRunAndSnapshot：边跑边拍，`-race` 必须干净。
func TestSnapshotConcurrentRunAndSnapshot(t *testing.T) {
	ctx := context.Background()
	a := NewKey[int]("conc.a")
	b := NewKey[int]("conc.b")
	c := NewKey[int]("conc.c")
	g, err := New(ctx, "conc", "")
	if err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g, NewNode("n1", "", nil, Provides(a), func(rc *RunCtx) error { return Set(rc, a, 1) }))
	mustAdd(t, g, NewNode("n2", "", Requires(a), Provides(b), func(rc *RunCtx) error {
		v, err := Get(rc, a)
		if err != nil {
			return err
		}
		return Set(rc, b, v+1)
	}))
	mustAdd(t, g, NewNode("n3", "", Requires(b), Provides(c), func(rc *RunCtx) error {
		v, err := Get(rc, b)
		if err != nil {
			return err
		}
		return Set(rc, c, v+1)
	}))
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := g.Snapshot()
				if len(s.Nodes) != 3 {
					t.Errorf("快照行数 %d，期望 3", len(s.Nodes))
					return
				}
			}
		}()
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
}

// TestSnapshotConcurrentAddAndSnapshot：**未启动时并发 `Add` + 拍快照**本身
// 就是一处 data race——读取算法的第一步（持 `g.mu` 复制）就是为它写的。
func TestSnapshotConcurrentAddAndSnapshot(t *testing.T) {
	g, err := New(context.Background(), "concadd", "")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			_ = g.Snapshot()
		}
	}()
	for i := 0; i < 40; i++ {
		k := NewKey[int]("concadd.k" + string(rune('a'+i%26)) + string(rune('a'+i/26)))
		n := NewNode("n"+string(rune('a'+i%26))+string(rune('a'+i/26)), "", nil, Provides(k),
			func(rc *RunCtx) error { return Set(rc, k, 1) })
		if err := g.Add(n); err != nil {
			close(done)
			wg.Wait()
			t.Fatalf("Add 第 %d 个节点失败：%v", i, err)
		}
	}
	close(done)
	wg.Wait()
	if got := len(g.Snapshot().Nodes); got != 40 {
		t.Fatalf("快照 %d 行，期望 40", got)
	}
}
