package pulse

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLifecycleTimeoutReleasesFinishedFork(t *testing.T) {
	for _, shape := range []string{"success", "failure", "retry", "without-timeout"} {
		t.Run(shape, func(t *testing.T) {
			ctx := checkpointTestContext(t)
			g := mustNew(t, ctx, "timeout-cleanup")
			remaining, calls := -1, 0
			probe := func(rc *RunCtx, next func(*RunCtx) error) error {
				original := rc.ctx
				tracked := &lifecycleTrackingContext{Context: original}
				rc.ctx = tracked
				defer func() { rc.ctx = original }()
				err := next(rc)
				// 节点还没退出；不能靠 runNode 的最终 Cancel 掩盖 Timeout 留下的 Fork。
				remaining = tracked.registrations()
				return err
			}
			aspects := []Aspect{probe}
			attempts := 1
			if shape == "retry" || shape == "without-timeout" {
				attempts = 20
				aspects = append(aspects, Retry(attempts, 0))
			}
			if shape != "without-timeout" {
				aspects = append(aspects, Timeout(time.Hour))
			}
			cause := errors.New("quick failure")
			mustAdd(t, g, NewNode("n", "快速返回的 Fork 必须释放", nil, nil, func(*RunCtx) error {
				calls++
				if shape == "success" {
					return nil
				}
				return cause
			}, aspects...))
			err := g.Run(ctx, Inputs{})
			if (shape == "success" && err != nil) || (shape != "success" && !errors.Is(err, cause)) || calls != attempts {
				t.Fatalf("Run err=%v calls=%d want=%d", err, calls, attempts)
			}
			if remaining != 0 {
				t.Fatalf("%s retained %d completed Fork contexts before node cleanup", shape, remaining)
			}
		})
	}
}

func TestLifecycleTimeoutStillWaitsForInnerExit(t *testing.T) {
	ctx := checkpointTestContext(t)
	g := mustNew(t, ctx, "timeout-cooperation")
	canceled, release := make(chan struct{}), make(chan struct{})
	exited := false
	mustAdd(t, g, NewNode("n", "取消之后仍须等业务退出", nil, nil, func(rc *RunCtx) error {
		<-rc.Context().Done()
		close(canceled)
		<-release
		exited = true
		return nil
	}, Timeout(time.Millisecond)))
	e, err := g.NewExecution(ctx, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cancel()
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- e.Wait() }()
	select {
	case <-canceled:
	case <-ctx.Done():
		close(release)
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-finished:
		close(release)
		t.Fatalf("Wait returned before the canceled body exited: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "timeout") || !exited {
			t.Fatalf("timeout completion err=%v exited=%v", err, exited)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got := e.Snapshot(); got.Graph.Status != GraphStatusFailed || got.Nodes[0].Status != NodeStatusFailed {
		t.Fatalf("timeout outcome = %+v / %+v", got.Graph, got.Nodes[0])
	}
}
