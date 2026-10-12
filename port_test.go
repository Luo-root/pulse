package pulse

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 端口（DeclareInput / DeclareOutput）与「通过即封口」的契约用例。
//
// 这一组管三件事：
//  1. 入口声明让「这本来就是外部输入」与「你忘了接线」分得开（前者不该被报成毛病）；
//  2. 静态拓扑里入口的来源是 `SourceInput`，与运行期的 `SourceSeed` 分得开、也不建边；
//  3. 声明是两段式的（失败不留痕），封口之后不再接受声明。
func TestDeclareInputPorts(t *testing.T) {
	ctx := context.Background()
	in := NewKey[string]("pt.in")
	out := NewKey[string]("pt.out")
	other := NewKey[string]("pt.other")

	t.Run("没声明端口时，缺来源照旧被报", func(t *testing.T) {
		g := mustNew(t, ctx, "no-port")
		mustAdd(t, g, NewNode("n", "", Requires(in), Provides(out), func(rc *RunCtx) error {
			return Set(rc, out, "v")
		}))
		err := g.Validate()
		if err == nil || !strings.Contains(err.Error(), "nothing provides or seeds it") {
			t.Fatalf("缺来源该被报，got %v", err)
		}
	})

	t.Run("声明入口之后通过，且来源是 input、不建边", func(t *testing.T) {
		g := mustNew(t, ctx, "with-port")
		if err := g.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		mustAdd(t, g, NewNode("n", "", Requires(in), Provides(out), func(rc *RunCtx) error {
			return Set(rc, out, "v")
		}))
		if err := g.Validate(); err != nil {
			t.Fatalf("声明过入口之后不该报缺来源：%v", err)
		}

		top := g.Topology()
		if len(top.Nodes) != 1 || len(top.Nodes[0].Requires) != 1 {
			t.Fatalf("拓扑形状不对：%+v", top.Nodes)
		}
		from := top.Nodes[0].Requires[0].From
		if from.Kind != SourceInput {
			t.Fatalf("入口的来源该是 SourceInput，got %+v", from)
		}
		if got := from.String(); got != "input" {
			t.Fatalf("SourceInput.String() = %q, want %q", got, "input")
		}
		// 入口是**外部**来源，与 Seed 一样不建边
		if len(top.Edges) != 0 {
			t.Fatalf("入口不该参与建边，got %+v", top.Edges)
		}

		// 运行期照旧：Seed 就是「这一次给入口喂值」
		if err := Seed(g, in, "from-host"); err != nil {
			t.Fatalf("Seed 到声明的入口 = %v", err)
		}
		if err := g.Run(); err != nil {
			t.Fatalf("Run = %v", err)
		}
	})

	t.Run("同一条键重复声明是幂等的；类型不同则报错", func(t *testing.T) {
		g := mustNew(t, ctx, "idempotent")
		if err := g.DeclareInput(NewPort(in), NewPort(out)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := g.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("同名同类型的重复声明该是幂等的，got %v", err)
		}
		// 同名不同类型：在声明处就红，别留到绑定时才说类型不符
		bad := NewKey[int]("pt.in")
		err := g.DeclareInput(NewPort(bad))
		if err == nil || !strings.Contains(err.Error(), "already registered") {
			t.Fatalf("同名不同类型该被拒，got %v", err)
		}
		// 失败那次不留痕：图还能接着装配、接着校验
		if err := g.Validate(); err != nil {
			t.Fatalf("失败声明之后图该仍是可用的，got %v", err)
		}
	})

	t.Run("零值端口被拒", func(t *testing.T) {
		g := mustNew(t, ctx, "zero-port")
		err := g.DeclareInput(Port{})
		if err == nil || !strings.Contains(err.Error(), "zero Port{}") {
			t.Fatalf("零值端口该被拒并说清怎么改，got %v", err)
		}
	})

	t.Run("入口与出口是两张表：同一条键可以既是入口又是出口", func(t *testing.T) {
		g := mustNew(t, ctx, "through")
		if err := g.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := g.DeclareOutput(NewPort(in)); err != nil {
			t.Fatalf("同一条键声明成出口（透传）该被接受，got %v", err)
		}
		mustAdd(t, g, NewNode("n", "", Requires(in), nil, func(rc *RunCtx) error { return nil }))
		if err := g.Validate(); err != nil {
			t.Fatalf("Validate = %v", err)
		}
	})

	t.Run("封口之后不能再声明，报错点明是定义封口", func(t *testing.T) {
		g := mustNew(t, ctx, "sealed-declare")
		mustAdd(t, g, NewNode("n", "", nil, Provides(other), func(rc *RunCtx) error {
			return Set(rc, other, "v")
		}))
		if err := g.Validate(); err != nil {
			t.Fatalf("Validate = %v", err)
		}
		err := g.DeclareInput(NewPort(in))
		if !errors.Is(err, ErrGraphStarted) {
			t.Fatalf("封口后的声明该报 ErrGraphStarted，got %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "sealed") {
			t.Fatalf("报错该点明是定义封口，got %v", err)
		}
	})
}
