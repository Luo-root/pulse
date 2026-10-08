package observe

import "errors"

// ObserveConfig 是观测适配的装配参数：出口 + 归因身份。
//
// 生命周期 = 一次运行（图的一次 Run）：同一次运行的适配器复用同一值
// ——共享 Sink 与 TraceID 正是运行级关联的语义；值类型无内部状态，
// 传递即拷贝，可安全并发。跨运行必须新建（TraceID 每次唯一，复用旧
// 值会制造假关联）。
type ObserveConfig struct {
	// Sink 是记录出口；实现必须并发安全（见 Sink 契约）。
	Sink Sink
	// HostID 是宿主稳定标识。
	HostID string
	// TraceID 由宿主单一生成源注入（见 NewTraceID），同一次运行的全部
	// 记录共享；适配层从不自造序号。
	TraceID string
}

// ErrNilSink 表示适配配置缺出口。
var ErrNilSink = errors.New("observe: nil sink in ObserveConfig")
