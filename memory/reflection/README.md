[English](README.md) | [中文](README_zh.md)

# memory/reflection

The configurable background reflection of P2-D4 (§10.3, Issue #92, P2 wrap-up): input budget truncation → candidate extraction → counting → audited result. **This package is off by default** — no background loop, no timers (trigger timing belongs to the host: end of session / every N turns / idle hooks; nothing runs unless constructed via `New`, zero cost).

Design stance: reflection **outputs only to candidates** (stored as Pending) — it does not Approve/Reject automatically; the approver stamps it (HITL stance). It does not import session (the host takes the surface out and feeds it in — compaction depends on session because it needs to fold/write back; this package only reads input, so zero dependencies keeps it thinner) and does not import observability (auditing = the `ReflectionResult` return value, bridged by the assembly layer). Package documentation (godoc) is in `doc.go`.

## Wiring

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

## Truncation rules

`MaxInputChars` is counted in runes (the counted set aligns with `compaction.CharMeter`: Text/Reasoning + ToolCall Name/Arguments + ToolResult Content Text). When over budget, **whole messages are dropped from the head** (the tail is kept — extraction looks at recent content; at least the last message is kept, and if the final message itself exceeds the budget it is kept whole; whole messages are the granularity: tool pairing stays structurally intact and multi-byte characters are never cut in half). Errors are passed through, never silenced; errored runs are not counted (counters only reflect fully successful runs).

## Metrics surface (the six D4 metrics)

| Metric | Snapshot | Counting point |
|---|---|---|
| Extraction rate | `candidate.Metrics`: Stored/Extracted | `Pipeline.Extract` |
| Approval rate | `candidate.Metrics`: Approved/(Approved+Rejected) | `Pipeline.Approve` |
| Revocation rate | `candidate.Metrics`: Rejected/(Approved+Rejected) | `Pipeline.Reject` (= Revoke) |
| Recall hits | `index.Counted`: Searches/Hits (average hits per search, ≠ Recall@K offline evaluation) | `Counted.Search` |
| Token cost | `reflection.Metrics`: Runs/TotalInputChars/TruncatedChars (real usage belongs to the host bridge, same standard as `compaction.request.usage`) | `Reflector.Reflect` |
| Taint rejection rate | `candidate.Metrics`: RejectedUntrusted/Rejected (only the untrusted-external tier is counted) | `Pipeline.Reject` |

These three snapshots are the whole of the D4 metrics surface — **no separate metrics aggregation package is built** (settled in ticket #92).

## Tests

```bash
go test -race -count=1 ./memory/reflection/
```
