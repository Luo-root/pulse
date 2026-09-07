package observability

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

// traceSeq 保证同一纳秒内多次调用仍进程内唯一。
var traceSeq atomic.Uint64

// NewTraceID 生成一次请求的 trace 标识：UnixNano 时间戳 + 随机段 +
// 进程内序号。进程内并发唯一，跨进程靠随机段区分。
//
// D3 约定 TraceID 由宿主单一生成源注入：宿主每请求调用本函数一次
// 即构成单一生成源；也可以完全自带方案（如 demoapp 的 hostID 前缀
// 格式）。返回值无契约语义，消费方不要解析其结构。
func NewTraceID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在受支持平台上不会失败；退化为纯序号仍进程内唯一。
		return fmt.Sprintf("%d-%d", time.Now().UnixNano(), traceSeq.Add(1))
	}
	return fmt.Sprintf("%d-%s-%d", time.Now().UnixNano(), hex.EncodeToString(b[:]), traceSeq.Add(1))
}
