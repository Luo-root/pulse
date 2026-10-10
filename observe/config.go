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
	// Path 是这次运行的**嵌套层级路径**（不透明字符串、`/` 连接各层），
	// 由引擎给值、本包写进**每一条**记录（`pulse.AttrPath`）：建子图的
	// 出口把它填成 `sc.Path()`，宿主自己的出口留空。
	//
	// 它是**出口实例**上的一个值，不是每条记录各带一个：嵌套就是按层各建一个
	// 出口（`Sub` 的 `build` 里正合适——那里本来就要建子图）。想让同一个出口
	// 同时管多层，就得改用自己拼的 Attr，别指望这个字段。
	//
	// **空串 = 不写这个 attr**（`Attrs` 是「有才有」）：根图的记录本来就没
	// 有层级，写空串会让「根」与「忘了传」长得一样。层级怎么拆是出口的事
	// （按 `/` 分即可），引擎与本包都不提供结构化树。
	Path string
}

// ErrNilSink 表示适配配置缺出口。
var ErrNilSink = errors.New("observe: nil sink in ObserveConfig")
