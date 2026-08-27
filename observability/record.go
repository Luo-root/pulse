// Package observability 是 pulse v2 的正式观测包：SpringBoot 式装配
// 日志的最小实现。
//
// 分层纪律（方案 A）：本包只 import kernel，绝不 import llm/loop/flow。
// 它只认识两样东西——kernel 发出的装配期事实（typed 事件），以及
// 下游的 Sink 出口。运行期业务事件（token 计数、HITL 结果、节点耗时）
// 由装配层桥（如 examples/demoapp/bridge.go）订阅后折进 Record 信封写
// 同一 Sink，不经过本包。
//
// 接入姿态（v1 仅一种）：
//
//	host := kernel.New()
//	// 必须最先 Use：完整装载轨迹的前提（kernel 事件不回放）
//	if _, err := kernel.Use(host, observability.Bootstrap("host-1", sink)); err != nil { ... }
//	// 此后其它插件正常 Use，每次状态迁移都会进入 Sink
package observability

import (
	"time"
)

// Source 标识记录来源层。正式包只会产生 kernel 来源；其余来源由
// 装配层桥产生，本包不校验枚举。
type Source string

const (
	SourceKernel Source = "kernel" // fiber_state / loader_action（Bootstrap 产生）
	SourceBridge Source = "bridge" // 装配层桥的运行期事件（trace_id 必填）
)

// Event 名称常量。仅列正式包自己产出的事件；桥的事件名自定义，
// 建议保持 <组件>.<事实> 的点分约定以便日志聚合分组。
const (
	EventFiberState   = "pulse.kernel.fiber_state"
	EventLoaderAction = "pulse.kernel.loader_action"
	EventHostReady    = "observability.host_ready"
)

// Record 是观测信封：通用可空字段 + 装配专用具名段。
//
// 隐私边界由编译器保证：没有 Attributes map 或任何任意 kv 注入口。
// prompt、附件字节、密钥、思维链等payload 内容在类型上就无法进入。
//
// 字段填充规则：
//   - kernel 装配记录（Bootstrap 产生）：TraceID/Duration 为零值
//   - 桥记录：填 TraceID/Duration/Status；token 数等装不进信封的
//     业务指标走 SlogSink 附加键或桥自己的类型，不要扩本结构
type Record struct {
	Time    time.Time
	HostID  string
	TraceID string // 装配期为空；运行期桥必填
	Source  Source
	Event   string

	// Duration 仅运行期指标记录使用；装配迁移恒为 0。
	Duration time.Duration
	// Status 是结果状态字符串（finish reason / completed|failed 等）；
	// 迁移事件为空（状态已在 From/To）。
	Status string

	// Err 非 nil 表示该记录关联一次失败。
	Err error

	// ---- 以下为装配期专用字段，桥记录留零值 ----

	FiberName  string // fiber_state: 实例诊断名
	From, To   string // fiber_state: 状态名（FiberState.String()）
	LoaderKind string // loader_action: mount|unmount|recreate|disable
	EntryID    string // loader_action: 条目 ID
	PluginName string // loader_action: plugin 注册名
}

// Sink 是记录出口。实现必须并发安全且不得长时间阻塞调用方
// （Emit 处于 kernel 派发路径上）。
//
// 契约：无 context.Context——kernel Emit 路径不带 ctx；需要截止时间
// 的导出器自行持有内部队列，不把阻塞回传到派发路径。
type Sink interface {
	Write(r Record)
}

// stampTime 在 Time 为零时补 wall clock，避免调用方漏填导致死字段。
func stampTime(r Record) Record {
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	return r
}

// MultiSink 扇出到多个 Sink；nil 成员跳过。
type MultiSink []Sink

// Write 实现 Sink。Time 由叶子 Sink（SlogSink / MemorySink）补齐。
func (s MultiSink) Write(r Record) {
	for _, sink := range s {
		if sink != nil {
			sink.Write(r)
		}
	}
}
