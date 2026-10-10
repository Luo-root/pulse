package pulse

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 节点 ID 恰好叫 `seed` 时，「一个 Key 一个来源」不能被绕过。
//
// 旧实现把外部来源记成字符串哨兵 `"seed"`：节点 `seed` 提供 K 之后来源表里存的就是
// `"seed"`，于是 `Seed(g, K, …)` 的冲突判据认为「同一个来源」——**静默返回 nil**，
// 宿主的值抢先占住槽位，下游读到宿主值，整图照样成功。结构化来源之后，两条路都必须
// 报 ErrDuplicateSource。
func TestSourceConflictWhenNodeIsNamedSeed(t *testing.T) {
	k := NewKey[string]("src.clash")
	provide := func(*RunCtx) error { return nil }

	// 先 Add 后 Seed：旧实现这里静默通过。
	g := mustNew(t, context.Background(), "P")
	mustAdd(t, g, NewNode("seed", nil, Provides(k), provide))
	if err := Seed(g, k, "host"); !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("先 Add 后 Seed：want ErrDuplicateSource，got %v", err)
	}
	// 失败的 Seed 不许留下痕迹（与 Add 同一条纪律）。
	if st, _ := g.slotOf(k.asRef()).snapshot(); st != slotPending {
		t.Fatalf("失败的 Seed 写进了槽位：state = %v, want slotPending", st)
	}

	// 先 Seed 后 Add：这一侧旧实现也报错（prev 是 "seed"、owner 是节点 id "seed"，
	// 恰好也相等——所以它**同样**静默通过）。两条一起钉住。
	g2 := mustNew(t, context.Background(), "P2")
	if err := Seed(g2, k, "host"); err != nil {
		t.Fatal(err)
	}
	if err := g2.Add(NewNode("seed", nil, Provides(k), provide)); !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("先 Seed 后 Add：want ErrDuplicateSource，got %v", err)
	}

	// 来源表里不许出现零值（sourceNone）——「没有来源」的表达是「表里没有这个 Key」。
	for name, src := range g.producer {
		if !src.present() {
			t.Fatalf("来源表里出现了零值：%s → %+v", name, src)
		}
	}
}

// 节点 ID 恰好叫 `seed` 的**合法无环图**必须跑得起来。
//
// 旧实现在环检查里拿来源名去查节点索引：external 由外部 Seed 提供、来源名是 `"seed"`，
// 正好查到那个节点自己 → 造出一条**自环** → `Start` 报一个不存在的环。对照组（节点改个
// 名字）旧实现是通过的——两条一起测，才说明修的是「来源名偶然等于节点名」，而不是
// 把合法图拒掉。
//
// 图里必须有**两个以上节点**：`checkAcyclicLocked` 在只有一个节点时直接返回（自环
// 在 `Add` 就被拒了，不可能存在），单节点图根本走不到这段判据——用它测不出这个 bug。
func TestNodeNamedSeedDoesNotFakeCycle(t *testing.T) {
	for _, id := range []string{"seed", "loader"} {
		external := NewKey[string]("src.fake.ext." + id)
		out := NewKey[string]("src.fake.out." + id)
		final := NewKey[string]("src.fake.final." + id)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		g := mustNew(t, ctx, "P-"+id)
		// 第一个节点不依赖自己：它读的 external 由外部 Seed 提供。
		err := g.Add(NewNode(id, Requires(external), Provides(out), func(rc *RunCtx) error {
			v, err := Get(rc, external)
			if err != nil {
				return err
			}
			return Set(rc, out, v+"-ok")
		}))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		// 第二个节点让图「不止一个节点」，环检查才会真的跑。
		err = g.Add(NewNode("sink", Requires(out), Provides(final), func(rc *RunCtx) error {
			v, err := Get(rc, out)
			if err != nil {
				return err
			}
			return Set(rc, final, v+"!")
		}))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if err := Seed(g, external, "v"); err != nil {
			cancel()
			t.Fatal(err)
		}
		err = g.Run()
		cancel()
		if err != nil {
			t.Fatalf("节点 id %q：合法的无环图被拒了：%v", id, err)
		}
		if st, v := g.slotOf(final.asRef()).snapshot(); st != slotReady || v != "v-ok!" {
			t.Fatalf("节点 id %q：final = (%v, %v)，want (ready, v-ok!)", id, st, v)
		}
	}
}

// 冲突文案：普通场景**逐字不变**；节点恰好叫 `seed` 时写成 `node "seed"`，否则报错里
// 两个 `seed` 无从分辨——而那正是这次修的路径。
func TestSourceConflictText(t *testing.T) {
	provide := func(*RunCtx) error { return nil }
	cases := []struct {
		name string
		want string
		run  func(t *testing.T) error
	}{
		{
			name: "Add 普通节点后 Seed：报节点 id（旧文案）",
			want: `pulse: key already has a source: "src.txt.a" already sourced by load`,
			run: func(t *testing.T) error {
				k := NewKey[string]("src.txt.a")
				g := mustNew(t, context.Background(), "P")
				mustAdd(t, g, NewNode("load", nil, Provides(k), provide))
				return Seed(g, k, "v")
			},
		},
		{
			name: "Seed 后 Add 普通节点：报外部来源 seed（旧文案）",
			want: `pulse: key already has a source: "src.txt.b" already sourced by seed`,
			run: func(t *testing.T) error {
				k := NewKey[string]("src.txt.b")
				g := mustNew(t, context.Background(), "P")
				if err := Seed(g, k, "v"); err != nil {
					t.Fatal(err)
				}
				return g.Add(NewNode("load", nil, Provides(k), provide))
			},
		},
		{
			name: "Add 节点 seed 后 Seed：已有来源是**节点**，写成 node \"seed\"",
			want: `pulse: key already has a source: "src.txt.c" already sourced by node "seed"`,
			run: func(t *testing.T) error {
				k := NewKey[string]("src.txt.c")
				g := mustNew(t, context.Background(), "P")
				mustAdd(t, g, NewNode("seed", nil, Provides(k), provide))
				return Seed(g, k, "v")
			},
		},
		{
			name: "Seed 后 Add 节点 seed：已有来源是**外部**，写成 seed",
			want: `pulse: key already has a source: "src.txt.d" already sourced by seed`,
			run: func(t *testing.T) error {
				k := NewKey[string]("src.txt.d")
				g := mustNew(t, context.Background(), "P")
				if err := Seed(g, k, "v"); err != nil {
					t.Fatal(err)
				}
				return g.Add(NewNode("seed", nil, Provides(k), provide))
			},
		},
	}
	for _, c := range cases {
		err := c.run(t)
		if err == nil {
			t.Fatalf("%s：want 冲突，got nil", c.name)
		}
		if !errors.Is(err, ErrDuplicateSource) {
			t.Fatalf("%s：want ErrDuplicateSource 在错误链里，got %v", c.name, err)
		}
		if err.Error() != c.want {
			t.Fatalf("%s：文案不对\n got %s\nwant %s", c.name, err.Error(), c.want)
		}
	}
}

// 来源表里落了零值（`sourceNone`）时，所有判据都必须把它当「没有来源」。
//
// 零值只可能由包内写进去（字段未导出），所以这条只能在包内测：一旦谁把它当「有来源」，
// 等这个 Key 的节点会永远停在 pending——`Run` 不返回，也没有任何报错。
func TestZeroSourceInTableCountsAsMissing(t *testing.T) {
	k := NewKey[string]("src.zero")

	g := mustNew(t, context.Background(), "P")
	mustAdd(t, g, NewNode("waiter", Requires(k), nil, func(*RunCtx) error { return nil }))

	// 正常路径：没声明过的 Key 根本不在表里。
	if _, ok := g.producer[k.name]; ok {
		t.Fatal("没声明过的 Key 不该出现在来源表里")
	}
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "nothing provides or seeds it") {
		t.Fatalf("没有来源的 Key 必须被拒：%v", err)
	}

	// 防御：把零值塞进表里，判据仍须按「没有来源」拒。
	g.mu.Lock()
	g.producer[k.name] = source{}
	g.mu.Unlock()
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "nothing provides or seeds it") {
		t.Fatalf("零值来源必须按「没有来源」拒：%v", err)
	}

	// 同一个零值也不能让 `Start` 放行——那是「等不到值的图」被判成合法。
	if err := g.Start(); err == nil || !strings.Contains(err.Error(), "nothing provides or seeds it") {
		t.Fatalf("零值来源必须挡住 Start：%v", err)
	}
}
