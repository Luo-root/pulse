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

// 本文件是**阶段 3（事务性 `Retry` / attempt 写集）**的验收用例：把「失败的尝试写出去的
// 东西，下游从来没有见过」这条硬保证钉在各个角上，并覆盖它与 ctx、切面顺序、收尾、多 Key
// 的组合语义。基础重试行为（次数、延迟、跳过不重试、子图步骤重试）在 graph_test.go 与
// sub_static_test.go 里，无边界重入在 graph_test.go 的推进闸用例里。
//
// 三条**没有**在这里新写的，因为既有用例就是它们的证据：
//
//   - 「不装 `Retry` 的节点行为逐条不变」——写即发布、收尾跳过未写的、跳过不重试：
//     整套既有用例全绿即是这条的证明，新写一份只会自说自话；
//   - 「用户自己写的重试切面仍被拒」——`TestSubReentryGuarded` 与
//     `TestAspectSequentialNextWithoutBoundaryRejected`；
//   - 「`Retry` 装在子图步骤上」——`TestRetryOnSubgraphStepRunsEveryAttemptOnAFreshChildExecution`。

// TestRetryFailedAttemptKeyEndsSkipped 多 Key 的可见性：一次尝试只写了 K1 之后失败、下一次
// 只写了 K2 就成功——**K1 不能留着那条作废的值**，它该以跳过收尾（没有提交过的东西不该有个
// 值），K2 才是就绪。
//
// 这条钉的是**收尾判据**：判「这个节点写过谁」会漏掉「写过、但那次作废了」，于是 K1 永远停在
// `pending`，等它的下游一直等到取消（一句看不出病因的超时）。判「落没落地」才对。
func TestRetryFailedAttemptKeyEndsSkipped(t *testing.T) {
	k1 := NewKey[string]("rtx.k1")
	k2 := NewKey[string]("rtx.k2")
	var attempts atomic.Int32

	g := mustNew(t, context.Background(), "rtx-multi")
	mustAdd(t, g, NewNode("up", "", nil, Provides(k1, k2), func(rc *RunCtx) error {
		if attempts.Add(1) == 1 {
			if err := Set(rc, k1, "a1"); err != nil {
				return err
			}
			return errors.New("第一次注定失败")
		}
		return Set(rc, k2, "a2")
	}, Retry(2, 0)))

	if err := runOn(g); err != nil {
		t.Fatalf("第二次尝试成功，整轮该成功，got %v", err)
	}
	if st, v := slotOn(g, k1.asRef()); st != slotSkipped || v != nil {
		t.Fatalf("k1 槽位 = (%v, %v)，want (slotSkipped, nil)——失败那一次写过的键该以跳过收尾", st, v)
	}
	if st, v := slotOn(g, k2.asRef()); st != slotReady || v != "a2" {
		t.Fatalf("k2 槽位 = (%v, %v)，want (slotReady, a2)", st, v)
	}
}

// TestRetryDownstreamSeesSkippedNotStaleValue 同一条边界的**下游视角**：等这两条键的 fan-in
// 节点该拿到「K1 跳过 + K2 = a2」，而不是「K1 = a1」。
func TestRetryDownstreamSeesSkippedNotStaleValue(t *testing.T) {
	k1 := NewKey[string]("rtxd.k1")
	k2 := NewKey[string]("rtxd.k2")
	var attempts atomic.Int32
	var k1Err, k2Val atomic.Value

	g := mustNew(t, context.Background(), "rtxd")
	mustAdd(t, g, NewNode("up", "", nil, Provides(k1, k2), func(rc *RunCtx) error {
		if attempts.Add(1) == 1 {
			if err := Set(rc, k1, "a1"); err != nil {
				return err
			}
			return errors.New("第一次注定失败")
		}
		return Set(rc, k2, "a2")
	}, Retry(2, 0)))
	mustAdd(t, g, NewNode("sink", "", Requires(k1, k2), nil, func(rc *RunCtx) error {
		if v, err := Get(rc, k1); err != nil {
			if !errors.Is(err, ErrSkipped) {
				return err
			}
			k1Err.Store(err.Error())
		} else {
			k1Err.Store("读到值 " + v)
		}
		v, err := Get(rc, k2)
		if err != nil {
			return err
		}
		k2Val.Store(v)
		return nil
	}))

	if err := runOn(g); err != nil {
		t.Fatalf("第二次尝试成功，整轮该成功，got %v", err)
	}
	got, _ := k1Err.Load().(string)
	if got == "" || !strings.Contains(got, "skipped") {
		t.Fatalf("下游读 k1 = %q，want 一条 *SkipError（失败那一次的值不该露面）", got)
	}
	if v := k2Val.Load(); v != "a2" {
		t.Fatalf("下游读 k2 = %v，want a2", v)
	}
}

// TestRetryAllAttemptsFailedLeavesOutputsSkipped 全部尝试都失败时：**失败那一次写出去的值
// 不作数**——输出以跳过收尾，图报的是**最后一次**尝试的错误，下游没被那条写唤醒过。
func TestRetryAllAttemptsFailedLeavesOutputsSkipped(t *testing.T) {
	out := NewKey[string]("rtxa.out")
	var attempts, downRuns atomic.Int32

	g := mustNew(t, context.Background(), "rtxa")
	mustAdd(t, g, NewNode("up", "", nil, Provides(out), func(rc *RunCtx) error {
		n := attempts.Add(1)
		if err := Set(rc, out, fmt.Sprintf("a%d", n)); err != nil {
			return err
		}
		return fmt.Errorf("boom-%d", n)
	}, Retry(2, 0)))
	mustAdd(t, g, NewNode("down", "", Requires(out), nil, func(rc *RunCtx) error {
		downRuns.Add(1)
		return nil
	}))

	err := runOn(g)
	if err == nil || !strings.Contains(err.Error(), "boom-2") {
		t.Fatalf("Run = %v，want 最后一次尝试的错误（Retry 返回的就是它）", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if got := downRuns.Load(); got != 0 {
		t.Fatalf("下游进入 Run %d 次，want 0——它不该被「注定失败的那一次」的写唤醒", got)
	}
	if st, v := slotOn(g, out.asRef()); st != slotSkipped || v != nil {
		t.Fatalf("输出槽 = (%v, %v)，want (slotSkipped, nil)——没有提交过的东西不该有个值", st, v)
	}
}

// TestRetryAttemptReadsItsOwnWrite attempt 内读回自己刚写的值是**自洽**的：读到的是这一
// 次尝试的值，而不是上一条尝试留下的（那条已经作废）。
func TestRetryAttemptReadsItsOwnWrite(t *testing.T) {
	out := NewKey[string]("rtxs.out")
	var attempts atomic.Int32
	var mu sync.Mutex
	var readBack []string

	g := mustNew(t, context.Background(), "rtxs")
	mustAdd(t, g, NewNode("up", "", nil, Provides(out), func(rc *RunCtx) error {
		n := attempts.Add(1)
		if err := Set(rc, out, fmt.Sprintf("a%d", n)); err != nil {
			return err
		}
		v, err := Get(rc, out)
		if err != nil {
			return err
		}
		mu.Lock()
		readBack = append(readBack, v)
		mu.Unlock()
		if n == 1 {
			return errors.New("第一次注定失败")
		}
		return nil
	}, Retry(2, 0)))

	if err := runOn(g); err != nil {
		t.Fatalf("第二次尝试成功，整轮该成功，got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(readBack) != 2 || readBack[0] != "a1" || readBack[1] != "a2" {
		t.Fatalf("两次尝试读回自己写的值 = %v，want [a1 a2]——第二次不该读到作废的 a1", readBack)
	}
}

// TestRetryCtxJudgement 阶段零定案的那条判据：**进 `Retry` 那一层拿到的 ctx 已经取消 / 到期
// 就不重试**；`Retry` **内层**自己包的超时只是这一次尝试的失败，照常重试。四条一起钉住，
// 免得后面改实现时走样（阶段零写明：这几条要先有用例才允许改实现）。
func TestRetryCtxJudgement(t *testing.T) {
	t.Run("内层 Timeout 只算这一次尝试失败，照常重试", func(t *testing.T) {
		out := NewKey[string]("rtxc.inner.out")
		var attempts atomic.Int32
		g := mustNew(t, context.Background(), "rtxc-inner")
		// aspects 先列的更靠外 → Retry 在外、Timeout 在内。
		mustAdd(t, g, NewNode("flaky", "", nil, Provides(out), func(rc *RunCtx) error {
			if attempts.Add(1) == 1 {
				<-rc.Context().Done() // 等内层超时把它取消
				return rc.Context().Err()
			}
			return Set(rc, out, "ok")
		}, Retry(2, 0), Timeout(30*time.Millisecond)))

		if err := runOn(g); err != nil {
			t.Fatalf("内层超时该被重试掉，Run = %v", err)
		}
		if got := attempts.Load(); got != 2 {
			t.Fatalf("attempts = %d, want 2（Retry 自己那一层的 ctx 并没有到期）", got)
		}
	})

	t.Run("外层 Timeout 到期之后不重试", func(t *testing.T) {
		out := NewKey[string]("rtxc.outer.out")
		var attempts atomic.Int32
		g := mustNew(t, context.Background(), "rtxc-outer")
		mustAdd(t, g, NewNode("slow", "", nil, Provides(out), func(rc *RunCtx) error {
			attempts.Add(1)
			<-rc.Context().Done() // 外层超时把它取消
			return rc.Context().Err()
		}, Timeout(30*time.Millisecond), Retry(2, 0)))

		err := runOn(g)
		if err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("Run = %v，want 外层超时", err)
		}
		if got := attempts.Load(); got != 1 {
			t.Fatalf("attempts = %d，want 1（进 Retry 那层的 ctx 已经到期，再试也不会变好）", got)
		}
	})

	t.Run("父取消之后不重试", func(t *testing.T) {
		out := NewKey[string]("rtxc.cancel.out")
		var attempts atomic.Int32
		ctx, cancel := context.WithCancel(context.Background())
		g := mustNew(t, context.Background(), "rtxc-cancel")
		mustAdd(t, g, NewNode("flaky", "", nil, Provides(out), func(rc *RunCtx) error {
			if attempts.Add(1) == 1 {
				cancel() // 宿主把这一次运行拆了
				return errors.New("第一次失败")
			}
			return Set(rc, out, "ok")
		}, Retry(2, 0)))

		if err := g.Run(ctx, Inputs{}); err == nil {
			t.Fatal("父取消之后整轮该报错")
		}
		if got := attempts.Load(); got != 1 {
			t.Fatalf("attempts = %d，want 1（外层已经不再等它了）", got)
		}
	})

	t.Run("尝试自己返回 ctx 错误：那只是错误，ctx 没到期就照试", func(t *testing.T) {
		out := NewKey[string]("rtxc.err.out")
		var attempts atomic.Int32
		g := mustNew(t, context.Background(), "rtxc-err")
		mustAdd(t, g, NewNode("flaky", "", nil, Provides(out), func(rc *RunCtx) error {
			if attempts.Add(1) == 1 {
				return context.DeadlineExceeded // 自己造的 ctx 错误，本层 ctx 其实没到期
			}
			return Set(rc, out, "ok")
		}, Retry(2, 0)))

		if err := runOn(g); err != nil {
			t.Fatalf("Run = %v，want 成功（判据是 ctx 到期没有，不是错误长什么样）", err)
		}
		if got := attempts.Load(); got != 2 {
			t.Fatalf("attempts = %d, want 2", got)
		}
	})
}

// TestRetryOuterAspectWritesDoNotRollBack 事务边界就是 `Retry` 套的那一层：**它外面**（更外层
// 切面）的写不属于任何 attempt，也不参与回滚。文档点名了这条边界，这里钉住它——不能把外层的写
// 默默说成事务内的。
func TestRetryOuterAspectWritesDoNotRollBack(t *testing.T) {
	side := NewKey[string]("rtxo.side")
	out := NewKey[string]("rtxo.out")
	var attempts atomic.Int32

	// 更靠外的切面：在 `next` 之前往**自己的** Provide 里写一笔。
	outer := func(rc *RunCtx, next func(*RunCtx) error) error {
		if err := Set(rc, side, "from-outer"); err != nil {
			return err
		}
		return next(rc)
	}

	g := mustNew(t, context.Background(), "rtxo")
	mustAdd(t, g, NewNode("flaky", "", nil, Provides(side, out), func(rc *RunCtx) error {
		if attempts.Add(1) == 1 {
			return errors.New("第一次注定失败")
		}
		return Set(rc, out, "ok")
	}, outer, Retry(2, 0)))

	if err := runOn(g); err != nil {
		t.Fatalf("第二次尝试成功，整轮该成功，got %v", err)
	}
	if st, v := slotOn(g, side.asRef()); st != slotReady || v != "from-outer" {
		t.Fatalf("外层切面写的槽 = (%v, %v)，want (slotReady, from-outer)——它不属于任何 attempt",
			st, v)
	}
	if st, v := slotOn(g, out.asRef()); st != slotReady || v != "ok" {
		t.Fatalf("节点自己的输出 = (%v, %v)，want (slotReady, ok)", st, v)
	}
}

// TestAdvanceLedgerIsPerExecution 推进闸的账记在**这一次执行**上：同一张图跑第二遍是全新的
// 执行上下文，照常跑（`#311` 定案 (f)：「不是按 NodeID 全局禁止再次执行」）。
func TestAdvanceLedgerIsPerExecution(t *testing.T) {
	out := NewKey[string]("adv.per.out")
	var runs atomic.Int32
	g := mustNew(t, context.Background(), "adv-per")
	mustAdd(t, g, NewNode("n", "", nil, Provides(out), func(rc *RunCtx) error {
		runs.Add(1)
		return Set(rc, out, "v")
	}))
	for i := 1; i <= 2; i++ {
		if err := runOn(g); err != nil {
			t.Fatalf("第 %d 次 Run = %v", i, err)
		}
	}
	if got := runs.Load(); got != 2 {
		t.Fatalf("节点跑了 %d 次，want 2", got)
	}
}

// TestRetryKeepsRetryingPastSeveralFailures `Retry` 的常规形状：**连续失败多次**、最后成功。
// 每一次作废的暂存各丢各的——第三次的值既不是第一次的、也不是第二次的。
//
// （旧的隐藏实现只在「失败一次就成功」上过过眼；这条把「多次失败」也钉住，同时确认
// 判定「要不要再试」的那条路线在前一次作废之后照常工作。）
func TestRetryKeepsRetryingPastSeveralFailures(t *testing.T) {
	out := NewKey[string]("rtx.many.out")
	var attempts, downstreamRuns atomic.Int32
	var seen atomic.Value

	g := mustNew(t, context.Background(), "rtx-many")
	mustAdd(t, g, NewNode("flaky", "", nil, Provides(out), func(rc *RunCtx) error {
		n := attempts.Add(1)
		if err := Set(rc, out, fmt.Sprintf("v-%d", n)); err != nil {
			return err
		}
		if n < 3 {
			return fmt.Errorf("第 %d 次注定失败", n)
		}
		return nil
	}, Retry(3, 0)))
	mustAdd(t, g, NewNode("downstream", "", Requires(out), nil, func(rc *RunCtx) error {
		v, err := Get(rc, out)
		if err != nil {
			return err
		}
		seen.Store(v)
		downstreamRuns.Add(1)
		return nil
	}))

	if err := runOn(g); err != nil {
		t.Fatalf("第三次尝试成功，整轮该成功，got %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if got := downstreamRuns.Load(); got != 1 {
		t.Fatalf("下游进入 Run %d 次，want 1（前两次失败都不该唤醒它）", got)
	}
	if v := seen.Load(); v != "v-3" {
		t.Fatalf("下游读到 %v，want v-3", v)
	}
	if st, v := slotOn(g, out.asRef()); st != slotReady || v != "v-3" {
		t.Fatalf("槽位终态 = (%v, %v)，want (slotReady, v-3)", st, v)
	}
}

// TestRetryCommitConflictPublishesNothing 提交**先整批判过，再逐条写**：一条落不下去就整批
// 不发布。
//
// 为什么需要这条：「逐条写、写到一半才发现冲突」会留下一个没有可解释语义的中间态——按 key 名
// 排序时**排在前面的那几条已经落地**（下游可能已经被唤醒、拿它开始干活），而这个节点整轮是失败的。
// 那不是「多 Key 提交期间不承诺原子可见」那条窗口（那条讲的是提交期间读者能看到一部分），而是
// 「提交失败留下半批」，所以判据是整批先判。
//
// 可达形状：**外科层切面先 `Skip` 一条键、被重试的尝试又 `Set` 它**——同一个节点对同一条键
// 写两次、状态相反，提交时撞 `ErrConflict`。冲突那条故意起名排在后面（`k2`），好让「挡不住」
// 的实现先把 `k1` 发布出去。
func TestRetryCommitConflictPublishesNothing(t *testing.T) {
	k1 := NewKey[string]("rtx.cf.k1")
	k2 := NewKey[string]("rtx.cf.k2")

	// 外层切面不属于任何尝试：它写的是**执行槽位**（这里是把它标成跳过）。
	outer := func(rc *RunCtx, next func(*RunCtx) error) error {
		if err := Skip(rc, k2); err != nil {
			return err
		}
		return next(rc)
	}

	g := mustNew(t, context.Background(), "rtx-commit-conflict")
	mustAdd(t, g, NewNode("src", "", nil, Provides(k1, k2), func(rc *RunCtx) error {
		if err := Set(rc, k1, "a"); err != nil {
			return err
		}
		return Set(rc, k2, "b") // 提交时撞「k2 已经以跳过到达」
	}, outer, Retry(2, 0)))

	err := runOn(g)
	if err == nil || !errors.Is(err, ErrConflict) {
		t.Fatalf("Run = %v，want 提交冲突（ErrConflict）", err)
	}
	if !strings.Contains(err.Error(), "rtx.cf.k2") {
		t.Fatalf("报文要点名落不下去的那条键：%v", err)
	}

	// 硬判据：**一条都不该发布**。挡不住冲突的实现会把 k1 留在 ready("a") 上。
	if st, v := slotOn(g, k1.asRef()); st == slotReady {
		t.Fatalf("k1 = (%v, %v)：提交整批判不过时，一条都不该发布", st, v)
	}
	if st, v := slotOn(g, k2.asRef()); st != slotSkipped || v != nil {
		t.Fatalf("k2 = (%v, %v)，want (slotSkipped, nil)——外层那一笔不动", st, v)
	}
}

// TestNestedRetryInnerResultBelongsToOuterAttempt 嵌套 `Retry`：**内层的结果属于外层那一次
// 尝试**——内层提交之后外层又失败，那一批写必须随外层的暂存一起作废，不能直接落到执行槽位上。
//
// 这里用「中间那个切面第一次必失败」把这条路走出来：外层尝试 1 里内层成功、写进外层暂存，
// 随后中间那层报错 → 外层的整条暂存丢掉；外层尝试 2 里内层再写一次、这次整条交出去。
// 挡不住「内层直接落执行槽位」的实现会在尝试 1 就把 `v-1` 发出去，而 `v-2` 会被幂等首写
// 静默吞掉——终态停在 `v-1`。
func TestNestedRetryInnerResultBelongsToOuterAttempt(t *testing.T) {
	out := NewKey[string]("rtx.nest.out")
	var rounds, outerCalls atomic.Int32
	var seen atomic.Value

	// 中间的切面：`next` 成功后，第一次必失败（外层因此需要重试）。
	tripwire := func(rc *RunCtx, next func(*RunCtx) error) error {
		if err := next(rc); err != nil {
			return err
		}
		if outerCalls.Add(1) == 1 {
			return errors.New("外层这一次注定失败")
		}
		return nil
	}

	g := mustNew(t, context.Background(), "rtx-nested")
	mustAdd(t, g, NewNode("src", "", nil, Provides(out), func(rc *RunCtx) error {
		n := rounds.Add(1)
		return Set(rc, out, fmt.Sprintf("v-%d", n))
	}, Retry(2, 0), tripwire, Retry(3, 0)))
	mustAdd(t, g, NewNode("downstream", "", Requires(out), nil, func(rc *RunCtx) error {
		v, err := Get(rc, out)
		if err != nil {
			return err
		}
		seen.Store(v)
		return nil
	}))

	if err := runOn(g); err != nil {
		t.Fatalf("外层第二次尝试成功，整轮该成功，got %v", err)
	}
	if got := rounds.Load(); got != 2 {
		t.Fatalf("节点体跑了 %d 次，want 2（每次外层尝试里的内层各成功一次）", got)
	}
	if got := outerCalls.Load(); got != 2 {
		t.Fatalf("外层尝试 %d 次，want 2", got)
	}
	if v := seen.Load(); v != "v-2" {
		t.Fatalf("下游读到 %v，want v-2——内层第一次的写属于外层那次失败的尝试", v)
	}
	if st, v := slotOn(g, out.asRef()); st != slotReady || v != "v-2" {
		t.Fatalf("槽位终态 = (%v, %v)，want (slotReady, v-2)", st, v)
	}
}

// TestRetryDownstreamReadFindsItsRealSource 有尝试边界时，**读路径不许在暂存里凭空建条目**：
// 读一条本次尝试根本没写过的键，必须找到它**真正的来源**（上游节点的槽位）。
//
// 建条目那条错写法（`slotOf` 而不是 `peek`）会留下一个永远 `pending` 的空位，正好把真正的来源
// 遮住——整张图从「跑得通」变成「挂到 ctx 到期」，而且看不出病因。这里在图级 `Retry` 下走一遍
// 上游 → 下游，读不到就挂到 3s 上限。
func TestRetryDownstreamReadFindsItsRealSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	mid := NewKey[string]("rtx.src.mid")
	out := NewKey[string]("rtx.src.out")
	var seen atomic.Value

	g := mustNew(t, ctx, "rtx-read-source", WithAspects(Retry(1, 0)))
	mustAdd(t, g, NewNode("up", "", nil, Provides(mid), func(rc *RunCtx) error {
		return Set(rc, mid, "upstream")
	}))
	mustAdd(t, g, NewNode("down", "", Requires(mid), Provides(out), func(rc *RunCtx) error {
		v, err := Get(rc, mid)
		if err != nil {
			return err
		}
		seen.Store(v)
		return Set(rc, out, v)
	}))

	if err := runOn(g); err != nil {
		t.Fatalf("图级 Retry 下读上游的键应当照常读得到，got %v", err)
	}
	if v := seen.Load(); v != "upstream" {
		t.Fatalf("下游读到 %v，want upstream", v)
	}
	if st, v := slotOn(g, out.asRef()); st != slotReady || v != "upstream" {
		t.Fatalf("下游输出 = (%v, %v)，want (slotReady, upstream)", st, v)
	}
}

// TestNodeWritesFinishBeforeTheChainReturns 「提交先整批判过、再逐条写」这条保证底下压着一个
// 前提：**一个节点对同一条输出的写入者是它自己那条顺序的尝试链**——体、切面、子图桥回都在同一个
// goroutine 上，所以「判过」与「写」之间插不进第三方。
//
// 这条用例把那个前提本身钉成可观察的：最外层切面拿到 `next` 的返回时，**这个节点已经没有在飞的
// 写**——① 成功那次的提交已经落地（槽位是就绪，不是「稍后才到」）；② 即使 `Timeout` 到期、内层体
// 还在睡（它不看 ctx），链也不会在体退出之前返回（提前返回会让收尾路径与还在跑的写并发碰同一批槽）。
//
// 引擎侧另外两半由既有用例守着，这里不重复：一条键只有一个来源（`Add` 第二次提供撞
// `ErrDuplicateSource`，见 `port_test.go` / `yaml`），并发进入同一个体的 `next` 被拒
// （`TestAspectConcurrentNextCalledOnce`）。谁把提交挪到后台 goroutine、或者让超时提前返回，
// 这条就红。
func TestNodeWritesFinishBeforeTheChainReturns(t *testing.T) {
	t.Run("提交在链返回前落地", func(t *testing.T) {
		out := NewKey[string]("seqw.ok.out")
		var attempts, stateSeen atomic.Int32

		// 最外层切面：`next` 返回之后立刻看槽位——提交必须已经做完（同一条栈上）。
		probe := func(rc *RunCtx, next func(*RunCtx) error) error {
			err := next(rc)
			st, _ := rc.exec.slotOf(out.asRef()).snapshot()
			stateSeen.Store(int32(st))
			return err
		}

		g := mustNew(t, context.Background(), "seqw-ok")
		mustAdd(t, g, NewNode("n", "", nil, Provides(out), func(rc *RunCtx) error {
			n := attempts.Add(1)
			if err := Set(rc, out, fmt.Sprintf("v-%d", n)); err != nil {
				return err
			}
			if n == 1 {
				return errors.New("第一次注定失败")
			}
			return nil
		}, probe, Retry(2, 0)))

		if err := runOn(g); err != nil {
			t.Fatalf("第二次尝试成功，整轮该成功，got %v", err)
		}
		if got := attempts.Load(); got != 2 {
			t.Fatalf("attempts = %d, want 2", got)
		}
		if got := slotState(stateSeen.Load()); got != slotReady {
			t.Fatalf("链返回时槽位 = %v，want slotReady——提交必须与链在同一条栈上做完", got)
		}
	})

	t.Run("超时也不在体退出前返回", func(t *testing.T) {
		out := NewKey[string]("seqw.slow.out")
		var bodyExited, exitedWhenChainReturned atomic.Bool

		probe := func(rc *RunCtx, next func(*RunCtx) error) error {
			err := next(rc)
			exitedWhenChainReturned.Store(bodyExited.Load())
			return err
		}

		g := mustNew(t, context.Background(), "seqw-slow")
		mustAdd(t, g, NewNode("n", "", nil, Provides(out), func(rc *RunCtx) error {
			defer bodyExited.Store(true) // 体不看 ctx：`Timeout` 只能等它自己结束
			time.Sleep(80 * time.Millisecond)
			return Set(rc, out, "late")
		}, probe, Timeout(20*time.Millisecond)))

		err := runOn(g)
		if err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("Run = %v，want 超时", err)
		}
		if !exitedWhenChainReturned.Load() {
			t.Fatal("链在节点体退出之前就返回了：收尾路径会与还在跑的写并发碰同一批槽")
		}
	})
}

// TestRetryCrossAttemptReadWaitsForItsOwnSource 跨尝试读**不回**上一次被丢弃的写。
//
// 这是**有意保留**的语义（不是缺陷）：那条键在这一次尝试里从来没有过值，读它就是等一个没人会写
// 的槽——与「一条键没人产出」同一个形状。它会一直等到本层 ctx 取消，所以很容易被误当成死锁；
// 文档点名了这条边界，用例在这里把它钉成两半：① 非阻塞地问（`TryGet`）看到的是 `pending`
// （既不是作废的值、也不是一个凭空造出来的空位）；② 真的去读（`Get`）就是等到 ctx 到期。
//
// 谁要是在将来为了「消除等待」把作废的写恢复成可读，① 会立刻变红。
func TestRetryCrossAttemptReadWaitsForItsOwnSource(t *testing.T) {
	t.Run("TryGet 看到 pending", func(t *testing.T) {
		a := NewKey[string]("rtx.cross.a")
		var attempts, okSeen, skippedSeen atomic.Int32
		var valueSeen atomic.Value

		g := mustNew(t, context.Background(), "rtx-cross-try")
		mustAdd(t, g, NewNode("src", "", nil, Provides(a), func(rc *RunCtx) error {
			if attempts.Add(1) == 1 {
				if err := Set(rc, a, "a1"); err != nil {
					return err
				}
				return errors.New("第一次注定失败")
			}
			// 第二次尝试：读第一次试过、但被丢弃的那条键。非阻塞地问，看到的是 pending。
			v, ok, skipped, err := TryGet(rc, a)
			if err != nil {
				return err
			}
			if ok {
				valueSeen.Store(v)
				okSeen.Add(1)
			}
			if skipped {
				skippedSeen.Add(1)
			}
			return nil
		}, Retry(2, 0)))

		if err := runOn(g); err != nil {
			t.Fatalf("第二次尝试正常返回，整轮该成功（输出按跳过收尾），got %v", err)
		}
		if got := attempts.Load(); got != 2 {
			t.Fatalf("attempts = %d, want 2", got)
		}
		if got := okSeen.Load(); got != 0 {
			t.Fatalf("第二次尝试读到了上一次被丢弃的值 %v——作废的写不该重新变得可读", valueSeen.Load())
		}
		if got := skippedSeen.Load(); got != 0 {
			t.Fatalf("读到的是「跳过」而不是「还没到」：被判成 %d 次", got)
		}
		if st, v := slotOn(g, a.asRef()); st != slotSkipped || v != nil {
			t.Fatalf("输出槽终态 = (%v, %v)，want (slotSkipped, nil)", st, v)
		}
	})

	t.Run("Get 等到 ctx 到期", func(t *testing.T) {
		a := NewKey[string]("rtx.cross.get")
		var attempts atomic.Int32

		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()

		g := mustNew(t, ctx, "rtx-cross-get")
		mustAdd(t, g, NewNode("src", "", nil, Provides(a), func(rc *RunCtx) error {
			if attempts.Add(1) == 1 {
				if err := Set(rc, a, "a1"); err != nil {
					return err
				}
				return errors.New("第一次注定失败")
			}
			if _, err := Get(rc, a); err != nil {
				return err // 没人会写这条键：等到本层 ctx 到期
			}
			return nil
		}, Retry(2, 0)))

		err := runOn(g)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run = %v，want context.DeadlineExceeded（等一个没人会写的槽，直到本层 ctx 取消）", err)
		}
		if got := attempts.Load(); got != 2 {
			t.Fatalf("attempts = %d, want 2（ctx 到期之后不该再试）", got)
		}
	})
}

// TestAspectSequentialNextWithoutBoundaryRejected 钉住**推进闸**：同一个节点执行上下文里，
// `next()` 只允许成功推进一次——切面在没有开新尝试边界的情况下再推一次，整轮报错。
//
// 阶段 3 之前这条契约是**反的**（当时那条用例叫 `TestAspectSequentialNextAllowed`，断言
// 「再推一次合法、节点跑两次、第二次的 Set 被幂等首写静默忽略」）。翻面它的是 #311 阶段 3
// 的判据：没有隔离的第二次推进会让两次执行的输出混装，而引擎当时是**安静地放过**。
//
// `Retry` 不受影响——它每次尝试都换一条新的写暂存，那是「有边界的重入」（见
// `TestRetryHidesFailedAttemptWrites` 与 Retry 自己的 godoc）；另一次 `Run` 是全新的执行
// 上下文，账从零开始。
func TestAspectSequentialNextWithoutBoundaryRejected(t *testing.T) {
	g := mustNew(t, context.Background(), "advance-gate")
	var runs atomic.Int32
	aspect := Aspect(func(rc *RunCtx, next func(*RunCtx) error) error {
		if err := next(rc); err != nil {
			return err
		}
		// 顺序再调一次：**没有开新的尝试边界**，该被推进闸拒。
		return next(rc)
	})
	mustAdd(t, g, NewNode("n", "", nil, Provides(kA), func(rc *RunCtx) error {
		runs.Add(1)
		return Set(rc, kA, "value")
	}, aspect))

	err := runOn(g)
	if err == nil || !errors.Is(err, ErrNextCalledTwice) {
		t.Fatalf("Run = %v，want ErrNextCalledTwice", err)
	}
	if !strings.Contains(err.Error(), "without a new attempt boundary") {
		t.Fatalf("报文要点明「没有新边界的重入」，got %v", err)
	}
	if !strings.Contains(err.Error(), "pulse.Retry") {
		t.Fatalf("报文要给出能照着改的写法，got %v", err)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("节点跑了 %d 次，want 1（第二次推进该被拒在体之前）", got)
	}
}

func TestAspectConcurrentNextCalledOnce(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	var runs atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	aspect := Aspect(func(rc *RunCtx, next func(*RunCtx) error) error {
		errCh := make(chan error, 1)
		go func() { errCh <- next(rc) }()
		<-started // 第一个已进入 core 并持有 depth
		err2 := next(rc)
		close(release)
		err1 := <-errCh
		if !errors.Is(err2, ErrNextCalledTwice) {
			return fmt.Errorf("overlapping next want ErrNextCalledTwice, got %v (first=%v)", err2, err1)
		}
		return ErrNextCalledTwice
	})
	mustAdd(t, g, NewNode("n", "", nil, Provides(kA), func(rc *RunCtx) error {
		started <- struct{}{}
		<-release
		runs.Add(1)
		return Set(rc, kA, "value")
	}, aspect))
	if err := runOn(g); !errors.Is(err, ErrNextCalledTwice) {
		t.Fatalf("want ErrNextCalledTwice, got %v", err)
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("node ran %d times, want 1", got)
	}
}

// 跳过不是失败：输入一条值都没到时 Retry 必须立即返回，不得重跑整段
// 「等输入 + 执行」——否则白等 (attempts-1) × delay，内层切面
// （观测/埋点/记账）也被重复执行。
func TestRetryDoesNotRerunSkippedNode(t *testing.T) {
	in := NewKey[string]("retry.skip.in")
	out := NewKey[string]("retry.skip.out")
	const delay = 300 * time.Millisecond

	g := mustNew(t, context.Background(), "test", WithAspects(Retry(3, delay)))
	declareIn(t, g, NewPort(in))
	if err := skipSeedOn(g, in); err != nil {
		t.Fatal(err)
	}

	var runs, inner atomic.Int32
	mustAdd(t, g, NewNode("down", "", Requires(in), Provides(out), func(rc *RunCtx) error {
		runs.Add(1) // 断言在主 goroutine 上做（节点跑在自己的 goroutine 里）
		return nil
	}, Aspect(func(rc *RunCtx, next func(*RunCtx) error) error {
		inner.Add(1)
		return next(rc)
	})))

	start := time.Now()
	if err := runOn(g); err != nil {
		t.Fatalf("a skip is not a failure: %v", err)
	}
	elapsed := time.Since(start)

	if got := inner.Load(); got != 1 {
		t.Fatalf("inner aspect ran %d times, want 1 (a skipped input must not be retried)", got)
	}
	if got := runs.Load(); got != 0 {
		t.Fatalf("node Run ran %d times, want 0 when its input is skipped", got)
	}
	if elapsed >= delay {
		t.Fatalf("skip path took %v, want no retry delay (Retry must not add attempts-1 waits)", elapsed)
	}
	rc := inspect(g)
	if _, ok, skipped, err := TryGet(rc, out); err != nil || ok || !skipped {
		t.Fatalf("out = ok=%v skipped=%v err=%v, want skipped", ok, skipped, err)
	}
}

func TestTimeoutInterruptsWait(t *testing.T) {
	g := mustNew(t, context.Background(), "test")
	// kA 必须有来源（Start 的来源校验），但它迟迟不写：slow 会真的**在等**，
	// 这条用例测的就是超时能不能打断等待段。
	mustAdd(t, g, NewNode("late", "", nil, Provides(kA), func(rc *RunCtx) error {
		<-rc.Context().Done()
		return rc.Context().Err()
	}))
	mustAdd(t, g, NewNode("slow", "", Requires(kA), Provides(kB), func(rc *RunCtx) error {
		_, err := Get(rc, kA)
		return err
	}, Timeout(30*time.Millisecond)))
	err := runOn(g)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Run err = %v, want timeout", err)
	}
}

// TestTimeoutWaitsForInnerRun 超时到期后必须等内层收干净再返回：
// 切面共享 `wrote`，提前返回会让收尾路径与还在跑的 Run 同时碰同一批槽
// （数据竞态），也会让 Run 的「阻塞到全部终止」失真。
func TestTimeoutWaitsForInnerRun(t *testing.T) {
	out := NewKey[string]("to.out")
	var bodyDone atomic.Bool

	g := mustNew(t, context.Background(), "test", WithAspects(Timeout(20*time.Millisecond)))
	mustAdd(t, g, NewNode("slow", "", nil, Provides(out), func(rc *RunCtx) error {
		time.Sleep(120 * time.Millisecond)
		bodyDone.Store(true)
		return Set(rc, out, "late")
	}))

	start := time.Now()
	err := runOn(g)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Run err = %v, want timeout", err)
	}
	if !bodyDone.Load() {
		t.Fatalf("Run 在 %v 就返回了，节点体还没退出（超时提前返回）", elapsed.Round(time.Millisecond))
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("Run 耗时 %v，短于节点体自身耗时——没等内层", elapsed.Round(time.Millisecond))
	}
}

// TestRetryHidesFailedAttemptWrites 钉住阶段 3 的硬保证：**失败的尝试写出去的东西，
// 下游从来没有见过**。
//
// 阶段 2 及以前这条契约是**反的**（当时那条用例叫 `TestRetryDoesNotRollbackPublishedSlots`）：
// 槽位「到达即发布、幂等首写」，第一次尝试的 Set 当场唤醒下游并留在槽里，后续尝试的写被
// 静默忽略——下游拿到的是「注定失败的那一次」的值，而整轮还可能报成功。
//
// 判据落在**下游读到的值**上，不落在「下游有没有被唤醒」上：后者在时间窗里等「没被唤醒」
// 只能靠超时兜底，而值本身是确定性的证据——只有成功的那一次被提交过，下游就不可能读到别的。
func TestRetryHidesFailedAttemptWrites(t *testing.T) {
	out := NewKey[string]("retry.hide.out")
	var attempts, downstreamRuns atomic.Int32
	var seen atomic.Value

	g := mustNew(t, context.Background(), "retry-hide")
	mustAdd(t, g, NewNode("flaky", "", nil, Provides(out), func(rc *RunCtx) error {
		n := attempts.Add(1)
		if err := Set(rc, out, fmt.Sprintf("attempt-%d", n)); err != nil {
			return err
		}
		if n == 1 {
			return errors.New("第一次注定失败")
		}
		return nil
	}, Retry(2, 0)))
	mustAdd(t, g, NewNode("downstream", "", Requires(out), nil, func(rc *RunCtx) error {
		v, err := Get(rc, out)
		if err != nil {
			return err
		}
		seen.Store(v)
		downstreamRuns.Add(1)
		return nil
	}))

	if err := runOn(g); err != nil {
		t.Fatalf("第二次尝试成功，整轮该成功，got %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if got := downstreamRuns.Load(); got != 1 {
		t.Fatalf("downstream 进入 Run %d 次，want 1", got)
	}
	if v := seen.Load(); v != "attempt-2" {
		t.Fatalf("下游读到 %v，want attempt-2——失败那一次的写不该被任何人看见", v)
	}
	if st, v := slotOn(g, out.asRef()); st != slotReady || v != "attempt-2" {
		t.Fatalf("槽位终态 = (%v, %v)，want (slotReady, attempt-2)", st, v)
	}
}
