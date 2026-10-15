package pulse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func snapshotExec(t *testing.T, g *Graph, in Inputs) *Execution {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	e, err := g.NewExecution(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Cancel(); _ = e.Wait() })
	return e
}

func snapshotNode(t *testing.T, s *Snapshot, id string) NodeState {
	t.Helper()
	for _, n := range s.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("snapshot 没有节点 %q: %+v", id, s.Nodes)
	return NodeState{}
}

func snapshotSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("没有到达用例会合点")
	}
}

func TestSnapshotLifecycleAndObserverOrder(t *testing.T) {
	input, optional := NewKey[string]("snap.input"), NewKey[string]("snap.optional")
	output, unwritten := NewKey[string]("snap.output"), NewKey[string]("snap.unwritten")
	var e *Execution
	var callbacks atomic.Int32
	checkNode := func(id string, status NodeStatus) {
		n := snapshotNode(t, e.Snapshot(), id)
		if n.Status != status {
			t.Errorf("回调里状态 = %s, want %s", n.Status, status)
		}
		callbacks.Add(1)
	}
	g := mustNew(t, context.Background(), "snap.lifecycle", WithObserver(ObserverFunc{
		GraphStarted: func(site Site) {
			s := e.Snapshot()
			if s.Graph.Status != GraphStatusRunning || s.Graph.Execution != site.Execution {
				t.Errorf("started 快照 = %+v", s.Graph)
			}
			for _, n := range s.Nodes {
				if n.Status != NodeStatusPending {
					t.Errorf("图开始回调先于节点: %s = %s", n.ID, n.Status)
				}
			}
		},
		GraphFinished: func(_ Site, reason NodeFinishReason, err error) {
			s := e.Snapshot()
			if s.Graph.Status != GraphStatus(reason) || s.Graph.Err != err {
				t.Errorf("finished 状态未写入: %+v", s.Graph)
			}
		},
		Waiting: func(_ Site, id string) { checkNode(id, NodeStatusWaiting) },
		Running: func(_ Site, id string) { checkNode(id, NodeStatusRunning) },
		Finished: func(_ Site, id string, reason NodeFinishReason, _ error) {
			checkNode(id, NodeStatus(reason))
		},
	}))
	declareIn(t, g, NewPort(input), NewPort(optional))
	mustAdd(t, g, NewNode("z", "上游", Requires(input, optional), Provides(output), func(rc *RunCtx) error {
		v, err := Get(rc, input)
		if err != nil {
			return err
		}
		return Set(rc, output, v)
	}))
	mustAdd(t, g, NewNode("a", "下游", Requires(output), Provides(unwritten), nil))
	var in Inputs
	if err := Seed(&in, input, "secret-value"); err != nil {
		t.Fatal(err)
	}
	if err := SkipSeed(&in, optional); err != nil {
		t.Fatal(err)
	}
	e = snapshotExec(t, g, in)
	before := e.Snapshot()
	if before.Graph.Status != GraphStatusPending || before.Graph.Execution != e.ID() {
		t.Fatalf("创建态 = %+v", before.Graph)
	}
	for i, n := range g.Topology().Nodes {
		if before.Nodes[i].ID != n.ID {
			t.Fatalf("快照不按定义声明序: %+v", before.Nodes)
		}
	}
	if before.Nodes[0].Inputs[0].State != SlotStateReady || before.Nodes[0].Inputs[1].State != SlotStateSkipped {
		t.Fatalf("输入到达状态 = %+v", before.Nodes[0].Inputs)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
	after := e.Snapshot()
	if after.Graph.Status != GraphStatusCompleted || after.Graph.Err != nil || e.Context().Err() == nil {
		t.Fatalf("清理 ctx 不改完成态: %+v / %v", after.Graph, e.Context().Err())
	}
	for i, n := range g.Topology().Nodes {
		if after.Nodes[i].ID != n.ID || after.Nodes[i].Description != n.Description {
			t.Fatalf("快照不按定义声明序: %+v", after.Nodes)
		}
	}
	if after.Nodes[0].Outputs[0].State != SlotStateReady || after.Nodes[1].Outputs[0].State != SlotStateSkipped {
		t.Fatalf("就绪与隐式跳过: %+v", after.Nodes)
	}
	if callbacks.Load() != 6 {
		t.Fatalf("节点回调次数变化: %d", callbacks.Load())
	}
	// 改快照的切片不能污染执行，也不能污染下一张快照。
	after.Nodes[0].ID = "changed"
	after.Nodes[0].Inputs[0].Name = "changed"
	after.Nodes[0].Outputs[0].State = SlotStatePending
	if got := e.Snapshot(); got.Nodes[0].ID != "z" || got.Nodes[0].Inputs[0].Name != input.Name() || got.Nodes[0].Outputs[0].State != SlotStateReady {
		t.Fatalf("快照副本别名: %+v", got)
	}
}

func TestSnapshotLateDefinitionAndReadOnlySlots(t *testing.T) {
	g := mustNew(t, context.Background(), "snap.late")
	e := snapshotExec(t, g, Inputs{})
	late := NewKey[int]("snap.late-output")
	mustAdd(t, g, NewNode("late", "晚加的节点", nil, Provides(late), nil))
	e.slotsMu.Lock()
	before := len(e.slots)
	e.slotsMu.Unlock()
	s := e.Snapshot()
	e.slotsMu.Lock()
	after := len(e.slots)
	e.slotsMu.Unlock()
	if len(s.Nodes) != 1 || s.Nodes[0].Status != NodeStatusPending || s.Nodes[0].Outputs[0].State != SlotStatePending || before != after {
		t.Fatalf("观察晚加节点不得创建运行槽位: %+v / %d -> %d", s, before, after)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := e.Snapshot().Nodes[0]; got.Status != NodeStatusCompleted || got.Outputs[0].State != SlotStateSkipped {
		t.Fatalf("晚加节点正常参与本次运行: %+v", got)
	}
}

func TestSnapshotWaitingInputsAndQuota(t *testing.T) {
	permit, result := NewKey[int]("snap.permit"), NewKey[int]("snap.result")
	entered, release := make(chan struct{}), make(chan struct{})
	queued, needsInput := make(chan struct{}), make(chan struct{})
	g := mustNew(t, context.Background(), "snap.wait", WithMaxRunning(1), WithObserver(ObserverFunc{
		Waiting: func(_ Site, id string) {
			switch id {
			case "queued":
				close(queued)
			case "needs-input":
				close(needsInput)
			}
		},
	}))
	mustAdd(t, g, NewNode("producer", "", nil, Provides(permit, result), func(rc *RunCtx) error {
		if err := Set(rc, permit, 1); err != nil {
			return err
		}
		close(entered)
		select {
		case <-release:
			return Set(rc, result, 2)
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	}))
	mustAdd(t, g, NewNode("queued", "", Requires(permit), nil, nil))
	mustAdd(t, g, NewNode("needs-input", "", Requires(result), nil, nil))
	e := snapshotExec(t, g, Inputs{})
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	snapshotSignal(t, entered)
	snapshotSignal(t, queued)
	snapshotSignal(t, needsInput)
	s := e.Snapshot()
	a, b := snapshotNode(t, s, "queued"), snapshotNode(t, s, "needs-input")
	if a.Status != NodeStatusWaiting || a.Inputs[0].State != SlotStateReady || b.Status != NodeStatusWaiting || b.Inputs[0].State != SlotStatePending {
		t.Fatalf("两种等待分不开: queued=%+v, input=%+v", a, b)
	}
	close(release)
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRetryStateAndStagedOutputs(t *testing.T) {
	output := NewKey[int]("snap.retry-output")
	second, release := make(chan struct{}), make(chan struct{})
	var attempts, waiting, running, finished atomic.Int32
	g := mustNew(t, context.Background(), "snap.retry", WithObserver(ObserverFunc{
		Waiting: func(_ Site, id string) {
			if id == "producer" {
				waiting.Add(1)
			}
		},
		Running: func(_ Site, id string) {
			if id == "producer" {
				running.Add(1)
			}
		},
		Finished: func(_ Site, id string, _ NodeFinishReason, _ error) {
			if id == "producer" {
				finished.Add(1)
			}
		},
	}))
	mustAdd(t, g, NewNode("producer", "", nil, Provides(output), func(rc *RunCtx) error {
		n := attempts.Add(1)
		if err := Set(rc, output, int(n)); err != nil {
			return err
		}
		if n == 1 {
			return errors.New("first attempt")
		}
		close(second)
		select {
		case <-release:
			return nil
		case <-rc.Context().Done():
			return rc.Context().Err()
		}
	}, Retry(2, 0)))
	mustAdd(t, g, NewNode("reader", "", Requires(output), nil, func(rc *RunCtx) error {
		v, err := Get(rc, output)
		if err == nil && v != 2 {
			return errors.New("reader saw failed attempt")
		}
		return err
	}))
	e := snapshotExec(t, g, Inputs{})
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	snapshotSignal(t, second)
	n := snapshotNode(t, e.Snapshot(), "producer")
	if n.Status != NodeStatusRunning || n.Outputs[0].State != SlotStatePending || waiting.Load() != 1 || running.Load() != 1 {
		t.Fatalf("Retry 不得倒退状态/暴露暂存: %+v / waiting %d running %d", n, waiting.Load(), running.Load())
	}
	close(release)
	if err := e.Wait(); err != nil {
		t.Fatal(err)
	}
	if finished.Load() != 1 || snapshotNode(t, e.Snapshot(), "producer").Status != NodeStatusCompleted {
		t.Fatal("Retry 终态必须只有一次")
	}
}

func TestSnapshotSubChildIsLiveAndRetainsFailure(t *testing.T) {
	for _, kind := range []string{"completed", "failed", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			out, parentOut := NewKey[int]("snap.child-out"), NewKey[int]("snap.parent-out")
			entered, release := make(chan struct{}), make(chan struct{})
			cause := errors.New("child failed")
			child := mustNew(t, context.Background(), "snap.child")
			mustAdd(t, child, NewNode("inner", "", nil, Provides(out), func(rc *RunCtx) error {
				close(entered)
				select {
				case <-release:
					if kind == "failed" {
						return cause
					}
					return Set(rc, out, 1)
				case <-rc.Context().Done():
					return rc.Context().Err()
				}
			}))
			if err := child.DeclareOutput(NewPort(out)); err != nil {
				t.Fatal(err)
			}
			parent := mustNew(t, context.Background(), "snap.parent")
			if err := Sub(parent, "step", "", []SubBind{Out(out, parentOut)}, child); err != nil {
				t.Fatal(err)
			}
			e := snapshotExec(t, parent, Inputs{})
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			snapshotSignal(t, entered)
			handle := e.Snapshot().Nodes[0].Child
			if handle == nil || handle.Site().Path != parent.Topology().Nodes[0].Sub.Graph.Path || handle.Snapshot().Graph.Status != GraphStatusRunning {
				t.Fatalf("Child 必须在开始之前记录且是活句柄: %v", handle)
			}
			if kind == "canceled" {
				e.Cancel()
			} else {
				close(release)
			}
			err := e.Wait()
			want := NodeStatus(kind)
			if kind == "failed" && !errors.Is(err, cause) {
				t.Fatalf("首错丢失: %v", err)
			}
			if kind == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("取消 = %v", err)
			}
			if kind == "completed" && err != nil {
				t.Fatal(err)
			}
			n := e.Snapshot().Nodes[0]
			if n.Status != want || n.Child != handle || handle.Snapshot().Graph.Status != GraphStatus(kind) {
				t.Fatalf("Child 不能成功后才记/递归拷贝: parent %+v, child %+v", n, handle.Snapshot().Graph)
			}
		})
	}
}

func TestSnapshotSubCreatedAndLastAttempt(t *testing.T) {
	t.Run("all-skipped", func(t *testing.T) {
		inKey, childIn := NewKey[int]("snap.parent-in"), NewKey[int]("snap.child-in")
		child := mustNew(t, context.Background(), "snap.unbuilt-child")
		declareIn(t, child, NewPort(childIn))
		mustAdd(t, child, NewNode("inner", "", Requires(childIn), nil, nil))
		g := mustNew(t, context.Background(), "snap.unbuilt")
		declareIn(t, g, NewPort(inKey))
		if err := Sub(g, "step", "", []SubBind{In(inKey, childIn)}, child); err != nil {
			t.Fatal(err)
		}
		var in Inputs
		if err := SkipSeed(&in, inKey); err != nil {
			t.Fatal(err)
		}
		e := snapshotExec(t, g, in)
		if err := e.Start(); err != nil {
			t.Fatal(err)
		}
		if err := e.Wait(); err != nil {
			t.Fatal(err)
		}
		n := e.Snapshot().Nodes[0]
		if n.Status != NodeStatusCompleted || n.Child == nil || e.Snapshot().Graph.Status != GraphStatusCompleted {
			t.Fatalf("挂载点完成并保留子执行: %+v", n)
		}
		if inner := n.Child.Snapshot().Nodes[0]; inner.Status != NodeStatusSkipped {
			t.Fatalf("只有依赖缺值的内部节点跳过: %+v", inner)
		}
	})
	t.Run("last-attempt", func(t *testing.T) {
		out, parentOut := NewKey[int]("snap.retry-child-out"), NewKey[int]("snap.retry-parent-out")
		var attempts atomic.Int32
		child := mustNew(t, context.Background(), "snap.retry-child")
		mustAdd(t, child, NewNode("inner", "", nil, Provides(out), func(rc *RunCtx) error {
			if attempts.Add(1) == 1 {
				return errors.New("retry me")
			}
			return Set(rc, out, 2)
		}))
		if err := child.DeclareOutput(NewPort(out)); err != nil {
			t.Fatal(err)
		}
		var e *Execution
		var children []*Execution
		g := mustNew(t, context.Background(), "snap.retry-parent", WithObserver(ObserverFunc{
			GraphFinished: func(site Site, _ NodeFinishReason, _ error) {
				if site.Path == "step" {
					children = append(children, e.Snapshot().Nodes[0].Child)
				}
			},
		}))
		if err := Sub(g, "step", "", []SubBind{Out(out, parentOut)}, child, Retry(2, 0)); err != nil {
			t.Fatal(err)
		}
		e = snapshotExec(t, g, Inputs{})
		if err := e.Start(); err != nil {
			t.Fatal(err)
		}
		if err := e.Wait(); err != nil {
			t.Fatal(err)
		}
		if len(children) != 2 || children[0] == nil || children[1] == nil || children[0] == children[1] || e.Snapshot().Nodes[0].Child != children[1] {
			t.Fatalf("父快照必须指向最后尝试: %+v", children)
		}
		if children[0].Snapshot().Graph.Status != GraphStatusFailed || children[1].Snapshot().Graph.Status != GraphStatusCompleted || children[0].ID() == children[1].ID() {
			t.Fatal("失败尝试的存在与成功尝试的身份必须分开")
		}
	})
}

func TestSnapshotConcurrentExecutionsAndReaders(t *testing.T) {
	input, output := NewKey[string]("snap.concurrent-in"), NewKey[string]("snap.concurrent-out")
	entered := make(chan struct{})
	g := mustNew(t, context.Background(), "snap.concurrent")
	declareIn(t, g, NewPort(input))
	mustAdd(t, g, NewNode("work", "", Requires(input), Provides(output), func(rc *RunCtx) error {
		v, err := Get(rc, input)
		if err != nil {
			return err
		}
		if v == "hold" {
			close(entered)
			<-rc.Context().Done()
			return rc.Context().Err()
		}
		return Set(rc, output, v)
	}))
	var first, second Inputs
	if err := Seed(&first, input, "hold"); err != nil {
		t.Fatal(err)
	}
	if err := Seed(&second, input, "finish"); err != nil {
		t.Fatal(err)
	}
	a, b := snapshotExec(t, g, first), snapshotExec(t, g, second)
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	snapshotSignal(t, entered)
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Wait(); err != nil {
		t.Fatal(err)
	}
	if a.ID() == b.ID() || a.Snapshot().Nodes[0].Status != NodeStatusRunning || b.Snapshot().Nodes[0].Status != NodeStatusCompleted {
		t.Fatal("两次执行快照污染")
	}
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 100; j++ {
				for _, e := range []*Execution{a, b} {
					if s := e.Snapshot(); s.Graph.Execution != e.ID() || len(s.Nodes) != 1 || s.Nodes[0].ID != "work" {
						t.Errorf("并发快照归属漂移: %+v", s)
					}
				}
			}
		}()
	}
	a.Cancel()
	if err := a.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消首份执行: %v", err)
	}
	readers.Wait()
	if a.Snapshot().Graph.Status != GraphStatusCanceled || b.Snapshot().Graph.Status != GraphStatusCompleted || b.Snapshot().Nodes[0].Outputs[0].State != SlotStateReady {
		t.Fatal("取消污染别的执行")
	}
}

func TestSnapshotSequentialRunsAndNestedIdentity(t *testing.T) {
	innerOut, childOut, rootOut := NewKey[int]("snap.inner-out"), NewKey[int]("snap.child-pass"), NewKey[int]("snap.root-pass")
	leaf := mustNew(t, context.Background(), "snap.leaf")
	mustAdd(t, leaf, NewNode("z", "叶节点", nil, Provides(innerOut), func(rc *RunCtx) error {
		return Set(rc, innerOut, 7)
	}))
	if err := leaf.DeclareOutput(NewPort(innerOut)); err != nil {
		t.Fatal(err)
	}
	child := mustNew(t, context.Background(), "snap.middle")
	if err := Sub(child, "inner", "中层挂载", []SubBind{Out(innerOut, childOut)}, leaf); err != nil {
		t.Fatal(err)
	}
	if err := child.DeclareOutput(NewPort(childOut)); err != nil {
		t.Fatal(err)
	}
	root := mustNew(t, context.Background(), "snap.root")
	if err := Sub(root, "outer", "根挂载", []SubBind{Out(childOut, rootOut)}, child); err != nil {
		t.Fatal(err)
	}
	var check func(*Execution, *Topology)
	check = func(e *Execution, top *Topology) {
		s := e.Snapshot()
		if s.Graph.ID != top.Graph.ID || s.Graph.Path != top.Graph.Path || s.Graph.Path != e.Site().Path || s.Graph.Execution != e.ID() || len(s.Nodes) != len(top.Nodes) {
			t.Fatalf("三视图身份不一致: snapshot %+v / site %+v / topology %+v", s.Graph, e.Site(), top.Graph)
		}
		for i, n := range top.Nodes {
			if s.Nodes[i].ID != n.ID {
				t.Fatalf("子树声明序不一致: %+v / %+v", s.Nodes, top.Nodes)
			}
			if n.Sub != nil {
				if s.Nodes[i].Child == nil {
					t.Fatal("已完成的挂载点缺少子执行")
				}
				check(s.Nodes[i].Child, n.Sub)
			}
		}
	}
	a := snapshotExec(t, root, Inputs{})
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if err := a.Wait(); err != nil {
		t.Fatal(err)
	}
	b := snapshotExec(t, root, Inputs{})
	if a.ID() == b.ID() || b.Snapshot().Graph.Status != GraphStatusPending || a.Snapshot().Graph.Status != GraphStatusCompleted || b.Snapshot().Nodes[0].Child != nil {
		t.Fatal("顺序两轮继承了上次运行状态")
	}
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Wait(); err != nil {
		t.Fatal(err)
	}
	check(a, root.Topology())
	check(b, root.Topology())
	if a.Snapshot().Nodes[0].Child.ID() == b.Snapshot().Nodes[0].Child.ID() {
		t.Fatal("第二轮复用了第一轮子执行")
	}
}

func TestSnapshotParentAndChildConcurrentReaders(t *testing.T) {
	entered := make(chan struct{})
	output, parentOutput := NewKey[int]("snap.concurrent-child-out"), NewKey[int]("snap.concurrent-parent-out")
	leaf := mustNew(t, context.Background(), "snap.concurrent-child")
	mustAdd(t, leaf, NewNode("hold", "", nil, Provides(output), func(rc *RunCtx) error {
		close(entered)
		<-rc.Context().Done()
		return rc.Context().Err()
	}))
	if err := leaf.DeclareOutput(NewPort(output)); err != nil {
		t.Fatal(err)
	}
	root := mustNew(t, context.Background(), "snap.concurrent-parent")
	if err := Sub(root, "step", "", []SubBind{Out(output, parentOutput)}, leaf); err != nil {
		t.Fatal(err)
	}
	e := snapshotExec(t, root, Inputs{})
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	snapshotSignal(t, entered)
	child := e.Snapshot().Nodes[0].Child
	if child == nil {
		t.Fatal("运行中应能取得子执行")
	}
	var readers sync.WaitGroup
	for i := 0; i < 6; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 100; j++ {
				p, c := e.Snapshot(), child.Snapshot()
				if p.Nodes[0].Child != child || c.Graph.Path != "step" || c.Graph.Execution != child.ID() {
					t.Error("父子并发读取丢失身份或活句柄")
				}
			}
		}()
	}
	e.Cancel()
	if err := e.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消 = %v", err)
	}
	readers.Wait()
}

func TestSnapshotDefinitionReadsDuringAssembly(t *testing.T) {
	g := mustNew(t, context.Background(), "snap.assembling")
	e := snapshotExec(t, g, Inputs{})
	start := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for j := 0; j < 300; j++ {
				s := e.Snapshot()
				for _, n := range s.Nodes {
					if n.Status != NodeStatusPending {
						t.Error("尚未启动的节点状态漂移")
					}
				}
			}
		}()
	}
	close(start)
	for i := 0; i < 100; i++ {
		mustAdd(t, g, NewNode(fmt.Sprintf("n%d", i), "", nil, Provides(NewKey[int](fmt.Sprintf("snap.assembly-k%d", i))), nil))
	}
	readers.Wait()
	if len(e.Snapshot().Nodes) != 100 {
		t.Fatal("装配后的快照漏节点")
	}
}

func TestSnapshotContainsNoValueAndMatchesVocabulary(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[Snapshot](), reflect.TypeFor[GraphState](), reflect.TypeFor[NodeState](), reflect.TypeFor[KeyState]()} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if f.Name == "Value" || f.Name == "Values" || f.Name == "Payload" || f.Type == reflect.TypeFor[any]() {
				t.Fatalf("%s.%s 不得承载业务值", typ, f.Name)
			}
		}
	}
	for _, pair := range []struct {
		status NodeStatus
		reason NodeFinishReason
	}{
		{NodeStatusCompleted, NodeCompleted}, {NodeStatusSkipped, NodeSkipped},
		{NodeStatusFailed, NodeFailed}, {NodeStatusCanceled, NodeCanceled},
	} {
		if string(pair.status) != string(pair.reason) {
			t.Fatalf("终态词表漂移: %+v", pair)
		}
	}
	if (*Execution)(nil).Snapshot() != nil {
		t.Fatal("nil execution 应返回 nil 快照")
	}
}
