# memory/reflection

P2-D4 的可配置 background reflection（§10.3，Issue #92，P2 收口）：输入预算截断 → candidate 提炼 → 计数 → 审计结果。**本包默认关**——无后台循环、无计时器（触发时机归宿主：会话末/每 N 轮/空闲钩子；不 New 不运行、零成本）。

设计立场：反思**输出只到候选**（Pending 入库）——不自动 Approve/Reject，审批人盖章（HITL 立场）；不 import session（surface 宿主取出喂入——compaction 依赖 session 是因为要 fold/写回，本包只读输入，零依赖更薄）、不 import observability（审计 = `ReflectionResult` 返回值，装配层桥）。包文档（godoc）见 `doc.go`。

## 接入

```go
r, err := reflection.New(reflection.Options{
    Pipeline:      cand,   // candidate.Pipeline（必填；模型路由 = Extractor seam）
    MaxInputChars: 8000,   // 0 = 不限；超限头部丢整条消息（尾部保留）
})

surface, _ := sess.Surface(ctx)      // 宿主从 session 取 surface 喂入
res, err := r.Reflect(ctx, surface)  // 会话末/每 N 轮由宿主调
// res = {Items: 本轮入库 Pending 候选, Report: 提炼计数,
//        InputChars, TruncatedChars}——宿主桥 observability 的审计原料
m := r.Metrics() // {Runs, TotalInputChars, TruncatedChars}
```

## 截断口径

`MaxInputChars` 按 rune 计（计数集合对齐 `compaction.CharMeter`：Text/Reasoning + ToolCall Name/Arguments + ToolResult Content Text）。超限从**头部丢弃整条消息**（尾部保留——提取看近期内容；至少保最后一条，末条自身超预算时整条保留；整条为粒度：tool pairing 结构完整、多字节字符不截半）。错误透传不静默，错误轮不计数（计数只反映完整成功轮）。

## 指标面（D4 六项）

| 指标 | 快照 | 计数点 |
|---|---|---|
| 提炼率 | `candidate.Metrics`：Stored/Extracted | `Pipeline.Extract` |
| 批准率 | `candidate.Metrics`：Approved/(Approved+Rejected) | `Pipeline.Approve` |
| 撤销率 | `candidate.Metrics`：Rejected/(Approved+Rejected) | `Pipeline.Reject`（= Revoke） |
| 召回命中 | `index.Counted`：Searches/Hits（次均命中，≠ Recall@K 离线评测） | `Counted.Search` |
| token 成本 | `reflection.Metrics`：Runs/TotalInputChars/TruncatedChars（真实 usage 归宿主桥，`compaction.request.usage` 同口径） | `Reflector.Reflect` |
| 污染拒绝率 | `candidate.Metrics`：RejectedUntrusted/Rejected（仅 untrusted-external 档计入） | `Pipeline.Reject` |

三处快照即 D4 指标面全貌——**不建独立 metrics 聚合包**（票 #92 定案）。

## 测试

```bash
go test -race -count=1 ./memory/reflection/
```
