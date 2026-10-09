package pulse

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 汇聚锚：三路里有一路以跳过到达 → fn 照样执行，缺项在 Batch 里可见。
func TestJoinCollectsWhatArrives(t *testing.T) {
	a := NewKey[string]("join.a")
	b := NewKey[string]("join.b")
	c := NewKey[string]("join.c")
	out := NewKey[string]("join.out")
	g := mustNew(t, context.Background(), "join")
	if err := Seed(g, a, "A"); err != nil {
		t.Fatal(err)
	}
	if err := SkipSeed(g, b); err != nil {
		t.Fatal(err)
	}
	if err := Seed(g, c, "C"); err != nil {
		t.Fatal(err)
	}
	var got Batch[string]
	if err := Join(g, "collect", Keys(a, b, c), out, func(rc *RunCtx, batch Batch[string]) (string, error) {
		got = batch
		return strings.Join(batch.Values(), "+"), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	vals := got.Values()
	if len(vals) != 2 || vals[0] != "A" || vals[1] != "C" {
		t.Fatalf("Values() = %v, want [A C]", vals)
	}
	miss := got.Missing()
	if len(miss) != 1 || miss[0] != b.Name() {
		t.Fatalf("Missing() = %v, want [%s]", miss, b.Name())
	}
	if got.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", got.Len())
	}
}

// Batch 带来源：**每一条声明都在 Items 里**（缺项也在），值可以按名字取——同类型
// 多槽位的顺序编译期锁不住，这条设计就是为了让「数错位次」不再有后果。
//
// 这里刻意让两条来源的值**一模一样**：只按位置读的话，拿到哪个都一样，分不出来。
func TestBatchCarriesSourceNames(t *testing.T) {
	a := NewKey[string]("batchsrc.a")
	b := NewKey[string]("batchsrc.b")
	c := NewKey[string]("batchsrc.c")
	out := NewKey[string]("batchsrc.out")
	g := mustNew(t, context.Background(), "batchsrc")
	if err := Seed(g, a, "same"); err != nil {
		t.Fatal(err)
	}
	if err := Seed(g, b, "same"); err != nil {
		t.Fatal(err)
	}
	if err := SkipSeed(g, c); err != nil {
		t.Fatal(err)
	}
	var got Batch[string]
	if err := Join(g, "collect", Keys(a, b, c), out, func(rc *RunCtx, batch Batch[string]) (string, error) {
		got = batch
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 3 {
		t.Fatalf("Items = %+v, want 三条（缺项也在）", got.Items)
	}
	want := []BatchItem[string]{
		{Key: a.Name(), Value: "same", Present: true},
		{Key: b.Name(), Value: "same", Present: true},
		{Key: c.Name()}, // 以跳过到达：Key 在、Present=false、Value 是零值
	}
	for i, w := range want {
		if got.Items[i] != w {
			t.Fatalf("Items[%d] = %+v, want %+v", i, got.Items[i], w)
		}
	}
	// 按名字取：有值的给值，缺项与不在这一束里的名字都不 ok。
	if v, ok := got.Value(b.Name()); !ok || v != "same" {
		t.Fatalf("Value(%q) = %q, %v; want same, true", b.Name(), v, ok)
	}
	if v, ok := got.Value(c.Name()); ok || v != "" {
		t.Fatalf("缺项按名字取不该 ok：%q, %v", v, ok)
	}
	if v, ok := got.Value("batchsrc.nope"); ok || v != "" {
		t.Fatalf("不在这一束里的名字不该 ok：%q, %v", v, ok)
	}
	// Values() 每次新建切片：调用方改它不影响 Batch（下面再取一次要还原样）。
	vals := got.Values()
	if len(vals) != 2 {
		t.Fatalf("Values() = %v, want 两条", vals)
	}
	vals[0] = "mutated"
	if again := got.Values(); again[0] != "same" {
		t.Fatalf("Values() 返回的切片被改后影响到了 Batch：%v", again)
	}
}

// 缺项不升格成失败：全部输入都跳过时 fn 不执行、节点跳过、Run 仍是 nil。
func TestJoinAllSkippedNeverRuns(t *testing.T) {
	a := NewKey[string]("joinsk.a")
	b := NewKey[string]("joinsk.b")
	out := NewKey[string]("joinsk.out")
	g := mustNew(t, context.Background(), "joinsk")
	if err := SkipSeed(g, a); err != nil {
		t.Fatal(err)
	}
	if err := SkipSeed(g, b); err != nil {
		t.Fatal(err)
	}
	if err := Join(g, "collect", Keys(a, b), out, func(rc *RunCtx, batch Batch[string]) (string, error) {
		t.Fatal("一条值都没到，fn 不该执行")
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatalf("全跳过是正常结果: %v", err)
	}
}

// 严格模式是**显式**的：fn 把 Batch.WaitAll() 直接返回出去，节点即以跳过收尾
// （不是失败），并且已经发布的输出不回滚。
func TestJoinWaitAllStrictOptOut(t *testing.T) {
	obs := &recordingObserver{}
	a := NewKey[string]("joinstrict.a")
	b := NewKey[string]("joinstrict.b")
	out := NewKey[string]("joinstrict.out")
	g := mustNew(t, context.Background(), "joinstrict", WithObserver(obs))
	if err := Seed(g, a, "A"); err != nil {
		t.Fatal(err)
	}
	if err := SkipSeed(g, b); err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := Join(g, "strict", Keys(a, b), out, func(rc *RunCtx, batch Batch[string]) (string, error) {
		ran = true
		if err := batch.WaitAll(); err != nil {
			return "", err
		}
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatalf("严格模式下的跳过不是失败: %v", err)
	}
	if !ran {
		t.Fatal("有一条值到了，fn 应当进入执行（再由它自己决定跳过）")
	}
	log := obs.snapshot()
	if countPref(log, "F:strict:skipped") != 1 {
		t.Fatalf("严格模式应当以 skipped 收尾，log = %v", log)
	}
	if countPref(log, "R:strict") != 1 {
		t.Fatalf("应当真的进入过执行，log = %v", log)
	}
}

// 部分产出是正常路径：三个实例里第二个返回 NoValue()，下游 Join 在 Missing 里
// 看见它，整轮仍然是成功的——「跳过是到达，不是失败」在糖上同样成立。
func TestFanOutPartialValueIsMissingNotFailure(t *testing.T) {
	in := NewKey[string]("fanout.in")
	outs := Keys(NewKey[string]("fanout.r1"), NewKey[string]("fanout.r2"), NewKey[string]("fanout.r3"))
	joined := NewKey[string]("fanout.joined")
	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "fanout", WithObserver(obs))
	if err := Seed(g, in, "doc"); err != nil {
		t.Fatal(err)
	}
	if err := FanOut(g, "worker", in, outs, func(rc *RunCtx, shard int, v string) (string, error) {
		if shard == 2 {
			return "", NoValue() // 第 2 份没有产出：这正是「部分产出」的正常表达
		}
		return v + "#" + string(rune('0'+shard)), nil
	}); err != nil {
		t.Fatal(err)
	}
	var got Batch[string]
	if err := Join(g, "collect", outs, joined, func(rc *RunCtx, batch Batch[string]) (string, error) {
		got = batch
		return strings.Join(batch.Values(), ","), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatalf("部分产出不该让这一轮失败: %v", err)
	}
	vals := got.Values()
	if len(vals) != 2 || vals[0] != "doc#1" || vals[1] != "doc#3" {
		t.Fatalf("Values() = %v, want [doc#1 doc#3]", vals)
	}
	miss := got.Missing()
	if len(miss) != 1 || miss[0] != "fanout.r2" {
		t.Fatalf("Missing() = %v, want [fanout.r2]", miss)
	}
	log := obs.snapshot()
	if countPref(log, "F:worker-2:skipped") != 1 {
		t.Fatalf("没产出的实例应当以 skipped 收尾，log = %v", log)
	}
	if countPref(log, "F:worker-1:completed") != 1 || countPref(log, "F:worker-3:completed") != 1 {
		t.Fatalf("另外两个实例应当 completed，log = %v", log)
	}
}

// FanOut 的节点名与分片号一致（观测归因要靠它）：两个实例分别叫 worker-1 /
// worker-2，拿到的 shard 也是 1 / 2。
//
// 注意**不要在 worker 之间共享可变状态**（这里各自只写自己的输出槽，再由下游
// 汇聚读出来）：`FanOut` 的实例是真并发，往同一个切片 append 就是竞态。
func TestFanOutNamesWorkersByShard(t *testing.T) {
	in := NewKey[string]("fanoutname.in")
	outs := Keys(NewKey[string]("fanoutname.r1"), NewKey[string]("fanoutname.r2"))
	joined := NewKey[string]("fanoutname.joined")
	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "fanoutname", WithObserver(obs))
	if err := Seed(g, in, "doc"); err != nil {
		t.Fatal(err)
	}
	if err := FanOut(g, "worker", in, outs, func(rc *RunCtx, shard int, v string) (string, error) {
		return v + "#" + strconv.Itoa(shard), nil
	}); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := Join(g, "collect", outs, joined, func(rc *RunCtx, b Batch[string]) (string, error) {
		got = append(got, b.Values()...)
		return strings.Join(b.Values(), ","), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "doc#1" || got[1] != "doc#2" {
		t.Fatalf("分片号没传到 fn：%v, want [doc#1 doc#2]", got)
	}
	log := obs.snapshot()
	for _, want := range []string{"W:worker-1", "W:worker-2", "F:worker-1:completed", "F:worker-2:completed"} {
		if countPref(log, want) != 1 {
			t.Fatalf("节点命名/归因不对：log = %v，缺少 %q", log, want)
		}
	}
}

// 失败显式：任一实例报错 → 首错取消整图，Run 返回原错误（糖不吞错）。
func TestFanOutFailureCancelsGraph(t *testing.T) {
	in := NewKey[string]("fanoutfail.in")
	outs := Keys(NewKey[string]("fanoutfail.r1"), NewKey[string]("fanoutfail.r2"))
	boom := errors.New("worker boom")
	g := mustNew(t, context.Background(), "fanoutfail")
	if err := Seed(g, in, "doc"); err != nil {
		t.Fatal(err)
	}
	if err := FanOut(g, "worker", in, outs, func(rc *RunCtx, shard int, v string) (string, error) {
		if shard == 2 {
			return "", boom
		}
		return v, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); !errors.Is(err, boom) {
		t.Fatalf("Run = %v, want worker error", err)
	}
}

// 装配必须**整批原子**：N 个 worker 里只要有一个装不进去（输出槽已被别人占住），
// 整个 FanOut 失败且图里不留这一批的任何痕迹——否则调用方以为整个 fan-out 没装上，
// 图上却跑着前几个 worker，观测里冒出一批没人认领的节点。
func TestFanOutAssemblyIsAtomic(t *testing.T) {
	in := NewKey[string]("fanoutatomic.in")
	outs := Keys(NewKey[string]("fanoutatomic.r1"), NewKey[string]("fanoutatomic.r2"), NewKey[string]("fanoutatomic.r3"))
	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "fanoutatomic", WithObserver(obs))
	if err := Seed(g, in, "doc"); err != nil {
		t.Fatal(err)
	}
	if err := Seed(g, outs[2], "taken"); err != nil { // r3 先被占住 → worker-3 装不进去
		t.Fatal(err)
	}
	err := FanOut(g, "worker", in, outs, func(rc *RunCtx, shard int, v string) (string, error) {
		return v, nil
	})
	if !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("FanOut = %v, want ErrDuplicateSource", err)
	}
	// 判据一：r1 没被 worker-1 占住（真留下残留的话，这次 Add 会吃 ErrDuplicateSource）
	if err := g.Add(NewNode("solo", Requires(in), Provides(outs[0]), func(rc *RunCtx) error {
		return Set(rc, outs[0], "solo")
	})); err != nil {
		t.Fatalf("失败的 FanOut 在图上留下了 r1 的占位：%v", err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	// 判据二：没有任何 worker 真的跑起来（残留在图上就会在观测里出现）
	log := obs.snapshot()
	for _, n := range []string{"worker-1", "worker-2", "worker-3"} {
		if countPref(log, "W:"+n) != 0 || countPref(log, "F:"+n) != 0 {
			t.Fatalf("失败的 FanOut 留下了节点 %s：log = %v", n, log)
		}
	}
	if countPref(log, "F:solo:completed") != 1 {
		t.Fatalf("log = %v, want solo 跑完", log)
	}
}

// 回调拿到的 RunCtx 就是本节点自己的那个（取消看它）：这是给 fn 传 rc 的**唯一**
// 理由——长任务要能在整图取消时醒来，而不是把 ctx 藏在引擎里。
//
// 时序是确定的：实例 1 进入 fn 之后才放实例 2 去失败，所以「取消叫醒卡住的那个」
// 一定被测到（不靠调度碰运气）；5 秒看门狗把「没叫醒」直接报成一句可读的失败，
// 而不是让整个测试套件挂到超时。
func TestFanOutCallbackSeesCancellation(t *testing.T) {
	in := NewKey[string]("fanoutctx.in")
	outs := Keys(NewKey[string]("fanoutctx.r1"), NewKey[string]("fanoutctx.r2"))
	boom := errors.New("instance 2 boom")
	g := mustNew(t, context.Background(), "fanoutctx")
	if err := Seed(g, in, "doc"); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}) // 实例 1 已进入 fn
	var (
		sawCancel bool
		sawNode   string
		sawNode2  string
	)
	if err := FanOut(g, "ctx", in, outs, func(rc *RunCtx, shard int, v string) (string, error) {
		if shard == 2 {
			<-entered // 等实例 1 确实进了 fn，再制造失败
			sawNode2 = rc.NodeID()
			return "", boom
		}
		sawNode = rc.NodeID()
		close(entered)
		<-rc.Context().Done() // 业务在这里等外部资源；整图取消应当把它叫醒
		sawCancel = true
		return "", rc.Context().Err()
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- g.Run() }()
	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Fatalf("Run = %v, want 首错 %v", err, boom)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("整图取消没能叫醒卡在回调里的实例：图挂住了")
	}
	if !sawCancel {
		t.Fatal("回调没能通过 rc.Context() 感知到取消")
	}
	if sawNode != "ctx-1" || sawNode2 != "ctx-2" {
		t.Fatalf("回调拿到的是别人的上下文：%q / %q, want ctx-1 / ctx-2", sawNode, sawNode2)
	}
}

// NoValue 是「这次没有值」的显式结果：手写节点返回它 → 以跳过收尾，不是失败。
func TestNoValueSkipsNode(t *testing.T) {
	out := NewKey[string]("novalue.out")
	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "novalue", WithObserver(obs))
	if err := g.Add(NewNode("n", nil, Provides(out), func(rc *RunCtx) error {
		return NoValue(out.Name())
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatalf("跳过不是失败: %v", err)
	}
	log := obs.snapshot()
	if countPref(log, "F:n:skipped") != 1 {
		t.Fatalf("log = %v, want 跳过收尾", log)
	}
}

// NoValue 与「跳过某一条输出」的差别在**终态**上，不只是措辞：前者整个节点
// skipped，后者只让那条槽跳过、节点自己照常 completed。文档里那句区分钉在这里。
func TestNoValueVersusPerKeySkip(t *testing.T) {
	a := NewKey[string]("nvskip.a")
	b := NewKey[string]("nvskip.b")
	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "nvskip", WithObserver(obs))
	mustAdd(t, g, NewNode("novalue", nil, Provides(a), func(rc *RunCtx) error {
		return NoValue(a.Name())
	}))
	mustAdd(t, g, NewNode("perkey", nil, Provides(b), func(rc *RunCtx) error {
		return Skip(rc, b) // 只把这条输出标成跳过，节点自己正常收尾
	}))
	if err := g.Run(); err != nil {
		t.Fatalf("两种跳过都不是失败: %v", err)
	}
	log := obs.snapshot()
	if countPref(log, "F:novalue:skipped") != 1 {
		t.Fatalf("NoValue 应当整个节点跳过：log = %v", log)
	}
	if countPref(log, "F:perkey:completed") != 1 {
		t.Fatalf("Skip 只跳输出槽，节点应当 completed：log = %v", log)
	}
}

// 装配期入参校验：nil 图 / 空 id / 空输入 / 空输出 / nil fn。
//
// 空 id 单独钉：`FanOut` 的节点名是「id + "-" + 分片号」拼出来的，不前置拦下就会
// 造出叫 `-1` / `-2` 的节点（观测里归因不了，也和 Join 的行为不一致）。
func TestSugarRejectsBadArgs(t *testing.T) {
	k := NewKey[string]("sugar.bad")
	out := NewKey[string]("sugar.bad.out")
	g := mustNew(t, context.Background(), "sugar-bad")
	join := func(rc *RunCtx, b Batch[string]) (string, error) { return "", nil }
	work := func(rc *RunCtx, shard int, v string) (string, error) { return v, nil }
	if err := Join(nil, "j", Keys(k), out, join); err == nil {
		t.Fatal("nil graph 应当报错")
	}
	if err := Join(g, "", Keys(k), out, join); err == nil {
		t.Fatal("空 id 应当报错")
	}
	if err := Join(g, "j", nil, out, join); err == nil {
		t.Fatal("空输入应当报错")
	}
	if err := Join(g, "j", Keys(k), out, nil); err == nil {
		t.Fatal("nil fn 应当报错")
	}
	if err := FanOut(nil, "s", k, Keys(out), work); err == nil {
		t.Fatal("nil graph 应当报错")
	}
	if err := FanOut(g, "", k, Keys(out), work); err == nil {
		t.Fatal("空 id 应当报错")
	}
	if err := FanOut(g, "s", k, nil, work); err == nil {
		t.Fatal("空输出应当报错")
	}
	if err := FanOut(g, "s", k, Keys(out), nil); err == nil {
		t.Fatal("nil fn 应当报错")
	}
	// 上面每一次都必须什么都没留下：`-1` 这种名字只有空 id 被放行才会出现。
	if err := g.Add(NewNode("-1", nil, nil, func(rc *RunCtx) error { return nil })); err != nil {
		t.Fatalf("入参校验失败的装配在图里留下了残留：%v", err)
	}
}

// 编译期对齐的反向用例：元素类型不匹配时**必须编译失败**——糖买的正是这个。
// 坏例放 testdata（go 工具链的 wildcard 默认忽略该目录），这里显式 build 一次。
func TestSugarTypeMismatchFailsToCompile(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go 不在 PATH 上，跳过编译期对齐的负例")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "./testdata/compilefail")
	cmd.Dir = wd
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("元素类型不匹配的 Join 竟然编译通过了：\n%s", out)
	}
	// 断言失败原因就是**我们种下的**那处不匹配：报错里必须同时出现两个元素类型
	// （只断言「失败了」会被「坏例本身坏了」蒙混过去——比如写错了包名）。
	msg := string(out)
	if !strings.Contains(msg, "Batch[int]") || !strings.Contains(msg, "Batch[string]") {
		t.Fatalf("编译失败的原因不是那处类型不匹配（可能坏例本身坏了）：\n%s", msg)
	}
}
