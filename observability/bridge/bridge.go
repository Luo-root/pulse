// Package bridge 是装配层观测桥的正式实现：订阅 llm/loop 的公开
// 运行期事件，折叠为 observability.Record 写入宿主 Sink；flow 图的
// 节点计时经 FlowObserver 适配；业务插件经 Collector 服务直写。
//
// 分层位置（observability-v1-design.md 方案 A）：observability 本体
// 只 import kernel；本包是它的伴生装配层，允许 import llm/loop/flow。
// 桥只做机制——事件折叠、标识注入、Collector 暴露；不改 kernel 事件
// 系统，不做 OTel 导出（宿主侧 Sink 自行实现）。
//
// 折叠映射（v1 定案）：
//   - llm.before_generate：只计时，不写记录（waterfall 透传）；
//   - llm.after_response：写 llm.generate_finished（模型/token 进 Attrs，
//     key 契约见 llm.Attr*）；
//   - loop.after_tool_call：写 loop.tool_finished（tool 进 Attrs）；
//   - loop.turn_end：写 loop.turn_finished（steps 进 Attrs；token 用量
//     以 after_response 单次口径为准，桥不做累计）；
//   - loop.before_tool_call：**刻意不订阅**——它是 Waterfall HITL 审批
//     挂载点，桥不得进入审批链污染人机决策。
//
// 接入姿态：
//
//	reqScope, _ := host.Ctx.Derive() // 每请求子作用域
//	defer reqScope.Dispose()
//	b, err := bridge.Attach(reqScope, bridge.Config{
//		Sink:    sink,
//		HostID:  "host-1",
//		TraceID: host.NewTraceID(), // D3：宿主单一生成源注入
//	})
//	// 监听必须挂在与 Agent（llm.WithEventScope）相同的 scope——
//	// llm/loop 派发走 EmitLocal/WaterfallLocal，只本 scope 可见。
//	// Collector 已注册进同一 scope，业务插件可直写：
//	if c, ok := kernel.Get(reqScope, bridge.CollectorKey); ok {
//		c.WriteAttrs("app.order_placed", "ok", func(a *observability.Attrs) {
//			observability.Set(a, "app.order_id", ordID)
//		})
//	}
package bridge

import (
	"sync"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/llm"
	"github.com/Luo-root/pulse/loop"
	"github.com/Luo-root/pulse/observability"
)

// 桥事件名：折叠后 Record.Event 的取值。约定 <组件>.<事实> 点分，
// 与官方包事件（observability.Event*）同风格但互不重叠。
const (
	EventGenerateFinished = "llm.generate_finished" // after_response 折叠
	EventToolFinished     = "loop.tool_finished"    // after_tool_call 折叠
	EventTurnFinished     = "loop.turn_finished"    // turn_end 折叠
	EventNodeWaitFinished = "flow.node_wait_finished"
	EventNodeRunFinished  = "flow.node_run_finished"
)

// 工具结果状态（loop.tool_finished 的 Status 取值）。
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusRejected  = "rejected"
)

// Config 是 Attach 的装配参数。
type Config struct {
	// Sink 是记录出口，与 observability.Bootstrap 共用同一实例即为
	//「装配轨迹与运行期事实同一出口」。
	Sink observability.Sink
	// HostID 是宿主稳定标识。
	HostID string
	// TraceID 由宿主的单一生成源注入（D3 两层标识）：桥不自己造
	// 序号，同一请求的全部桥记录共享该 ID。
	TraceID string
}

// Bridge 是一次请求的运行期观测桥。生命周期 = 该请求：监听随 Attach
// 的 scope 销毁自动摘除，Bridge 本身无 Close。
//
// 构造：有请求 scope 的完整接入走 Attach（挂监听 + Collector 服务）；
// 纯图运行 / 无内核足迹的出口直写走 New。
type Bridge struct {
	// Sink 是记录出口；HostID/TraceID 注入后只读（每请求一个 Bridge）。
	Sink   observability.Sink
	HostID string
	// TraceID 由宿主单一生成源注入（D3），同请求全部桥记录共享。
	TraceID string

	mu       sync.Mutex
	genStart time.Time
}

// New 创建桥实例（不挂任何监听）：配合 FlowObserver / Write /
// WriteAttrs 做无 scope 的纯出口用法。
func New(cfg Config) *Bridge {
	return &Bridge{Sink: cfg.Sink, HostID: cfg.HostID, TraceID: cfg.TraceID}
}

// Attach 创建请求桥，把 llm/loop 运行期事件的监听挂到 scope，并把
// Collector 服务注册进同一 scope（bridge.CollectorKey）。
//
// scope 必须与 Agent 的 llm.WithEventScope 相同：Local 派发下挂错
// scope 什么也听不到（kernel-local-events.md）。nil scope 返回错误。
func Attach(scope *kernel.Context, cfg Config) (*Bridge, error) {
	if scope == nil {
		return nil, errNilScope
	}
	if cfg.Sink == nil {
		return nil, errNilSink
	}
	b := New(cfg)

	// before_generate：只计时。waterfall 监听必须委托 next，桥不改请求。
	if _, err := kernel.OnWaterfall(scope, llm.EventBeforeGenerate,
		func(req *llm.GenerateRequest, next func(*llm.GenerateRequest) *llm.GenerateRequest) *llm.GenerateRequest {
			b.mu.Lock()
			b.genStart = time.Now()
			b.mu.Unlock()
			return next(req)
		}); err != nil {
		return nil, err
	}
	// after_response：生成事实（模型 + 单次 token 口径）。
	if _, err := kernel.On(scope, llm.EventAfterResponse, func(resp *llm.Response) {
		b.mu.Lock()
		started := b.genStart
		b.mu.Unlock()
		b.write(EventGenerateFinished, string(resp.FinishReason), time.Since(started), nil,
			func(a *observability.Attrs) {
				observability.Set(a, llm.AttrModel, resp.Model)
				// TokenUsage 用 int；attrs 契约锁 ~int64，此处显式收窄。
				observability.Set(a, llm.AttrTokensIn, int64(resp.Usage.InputTokens))
				observability.Set(a, llm.AttrTokensOut, int64(resp.Usage.OutputTokens))
				observability.Set(a, llm.AttrTokensCached, int64(resp.Usage.CachedInputTokens))
			})
	}); err != nil {
		return nil, err
	}
	// after_tool_call：工具执行事实。before_tool_call 刻意不订阅（HITL 中立）。
	if _, err := kernel.On(scope, loop.EventAfterToolCall, func(after *loop.AfterToolCall) {
		status := StatusCompleted
		switch {
		case after.Rejected:
			status = StatusRejected
		case after.Err != nil:
			status = StatusFailed
		}
		b.write(EventToolFinished, status, after.Duration, after.Err,
			func(a *observability.Attrs) {
				observability.Set(a, loop.AttrTool, after.Call.Name)
			})
	}); err != nil {
		return nil, err
	}
	// turn_end：回合事实。token 用量不在此重复（口径见包注释）。
	if _, err := kernel.On(scope, loop.EventTurnEnd, func(end *loop.TurnEnd) {
		b.write(EventTurnFinished, string(end.StoppedBy), 0, nil,
			func(a *observability.Attrs) {
				observability.Set(a, loop.AttrSteps, int64(end.Steps))
			})
	}); err != nil {
		return nil, err
	}

	// Collector 服务化：业务插件 kernel.Get(scope, CollectorKey) 直写
	// 同一出口，HostID/TraceID 自动携带。
	if _, err := kernel.Provide(scope, CollectorKey, b); err != nil {
		return nil, err
	}
	return b, nil
}

// Write 让宿主把自定义事实写进同一出口（状态型，无 attrs）。
func (b *Bridge) Write(event, status string) {
	b.write(event, status, 0, nil, nil)
}

// WriteAttrs 带 attrs 的自定义事实写入。set 收到空 Attrs，可多次
// observability.Set；set 为 nil 等价 Write。
func (b *Bridge) WriteAttrs(event, status string, set func(a *observability.Attrs)) {
	b.write(event, status, 0, nil, set)
}

// write 是全部桥记录的统一出口：补齐信封公共段，官方 Record 的装配
// 专用字段保持零值。
func (b *Bridge) write(event, status string, d time.Duration, err error, set func(a *observability.Attrs)) {
	rec := observability.Record{
		HostID:   b.HostID,
		TraceID:  b.TraceID,
		Source:   observability.SourceBridge,
		Event:    event,
		Status:   status,
		Duration: d,
		Err:      err,
	}
	if set != nil {
		set(&rec.Attrs)
	}
	b.Sink.Write(rec)
}
