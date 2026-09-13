package observability

import (
	"errors"

	"github.com/Luo-root/pulse/kernel"
)

// ObserveConfig 是各包观测适配的公共装配参数。
//
// 生命周期 = 请求：同一请求的 AttachCollector / 各包 Observe /
// NewRecordObserver 复用同一值——共享 Sink 与 TraceID 正是请求级
// 关联（D3）的语义；值类型无内部状态，传递即拷贝，可安全并发。
// 跨请求必须新建（TraceID 每请求唯一，复用旧值会制造假关联）。
type ObserveConfig struct {
	// Sink 与 Bootstrap 共用同一实例即为「同一出口」；
	// 实现必须并发安全（Sink 契约）。
	Sink Sink
	// HostID 是宿主稳定标识。
	HostID string
	// TraceID 由宿主单一生成源注入（D3），同请求全部记录共享；
	// 适配层从不自造序号。
	TraceID string
}

// 装配校验哨兵。
var (
	ErrNilScope = errors.New("observability: nil request scope")
	ErrNilSink  = errors.New("observability: nil sink in ObserveConfig")
)

// CollectorKey 把运行期直写服务暴露给业务插件：请求处理路径中
//
//	c, ok := kernel.Get(scope, observability.CollectorKey)
//
// 直写观测（Write / WriteAttrs），HostID/TraceID 自动携带，与各包
// Observe 折出的记录走同一 Sink（同一出口）。
//
// 可见性是**作用域局部**的（kernel.Provide(..., kernel.Local())）：
// Collector 只有本请求 scope 及其后代读得到——父 / 兄弟 / 其他请求
// 都读不到（并发请求互不串台）。读方因此要持请求 scope（或其子孙）
// 去 Get；服务随 scope 销毁撤除。
//
// 注意：作用域局部绑定**不参与 fiber 依赖解析**。声明
//
//	func (p *T) Inject() []kernel.Dependency {
//		return []kernel.Dependency{kernel.Require(observability.CollectorKey)}
//	}
//
// 的插件会永远停在 inactive——不报错、不打日志、不触发事件（局部绑定是
// 请求级数据，Require 只认全局绑定，见 kernel.Local）。Collector 的正确
// 消费形态是持请求 scope 用 Get 读取；插件无故不激活时，用
// FiberSnapshots() 的 WaitingFor 看未满足的依赖名。
var CollectorKey = kernel.NewServiceKey[*Collector]("pulse.observability.collector")

// Collector 是业务插件的观测直写面（双基座入口形态）：不认识任何
// 业务包，只按 Record 信封写记录；Source 恒为 SourceAdapter。
type Collector struct {
	sink    Sink
	hostID  string
	traceID string
}

// Write 直写一条状态型事实。
func (c *Collector) Write(event, status string) {
	c.write(event, status, nil)
}

// WriteAttrs 带 attrs 直写：set 收到空 Attrs，可多次 observability.Set；
// set 为 nil 等价 Write。引用语义见 Sink 接口契约（Write 后不再修改）。
func (c *Collector) WriteAttrs(event, status string, set func(a *Attrs)) {
	c.write(event, status, set)
}

// write 是 Collector 的统一出口：补齐信封公共段，装配专用字段保持零值。
// 直写面定位为状态型事实——不带 Duration/Err（运行期耗时与失败语义
// 由各包 Observe 折叠产生，不经业务直写口）。
func (c *Collector) write(event, status string, set func(a *Attrs)) {
	rec := Record{
		HostID:  c.hostID,
		TraceID: c.traceID,
		Source:  SourceAdapter,
		Event:   event,
		Status:  status,
	}
	if set != nil {
		set(&rec.Attrs)
	}
	c.sink.Write(rec)
}

// AttachCollector 把 Collector 注册进请求 scope：**作用域局部绑定**
// （kernel.Provide(..., kernel.Local())），只有该 scope 及其后代
// 读得到，父 / 兄弟 / 其他并发请求都读不到——随 scope 销毁撤除。
// 同一 scope 重复 Attach 按覆盖语义以最后一次为准。
// nil scope / nil Sink 返回哨兵错误。
//
// 因为是局部绑定，它**不满足 kernel.Require**——消费方用 kernel.Get
// 读取，不要声明成 fiber 依赖（见 CollectorKey）。
func AttachCollector(scope *kernel.Context, cfg ObserveConfig) (*Collector, error) {
	if scope == nil {
		return nil, ErrNilScope
	}
	if cfg.Sink == nil {
		return nil, ErrNilSink
	}
	c := &Collector{sink: cfg.Sink, hostID: cfg.HostID, traceID: cfg.TraceID}
	if _, err := kernel.Provide(scope, CollectorKey, c, kernel.Local()); err != nil {
		return nil, err
	}
	return c, nil
}
