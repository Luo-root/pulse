package pulse

import "fmt"

// ciGateProbeVet 是 CI go vet 门禁的探针文件：能编译，但 printf 检查会告警。
func ciGateProbeVet() string {
	return fmt.Sprintf("%d", "not-an-int")
}
