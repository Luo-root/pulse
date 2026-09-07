package bridge

import (
	"errors"

	"github.com/Luo-root/pulse/kernel"
)

// Attach 的装配校验哨兵。
var (
	errNilScope = errors.New("bridge: Attach requires a request scope")
	errNilSink  = errors.New("bridge: Attach requires a sink")
)

// CollectorKey 把请求桥作为观测收集服务暴露给业务插件：Attach 时
// 自动 Provide 进同一请求 scope，业务插件
//
//	c, ok := kernel.Get(scope, bridge.CollectorKey)
//
// 直写观测（c.Write / c.WriteAttrs，HostID/TraceID 自动携带）。
// 服务随 scope 销毁自动撤除。
var CollectorKey = kernel.NewServiceKey[*Bridge]("pulse.observability.collector")
