package pulse

import "fmt"

// 已确认键归完整节点帧，不归消息尝试；连续消息和恢复都不能重复保存普通键。
// 共用执行树锁只保护索引，不在锁内调用钩子或读取业务值。
func (rc *RunCtx) checkpointValueConfirmed(name string) bool {
	failure := rc.exec.checkpoint.failure
	failure.mu.Lock()
	defer failure.mu.Unlock()
	return failure.values[rc.checkpointWrites][name]
}

func (rc *RunCtx) confirmCheckpointValues(values []CheckpointOutput) {
	if len(values) == 0 {
		return
	}
	failure := rc.exec.checkpoint.failure
	failure.mu.Lock()
	defer failure.mu.Unlock()
	if failure.values == nil {
		failure.values = make(map[*writeSet]map[string]bool)
	}
	confirmed := failure.values[rc.checkpointWrites]
	if confirmed == nil {
		confirmed = make(map[string]bool, len(values))
		failure.values[rc.checkpointWrites] = confirmed
	}
	for _, value := range values {
		confirmed[value.Key.Name] = true
	}
}

func (rc *RunCtx) forgetCheckpointValues() {
	failure := rc.exec.checkpoint.failure
	failure.mu.Lock()
	defer failure.mu.Unlock()
	delete(failure.values, rc.checkpointWrites)
}

// 全量验证后才安装恢复子集；不能让存储钩子借它发布别人的键、流或未到达值。
func checkCheckpointAttemptValues(n *Node, values []CheckpointOutput) error {
	keys := make(map[string]keyRef, len(n.provides))
	for _, ref := range n.provides {
		keys[ref.name] = ref
	}
	for _, output := range values {
		ref, ok := keys[output.Key.Name]
		if !ok || output.Key != checkpointKey(ref) || isLiveStreamType(ref.typ) || output.Stream != nil {
			return fmt.Errorf("checkpoint node %q: undeclared, duplicate or non-ordinary message value %q", n.id, output.Key.Name)
		}
		delete(keys, output.Key.Name)
		if err := checkCheckpointOutput(output); err != nil {
			return err
		}
	}
	return nil
}
