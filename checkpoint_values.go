package pulse

import "fmt"

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
