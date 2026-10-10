package pulse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 正常结束：值按序到达，两端都在节点生命周期里。
func TestProduceConsumeRoundTrip(t *testing.T) {
	in := NewKey[<-chan int]("s.rt")
	g := mustNew(t, context.Background(), "roundtrip")

	if err := Produce(g, "src", in, func(rc *RunCtx, send func(int) error) error {
		for i := 1; i <= 5; i++ {
			if err := send(i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var got []int
	if err := Consume(g, "sink", in, func(rc *RunCtx, v int) error {
		got = append(got, v)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if want := []int{1, 2, 3, 4, 5}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// 带缓冲也按序到达（削峰不改变顺序）。
func TestWithBufferKeepsOrder(t *testing.T) {
	in := NewKey[<-chan string]("s.buf")
	g := mustNew(t, context.Background(), "buffered")

	if err := Produce(g, "src", in, func(rc *RunCtx, send func(string) error) error {
		for _, v := range []string{"a", "b", "c"} {
			if err := send(v); err != nil {
				return err
			}
		}
		return nil
	}, WithBuffer(2)); err != nil {
		t.Fatal(err)
	}

	var got []string
	if err := Consume(g, "sink", in, func(rc *RunCtx, v string) error {
		got = append(got, v)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint([]string{"a", "b", "c"}) {
		t.Fatalf("got %v", got)
	}
}

// 空流：下游**照样进入 Run**（槽位里放的是 channel，走的是「值到了」这条路），
// 零次回调、终态 completed、整轮成功——不是跳过。
func TestConsumeEmptyStreamEntersRun(t *testing.T) {
	in := NewKey[<-chan int]("s.empty")
	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "empty", WithObserver(obs))

	if err := Produce(g, "src", in, func(rc *RunCtx, send func(int) error) error {
		return nil // 一次都不发
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := Consume(g, "sink", in, func(rc *RunCtx, v int) error {
		calls++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("空流不该有回调，got %d", calls)
	}
	log := obs.snapshot()
	if countPref(log, "R:sink") != 1 {
		t.Fatalf("空流的下游仍应进入 Run：%v", log)
	}
	if countPref(log, "F:sink:completed") != 1 {
		t.Fatalf("空流的下游应以 completed 收尾：%v", log)
	}
}

// 装配期拦「名额养不起这条流」：maxRun == 1 直接拒，且图上不留痕迹。
func TestProduceRejectsSingleSlot(t *testing.T) {
	in := NewKey[<-chan int]("s.one")
	g := mustNew(t, context.Background(), "oneslot", WithMaxRunning(1))

	err := Produce(g, "src", in, func(rc *RunCtx, send func(int) error) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "WithMaxRunning") {
		t.Fatalf("maxRun=1 应在装配期被拒，got %v", err)
	}
	if n := len(g.nodes); n != 0 {
		t.Fatalf("拒绝之后图上不该留节点，got %d", n)
	}

	// 名额够（或不设）就装得下
	g2 := mustNew(t, context.Background(), "twoslot", WithMaxRunning(2))
	if err := Produce(g2, "src", in, func(rc *RunCtx, send func(int) error) error { return nil }); err != nil {
		t.Fatalf("maxRun=2 应当装得下: %v", err)
	}
}

// Tee 要 N+1 个名额（自己 + 每个下游一个）。
func TestTeeNeedsSlotForEveryDownstream(t *testing.T) {
	in := NewKey[<-chan int]("s.teein")
	a := NewKey[<-chan int]("s.teea")
	b := NewKey[<-chan int]("s.teeb")
	c := NewKey[<-chan int]("s.teec")

	g := mustNew(t, context.Background(), "tee2", WithMaxRunning(3))
	if err := Tee(g, "fan", in, Keys(a, b)); err != nil {
		t.Fatalf("两个下游 + 名额 3 应当装得下: %v", err)
	}

	g2 := mustNew(t, context.Background(), "tee3", WithMaxRunning(3))
	err := Tee(g2, "fan", in, Keys(a, b, c))
	if err == nil || !strings.Contains(err.Error(), "WithMaxRunning") {
		t.Fatalf("三个下游 + 名额 3 应在装配期被拒，got %v", err)
	}
	if n := len(g2.nodes); n != 0 {
		t.Fatalf("拒绝之后图上不该留节点，got %d", n)
	}
}

// 取消必须**被消费端看见**：Run 返回取消原因，而不是「成功」（取消没人看见就会
// 变成 nil —— #286）。
func TestConsumeSeesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := NewKey[<-chan int]("s.cancel")
	g := mustNew(t, ctx, "cancel")

	if err := Produce(g, "src", in, func(rc *RunCtx, send func(int) error) error {
		for i := 0; ; i++ {
			if err := send(i); err != nil {
				return err
			}
			time.Sleep(2 * time.Millisecond)
		}
	}); err != nil {
		t.Fatal(err)
	}
	var got int32
	if err := Consume(g, "sink", in, func(rc *RunCtx, v int) error {
		atomic.AddInt32(&got, 1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	err := g.Run()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if atomic.LoadInt32(&got) == 0 {
		t.Fatal("取消之前应当已经消费到一些值")
	}
}

// 广播：两个下游各自拿到**完整、同序**的数据（不是瓜分）。
func TestTeeBroadcastsEveryValue(t *testing.T) {
	in := NewKey[<-chan int]("s.bcin")
	s1 := NewKey[<-chan int]("s.bc1")
	s2 := NewKey[<-chan int]("s.bc2")
	g := mustNew(t, context.Background(), "broadcast")

	if err := Produce(g, "src", in, func(rc *RunCtx, send func(int) error) error {
		for i := 1; i <= 4; i++ {
			if err := send(i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := Tee(g, "fan", in, Keys(s1, s2)); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	got := map[string][]int{}
	for _, c := range []struct {
		id string
		k  Key[<-chan int]
	}{{"a", s1}, {"b", s2}} {
		id, k := c.id, c.k
		if err := Consume(g, id, k, func(rc *RunCtx, v int) error {
			mu.Lock()
			got[id] = append(got[id], v)
			mu.Unlock()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	want := "[1 2 3 4]"
	for _, id := range []string{"a", "b"} {
		if fmt.Sprint(got[id]) != want {
			t.Fatalf("下游 %s 拿到 %v，want %s（广播：每个下游都要拿到全部）", id, got[id], want)
		}
	}
}

// 上游把 channel 那条槽标成跳过 → 消费端随之跳过，fn 不执行、不报错。
func TestConsumeSkipsWhenUpstreamSkips(t *testing.T) {
	in := NewKey[<-chan int]("s.up")
	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "upskip", WithObserver(obs))

	if err := g.Add(NewNode("maybe", nil, Provides(in), func(rc *RunCtx) error {
		return Skip(rc, in) // 这一次没有值
	})); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := Consume(g, "sink", in, func(rc *RunCtx, v int) error {
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatalf("跳过是到达、不是失败：%v", err)
	}
	if called {
		t.Fatal("上游跳过时 fn 不该执行")
	}
	log := obs.snapshot()
	if countPref(log, "R:sink") != 0 {
		t.Fatalf("上游跳过时消费端不该进入 Run：%v", log)
	}
	if countPref(log, "F:sink:skipped") != 1 {
		t.Fatalf("消费端应以 skipped 收尾：%v", log)
	}
}

// Only：写选中的那条，其余出口自动作废。
func TestOnlyWritesChosenAndSkipsRest(t *testing.T) {
	a := NewKey[string]("s.onlya")
	b := NewKey[string]("s.onlyb")
	obs := &recordingObserver{}
	g := mustNew(t, context.Background(), "only", WithObserver(obs))

	if err := g.Add(NewNode("route", nil, Provides(a, b), func(rc *RunCtx) error {
		return Only(rc, a, "走A")
	})); err != nil {
		t.Fatal(err)
	}
	useA, useB := false, false
	if err := g.Add(NewNode("useA", Requires(a), nil, func(rc *RunCtx) error {
		useA = true
		v, err := Get(rc, a)
		if err != nil {
			return err
		}
		if v != "走A" {
			return fmt.Errorf("useA got %q", v)
		}
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Add(NewNode("useB", Requires(b), nil, func(rc *RunCtx) error {
		useB = true
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatal(err)
	}
	if !useA {
		t.Fatal("选中的那条下游应当执行")
	}
	if useB {
		t.Fatal("未选中的那条下游不该执行")
	}
	log := obs.snapshot()
	if countPref(log, "F:useB:skipped") != 1 {
		t.Fatalf("未选中的下游应以 skipped 收尾：%v", log)
	}
}

// Only 与手写 Set 并用 → 重复表态**变吵**（手写 Set 两次是静默忽略的）。
func TestOnlyConflictsWithEarlierSet(t *testing.T) {
	a := NewKey[string]("s.dup_a")
	b := NewKey[string]("s.dup_b")
	g := mustNew(t, context.Background(), "onlydup")

	if err := g.Add(NewNode("route", nil, Provides(a, b), func(rc *RunCtx) error {
		if err := Set(rc, b, "先写了B"); err != nil {
			return err
		}
		return Only(rc, a, "又想走A") // 会给已就绪的 b 补一次 Skip → ErrConflict
	})); err != nil {
		t.Fatal(err)
	}
	err := g.Run()
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Run = %v, want ErrConflict", err)
	}
	// 冲突是在**发布之前**预检出来的：不能为了报这个错先把选中的出口发出去
	// （下游可能已被唤醒，而引擎不回滚已发布的值）。
	if st := g.slotOf(a.asRef()).state; st == slotReady {
		t.Fatal("Only 在已知冲突时仍然发布了选中的出口")
	}
}

// 名额是「同时活着」的约束，而糖的局部检查看不全组合图。
//
// 实测（修之前）：`WithMaxRunning(2)` 下 `Produce → Tee([a]) → Consume(a)`
// ——三个流节点——装配全过，运行期直接死锁，只能等外部超时。所以校验挪到
// `Start`：名额容不下全部流节点就拒（与另两条静态校验同一口径）。
func TestStreamSlotsCheckedAtStart(t *testing.T) {
	src := NewKey[<-chan int]("s.compose.src")
	a := NewKey[<-chan int]("s.compose.a")

	build := func(g *Graph) {
		t.Helper()
		if err := Produce(g, "src", src, func(rc *RunCtx, send func(int) error) error {
			return nil // 空流：只为验证装配，不真的发
		}); err != nil {
			t.Fatal(err)
		}
		if err := Tee(g, "tee", src, Keys(a)); err != nil {
			t.Fatal(err)
		}
		if err := Consume(g, "sink", a, func(rc *RunCtx, v int) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}

	g := mustNew(t, context.Background(), "compose", WithMaxRunning(2))
	build(g)
	err := g.Start()
	if err == nil || !strings.Contains(err.Error(), "slots at the same time") {
		t.Fatalf("Start = %v, want 名额不足的装配错误", err)
	}

	// 同一张图（名额不设限）跑得通——错的只是名额，不是图本身。
	g2 := mustNew(t, context.Background(), "compose-ok")
	build(g2)
	if err := g2.Run(); err != nil {
		t.Fatalf("名额放开后应当跑得通：%v", err)
	}
}

// 流节点的每条出口都得有人要：漏挂一个消费者 = 发送端永久堵在那条出口上
// （`checkSourcesLocked` 只判「Requires 有来源」，判不了「Provides 有消费者」）。
func TestStreamOutputNeedsConsumer(t *testing.T) {
	src := NewKey[<-chan int]("s.nc.src")
	a := NewKey[<-chan int]("s.nc.a")
	b := NewKey[<-chan int]("s.nc.b")

	// 1) Tee 的一条出口没人接
	g := mustNew(t, context.Background(), "nc-tee")
	if err := Produce(g, "src", src, func(rc *RunCtx, send func(int) error) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := Tee(g, "tee", src, Keys(a, b)); err != nil {
		t.Fatal(err)
	}
	if err := Consume(g, "sink", a, func(rc *RunCtx, v int) error { return nil }); err != nil {
		t.Fatal(err)
	}
	err := g.Start()
	if err == nil || !strings.Contains(err.Error(), "nothing consumes it") {
		t.Fatalf("Start = %v, want 「出口没人消费」的装配错误", err)
	}

	// 2) Produce 的出口没人接
	g2 := mustNew(t, context.Background(), "nc-produce")
	if err := Produce(g2, "src", src, func(rc *RunCtx, send func(int) error) error { return nil }); err != nil {
		t.Fatal(err)
	}
	err = g2.Start()
	if err == nil || !strings.Contains(err.Error(), "nothing consumes it") {
		t.Fatalf("Start = %v, want 「出口没人消费」的装配错误", err)
	}
}

// 手写消费者同样要占一个名额：`Start` 要的「整张流图」= 流节点 + 流出口的读取者。
//
// 复现（修之前）：`WithMaxRunning(2)` 下 `Produce(src) → Tee([a]) → 手写 sink(a)`
// 装配全过——局部检查各看各的（`Produce` 要 2、`Tee` 要 `len(outs)+1 == 2`），
// 手写消费者不在任何一处统计里；运行时 `Produce` 与 `Tee` 占满两个名额，`Tee`
// 堵在向 `a` 发送、手写 sink 排队等名额 → 挂死到外部超时（实测 400ms 超时，
// 一个值都没过）。
//
// 修之后：三个节点一起要名额 → `maxRun=2` 在 `Start` 直接拒；给够 3 个就跑得通。
func TestStreamSlotsCountHandWrittenReader(t *testing.T) {
	src := NewKey[<-chan int]("s.hw.src")
	a := NewKey[<-chan int]("s.hw.a")
	var got atomic.Int32

	// 糖之外的那条路：手写节点自己 Get 出 channel、自己收。
	sink := func(rc *RunCtx) error {
		ch, err := Get(rc, a)
		if err != nil {
			return err
		}
		for range ch {
			got.Add(1)
		}
		return nil
	}
	build := func(g *Graph) {
		t.Helper()
		if err := Produce(g, "src", src, func(rc *RunCtx, send func(int) error) error {
			for i := 0; i < 5; i++ {
				if err := send(i); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := Tee(g, "tee", src, Keys(a)); err != nil {
			t.Fatal(err)
		}
		if err := g.Add(NewNode("sink", Requires(a), nil, sink)); err != nil {
			t.Fatal(err)
		}
	}

	g := mustNew(t, context.Background(), "hw-2", WithMaxRunning(2))
	build(g)
	err := g.Start()
	if err == nil || !strings.Contains(err.Error(), "slots at the same time") {
		t.Fatalf("手写消费者没算进名额：Start = %v, want 名额不足的装配错误", err)
	}
	if !strings.Contains(err.Error(), "sink") {
		t.Fatalf("报错要点名那个读取者：%v", err)
	}

	// 同一张图给够名额（三个节点同时活着）就跑得通——错的只是名额，不是图本身。
	g2 := mustNew(t, context.Background(), "hw-3", WithMaxRunning(3))
	build(g2)
	if err := g2.Run(); err != nil {
		t.Fatalf("名额给够（3）应当跑得通：%v", err)
	}
	if n := got.Load(); n != 5 {
		t.Fatalf("手写消费者拿到 %d 个值，want 5", n)
	}
}

// 一条流出口只喂一个下游：两个各写各的读取者会在**静默**里瓜分值，
// 而 Tee 的承诺是「每个下游拿到完整、同序的数据」。
//
// 实测（修之前）：`Tee([a])` 加两个 `Consume(a)` 装配全过，10 个值被 8/2 瓜分、
// 整轮 err=nil。`Produce` 的出口同理（两个手写读取者 10/0）——两边都跑、都只
// 拿到一部分，谁也不会报错。
//
// 「抢」是 `FanOut` 的用法（N 个 worker 共读一条流），所以**同组不算多消费者**，
// 见 TestFanOutCompetesOnStreamOutput。
func TestStreamOutputRejectsSplitReaders(t *testing.T) {
	ctx := context.Background()

	// 1) Tee 的一条出口挂两个 Consume
	{
		src := NewKey[<-chan int]("s.split1.src")
		a := NewKey[<-chan int]("s.split1.a")
		g := mustNew(t, ctx, "split-tee")
		if err := Produce(g, "src", src, func(rc *RunCtx, send func(int) error) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if err := Tee(g, "tee", src, Keys(a)); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"sink-1", "sink-2"} {
			if err := Consume(g, id, a, func(rc *RunCtx, v int) error { return nil }); err != nil {
				t.Fatal(err)
			}
		}
		err := g.Start()
		if err == nil || !strings.Contains(err.Error(), "split between them") {
			t.Fatalf("Tee 出口两个读取者：Start = %v, want 瓜分错误", err)
		}
		if !strings.Contains(err.Error(), "sink-1") || !strings.Contains(err.Error(), "sink-2") {
			t.Fatalf("报错要列出两个读取者：%v", err)
		}
	}

	// 2) Produce 的出口挂两个手写读取者
	{
		src := NewKey[<-chan int]("s.split2.src")
		g := mustNew(t, ctx, "split-produce")
		if err := Produce(g, "src", src, func(rc *RunCtx, send func(int) error) error { return nil }); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"r1", "r2"} {
			if err := g.Add(NewNode(id, Requires(src), nil, func(rc *RunCtx) error {
				ch, err := Get(rc, src)
				if err != nil {
					return err
				}
				for range ch {
				}
				return nil
			})); err != nil {
				t.Fatal(err)
			}
		}
		err := g.Start()
		if err == nil || !strings.Contains(err.Error(), "split between them") {
			t.Fatalf("Produce 出口两个读取者：Start = %v, want 瓜分错误", err)
		}
	}

	// 3) 两次独立的 FanOut 各自读同一条出口：各是一组，仍算多消费者
	{
		src := NewKey[<-chan int]("s.split3.src")
		o1 := NewKey[int]("s.split3.o1")
		o2 := NewKey[int]("s.split3.o2")
		o3 := NewKey[int]("s.split3.o3")
		o4 := NewKey[int]("s.split3.o4")
		g := mustNew(t, ctx, "split-two-fanout")
		if err := Produce(g, "src", src, func(rc *RunCtx, send func(int) error) error { return nil }); err != nil {
			t.Fatal(err)
		}
		drain := func(rc *RunCtx, shard int, in <-chan int) (int, error) {
			for range in {
			}
			return shard, nil
		}
		if err := FanOut(g, "w", src, Keys(o1, o2), drain); err != nil {
			t.Fatal(err)
		}
		if err := FanOut(g, "v", src, Keys(o3, o4), drain); err != nil {
			t.Fatal(err)
		}
		err := g.Start()
		if err == nil || !strings.Contains(err.Error(), "split between them") {
			t.Fatalf("两组 FanOut 读同一条出口：Start = %v, want 瓜分错误", err)
		}
	}
}

// FanOut 挂在一根流上正是「抢」——同组 worker 共读一条出口是**显式**声明，
// 不是静默瓜分，`Start` 必须放过（文档点名推荐的玩法）。
//
// 这条钉的是**契约**（修之前也通过）：P2 的拦截不能把 FanOut 一起拦掉。
func TestFanOutCompetesOnStreamOutput(t *testing.T) {
	src := NewKey[<-chan int]("s.fanout.src")
	o1 := NewKey[int]("s.fanout.o1")
	o2 := NewKey[int]("s.fanout.o2")
	g := mustNew(t, context.Background(), "fanout-stream")

	if err := Produce(g, "src", src, func(rc *RunCtx, send func(int) error) error {
		for i := 0; i < 10; i++ {
			if err := send(i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var counts [2]atomic.Int32
	if err := FanOut(g, "w", src, Keys(o1, o2),
		func(rc *RunCtx, shard int, in <-chan int) (int, error) {
			for range in {
				counts[shard-1].Add(1)
			}
			return shard, nil
		}); err != nil {
		t.Fatal(err)
	}
	if err := g.Run(); err != nil {
		t.Fatalf("FanOut 抢一根流应当跑得通：%v", err)
	}
	if total := counts[0].Load() + counts[1].Load(); total != 10 {
		t.Fatalf("两个 worker 合计拿到 %d 个值，want 10（抢：每个值只做一次）", total)
	}
}

// 隔离消费端：取消与「有值可读 / channel 已关闭」同时就绪时，取消必须赢。
// 这条钉的是「进分支后复查 ctx」那一行——select 在两个分支都就绪时随机挑
// （与 `acquire` 同一条纪律），少了复查就会在取消之后继续调 fn，甚至把
// 「channel 已关闭」走成 `return nil`、把取消报成成功。
//
// 与 `TestAcquireReturnsSlotWhenCanceled` 同理：漏了复查约有**一半**概率失败，
// 所以这条要用 `-count` 跑才说明问题（`go test -run TestConsumeStopsCallingAfterCancel
// -count=20`）。
func TestConsumeStopsCallingAfterCancel(t *testing.T) {
	in := NewKey[<-chan int]("s.stop")
	ch := make(chan int, 64)
	for i := 0; i < 64; i++ {
		ch <- i
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := mustNew(t, ctx, "stop")
	if err := Seed(g, in, (<-chan int)(ch)); err != nil {
		t.Fatal(err)
	}

	var cancelled atomic.Bool
	var afterCancel atomic.Int32
	calls := 0
	if err := Consume(g, "sink", in, func(rc *RunCtx, v int) error {
		calls++
		if cancelled.Load() {
			afterCancel.Add(1)
		}
		if calls == 1 {
			cancel() // 第一轮回调里取消：此后两个分支同时就绪
			cancelled.Store(true)
		}
		time.Sleep(time.Millisecond)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	err := g.Run()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if n := afterCancel.Load(); n > 0 {
		t.Fatalf("取消之后还在调 fn：%d 次（进分支后没复查 ctx）", n)
	}
}

// 装配期参数校验（与 Join / FanOut 同形）。
func TestStreamAssemblyErrors(t *testing.T) {
	in := NewKey[<-chan int]("s.args")
	g := mustNew(t, context.Background(), "args")

	if err := Produce(nil, "src", in, func(rc *RunCtx, send func(int) error) error { return nil }); err == nil {
		t.Fatal("nil graph 应当报错")
	}
	if err := Produce(g, "", in, func(rc *RunCtx, send func(int) error) error { return nil }); err == nil {
		t.Fatal("空 id 应当报错")
	}
	if err := Produce(g, "src", in, nil); err == nil {
		t.Fatal("nil fn 应当报错")
	}
	if err := Consume(g, "sink", in, nil); err == nil {
		t.Fatal("nil fn 应当报错")
	}
	if err := Tee(g, "fan", in, nil); err == nil {
		t.Fatal("没有出口应当报错")
	}
	if n := len(g.nodes); n != 0 {
		t.Fatalf("装配失败不该往图上留节点，got %d", n)
	}
}
