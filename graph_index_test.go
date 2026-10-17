package pulse

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"testing"
)

// 名字和来源一样，只能在整批通过后占用。既有节点让本用例确实走已经建好的索引。
func TestNodeIDsRejectedBatchLeavesNamesReusable(t *testing.T) {
	for _, failure := range []string{"nil", "same-batch-id", "existing-id", "existing-source", "key-type"} {
		t.Run(failure, func(t *testing.T) {
			g, err := New("node-id-atomic", "失败装配不保留名字")
			if err != nil {
				t.Fatal(err)
			}
			owned := NewKey[string]("owned")
			if err := g.Add(NewNode("owner", "", nil, Provides(owned), nil)); err != nil {
				t.Fatal(err)
			}
			before := g.Topology()
			first := NewNode("candidate", "", nil, nil, nil)
			var last *Node
			switch failure {
			case "same-batch-id":
				last = NewNode("candidate", "", nil, nil, nil)
			case "existing-id":
				last = NewNode("owner", "", nil, nil, nil)
			case "existing-source":
				last = NewNode("later", "", nil, Provides(owned), nil)
			case "key-type":
				last = NewNode("later", "", nil, Provides(NewKey[int](owned.Name())), nil)
			}
			if err := g.addAll([]*Node{first, last}); err == nil {
				t.Fatal("expected rejected batch")
			}
			if after := g.Topology(); !reflect.DeepEqual(after, before) {
				t.Fatalf("rejected batch changed topology: before=%v after=%v", before, after)
			}
			for _, id := range []string{"candidate", "later"} {
				if err := g.Add(NewNode(id, "", nil, nil, nil)); err != nil {
					t.Fatalf("rejected batch reserved %q: %v", id, err)
				}
			}
		})
	}
}

func TestNodeIDsFirstRejectedBatchDoesNotInitializeIndex(t *testing.T) {
	g, err := New("empty-node-index", "首次失败不占名字")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.addAll([]*Node{NewNode("free", "", nil, nil, nil), nil}); err == nil {
		t.Fatal("expected nil node error")
	}
	if g.nodeIDs != nil || len(g.nodes) != 0 {
		t.Fatal("first rejected batch changed the definition")
	}
	if err := g.Add(NewNode("free", "", nil, nil, nil)); err != nil {
		t.Fatalf("first failure reserved node ID: %v", err)
	}
}

func TestNodeIDsCommittedBatchKeepsOrderAndRejectsDuplicates(t *testing.T) {
	g, err := New("batch-node-index", "批量与单个装配共用查重")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"z", "a", "m"}
	batch := make([]*Node, len(ids))
	for i, id := range ids {
		batch[i] = NewNode(id, "", nil, nil, nil)
	}
	if err := g.addAll(batch); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		// 重名必须先于非法 Key 报错，不能因索引而改变原有错误优先级。
		err := g.Add(NewNode(id, "", nil, Provides(NewKey[int]("")), nil))
		if want := fmt.Sprintf("pulse: duplicate node id %q", id); err == nil || err.Error() != want {
			t.Fatalf("duplicate %q: got %v, want %s", id, err, want)
		}
	}
	topology := g.Topology()
	for i, id := range ids {
		if topology.Nodes[i].ID != id {
			t.Fatalf("node %d = %q, want %q", i, topology.Nodes[i].ID, id)
		}
	}
	if err := g.Run(context.Background(), Inputs{}); err != nil {
		t.Fatal(err)
	}
}

func TestNodeIDsConcurrentDuplicateHasOneWinner(t *testing.T) {
	g, err := New("concurrent-node-index", "并发装配仍只有一个名字来源")
	if err != nil {
		t.Fatal(err)
	}
	const workers = 16
	start := make(chan struct{})
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- g.Add(NewNode("same", "", nil, nil, nil))
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if err.Error() != `pulse: duplicate node id "same"` {
			t.Fatalf("unexpected duplicate error: %v", err)
		}
	}
	if winners != 1 || len(g.Topology().Nodes) != 1 {
		t.Fatalf("winners=%d nodes=%d, want one of each", winners, len(g.Topology().Nodes))
	}
}

// 只测公开的逐个装配路径；没有墙钟阈值，便于和修改前做 benchmem 对照。
func BenchmarkGraphNodeAssembly(b *testing.B) {
	for _, size := range []int{1, 10, 100, 1000, 10000} {
		b.Run(fmt.Sprintf("N%d", size), func(b *testing.B) {
			names := make([]string, size)
			for i := range names {
				names[i] = fmt.Sprintf("n%d", i)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				g, err := New("assembly", "")
				if err != nil {
					b.Fatal(err)
				}
				for _, name := range names {
					if err := g.Add(NewNode(name, "", nil, nil, nil)); err != nil {
						b.Fatal(err)
					}
				}
				runtime.KeepAlive(g)
			}
		})
	}
}
