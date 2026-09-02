# 可观测性

`observability` 是 v2 的正式观测包：**Bootstrap + Record + Sink** 三件事，只依赖 kernel——零业务依赖，不 import llm/loop/flow。

## 三件事

1. **Bootstrap**：观测插件，**最先 Use**（完整轨迹要求观测先于一切业务插件）；订阅 `fiber_state` / `loader_action` 生命周期事件，并输出装配期状态快照横幅；
2. **Record**：构造 `Record`（结构化观测记录，带 host_id / trace_id 双层 trace——装配期 / 请求期）；
3. **Sink**：已构造 Record 的同步入口，`Write(observability.Record)` 一个方法；benchmark 用黑洞 Sink（nopSink），生产接任意后端。

## 设计要点

- **旁路事件**：观测用 kernel 的 On/Emit 订阅，**不进** Waterfall 拦截链——观测永远不改变业务行为；
- **订阅后快照**：Bootstrap 横幅是订阅后的状态快照（后装的观测不会错过历史，因为快照重建当前态）；
- **trace 双层**：`host_id`（装配期身份）+ `trace_id`（请求期身份），运行期四层贯通（宿主 → scope → 模型 → 工具）。

## 最短用法

```go
host := kernel.New()
defer host.Dispose()

// 观测最先装载；Sink 是唯一需要你实现的接口
if _, err := kernel.Use(host, observability.Bootstrap("myapp", mySink)); err != nil {
	return err
}
// 之后装载业务插件：llm.Plugin()、toolset.Plugin() …
```

llm / loop / flow 通过装配层桥接把事件转发为 Record（模型调用、工具调用、节点状态），宿主无需手写埋点即可获得完整轨迹。

详见 [observability 包文档](/packages/observability/)。
