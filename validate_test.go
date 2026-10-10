package pulse

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 装配期可以先问一句「这张图跑得起来吗」：Validate 跑的是 Start 那套只读判据
// （每个 Requires 都有来源 / 依赖无环 / 流式名额与出口），但**不改图的状态**
// ——不置 started、不建 goroutine，所以问完还能接着装、接着跑。
//
// 它存在的理由在装配器那一侧：声明式装配（pulse/yaml）里的子图是运行到才建的，
// 没有这个入口，「子图里这条键没人提供」与「子图里依赖成环」就只能等它第一次
// 运行才由引擎拒掉——而这两条读一遍声明就能定。
func TestValidateReportsWhatStartWouldReject(t *testing.T) {
	x := NewKey[int]("val.x")
	y := NewKey[int]("val.y")

	t.Run("依赖成环：与 Start 报同一句话", func(t *testing.T) {
		g := mustNew(t, context.Background(), "test")
		mustAdd(t, g, NewNode("A", Requires(y), Provides(x), func(rc *RunCtx) error { return nil }))
		mustAdd(t, g, NewNode("B", Requires(x), Provides(y), func(rc *RunCtx) error { return nil }))

		verr := g.Validate()
		if verr == nil {
			t.Fatal("Validate 放行了一张含环的图")
		}
		if !strings.Contains(verr.Error(), "dependency cycle") {
			t.Fatalf("环要报出成环的那条路径，got %v", verr)
		}
		// 一份判据、两个入口：两边的报文逐字相同，装图期提前报的那句才不需要翻译
		serr := g.Start()
		if serr == nil || serr.Error() != verr.Error() {
			t.Fatalf("Validate 与 Start 的判据不一致：\nValidate %v\nStart    %v", verr, serr)
		}
		// 校验不改状态：图仍是「没启动」，Wait 照报 ErrGraphNotStarted
		if werr := g.Wait(); !errors.Is(werr, ErrGraphNotStarted) {
			t.Fatalf("Validate 把图启动起来了？Wait() = %v", werr)
		}
	})

	t.Run("无来源的 Requires", func(t *testing.T) {
		g := mustNew(t, context.Background(), "test")
		mustAdd(t, g, NewNode("waiter", Requires(kA), nil, func(rc *RunCtx) error { return nil }))
		err := g.Validate()
		if err == nil {
			t.Fatal("Validate 放行了一张等不到值的图")
		}
		if !strings.Contains(err.Error(), `"waiter"`) || !strings.Contains(err.Error(), `nothing provides or seeds it`) {
			t.Fatalf("错误里要能读到节点 id 与病因，got %v", err)
		}
		// 补上来源（这正是装配器要留出的余地：问一次不该把图问死）
		if err := Seed(g, kA, "seeded"); err != nil {
			t.Fatalf("Validate 之后补 Seed 被拒：%v（说明它改了状态）", err)
		}
		if err := g.Validate(); err != nil {
			t.Fatalf("补上来源后应通过：%v", err)
		}
	})

	t.Run("通过之后还能接着装、接着跑", func(t *testing.T) {
		g := mustNew(t, context.Background(), "test")
		mustAdd(t, g, NewNode("n", nil, Provides(kB), func(rc *RunCtx) error {
			return Set(rc, kB, "v")
		}))
		if err := g.Validate(); err != nil {
			t.Fatalf("干净的图不该被 Validate 拒：%v", err)
		}
		mustAdd(t, g, NewNode("m", Requires(kB), nil, func(rc *RunCtx) error { return nil }))
		if err := g.Validate(); err != nil {
			t.Fatalf("补节点后仍该通过：%v", err)
		}
		if err := g.Run(); err != nil {
			t.Fatalf("Validate 之后照常跑：%v", err)
		}
		// 已经启动的图直接通过：它在 Start 那一刻就过了同一套判据
		if err := g.Validate(); err != nil {
			t.Fatalf("已跑完的图 Validate = %v, want nil", err)
		}
	})
}
