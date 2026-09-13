# 可观测性

`observability` 是 v2 的正式观测包：**Bootstrap + Record + Sink** 三件事 + 每请求 TraceID 默认生成器（`NewTraceID`），只依赖 kernel——零业务依赖，不 import llm/loop/flow。

## 三件事

1. **Bootstrap**：观测插件，**最先 Use**（完整轨迹要求观测先于一切业务插件）；订阅 `fiber_state` / `loader_action` 生命周期事件，并输出装配期状态快照横幅；
2. **Record**：构造 `Record`（结构化观测记录，带 host_id / trace_id 双层 trace——装配期 / 请求期）；业务维度走 `Attrs` 开放段，key 契约由事实归属包定义（`llm.AttrModel` / `loop.AttrTool` / `flow.AttrNode`）；
3. **Sink**：已构造 Record 的同步入口，`Write(observability.Record)` 一个方法——这是唯一需要你实现的接口；也可以直接用内置出口。

## 内置出口

| 出口 | 形态 | 适用 |
|---|---|---|
| `SlogSink` | `log/slog`，Text / JSON handler | 接宿主既有 logger、要 JSON 结构化 |
| `LineSink` | 自带缓冲的行式文本（logfmt 风格），**不经 slog** | 单机 / 文件落盘的高频路径（推荐） |
| `MemorySink` | 内存收集 | 测试断言与演示 |
| `MultiSink` | `[]Sink` 切片，扇出到多个出口 | 同时落盘 + 收集 |

`AsyncSink` 是**包装器**而不是出口：包住任一慢出口（文件 / 网络）把投递移出请求路径，`Write` 只做 `Attrs` 深拷 + 入队。它对已经很快的出口（如 `MemorySink`）是负优化，别默认套；持续速率超过出口能力时队列会回压到出口速率——这正是「不丢记录」的代价。

## 设计要点

- **旁路事件**：观测用 kernel 的 On/Emit 订阅，**不进** Waterfall 拦截链——观测永远不改变业务行为；
- **订阅后快照**：Bootstrap 横幅是订阅后的状态快照（后装的观测不会错过历史，因为快照重建当前态）；
- **trace 双层**：`host_id`（装配期身份）+ `trace_id`（请求期身份），运行期四层贯通（宿主 → scope → 模型 → 工具）；
- **`Attrs` 是插入序切片，不是 map**：首次写入按 6 条预留容量，常见记录一次分配；同名覆盖保持原位置；写入面只有泛型 `Set[T AttrValue]`，没有 `map[string]any` 逃生舱；
- **请求级服务是局部绑定**：`AttachCollector` 装的 `CollectorKey` 只有本请求 scope 及其后代 `Get` 得到，父 / 兄弟 / 其他并发请求读不到（互不串台）；它**不参与 fiber 依赖解析**——用 `kernel.Require(CollectorKey)` 声明依赖的插件会静默停在 `inactive`，靠 `FiberSnapshots().WaitingFor` 排查。

## 最短用法

```go
sink := observability.NewLineSink(os.Stdout) // 也可换 &observability.MemorySink{}
defer sink.Flush()                           // 关闭前必须 Flush（最后一批还在缓冲里）

host := kernel.New()
defer host.Dispose()                         // LIFO：Dispose 先于 Flush 执行

// 观测最先装载；Sink 是唯一需要你实现的接口，也可直接用内置出口
if _, err := kernel.Use(host, observability.Bootstrap("myapp", sink)); err != nil {
	return err
}
// 之后装载业务插件：llm.Plugin()、toolset.Plugin() …
```

llm / loop / flow 通过装配层桥接把事件转发为 Record（模型调用、工具调用、节点状态），宿主无需手写埋点即可获得完整轨迹。

详见 [observability 包文档](/packages/observability/)（含出口选择与基准口径）。
