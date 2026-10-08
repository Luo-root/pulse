package pulse

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// flipErrCtx 第一次 Err() 返回 nil、之后返回 context.Canceled。
//
// 专门用来复现 `slot.wait` 里「入口那次检查与快照之间被取消」那个窗口：
// 走调度的话概率约千分之一（实测 `-race -count=200` 的 50 轮用例里稳定能
// 抓到一两次，写不成可靠用例）；换成一个会「翻面」的 ctx 就能确定性地打到
// 那条路径——判据不靠时序，靠「第二次问它，它说取消了」。
type flipErrCtx struct {
	context.Context
	flipped atomic.Bool
}

func (c *flipErrCtx) Err() error {
	if c.flipped.CompareAndSwap(false, true) {
		return nil // 第一次：入口检查看到「还没取消」
	}
	return context.Canceled
}

// TestSlotWaitRecheckAfterSnapshot 钉住「取消优先」在**每个**报到达的出口都成立：
// 入口检查与快照之间有窗口，槽位在窗口里到达、ctx 在窗口里被取消，漏了复查就会
// 把「被取消」报成 skipped——下游终态随之在 skipped / canceled 之间抖。
func TestSlotWaitRecheckAfterSnapshot(t *testing.T) {
	t.Run("快照前没取消、快照后取消：跳过让位给取消", func(t *testing.T) {
		s := newSlot()
		if err := s.resolveSkip(); err != nil {
			t.Fatal(err)
		}
		if _, err := s.wait(&flipErrCtx{Context: context.Background()}); !errors.Is(err, context.Canceled) {
			t.Fatalf("wait = %v, want context.Canceled（快照后的复查漏了）", err)
		}
	})

	t.Run("值那条出口同口径：取消优先于到达", func(t *testing.T) {
		s := newSlot()
		if err := s.resolveValue("v"); err != nil {
			t.Fatal(err)
		}
		v, err := s.wait(&flipErrCtx{Context: context.Background()})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait = (%v, %v), want context.Canceled", v, err)
		}
	})

	t.Run("没被取消时两条出口照常", func(t *testing.T) {
		ready := newSlot()
		if err := ready.resolveValue("v"); err != nil {
			t.Fatal(err)
		}
		v, err := ready.wait(context.Background())
		if err != nil || v != "v" {
			t.Fatalf("wait = (%v, %v), want (v, nil)", v, err)
		}

		skipped := newSlot()
		if err := skipped.resolveSkip(); err != nil {
			t.Fatal(err)
		}
		if _, err := skipped.wait(context.Background()); !errors.Is(err, ErrSkipped) {
			t.Fatalf("wait = %v, want ErrSkipped", err)
		}
	})
}
