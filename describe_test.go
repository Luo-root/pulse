package pulse

import (
	"context"
	"testing"
)

// 描述是**构造参数**、不是 Option：#303 阶段一的三处（图 / 节点 / `Sub` 那一步）
// 都能读回来，空串合法，且入图后不再变（只由构造函数写入，没有 setter）。
func TestDescriptionRoundTrip(t *testing.T) {
	g, err := New(context.Background(), "P", "主流程")
	if err != nil {
		t.Fatal(err)
	}
	if got := g.Description(); got != "主流程" {
		t.Fatalf("Graph.Description() = %q, want 主流程", got)
	}

	n := NewNode("load", "读入库", nil, nil, func(*RunCtx) error { return nil })
	if got := n.Description(); got != "读入库" {
		t.Fatalf("Node.Description() = %q, want 读入库", got)
	}

	// `Sub` 那一步的描述落在**父侧那个节点**上（子图自己的描述走它自己的 `New`）。
	in := NewKey[string]("desc.in")
	cin := NewKey[string]("desc.cin")
	if err := Seed(g, in, "v"); err != nil {
		t.Fatal(err)
	}
	err = Sub(g, "step", "跑一张子图", []SubBind{In(in, cin)}, func(sc *SubCtx) (*Graph, error) {
		return New(sc.Context(), sc.GraphID(), "内层子图")
	})
	if err != nil {
		t.Fatal(err)
	}
	var step *Node
	for _, cand := range g.nodes {
		if cand.id == "step" {
			step = cand
		}
	}
	if step == nil {
		t.Fatal("没找到 Sub 那一步")
	}
	if got := step.Description(); got != "跑一张子图" {
		t.Fatalf("Sub 那一步的描述 = %q, want 跑一张子图", got)
	}

	// 空串 = 没写，合法。
	empty, err := New(context.Background(), "Q", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := empty.Description(); got != "" {
		t.Fatalf("没写描述时应该是 \"\"，got %q", got)
	}
}
