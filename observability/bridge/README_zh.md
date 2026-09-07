# observability/bridge

装配层观测桥的官方实现：订阅 llm/loop 的公开运行期事件，折叠为 `observability.Record` 写入宿主 Sink；flow 节点计时经 `FlowObserver` 适配；业务插件经 `CollectorKey` 服务直写。

分层位置：`observability` 本体只 import kernel（方案 A）；本包是它的伴生装配层，允许 import llm/loop/flow。桥只做机制——事件折叠、标识注入、Collector 暴露。

## 折叠映射（定案）

| 事件 | 处理 | Record |
|---|---|---|
| `llm.before_generate` | **只计时**（waterfall 透传）。必须订阅：`after_response` 不携带耗时，Waterfall 回调是唯一计时起点；恒 `next` 不改参的观察者不改变 Waterfall 语义 | 不写 |
| `llm.after_response` | 写记录 | `llm.generate_finished`：Status=finish_reason，Duration=本次生成耗时，Attrs=model / tokens_in / tokens_out / tokens_cached |
| `loop.after_tool_call` | 写记录 | `loop.tool_finished`：Status=completed\|failed\|rejected，Duration/Err 透传，Attrs=tool |
| `loop.turn_end` | 写记录 | `loop.turn_finished`：Status=stopped_by，Attrs=steps；token 不在此重复（以 after_response 单次口径为准） |
| `loop.before_tool_call` | **刻意不订阅** | `AfterToolCall` 已自带 Duration/Err，订阅无观测增益，少一份与 HITL 审批链的顺序耦合 |
| flow Observer | 适配器分段计时 | `flow.node_wait_finished` / `flow.node_run_finished` 两条，Attrs=node |

## 接入

```go
host := kernel.New()
if _, err := kernel.Use(host, observability.Bootstrap("host-1", sink)); err != nil { ... }

// 每请求：scope 必须与 Agent 的 llm.WithEventScope 相同——
// llm/loop 派发走 EmitLocal/WaterfallLocal，只本 scope 可见。
reqScope, _ := host.Derive()
defer reqScope.Dispose()
b, err := bridge.Attach(reqScope, bridge.Config{
    Sink:    sink,                // 与 Bootstrap 同一实例 = 同一出口
    HostID:  "host-1",
    TraceID: host.NewTraceID(),   // D3：宿主单一生成源注入，桥不自造序号
})

// flow 图挂分段计时（可与宿主自有 Observer 用 MultiObserver 组合）：
g := flow.New(ctx, flow.WithObserver(b.FlowObserver()))

// 业务插件直写（Collector 随 Attach 注册进 reqScope）：
if c, ok := kernel.Get(reqScope, bridge.CollectorKey); ok {
    c.WriteAttrs("app.order_placed", "ok", func(a *observability.Attrs) {
        observability.Set(a, "app.order_id", ordID)
    })
}
```

无请求 scope 的纯图运行用 `bridge.New(cfg)`：不挂监听，仅供 `FlowObserver` / `Write` / `WriteAttrs` 出口直写。

## 公开面

```go
func Attach(scope *kernel.Context, cfg Config) (*Bridge, error) // 挂监听 + Collector 服务
func New(cfg Config) *Bridge                                    // 无 scope 纯出口
func (b *Bridge) Write(event, status string)
func (b *Bridge) WriteAttrs(event, status string, set func(*observability.Attrs))
func (b *Bridge) FlowObserver() flow.Observer
var CollectorKey = kernel.NewServiceKey[*Bridge]("pulse.observability.collector")
```

- 监听随 scope 销毁自动摘除，`Bridge` 本身无 Close。
- `CollectorKey` 服务随 scope 销毁撤除；直写记录自动携带 HostID/TraceID。
- nodeID 走 `flow.AttrNode`，不占用 Record 的装配专用具名字段。

## 明确不做

- 不做 OTel / Prometheus 导出器（宿主侧 Sink 自行实现）
- 不订阅 `loop.before_tool_call`（无观测增益，见上表）
- 不改 kernel 事件系统；业务自定义观测走 Collector 直写，不走总线
- 不做 map[string]any 逃生舱（attrs 值域经 `observability.Set` 泛型锁死）
