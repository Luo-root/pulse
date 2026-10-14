package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func streamTestMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func streamTestGraph(t *testing.T, id string, opts ...Option) *Graph {
	t.Helper()
	g, err := New(id, "", opts...)
	streamTestMust(t, err)
	return g
}

func streamTestReject(t *testing.T, g *Graph, in Inputs, wants ...string) {
	t.Helper()
	problems := g.Topology().Problems
	if len(problems) == 0 {
		t.Fatal("Topology accepted the invalid stream")
	}
	first := problems[0].Error()
	for _, want := range wants {
		if !strings.Contains(first, want) {
			t.Fatalf("diagnostic %q does not contain %q", first, want)
		}
	}
	for i := 0; i < 3; i++ {
		if got := fmt.Sprint(g.Topology().Problems); got != fmt.Sprint(problems) {
			t.Fatalf("unstable diagnostics: %s != %v", got, problems)
		}
	}
	if err := g.Validate(); err == nil || err.Error() != first {
		t.Fatalf("Validate = %v; Topology first = %s", err, first)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	e, err := g.NewExecution(ctx, in)
	streamTestMust(t, err)
	defer e.Cancel()
	before := len(e.slots)
	if err := e.Start(); err == nil || err.Error() != first {
		t.Fatalf("Start = %v; Topology first = %s", err, first)
	}
	if len(e.slots) != before || !errors.Is(e.Wait(), ErrGraphNotStarted) {
		t.Fatal("failed Start created slots or started the execution")
	}
	// 失败检查不能封任何一层；本层与子层仍允许装入无关节点。
	streamTestMust(t, g.Add(NewNode("after-rejected-check", "", nil, nil, func(*RunCtx) error { return nil })))
	for _, n := range g.nodes {
		if n.child != nil {
			streamTestMust(t, n.child.Add(NewNode("after-rejected-child-check", "", nil, nil, func(*RunCtx) error { return nil })))
			break
		}
	}
	t.Log(first)
}

func streamTestRun(t *testing.T, g *Graph) {
	t.Helper()
	if problems := g.Topology().Problems; len(problems) != 0 {
		t.Fatalf("legal graph rejected: %v", problems)
	}
	streamTestMust(t, g.Validate())
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	streamTestMust(t, g.Run(ctx, Inputs{}))
}

type streamTestValues struct {
	mu   sync.Mutex
	rows map[string][]int
}

func (v *streamTestValues) add(id string, value int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.rows == nil {
		v.rows = make(map[string][]int)
	}
	v.rows[id] = append(v.rows[id], value)
}

func (v *streamTestValues) full(t *testing.T, count int, ids ...string) {
	t.Helper()
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, id := range ids {
		row := v.rows[id]
		if len(row) != count {
			t.Fatalf("%s received %v, want %d items", id, row, count)
		}
		for i, value := range row {
			if value != i+1 {
				t.Fatalf("%s received out of order: %v", id, row)
			}
		}
	}
}

func streamTestProduce(t *testing.T, g *Graph, id string, out Key[<-chan int], count, buf int) {
	t.Helper()
	streamTestMust(t, Produce(g, id, out, func(rc *RunCtx, send func(int) error) error {
		for i := 1; i <= count; i++ {
			if err := send(i); err != nil {
				return err
			}
		}
		return nil
	}, WithBuffer(buf)))
}

func streamTestConsume(t *testing.T, g *Graph, id string, in Key[<-chan int], values *streamTestValues, label string) {
	t.Helper()
	streamTestMust(t, Consume(g, id, in, func(rc *RunCtx, value int) error {
		values.add(label, value)
		return nil
	}))
}

func streamTestBroadcastSub(t *testing.T, capacity int, childOpts []Option, nested bool, buf, count int) (*Graph, *streamTestValues) {
	t.Helper()
	g := streamTestGraph(t, "broadcast-parent", WithMaxRunning(capacity))
	child := streamTestGraph(t, "broadcast-child", childOpts...)
	values := &streamTestValues{}
	src, a, b := NewKey[<-chan int]("src"), NewKey[<-chan int]("a"), NewKey[<-chan int]("b")
	ca, cb := NewKey[<-chan int]("child.a"), NewKey[<-chan int]("child.b")
	streamTestMust(t, child.DeclareInput(NewPort(ca), NewPort(cb)))
	streamTestConsume(t, child, "ca", ca, values, "a")
	streamTestConsume(t, child, "cb", cb, values, "b")
	if nested {
		outer := streamTestGraph(t, "broadcast-outer")
		streamTestMust(t, outer.DeclareInput(NewPort(ca), NewPort(cb)))
		streamTestMust(t, Sub(outer, "inner", "", []SubBind{In(ca, ca), In(cb, cb)}, child))
		child = outer
	}
	streamTestProduce(t, g, "produce", src, count, buf)
	streamTestMust(t, Tee(g, "tee", src, Keys(a, b), WithBuffer(buf)))
	streamTestMust(t, Sub(g, "sub", "", []SubBind{In(a, ca), In(b, cb)}, child))
	return g, values
}

func TestStreamSubSharedQuota(t *testing.T) {
	for _, c := range []struct {
		name   string
		buf, n int
		nested bool
	}{
		{"unbuffered", 0, 2, false},
		{"buffer-fills", 1, 5, false},
		{"buffer-masks-small-input", 8, 2, false},
		{"nested", 0, 2, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, _ := streamTestBroadcastSub(t, 3, nil, c.nested, c.buf, c.n)
			path := "sub/ca"
			if c.nested {
				path = "sub/inner/ca"
			}
			streamTestReject(t, g, Inputs{}, "needs 4 slots", "WithMaxRunning(3)", path, "cb")
		})
	}
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("quota4-nested-%t", nested), func(t *testing.T) {
			g, values := streamTestBroadcastSub(t, 4, nil, nested, 0, 20)
			streamTestRun(t, g)
			values.full(t, 20, "a", "b")
		})
	}
}

func TestStreamSubIndependentPools(t *testing.T) {
	for _, c := range []struct {
		name   string
		opts   []Option
		nested bool
	}{
		{"same-capacity", []Option{WithMaxRunning(3)}, false},
		{"same-capacity-nested", []Option{WithMaxRunning(3)}, true},
		{"explicit-unlimited", []Option{WithMaxRunning(0)}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			g, values := streamTestBroadcastSub(t, 3, c.opts, c.nested, 0, 20)
			streamTestRun(t, g)
			values.full(t, 20, "a", "b")
		})
	}
}

func TestStreamSubExecQuotaOverride(t *testing.T) {
	g, _ := streamTestBroadcastSub(t, 3, nil, false, 0, 20)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	// 这次运行的 root override 传给未声明的子层，Sub 本身不占第五个额度。
	streamTestMust(t, g.Run(ctx, Inputs{}, WithExecMaxRunning(4)))
	g2, _ := streamTestBroadcastSub(t, 4, nil, false, 0, 20)
	e, err := g2.NewExecution(ctx, Inputs{}, WithExecMaxRunning(3))
	streamTestMust(t, err)
	defer e.Cancel()
	if err := e.Start(); err == nil || !strings.Contains(err.Error(), "needs 4 slots") {
		t.Fatalf("Start did not use root override: %v", err)
	}
}

func TestStreamSubRejectsTwoReadersOfOneInlet(t *testing.T) {
	g := streamTestGraph(t, "split-parent")
	child := streamTestGraph(t, "split-child")
	src, in := NewKey[<-chan int]("src"), NewKey[<-chan int]("child.in")
	streamTestMust(t, child.DeclareInput(NewPort(in)))
	values := &streamTestValues{}
	streamTestConsume(t, child, "ca", in, values, "a")
	streamTestConsume(t, child, "cb", in, values, "b")
	streamTestProduce(t, g, "produce", src, 1, 0)
	streamTestMust(t, Sub(g, "sub", "", []SubBind{In(src, in)}, child))
	streamTestReject(t, g, Inputs{}, "split between them", "sub/ca", "sub/cb")
}

func TestStreamSubPassThrough(t *testing.T) {
	for _, readers := range []int{1, 2} {
		t.Run(fmt.Sprintf("readers-%d", readers), func(t *testing.T) {
			g := streamTestGraph(t, "pass-parent", WithMaxRunning(3))
			pass := streamTestGraph(t, "pass-child")
			src, port := NewKey[<-chan int]("src"), NewKey[<-chan int]("pass")
			streamTestMust(t, pass.DeclareInput(NewPort(port)))
			streamTestMust(t, pass.DeclareOutput(NewPort(port)))
			streamTestProduce(t, g, "produce", src, 20, 0)
			values := &streamTestValues{}
			for i := 0; i < readers; i++ {
				out := NewKey[<-chan int](fmt.Sprintf("returned-%d", i))
				id := fmt.Sprintf("pass-%d", i)
				streamTestMust(t, Sub(g, id, "", []SubBind{In(src, port), Out(port, out)}, pass))
				streamTestConsume(t, g, fmt.Sprintf("read-%d", i), out, values, id)
			}
			if readers == 2 {
				streamTestReject(t, g, Inputs{}, "split between them", "read-0", "read-1")
			} else {
				streamTestRun(t, g)
				values.full(t, 20, "pass-0")
			}
		})
	}
}

func TestStreamSubRepeatedDefinition(t *testing.T) {
	for _, own := range []bool{false, true} {
		for _, capacity := range []int{3, 4} {
			t.Run(fmt.Sprintf("own-%t-capacity-%d", own, capacity), func(t *testing.T) {
				var opts []Option
				if own {
					opts = []Option{WithMaxRunning(1)}
				}
				g := streamTestGraph(t, "mount-parent", WithMaxRunning(capacity))
				child := streamTestGraph(t, "same-definition", opts...)
				in := NewKey[<-chan int]("in")
				streamTestMust(t, child.DeclareInput(NewPort(in)))
				values := &streamTestValues{}
				streamTestMust(t, Consume(child, "read", in, func(rc *RunCtx, value int) error {
					values.add(rc.exec.Site().Path, value)
					return nil
				}))
				src, a, b := NewKey[<-chan int]("src"), NewKey[<-chan int]("a"), NewKey[<-chan int]("b")
				streamTestProduce(t, g, "produce", src, 20, 0)
				streamTestMust(t, Tee(g, "tee", src, Keys(a, b)))
				streamTestMust(t, Sub(g, "left", "", []SubBind{In(a, in)}, child))
				streamTestMust(t, Sub(g, "right", "", []SubBind{In(b, in)}, child))
				if !own && capacity == 3 {
					streamTestReject(t, g, Inputs{}, "needs 4 slots", "left/read", "right/read")
				} else {
					streamTestRun(t, g)
					values.full(t, 20, "left", "right")
				}
			})
		}
	}
}

func TestStreamSubFanOutCompetition(t *testing.T) {
	for _, mounts := range []int{1, 2} {
		t.Run(fmt.Sprintf("mounts-%d", mounts), func(t *testing.T) {
			g := streamTestGraph(t, "fanout-parent")
			child := streamTestGraph(t, "fanout-child")
			src, in := NewKey[<-chan int]("src"), NewKey[<-chan int]("in")
			streamTestMust(t, child.DeclareInput(NewPort(in)))
			streamTestMust(t, FanOut(child, "workers", in, Keys(NewKey[int]("one"), NewKey[int]("two")),
				func(rc *RunCtx, shard int, ch <-chan int) (int, error) {
					count := 0
					for {
						select {
						case <-rc.Context().Done():
							return 0, rc.Context().Err()
						case _, ok := <-ch:
							if !ok {
								return count, nil
							}
							count++
						}
					}
				}))
			streamTestProduce(t, g, "produce", src, 20, 0)
			for i := 0; i < mounts; i++ {
				streamTestMust(t, Sub(g, fmt.Sprintf("mount-%d", i), "", []SubBind{In(src, in)}, child))
			}
			if mounts > 1 {
				streamTestReject(t, g, Inputs{}, "read by 4 nodes", "mount-0/workers", "mount-1/workers")
			} else {
				streamTestRun(t, g)
			}
		})
	}
}

func TestStreamSubTwoInputAliases(t *testing.T) {
	for _, twoReaders := range []bool{false, true} {
		t.Run(fmt.Sprintf("two-readers-%t", twoReaders), func(t *testing.T) {
			g := streamTestGraph(t, "two-input-parent", WithMaxRunning(3))
			child := streamTestGraph(t, "two-input-child")
			src, one, two := NewKey[<-chan int]("src"), NewKey[<-chan int]("one"), NewKey[<-chan int]("two")
			streamTestMust(t, child.DeclareInput(NewPort(one), NewPort(two)))
			values := &streamTestValues{}
			if twoReaders {
				streamTestConsume(t, child, "readOne", one, values, "one")
				streamTestConsume(t, child, "readTwo", two, values, "two")
			} else {
				streamTestMust(t, child.Add(NewNode("readBothAliases", "", Requires(one, two), nil, func(rc *RunCtx) error {
					ch1, err := Get(rc, one)
					if err != nil {
						return err
					}
					ch2, err := Get(rc, two)
					if err != nil {
						return err
					}
					if ch1 != ch2 {
						return fmt.Errorf("In aliases did not preserve channel identity")
					}
					for {
						select {
						case <-rc.Context().Done():
							return rc.Context().Err()
						case value, ok := <-ch1:
							if !ok {
								return nil
							}
							values.add("both", value)
						}
					}
				})))
			}
			streamTestProduce(t, g, "produce", src, 20, 0)
			streamTestMust(t, Sub(g, "sub", "", []SubBind{In(src, one), In(src, two)}, child))
			if twoReaders {
				streamTestReject(t, g, Inputs{}, "split between them", "sub/readOne", "sub/readTwo")
			} else {
				// 同一真实读取者经过两个入口，不能重复计入额度或误判瓜分。
				ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				defer cancel()
				streamTestMust(t, g.Run(ctx, Inputs{}, WithExecMaxRunning(2)))
				values.full(t, 20, "both")
			}
		})
	}
}

func TestStreamSubSiblingStreamsSharePool(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(fmt.Sprintf("own-%t", own), func(t *testing.T) {
			g := streamTestGraph(t, "sibling-parent", WithMaxRunning(3))
			var opts []Option
			if own {
				opts = []Option{WithMaxRunning(2)}
			}
			child := streamTestGraph(t, "sibling-definition", opts...)
			src, done := NewKey[<-chan int]("src"), NewKey[bool]("done")
			streamTestProduce(t, child, "produce", src, 20, 0)
			values := &streamTestValues{}
			streamTestMust(t, Consume(child, "consume", src, func(rc *RunCtx, value int) error {
				values.add(rc.exec.Site().Path, value)
				return nil
			}))
			streamTestMust(t, child.DeclareOutput(NewPort(done)))
			streamTestMust(t, child.Add(NewNode("done", "", nil, Provides(done), func(rc *RunCtx) error { return Set(rc, done, true) })))
			for _, id := range []string{"left", "right"} {
				streamTestMust(t, Sub(g, id, "", []SubBind{Out(done, NewKey[bool](id+".done"))}, child))
			}
			if !own {
				streamTestReject(t, g, Inputs{}, "needs 4 slots", "left/produce", "right/consume")
			} else {
				streamTestRun(t, g)
				values.full(t, 20, "left", "right")
			}
		})
	}
}

func TestStreamSubCountsHandwrittenReader(t *testing.T) {
	g := streamTestGraph(t, "manual-parent", WithMaxRunning(2))
	child := streamTestGraph(t, "manual-child")
	src, branch, in := NewKey[<-chan int]("src"), NewKey[<-chan int]("branch"), NewKey[<-chan int]("in")
	streamTestMust(t, child.DeclareInput(NewPort(in)))
	streamTestMust(t, child.Add(NewNode("manual", "", Requires(in), nil, func(*RunCtx) error { return nil })))
	streamTestProduce(t, g, "produce", src, 1, 0)
	streamTestMust(t, Tee(g, "tee", src, Keys(branch)))
	streamTestMust(t, Sub(g, "sub", "", []SubBind{In(branch, in)}, child))
	streamTestReject(t, g, Inputs{}, "needs 3 slots", "sub/manual")
}
