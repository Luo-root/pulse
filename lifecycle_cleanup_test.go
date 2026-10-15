package pulse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// 隐去 cancelCtx 的私有 Value，使标准 context 通过公开 AfterFunc 协议注册取消。
// 计数只描述仍注册的回调，不依赖标准库私有 children 字段，也不声称保留业务载荷。
type lifecycleTrackingContext struct {
	context.Context
	mu     sync.Mutex
	active int
}

func (c *lifecycleTrackingContext) Value(any) any { return nil }

func (c *lifecycleTrackingContext) AfterFunc(f func()) func() bool {
	c.mu.Lock()
	c.active++
	c.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			c.mu.Lock()
			c.active--
			c.mu.Unlock()
		})
	}
	stop := context.AfterFunc(c.Context, func() { release(); f() })
	return func() bool {
		stopped := stop()
		if stopped {
			release()
		}
		return stopped
	}
}

func (c *lifecycleTrackingContext) registrations() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active
}

func TestLifecycleRunCleansRejectedStart(t *testing.T) {
	for _, shape := range []string{"missing-source", "cycle", "missing-output", "checkpoint-hooks"} {
		t.Run(shape, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			tracked := &lifecycleTrackingContext{Context: parent}
			g := mustNew(t, parent, "rejected-start")
			x, y := NewKey[int]("x"), NewKey[int]("y")
			var opts []ExecOption
			switch shape {
			case "missing-source":
				mustAdd(t, g, NewNode("n", "缺来源", Requires(x), nil, nil))
			case "cycle":
				mustAdd(t, g, NewNode("a", "环的一边", Requires(x), Provides(y), nil))
				mustAdd(t, g, NewNode("b", "环的另一边", Requires(y), Provides(x), nil))
			case "missing-output":
				declareOut(t, g, NewPort(x))
			case "checkpoint-hooks":
				opts = append(opts, WithExecCheckpoint("root", CheckpointHooks{}))
			}
			for range 20 {
				if err := g.Run(tracked, Inputs{}, opts...); err == nil {
					t.Fatal("invalid Start accepted")
				}
			}
			if got := tracked.registrations(); got != 0 {
				t.Fatalf("20 rejected Run calls retained %d host cancellation registrations", got)
			}
			if g.sealed || parent.Err() != nil {
				t.Fatal("rejected Run sealed the definition or canceled the host")
			}
		})
	}
}

func TestLifecycleRunCleansTerminalExecution(t *testing.T) {
	for _, shape := range []string{"success", "failure", "cancel"} {
		t.Run(shape, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			tracked := &lifecycleTrackingContext{Context: parent}
			g := mustNew(t, parent, "terminal-run")
			cause := errors.New("business failure")
			mustAdd(t, g, NewNode("n", "终态清理", nil, nil, func(rc *RunCtx) error {
				switch shape {
				case "failure":
					return cause
				case "cancel":
					cancel()
					<-rc.Context().Done()
					return rc.Context().Err()
				}
				return nil
			}))
			err := g.Run(tracked, Inputs{})
			if (shape == "success" && err != nil) || (shape == "failure" && !errors.Is(err, cause)) || (shape == "cancel" && !errors.Is(err, context.Canceled)) {
				t.Fatalf("%s Run = %v", shape, err)
			}
			if got := tracked.registrations(); got != 0 {
				t.Fatalf("terminal Run retained %d registrations", got)
			}
		})
	}
}

func TestLifecycleRejectedExecutionStartCanBeRepaired(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprintf("restore=%v", restore), func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			tracked := &lifecycleTrackingContext{Context: parent}
			g := mustNew(t, parent, "repair-start")
			x, y := NewKey[int]("x"), NewKey[int]("y")
			mustAdd(t, g, NewNode("n", "补装配后能启动", Requires(x), Provides(y), func(rc *RunCtx) error { return Set(rc, y, 2) }))
			var opts []ExecOption
			if restore {
				opts = append(opts, WithExecCheckpoint("root", (&checkpointProbe{}).hooks()))
			}
			e, err := g.NewExecution(tracked, Inputs{}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Cancel()
			if restore {
				if err := e.RestoreNodes([]NodeCheckpoint{{NodeID: "n", Reason: NodeCompleted, Outputs: []CheckpointOutput{{Key: CheckpointKey{Name: "y", Type: reflect.TypeFor[int]()}, State: SlotStateReady, Value: 2}}}}); err != nil {
					t.Fatal(err)
				}
			}
			if !restore {
				if err := e.Start(); err == nil {
					t.Fatal("missing source accepted")
				}
			}
			mustAdd(t, g, NewNode("source", "补来源", nil, Provides(x), func(rc *RunCtx) error { return Set(rc, x, 1) }))
			if restore {
				if err := e.Start(); err == nil {
					t.Fatal("late-invalidated restore accepted")
				}
				if err := e.RestoreNodes(nil); err != nil {
					t.Fatal(err)
				}
			}
			if e.Context().Err() != nil || e.started || g.sealed || tracked.registrations() != 1 {
				t.Fatalf("rejected standalone Start lost repairability: ctx=%v started=%v sealed=%v registrations=%d", e.Context().Err(), e.started, g.sealed, tracked.registrations())
			}
			if err := e.Start(); err != nil {
				t.Fatal(err)
			}
			if err := e.Wait(); err != nil {
				t.Fatal(err)
			}
			if got := tracked.registrations(); got != 0 {
				t.Fatalf("repaired Execution retained %d registrations", got)
			}
		})
	}
}
