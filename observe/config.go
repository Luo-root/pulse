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
	// Path 是 Site.Path 为空时的挂载路径兜底，通常留空。
	// 引擎在每条回调的 Site.Path 中提供实际挂载位置（根为空串，一层为
	// "step"，更深为 "outer/inner"）；记录优先使用它写入 pulse.AttrPath，
	// 因此同一个观察者可由子图继承并服务多层，无需逐层另建出口。
	//
	// 只有 Site.Path 为空时才使用本字段；两者都为空就不写 pulse.AttrPath。
	// 非空兜底也会作用于根图记录，因此普通引擎装配应留空，让 Site 表达层级。
	// 路径是不透明字符串，记录消费方可按 `/` 拆分；静态树和活子执行分别
	// 从 Topology 与 Execution 的 Snapshot 查看。
	Path string
}

// ErrNilSink 表示适配配置缺出口。
var ErrNilSink = errors.New("observe: nil sink in ObserveConfig")
