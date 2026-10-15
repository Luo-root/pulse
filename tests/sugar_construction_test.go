package pulse_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse"
)

func sugarConstructionMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func sugarConstructionRun(t *testing.T, g *pulse.Graph, in pulse.Inputs) *pulse.Execution {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	e, err := g.NewExecution(ctx, in)
	sugarConstructionMust(t, err)
	sugarConstructionMust(t, e.Start())
	sugarConstructionMust(t, e.Wait())
	return e
}

// 五个入口原样保留说明，FanOut 的每个 worker 都有同一份说明；空串也能正常运行。
func TestSugarConstructionDescriptions(t *testing.T) {
	for _, tc := range []struct {
		name, description string
	}{
		{"text", "  节点说明\n保留空格、换行与中文  "},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := pulse.New("descriptions", "构造器说明")
			sugarConstructionMust(t, err)
			in := pulse.NewKey[int]("in")
			first, second, joined := pulse.NewKey[int]("first"), pulse.NewKey[int]("second"), pulse.NewKey[int]("joined")
			sugarConstructionMust(t, g.DeclareInput(pulse.NewPort(in)))
			sugarConstructionMust(t, g.DeclareOutput(pulse.NewPort(joined)))
			sugarConstructionMust(t, pulse.FanOut(g, "worker", tc.description, in, pulse.Keys(first, second),
				func(_ *pulse.RunCtx, shard, value int) (int, error) { return value + shard, nil }))
			sugarConstructionMust(t, pulse.Join(g, "join", tc.description, pulse.Keys(first, second), joined,
				func(_ *pulse.RunCtx, batch pulse.Batch[int]) (int, error) {
					sum := 0
					for _, value := range batch.Values() {
						sum += value
					}
					return sum, nil
				}))

			src, a, b := pulse.NewKey[<-chan int]("src"), pulse.NewKey[<-chan int]("a"), pulse.NewKey[<-chan int]("b")
			sugarConstructionMust(t, pulse.Produce(g, "produce", tc.description, src,
				func(_ *pulse.RunCtx, send func(int) error) error { return send(7) }))
			sugarConstructionMust(t, pulse.Tee(g, "tee", tc.description, src, pulse.Keys(a, b)))
			var received [2][]int
			for i, key := range pulse.Keys(a, b) {
				id := []string{"consume-a", "consume-b"}[i]
				sugarConstructionMust(t, pulse.Consume(g, id, tc.description, key, func(_ *pulse.RunCtx, value int) error {
					received[i] = append(received[i], value)
					return nil
				}))
			}
			sugarConstructionMust(t, g.Validate())
			ids := []string{"worker-1", "worker-2", "join", "produce", "tee", "consume-a", "consume-b"}
			top := g.Topology()
			if len(top.Nodes) != len(ids) {
				t.Fatalf("Topology nodes = %d, want %d", len(top.Nodes), len(ids))
			}
			for i, node := range top.Nodes {
				if node.ID != ids[i] || node.Description != tc.description {
					t.Fatalf("Topology node %d = %q / %q, want %q / %q", i, node.ID, node.Description, ids[i], tc.description)
				}
			}
			var inputs pulse.Inputs
			sugarConstructionMust(t, pulse.Seed(&inputs, in, 10))
			e := sugarConstructionRun(t, g, inputs)
			snapshot := e.Snapshot()
			if len(snapshot.Nodes) != len(ids) {
				t.Fatalf("Snapshot nodes = %d, want %d", len(snapshot.Nodes), len(ids))
			}
			for i, node := range snapshot.Nodes {
				if node.ID != ids[i] || node.Description != tc.description || node.Status != pulse.NodeStatusCompleted {
					t.Fatalf("Snapshot node %d = %+v, want %q / %q / completed", i, node, ids[i], tc.description)
				}
			}
			if value, state, err := pulse.ReadOutput(e, joined); err != nil || state != pulse.SlotStateReady || value != 23 {
				t.Fatalf("joined = %d / %s / %v, want 23 / ready / nil", value, state, err)
			}
			if want := [2][]int{{7}, {7}}; !reflect.DeepEqual(received, want) {
				t.Fatalf("stream values = %v, want %v", received, want)
			}
		})
	}
}

// 顺序修改原切片即可复现旧缺陷，不需要与运行并发：声明仍是 [a,b]，回调却读成 [a,a]。
func TestJoinConstructionCopiesInputs(t *testing.T) {
	for _, when := range []string{"after-assembly", "after-validation"} {
		for _, change := range []struct {
			name  string
			order [2]int
		}{
			{"duplicate", [2]int{0, 0}},
			{"reorder", [2]int{1, 0}},
			{"undeclared", [2]int{0, 2}},
		} {
			t.Run(when+"/"+change.name, func(t *testing.T) {
				g, err := pulse.New("join-copy", "固定汇聚接线")
				sugarConstructionMust(t, err)
				a, b, other, out := pulse.NewKey[string]("a"), pulse.NewKey[string]("b"), pulse.NewKey[string]("other"), pulse.NewKey[string]("out")
				sugarConstructionMust(t, g.DeclareInput(pulse.NewPort(a), pulse.NewPort(b)))
				sugarConstructionMust(t, g.DeclareOutput(pulse.NewPort(out)))
				ins := pulse.Keys(a, b)
				var received pulse.Batch[string]
				sugarConstructionMust(t, pulse.Join(g, "join", "汇聚", ins, out, func(_ *pulse.RunCtx, batch pulse.Batch[string]) (string, error) {
					received = batch
					return strings.Join(batch.Values(), ""), nil
				}))
				top := g.Topology()
				if when == "after-validation" {
					sugarConstructionMust(t, g.Validate())
				}
				keys := pulse.Keys(a, b, other)
				ins[0], ins[1] = keys[change.order[0]], keys[change.order[1]]
				sugarConstructionMust(t, g.Validate())
				if got := g.Topology(); !reflect.DeepEqual(got, top) {
					t.Fatalf("changing caller inputs changed topology: got %+v, want %+v", got.Nodes, top.Nodes)
				}
				var inputs pulse.Inputs
				sugarConstructionMust(t, pulse.Seed(&inputs, a, "A"))
				sugarConstructionMust(t, pulse.Seed(&inputs, b, "B"))
				e := sugarConstructionRun(t, g, inputs)
				want := []pulse.BatchItem[string]{{Key: "a", Value: "A", Present: true}, {Key: "b", Value: "B", Present: true}}
				if !reflect.DeepEqual(received.Items, want) {
					t.Fatalf("Batch = %+v, want %+v", received.Items, want)
				}
				if value, state, err := pulse.ReadOutput(e, out); err != nil || state != pulse.SlotStateReady || value != "AB" {
					t.Fatalf("joined = %q / %s / %v, want AB / ready / nil", value, state, err)
				}
			})
		}
	}
}

// WithBuffer(1) + 一个值钉住旧版的静默成功：[a,a] 让 b 被自动跳过，整轮却返回 nil。
func TestTeeConstructionCopiesOutputs(t *testing.T) {
	for _, when := range []string{"after-assembly", "after-validation"} {
		for _, change := range []struct {
			name  string
			order [2]int
		}{
			{"duplicate", [2]int{0, 0}},
			{"reorder", [2]int{1, 0}},
			{"undeclared", [2]int{0, 2}},
		} {
			t.Run(when+"/"+change.name, func(t *testing.T) {
				g, err := pulse.New("tee-copy", "固定广播接线")
				sugarConstructionMust(t, err)
				src, a, b, other := pulse.NewKey[<-chan int]("src"), pulse.NewKey[<-chan int]("a"), pulse.NewKey[<-chan int]("b"), pulse.NewKey[<-chan int]("other")
				sugarConstructionMust(t, pulse.Produce(g, "produce", "生产", src, func(_ *pulse.RunCtx, send func(int) error) error { return send(7) }))
				outs := pulse.Keys(a, b)
				sugarConstructionMust(t, pulse.Tee(g, "tee", "广播", src, outs, pulse.WithBuffer(1)))
				var received [2][]int
				for i, key := range pulse.Keys(a, b) {
					id := []string{"consume-a", "consume-b"}[i]
					sugarConstructionMust(t, pulse.Consume(g, id, "消费", key, func(_ *pulse.RunCtx, value int) error {
						received[i] = append(received[i], value)
						return nil
					}))
				}
				top := g.Topology()
				if when == "after-validation" {
					sugarConstructionMust(t, g.Validate())
				}
				keys := pulse.Keys(a, b, other)
				outs[0], outs[1] = keys[change.order[0]], keys[change.order[1]]
				sugarConstructionMust(t, g.Validate())
				if got := g.Topology(); !reflect.DeepEqual(got, top) {
					t.Fatalf("changing caller outputs changed topology: got %+v, want %+v", got.Nodes, top.Nodes)
				}
				e := sugarConstructionRun(t, g, pulse.Inputs{})
				if want := [2][]int{{7}, {7}}; !reflect.DeepEqual(received, want) {
					t.Fatalf("broadcast values = %v, want %v", received, want)
				}
				for _, node := range e.Snapshot().Nodes {
					if node.Status != pulse.NodeStatusCompleted {
						t.Fatalf("node %q status = %s, want completed", node.ID, node.Status)
					}
					if node.ID == "tee" {
						for i, output := range node.Outputs {
							if output.Name != []string{"a", "b"}[i] || output.State != pulse.SlotStateReady {
								t.Fatalf("Tee output %d = %+v, want original ready output", i, output)
							}
						}
					}
				}
			})
		}
	}
}
