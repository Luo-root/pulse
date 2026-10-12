package pulse

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 节点 ID 恰好叫 `seed` 时，「一个 Key 一个来源」不能被绕过。
//
// 旧实现把外部来源记成字符串哨兵 `"seed"`：节点 `seed` 提供 K 之后来源表里存的就是
// `"seed"`，于是那条冲突判据认为「同一个来源」——**静默返回 nil**，宿主的值抢先占住
// 槽位，下游读到宿主值，整图照样成功。结构化来源之后，表里只有节点来源、判据只按
// `kind` 走，名字再像也分得开。
//
// 阶段 2 之后「喂值」不再往定义里写（`Seed` 写的是**这一次运行的** `Inputs`），所以这次
// 撞车从「喂值那一刻报」搬到「建执行那一刻报」——纪律不变：**图内已有来源时，外面喂进来
// 的值一律被拒**（放进来的话那个节点的产出会被幂等首写静默顶掉），下游读到的仍是节点
// 那条值。两个方向都钉住，才算把「撞名」这件事盖全。
func TestSourceConflictWhenNodeIsNamedSeed(t *testing.T) {
	k := NewKey[string]("src.clash")
	provide := func(rc *RunCtx) error { return Set(rc, k, "from-node") }

	// 先 Add 后喂：建执行当场报。
	g := mustNew(t, context.Background(), "P")
	mustAdd(t, g, NewNode("seed", "", nil, Provides(k), provide))
	declareIn(t, g, NewPort(k))
	if err := seedOn(g, k, "host"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.NewExecution(context.Background(), pendingOf(g).in); err == nil {
		t.Fatal("先 Add 后喂：图内已有来源，外面喂的值必须被拒，不能静默顶掉节点的产出")
	}
	// 「值不许被顶掉」的正面证据：清掉那份输入再跑，读到的必须是节点那条。
	pendingOf(g).in = Inputs{}
	var got string
	mustAdd(t, g, NewNode("reader", "", Requires(k), nil, func(rc *RunCtx) error {
		v, err := Get(rc, k)
		if err != nil {
			return err
		}
		got = v
		return nil
	}))
	if err := runOn(g); err != nil {
		t.Fatal(err)
	}
	if got != "from-node" {
		t.Fatalf("读到的值 = %q，want from-node（宿主那份不该被写进去）", got)
	}

	// 先喂后 Add：装得进去（喂值不占来源），但拿那份输入去跑的建执行那一刻同样被拒
	// ——这一侧旧实现是**静默通过**的（来源表里存的是 `"seed"`，节点 id 恰好也叫
	// `"seed"`，相等那一支放行）。
	g2 := mustNew(t, context.Background(), "P2")
	declareIn(t, g2, NewPort(k))
	if err := seedOn(g2, k, "host"); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, g2, NewNode("seed", "", nil, Provides(k), provide))
	if _, err := g2.NewExecution(context.Background(), pendingOf(g2).in); err == nil {
		t.Fatal("先喂后 Add：装完节点再拿那份输入跑，必须报")
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
// 图里必须有**两个以上节点**：`problemsCycleLocked` 在只有一个节点时直接返回（自环
// 在 `Add` 就被拒了，不可能存在），单节点图根本走不到这段判据——用它测不出这个 bug。
func TestNodeNamedSeedDoesNotFakeCycle(t *testing.T) {
	for _, id := range []string{"seed", "loader"} {
		external := NewKey[string]("src.fake.ext." + id)
		out := NewKey[string]("src.fake.out." + id)
		final := NewKey[string]("src.fake.final." + id)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		g := mustNew(t, ctx, "P-"+id)
		// 第一个节点不依赖自己：它读的 external 由外部 Seed 提供。
		err := g.Add(NewNode(id, "", Requires(external), Provides(out), func(rc *RunCtx) error {
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
		err = g.Add(NewNode("sink", "", Requires(out), Provides(final), func(rc *RunCtx) error {
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
		declareIn(t, g, NewPort(external))
		if err := seedOn(g, external, "v"); err != nil {
			cancel()
			t.Fatal(err)
		}
		err = runOn(g)
		cancel()
		if err != nil {
			t.Fatalf("节点 id %q：合法的无环图被拒了：%v", id, err)
		}
		if st, v := slotOn(g, final.asRef()); st != slotReady || v != "v-ok!" {
			t.Fatalf("节点 id %q：final = (%v, %v)，want (ready, v-ok!)", id, st, v)
		}
	}
}

// 冲突文案：报文要**点名那条键与那个节点**，且节点 id 是任意用户字符串——一律写成
// `node %q` 统一格式（不是裸 id，也不是给 `seed` 这个哨兵名字开小灶），所以节点叫
// `seed`、叫 `none`、id 本身长成 `node "seed"` 的都分得开（见
// TestSourceNameDistinguishesKinds）。两个方向（先 Add 后喂 / 先喂后 Add）走同一条报文。
func TestSourceConflictText(t *testing.T) {
	const wantTmpl = `pulse: input src.txt.confl:string: this key is provided by node %q inside ` +
		`graph "P", so a value fed from outside would be silently ignored (a key has exactly one source)`
	for _, nodeID := range []string{"load", "seed", "none", `node "seed"`} {
		t.Run(nodeID, func(t *testing.T) {
			k := NewKey[string]("src.txt.confl")
			provide := func(*RunCtx) error { return nil }

			// 先 Add 后喂。
			g := mustNew(t, context.Background(), "P")
			mustAdd(t, g, NewNode(nodeID, "", nil, Provides(k), provide))
			declareIn(t, g, NewPort(k))
			if err := seedOn(g, k, "v"); err != nil {
				t.Fatal(err)
			}
			_, err := g.NewExecution(context.Background(), pendingOf(g).in)
			if err == nil {
				t.Fatal("want 冲突，got nil")
			}
			if want := fmt.Sprintf(wantTmpl, nodeID); err.Error() != want {
				t.Fatalf("文案不对\n got %s\nwant %s", err.Error(), want)
			}

			// 先喂后 Add：同一条报文（撞车在建执行那一刻判，不在 Add 那一刻）。
			g2 := mustNew(t, context.Background(), "P")
			declareIn(t, g2, NewPort(k))
			if err := seedOn(g2, k, "v"); err != nil {
				t.Fatal(err)
			}
			mustAdd(t, g2, NewNode(nodeID, "", nil, Provides(k), provide))
			_, err = g2.NewExecution(context.Background(), pendingOf(g2).in)
			if err == nil {
				t.Fatal("want 冲突，got nil")
			}
			if want := fmt.Sprintf(wantTmpl, nodeID); err.Error() != want {
				t.Fatalf("文案不对\n got %s\nwant %s", err.Error(), want)
			}
		})
	}
}

// 来源名必须**两两可分**：节点 id 是任意用户字符串，只给某个哨兵名字开小灶是不够的
// （实测：节点 id 叫 `none` 会与「没有来源」的 `none` 撞，字面量 id `node "seed"` 会与
// 节点 `seed` 的输出撞）。这里把几种名字钉死，并断言两两不同——「报错里分得开」正是
// 这次修的那两处静默错误在**文案层**的对应物。
func TestSourceNameDistinguishesKinds(t *testing.T) {
	cases := []struct {
		name string
		src  source
		want string
	}{
		{"声明过的入口", source{kind: sourceInput}, "input"},
		{"普通节点", source{kind: sourceNode, nodeID: "load"}, `node "load"`},
		{"节点 id 恰好是 seed", source{kind: sourceNode, nodeID: "seed"}, `node "seed"`},
		{"节点 id 恰好是 none", source{kind: sourceNode, nodeID: "none"}, `node "none"`},
		{"节点 id 本身长成 node \"seed\"", source{kind: sourceNode, nodeID: `node "seed"`}, `node "node \"seed\""`},
		{"没有来源（零值）", source{}, "none"},
	}
	seen := map[string]string{}
	for _, c := range cases {
		got := c.src.String()
		if got != c.want {
			t.Fatalf("%s：String() = %q，want %q", c.name, got, c.want)
		}
		if prev, dup := seen[got]; dup {
			t.Fatalf("%s 与 %s 都写成 %q——来源名必须两两可分", c.name, prev, got)
		}
		seen[got] = c.name
	}
}

// 来源表里落了零值（`sourceNone`）时，所有判据都必须把它当「没有来源」。
//
// 零值只可能由包内写进去（字段未导出），所以这条只能在包内测：一旦谁把它当「有来源」，
// 等这个 Key 的节点会永远停在 pending——`Run` 不返回，也没有任何报错。
func TestZeroSourceInTableCountsAsMissing(t *testing.T) {
	k := NewKey[string]("src.zero")

	g := mustNew(t, context.Background(), "P")
	mustAdd(t, g, NewNode("waiter", "", Requires(k), nil, func(*RunCtx) error { return nil }))

	// 正常路径：没声明过的 Key 根本不在表里。
	if _, ok := g.producer[k.name]; ok {
		t.Fatal("没声明过的 Key 不该出现在来源表里")
	}
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "nothing provides it") {
		t.Fatalf("没有来源的 Key 必须被拒：%v", err)
	}

	// 防御：把零值塞进表里，判据仍须按「没有来源」拒。
	g.mu.Lock()
	g.producer[k.name] = source{}
	g.mu.Unlock()
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "nothing provides it") {
		t.Fatalf("零值来源必须按「没有来源」拒：%v", err)
	}

	// 同一个零值也不能让 `Start` 放行——那是「等不到值的图」被判成合法。
	if err := startOn(g); err == nil || !strings.Contains(err.Error(), "nothing provides it") {
		t.Fatalf("零值来源必须挡住 Start：%v", err)
	}
}
