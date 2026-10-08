package observe

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
// 即构成单一生成源；也可以完全自带方案（宿主自有格式，如 hostID
// 前缀 + 自增序号）。返回值无契约语义，消费方不要解析其结构。
//
// **需要 W3C trace context 的宿主请注入自己的 trace id**（OTel SDK 的
// span.SpanContext().TraceID().String()，或按 W3C 规范生成的 32 位小写
// hex）——本函数的形状是「时间戳-随机段-序号」，人读友好但**不是** W3C
// 的 trace-id 格式。另外，只有 trace-id 也发不出一条合法的 traceparent：
// parent-id 与 trace-flags 是 span 语义，归宿主（本包只有「一次运行」
// 的概念，没有 span）。入站续接同理：把收到的 trace-id 填进
// ObserveConfig.TraceID 即可，非法 traceparent 的整条忽略是宿主的事。
func NewTraceID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在受支持平台上不会失败；退化为纯序号仍进程内唯一。
		return fmt.Sprintf("%d-%d", time.Now().UnixNano(), traceSeq.Add(1))
	}
	return fmt.Sprintf("%d-%s-%d", time.Now().UnixNano(), hex.EncodeToString(b[:]), traceSeq.Add(1))
}
