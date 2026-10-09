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
	if err := Join(g, "collect", Keys(a, b, c), out, func(batch Batch[string]) (string, error) {
		got = batch
		return strings.Join(batch.Values, "+"), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if len(got.Values) != 2 || got.Values[0] != "A" || got.Values[1] != "C" {
		t.Fatalf("Values = %v, want [A C]", got.Values)
	}
	if len(got.Missing) != 1 || got.Missing[0] != b.Name() {
		t.Fatalf("Missing = %v, want [%s]", got.Missing, b.Name())
	}
	if got.Len() != 2 {
		t.Fatalf("Len = %d, want 2", got.Len())
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
	if err := Join(g, "collect", Keys(a, b), out, func(batch Batch[string]) (string, error) {
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
	if err := Join(g, "strict", Keys(a, b), out, func(batch Batch[string]) (string, error) {
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
func TestSpreadPartialValueIsMissingNotFailure(t *testing.T) {
	in := NewKey[string]("spread.in")
	outs := Keys(NewKey[string]("spread.r1"), NewKey[string]("spread.r2"), NewKey[string]("spread.r3"))
	joined := NewKey[string]("spread.joined")
	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "spread", WithObserver(obs))
	if err := Seed(g, in, "doc"); err != nil {
		t.Fatal(err)
	}
	if err := Spread(g, "worker", in, outs, func(shard int, v string) (string, error) {
		if shard == 2 {
			return "", NoValue() // 第 2 份没有产出：这正是「部分产出」的正常表达
		}
		return v + "#" + string(rune('0'+shard)), nil
	}); err != nil {
		t.Fatal(err)
	}
	var got Batch[string]
	if err := Join(g, "collect", outs, joined, func(batch Batch[string]) (string, error) {
		got = batch
		return strings.Join(batch.Values, ","), nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatalf("部分产出不该让这一轮失败: %v", err)
	}
	if len(got.Values) != 2 || got.Values[0] != "doc#1" || got.Values[1] != "doc#3" {
		t.Fatalf("Values = %v, want [doc#1 doc#3]", got.Values)
	}
	if len(got.Missing) != 1 || got.Missing[0] != "spread.r2" {
		t.Fatalf("Missing = %v, want [spread.r2]", got.Missing)
	}
	log := obs.snapshot()
	if countPref(log, "F:worker-2:skipped") != 1 {
		t.Fatalf("没产出的实例应当以 skipped 收尾，log = %v", log)
	}
	if countPref(log, "F:worker-1:completed") != 1 || countPref(log, "F:worker-3:completed") != 1 {
		t.Fatalf("另外两个实例应当 completed，log = %v", log)
	}
}

// Spread 的节点名与分片号一致（观测归因要靠它）：两个实例分别叫 worker-1 /
// worker-2，拿到的 shard 也是 1 / 2。
//
// 注意**不要在 worker 之间共享可变状态**（这里各自只写自己的输出槽，再由下游
// 汇聚读出来）：`Spread` 的实例是真并发，往同一个切片 append 就是竞态。
func TestSpreadNamesWorkersByShard(t *testing.T) {
	in := NewKey[string]("spreadname.in")
	outs := Keys(NewKey[string]("spreadname.r1"), NewKey[string]("spreadname.r2"))
	joined := NewKey[string]("spreadname.joined")
	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "spreadname", WithObserver(obs))
	if err := Seed(g, in, "doc"); err != nil {
		t.Fatal(err)
	}
	if err := Spread(g, "worker", in, outs, func(shard int, v string) (string, error) {
		return v + "#" + strconv.Itoa(shard), nil
	}); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := Join(g, "collect", outs, joined, func(b Batch[string]) (string, error) {
		got = append(got, b.Values...)
		return strings.Join(b.Values, ","), nil
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
func TestSpreadFailureCancelsGraph(t *testing.T) {
	in := NewKey[string]("spreadfail.in")
	outs := Keys(NewKey[string]("spreadfail.r1"), NewKey[string]("spreadfail.r2"))
	boom := errors.New("worker boom")
	g := mustNew(t, context.Background(), "spreadfail")
	if err := Seed(g, in, "doc"); err != nil {
		t.Fatal(err)
	}
	if err := Spread(g, "worker", in, outs, func(shard int, v string) (string, error) {
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

// 装配期入参校验：nil 图 / 空输入 / 空输出 / nil fn。
func TestSugarRejectsBadArgs(t *testing.T) {
	k := NewKey[string]("sugar.bad")
	g := mustNew(t, context.Background(), "sugar-bad")
	fn := func(b Batch[string]) (string, error) { return "", nil }
	work := func(shard int, v string) (string, error) { return v, nil }
	if err := Join(nil, "j", Keys(k), k, fn); err == nil {
		t.Fatal("nil graph 应当报错")
	}
	if err := Join(g, "j", nil, k, fn); err == nil {
		t.Fatal("空输入应当报错")
	}
	if err := Join(g, "j", Keys(k), k, nil); err == nil {
		t.Fatal("nil fn 应当报错")
	}
	if err := Spread(g, "s", k, nil, work); err == nil {
		t.Fatal("空输出应当报错")
	}
	if err := Spread(nil, "s", k, Keys(k), work); err == nil {
		t.Fatal("nil graph 应当报错")
	}
	if err := Spread(g, "s", k, Keys(k), nil); err == nil {
		t.Fatal("nil fn 应当报错")
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
