# kernel 局部事件派发：EmitLocal / WaterfallLocal

> 状态：Accepted（Issue [#20](https://github.com/Luo-root/pulse/issues/20)，2026-08-27）
> 包：`kernel`（事件总线扩展）；消费方：`loop`、`llm.observed`、请求级 Bridge

## 一句话

全树 `Emit` 保留给宿主级观察；请求级事实改走 `EmitLocal` / `WaterfallLocal`——只本 scope，不向父、不向子、不向兄弟。

## 为什么需要

`Emit(scope)` 实际从 `scope.root()` 整树广播。请求级 Bridge / HITL 挂在各自 `reqScope` 上时会被打穿：

```text
AgentA(scopeA) 发 AfterToolCall
  → Emit(scopeA) → root walk → BridgeB(scopeB) 也收到
```

实测：`bridge cross-talk: A=1 B=1`。

## API 契约

```go
EmitLocal(c, key, payload)       // 只 c.events；nil 安全 no-op
WaterfallLocal(c, key, payload)  // 只 c 本层 around；nil 安全原样返回
```

| | Emit / Waterfall | EmitLocal / WaterfallLocal |
|---|---|---|
| 收集路径 | `root()` 整树 walk | 只本层 `eventBus` |
| 适用 | fiber_state / loader_action | tool / turn / HITL / llm generate |
| 父/子/兄弟 | 能收到 | **收不到** |

实现禁止「整树收集后再过滤」——必须不调用 `root()`。

## llm 事件口径（选项 A 钉死）

错误与正确不等价：

```text
❌ EmitLocal(reg.ctx, …)          // 串扰没了，reqScope Bridge 也听不到
✅ EmitLocal(reqScope, …)         // Bridge 只听本请求
```

注入方式：

1. `loop` 调用 `model.Stream/Generate` 前：`llm.WithEventScope(ctx, a.scope)`
2. `llm.observed`：`EventScopeFrom(ctx)` 优先；没有则 `EmitLocal(reg.ctx, …)`（仅宿主层监听）

## 与 observability 的关系

- Bootstrap / fiber_state / loader_action：**继续全树 Emit**
- 请求级 Bridge：挂 `reqScope`，吃 Local 事件
- 本契约已落地（PR [#21](https://github.com/Luo-root/pulse/pull/21)）：`TestRequestBridgesDoNotCrossTalk` / `llm/local_isolation_test.go` / `loop/local_isolation_test.go` 钉住隔离；#19 不再因 Local 阻塞

## 不做

父链冒泡、`EmitSubtree`、payload 塞 TraceID、Bridge 事后过滤。
