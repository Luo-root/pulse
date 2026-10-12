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
//  2. 静态拓扑里入口的来源是 `SourceInput`（定义层「外部来源」只有这一种写法）、也不建边；
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
		if err == nil || !strings.Contains(err.Error(), "nothing provides it") {
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
		// 入口是外部来源，不参与建边（它不是「上游节点」）
		if len(top.Edges) != 0 {
			t.Fatalf("入口不该参与建边，got %+v", top.Edges)
		}

		// 运行期：Seed 就是「这一次给入口喂值」（声明得排在 Validate 之前——通过即封口）
		if err := seedOn(g, in, "from-host"); err != nil {
			t.Fatalf("Seed 到声明的入口 = %v", err)
		}
		if err := runOn(g); err != nil {
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

	// 另一个冻结点：图**跑起来**之后（`Start` 成功）同样不再接受改定义——`Start` 成功会
	// 顺带封整棵树，所以判据与「通过即封口」是同一条；两个触发点共用 ErrGraphStarted，
	// 报文把两个都点出来（见 errSealed）。
	t.Run("跑起来的图也不能再声明端口，报文把两个触发点都点出来", func(t *testing.T) {
		g := mustNew(t, ctx, "started-declare")
		declareIn(t, g, NewPort(other))
		if err := seedOn(g, other, "v"); err != nil { // 空图也要有条来源才过静态校验
			t.Fatalf("Seed = %v", err)
		}
		if err := startOn(g); err != nil {
			t.Fatalf("Start = %v", err)
		}
		for _, err := range []error{g.DeclareInput(NewPort(in)), g.DeclareOutput(NewPort(out))} {
			if !errors.Is(err, ErrGraphStarted) {
				t.Fatalf("启动之后再声明该被拒，got %v", err)
			}
			// 封口与「跑过了」是同一个判据的两个触发点（`Start` 成功也封整棵树），
			// 报文把两个都点出来：单看报文的人得知道自己是踩了哪一个。
			if err == nil || !strings.Contains(err.Error(), "sealed") || !strings.Contains(err.Error(), "started") {
				t.Fatalf("报文该把「通过即封口」与「已经启动」都点出来，got %v", err)
			}
		}
		if err := waitOn(g); err != nil {
			t.Fatalf("Wait = %v", err)
		}
	})
}

// 一批端口要么全进表、要么一条都不进（all-or-nothing，与 `Add` 同一条纪律）。
//
// `DeclareInput(NewPort(NewKey[string]("x")), NewPort(NewKey[int]("x")))` 里的第一条会在
// 提交段真的写进登记表：校验段只查**旧**表的话第二条要到提交时才炸，那时第一条已经进去了
// ——实测：失败之后再单独声明一次 `int x`，报的还是「already registered as string」。
func TestDeclarePortsBatchIsAtomic(t *testing.T) {
	ctx := context.Background()

	t.Run("同一批里同名跨型：当场报，且第一条不进表", func(t *testing.T) {
		g := mustNew(t, ctx, "batch")
		err := g.DeclareInput(NewPort(NewKey[string]("batch.x")), NewPort(NewKey[int]("batch.x")))
		if err == nil || !strings.Contains(err.Error(), "declared twice in this call") {
			t.Fatalf("同一批里同名跨型该当场报，got %v", err)
		}
		// 关键判据：第一条**没有**进表——再声明一次 int x 该成功。
		// 进了表的话这里会报「already registered as string」（就是修复前那副样子）。
		if err := g.DeclareInput(NewPort(NewKey[int]("batch.x"))); err != nil {
			t.Fatalf("int batch.x 本该是干净的（整批没提交），got %v", err)
		}
		// 登记表里留下的也该是 int：此时把 batch.x 声明成 string 必须被拒。
		if err := g.DeclareOutput(NewPort(NewKey[string]("batch.x"))); err == nil {
			t.Fatal("batch.x 已经登记成 int 了，再声明成 string 该被拒")
		}
	})

	t.Run("同一批里同名同型：幂等，照常通过", func(t *testing.T) {
		g := mustNew(t, ctx, "batch2")
		if err := g.DeclareInput(
			NewPort(NewKey[string]("batch2.x")),
			NewPort(NewKey[string]("batch2.x")),
		); err != nil {
			t.Fatalf("同批同名同型该幂等通过，got %v", err)
		}
	})

	t.Run("整批全过才提交：混着好端口与坏端口时一条都不留", func(t *testing.T) {
		g := mustNew(t, ctx, "batch3")
		err := g.DeclareInput(
			NewPort(NewKey[string]("batch3.ok")),
			Port{}, // 零值端口：不该被声明
		)
		if err == nil || !strings.Contains(err.Error(), "zero Port") {
			t.Fatalf("零值端口该被拒，got %v", err)
		}
		// 好端口那条也不许留下：再声明一次该成功（若已进表，同名同型是幂等——所以换成
		// 另一条路径问：把它声明成**出口**且类型不同，仍该干净地按新表登记）。
		if err := g.DeclareInput(NewPort(NewKey[string]("batch3.ok"))); err != nil {
			t.Fatalf("batch3.ok 本该没进表，got %v", err)
		}
	})
}

// 出口和入口一样，是**装配期**就能判的事。`DeclareOutput` 了一条没人产出的键，父图照样
// 能用 `Out` 把它接出去（端口是声明，不是承诺），要等子图跑完才由 `bridgeOutputs` 报一句
// `never arrived`——那时已经跑了一半，病根也不在那句话里。
//
// 判据与 `problemsSourcesLocked`（「入口没人提供」）对称，用的是同一个来源判据
// （`sourceOfLocked`：谁会写这条槽优先，没人写就看它是不是声明过的入口）——所以
// **入口与出口同名（透传）不算没人产出**：父侧种进来的值就是这条槽的值。
func TestDeclareOutputNeedsProducer(t *testing.T) {
	ctx := context.Background()

	t.Run("没人产出的出口：Validate / Start 都拒，Report 也有一条", func(t *testing.T) {
		out := NewKey[string]("op.none")
		g := mustNew(t, ctx, "op.none")
		if err := g.DeclareOutput(NewPort(out)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		err := g.Validate()
		if err == nil || !strings.Contains(err.Error(), `declared output "op.none" is never produced`) {
			t.Fatalf("声明的出口没人产出该在装配期报，got %v", err)
		}
		// 一份判据、两个入口：Validate 与 Start 报的是同一句话
		if serr := startOn(g); serr == nil || serr.Error() != err.Error() {
			t.Fatalf("Validate 与 Start 的判据不一致：\nValidate %v\nStart    %v", err, serr)
		}
		var found bool
		for _, p := range g.Topology().Problems {
			if strings.Contains(p.Error(), "is never produced") {
				found = true
			}
		}
		if !found {
			t.Fatalf("Topology().Problems 里也该有这条：%v", g.Topology().Problems)
		}
	})

	t.Run("有节点产出就过", func(t *testing.T) {
		in := NewKey[string]("op.ok_in")
		out := NewKey[string]("op.ok_out")
		g := mustNew(t, ctx, "op.ok")
		if err := g.DeclareInput(NewPort(in)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := g.DeclareOutput(NewPort(out)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		mustAdd(t, g, NewNode("n", "", Requires(in), Provides(out), func(rc *RunCtx) error {
			return Set(rc, out, "v")
		}))
		if err := g.Validate(); err != nil {
			t.Fatalf("有人产出就不该报：%v", err)
		}
	})

	t.Run("透传（同名入口 + 出口）不算没人产出", func(t *testing.T) {
		through := NewKey[string]("op.through")
		g := mustNew(t, ctx, "op.through")
		if err := g.DeclareInput(NewPort(through)); err != nil {
			t.Fatalf("DeclareInput = %v", err)
		}
		if err := g.DeclareOutput(NewPort(through)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		if err := g.Validate(); err != nil {
			t.Fatalf("透传的出口有来源（就是那条入口），不该报：%v", err)
		}
	})

	t.Run("多条没产出的出口：报文顺序按名字排，不跟 map 迭代走", func(t *testing.T) {
		g := mustNew(t, ctx, "op.order")
		// 故意按倒序声明：顺序要是跟着声明走，这里就该反着来
		for _, name := range []string{"op.z2", "op.z3", "op.z1"} {
			if err := g.DeclareOutput(NewPort(NewKey[string](name))); err != nil {
				t.Fatalf("DeclareOutput = %v", err)
			}
		}
		probs := g.Topology().Problems
		if len(probs) != 3 {
			t.Fatalf("三条没产出的出口该各一条，got %d：%v", len(probs), probs)
		}
		want := []string{"op.z1", "op.z2", "op.z3"}
		for i, w := range want {
			if !strings.Contains(probs[i].Error(), w) {
				t.Fatalf("第 %d 条该是 %s（按名字排，两次问要一模一样）：%v", i, w, probs)
			}
		}
		// 同一张图问两次必须一样：map 迭代顺序是随机的，拿它当顺序的写法在这里会飘
		again := g.Topology().Problems
		for i := range probs {
			if probs[i].Error() != again[i].Error() {
				t.Fatalf("两次问的顺序不一样：\n%v\n%v", probs, again)
			}
		}
	})

	t.Run("挂进父图之后按挂载点报（装配期，不必等它跑完）", func(t *testing.T) {
		out := NewKey[string]("op.mount_out")
		sink := NewKey[string]("op.mount_sink")
		child := mustNew(t, ctx, "op.child")
		if err := child.DeclareOutput(NewPort(out)); err != nil {
			t.Fatalf("DeclareOutput = %v", err)
		}
		d := mustNew(t, ctx, "op.parent")
		if err := Sub(d, "mount", "", []SubBind{Out(out, sink)}, child); err != nil {
			t.Fatalf("Sub = %v", err)
		}
		err := d.Validate()
		if err == nil {
			t.Fatal("子图声明了一条没人产出的出口，父图装配期就该报")
		}
		if !strings.Contains(err.Error(), `subgraph "op.child" (mounted at "mount")`) ||
			!strings.Contains(err.Error(), "is never produced") {
			t.Fatalf("报文该点明是哪里挂的子图、病在出口上，got %v", err)
		}
	})
}
