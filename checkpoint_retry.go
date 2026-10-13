package pulse

import (
	"fmt"
	"time"
)

// retryCheckpoint 使用 Store 的活跃游标，不按本次调用循环的次数重建历史身份。
// 成功的内层尝试保持可认回，直到完整节点检查点确认；写集仍只交给外层写集。
func retryCheckpoint(rc *RunCtx, next func(*RunCtx) error, attempts int, delay time.Duration) error {
	if err := rc.enterCheckpointRetry(); err != nil {
		return err
	}
	parent := rc.checkpointAttempt
	boundary := fmt.Sprintf("aspect/%d", rc.aspectIndex)
	for {
		attempt, err := rc.openCheckpointAttempt(parent.Scope, boundary, attempts)
		if err != nil {
			return err
		}
		if attempt.Exhausted {
			return fmt.Errorf("pulse: checkpoint Retry on node %q exhausted after %d confirmed attempts", rc.NodeID(), attempts)
		}
		previous := rc.writes
		ws := &writeSet{outer: previous}
		rc.writes, rc.checkpointAttempt = ws, attempt
		err = invokeCheckpointChain(next, rc)
		rc.writes, rc.checkpointAttempt = previous, parent
		if failure := rc.exec.checkpointErr(); failure != nil {
			return failure
		}
		if (err == nil || isSkipped(err)) && rc.ctx.Err() != nil {
			err = rc.ctx.Err()
		}
		if err == nil || isSkipped(err) {
			if cerr := ws.commit(rc.exec); cerr != nil {
				return cerr
			}
			return err
		}
		if isCheckpointFailure(err) || rc.ctx.Err() != nil {
			return err
		}
		// 确认失败先于下一轮；退出发生在两者之间也不能把次数从 1 重来。
		if ferr := rc.failCheckpointAttempt(attempt.Scope); ferr != nil {
			return ferr
		}
		if attempt.Index == attempts {
			return err
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-rc.ctx.Done():
				timer.Stop()
				return rc.ctx.Err()
			}
		}
	}
}
