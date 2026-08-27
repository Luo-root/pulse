package observability

import (
	"fmt"

	"github.com/Luo-root/pulse/kernel"
)

// Bootstrap 返回装配期观测插件。
//
// 使用约束（违反即失去「完整装载轨迹」）：
//
//  1. 必须是 host 上**最先 Use 的插件**——kernel 事件不回放，后装
//     只能靠快照横幅兜底，之前的迁移不进 Sink；
//  2. 监听登记在本插件 Apply 的私有子 ctx 上（全树收集可听整树事件，
//     包括 host 根销毁时的 T7）；不要把观测监听手动挂到 root；
//  3. hostID 由调用方提供（进程内唯一即可），出现在横幅与全部
//     kernel 记录中。
func Bootstrap(hostID string, sink Sink) kernel.Plugin {
	return kernel.Func(func(c *kernel.Context) error {
		if sink == nil {
			return fmt.Errorf("observability: sink is required")
		}
		if _, err := kernel.On(c, kernel.EventFiberState, func(ch *kernel.FiberStateChange) {
			sink.Write(Record{
				HostID:    hostID,
				Source:    SourceKernel,
				Event:     EventFiberState,
				Err:       ch.Err,
				FiberName: ch.Name,
				From:      ch.From.String(),
				To:        ch.To.String(),
			})
		}); err != nil {
			return err
		}
		if _, err := kernel.On(c, kernel.EventLoaderAction, func(la *kernel.LoaderAction) {
			status := "ok"
			if la.Err != nil {
				status = "failed"
			}
			sink.Write(Record{
				HostID:     hostID,
				Source:     SourceKernel,
				Event:      EventLoaderAction,
				Status:     status,
				Err:        la.Err,
				LoaderKind: string(la.Kind),
				EntryID:    la.EntryID,
				PluginName: la.Name,
			})
		}); err != nil {
			return err
		}

		// 横幅 = 订阅后的状态快照：后装场景仍能给出正确的当前视图
		//（历史轨迹则只有"最先 Use"才保证）。
		writeBanner(hostID, c, sink)
		return nil
	})
}

// writeBanner 扫描整棵作用域树的 Fiber 快照并输出分类清单。
func writeBanner(hostID string, c *kernel.Context, sink Sink) {
	snaps := c.FiberSnapshots()
	var active, failed, waiting, other int
	for _, s := range snaps {
		switch s.State {
		case kernel.StateActive:
			active++
		case kernel.StateFailed:
			failed++
		case kernel.StateInactive:
			if len(s.WaitingFor) > 0 {
				waiting++
			} else {
				other++
			}
		default:
			other++
		}
	}
	sink.Write(Record{
		HostID: hostID,
		Source: SourceKernel,
		Event:  EventHostReady,
		Status: fmt.Sprintf("active=%d failed=%d waiting=%d idle=%d total=%d",
			active, failed, waiting, other, len(snaps)),
	})

	// 明细逐条列出，便于装配排障（SpringBoot 式体验的核心）。
	for _, s := range snaps {
		detail := ""
		switch {
		case len(s.WaitingFor) > 0:
			detail = fmt.Sprintf(" (waiting: %v)", s.WaitingFor)
		case s.Err != nil:
			detail = fmt.Sprintf(" (%v)", s.Err)
		}
		sink.Write(Record{
			HostID:    hostID,
			Source:    SourceKernel,
			Event:     EventHostReady,
			Status:    s.State.String() + detail,
			FiberName: s.Name,
		})
	}
}
