[English](README.md) | [中文](README_zh.md)

# observability/bridge

The official implementation of the assembly-layer observability bridge: subscribes to public llm/loop runtime events, folds them into `observability.Record` and writes them to the host Sink; flow node timing goes through the `FlowObserver` adapter; business plugins write directly via the `CollectorKey` service.

Layering position: the `observability` package itself imports only kernel (plan A); this package is its companion assembly layer and may import llm/loop/flow. The bridge does mechanism only — event folding, identifier injection, Collector exposure.

## Folding map (settled)

| Event | Handling | Record |
|---|---|---|
| `llm.before_generate` | **Timing only** (waterfall passthrough) | none |
| `llm.after_response` | write | `llm.generate_finished`: Status=finish_reason, Duration=this generation, Attrs=model / tokens_in / tokens_out / tokens_cached |
| `loop.after_tool_call` | write | `loop.tool_finished`: Status=completed\|failed\|rejected, Duration/Err passthrough, Attrs=tool |
| `loop.turn_end` | write | `loop.turn_finished`: Status=stopped_by, Attrs=steps; tokens are not repeated here (the after_response per-call figure is authoritative) |
| `loop.before_tool_call` | **deliberately not subscribed** | the Waterfall HITL approval mount point; the bridge must not enter the approval chain and pollute human decisions |
| flow Observer | adapter with segment timing | two records, `flow.node_wait_finished` / `flow.node_run_finished`, Attrs=node |

## Wiring

```go
host := kernel.New()
if _, err := kernel.Use(host, observability.Bootstrap("host-1", sink)); err != nil { ... }

// Per request: the scope must be the same one passed to the Agent via
// llm.WithEventScope — llm/loop dispatch uses EmitLocal/WaterfallLocal,
// visible only within that scope.
reqScope, _ := host.Derive()
defer reqScope.Dispose()
b, err := bridge.Attach(reqScope, bridge.Config{
    Sink:    sink,                // same instance as Bootstrap = same outlet
    HostID:  "host-1",
    TraceID: host.NewTraceID(),   // D3: injected from the host's single source; the bridge never mints its own
})

// Attach segment timing to a flow graph (combine with host-owned observers
// via MultiObserver):
g := flow.New(ctx, flow.WithObserver(b.FlowObserver()))

// Direct writes from business plugins (the Collector is registered into
// reqScope by Attach):
if c, ok := kernel.Get(reqScope, bridge.CollectorKey); ok {
    c.WriteAttrs("app.order_placed", "ok", func(a *observability.Attrs) {
        observability.Set(a, "app.order_id", ordID)
    })
}
```

For pure graph runs without a request scope use `bridge.New(cfg)`: no listeners attached, meant only for `FlowObserver` / `Write` / `WriteAttrs` outlet writes.

## Public surface

```go
func Attach(scope *kernel.Context, cfg Config) (*Bridge, error) // listeners + Collector service
func New(cfg Config) *Bridge                                    // scope-free pure outlet
func (b *Bridge) Write(event, status string)
func (b *Bridge) WriteAttrs(event, status string, set func(*observability.Attrs))
func (b *Bridge) FlowObserver() flow.Observer
var CollectorKey = kernel.NewServiceKey[*Bridge]("pulse.observability.collector")
```

- Listeners detach automatically when the scope is disposed; `Bridge` itself has no Close.
- The `CollectorKey` service is withdrawn when the scope is disposed; directly written records carry HostID/TraceID automatically.
- Node IDs go into `flow.AttrNode`, never into Record's assembly-only named fields.

## Deliberately out of scope

- No OTel / Prometheus exporter (implemented as a host-side Sink)
- No subscription to `loop.before_tool_call` (HITL neutrality, see the table above)
- No changes to the kernel event system; business custom observation goes through Collector direct writes, not the bus
- No map[string]any escape hatch (attr values are locked down by the generic `observability.Set`)
