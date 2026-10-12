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
