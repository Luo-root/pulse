[English](README.md) | [中文](README_zh.md)

# memory/compaction

The P2-B compaction layer: token meter, §9.1 eight-step compaction transaction, §9.2 deterministic tool result pruning.
Core invariant — **compaction is a transaction, not deletion**: the raw log only grows and never shrinks, `checkpoint.Replaced` records the complete source refs of the replaced window, and failures leave an audit trail. Design source of truth §9; implementation ticket #73.

## Integration

```go
rep, err := compaction.Compact(ctx, sess, compaction.Options{
    Engine:    &compaction.LLMSummarizer{Model: model, ModelName: "gpt-test"},
    Meter:     compaction.CharMeter{},   // nil 用默认 CharMeter
    ModelName: "gpt-test",
    // Window: &[2]int{0, 9},  // 选区；nil = 全量
})
// rep.Replaced      = 被替代窗口的 source event seqs
// rep.CheckpointSeq = compaction.checkpoint 事件的 Seq（新节点的溯源锚点）
```

After compaction, the corresponding window of `sess.Surface()` is replaced by a single `RoleUser` stable-prefix summary (the event type is `compaction.checkpoint`, **never disguised as `message.user`**); the session header's `FormatVersion` is raised to 2 and old readers refuse to open.

## Meter and pressure

- `CharMeter`: rune counting / `CharsPerToken` (default 4), zero tokenizer dependency (CGO-free, plan9/js never locked out); exact counting is provided by a host-supplied custom `Meter`.
- `Pressure(meter, surface, threshold)`: pressure detection. The **request-level retry orchestration after triggering belongs to the assembly layer**; this package only provides detection and a manual entry point.

## The §9.1 eight-step transaction

"Eight steps" is the design doc §9.1 naming: pressure detection (step 1, `Pressure`), window selection and pre-check (step 2, `Window` + `ValidateReplace`), and the post-compaction `Flush` (step 8, caller's responsibility) produce no events; **the persisted events/actions are the 5 items below**:

`Compact` persistence order (any step failing returns immediately; original events are not deleted):

1. `compaction.started` — the transaction lock (ID + window SourceRefs)
2. Summarize (Engine call, cancellable/fallible)
3. `compaction.summarized` — records the summary model, usage, sources
4. `compaction.checkpoint` — `SurfaceReplace` replaces the window, `Replaced` complete
5. `compaction.ended` — closes out

Failure semantics: Summarize fails → started persisted, no checkpoint, ended not written — **the unclosed compaction stays visible in the log** (recovery does not back-fill ended and does not pretend completion). If the window pre-check (`session.ValidateReplace`) fails, nothing is persisted at all.

## The two Engine implementations

| Implementation | Behavior |
|---|---|
| `LLMSummarizer` | produces the summary via `llm.ChatModel`; `Model == nil` errors out, never silently; usage propagates into the `summarized` audit |
| `DeterministicSummarizer` | no-model fallback: concatenates the window text in order, result reproducible (tests and degraded scenarios) |

## ValidateReplace: the "no new orphans" rule

Replace's pairing validation is **"no new breakage", not "the window must be self-contained"** — when §9.2 pruning replaces a single result node whose call lies outside the window, the replacement node keeps the same ToolCallID, so the pairing still holds (legal). The four rules:

1. A kept call whose result falls into the deleted window while the replacement does not keep that ID → reject (dangling call)
2. A kept result whose call falls into the deleted window while the replacement does not provide one → reject (orphaned result)
3. A replacement result with no call found in the surrounding context → reject (new orphaned result)
4. A replacement call with no result to land on → reject (new dangling call)

The orchestration pre-check and session's fold-replay re-check share the same rule (fail closed).

## §9.2 Tool result pruning

```go
n, checkpoints, err := compaction.PruneResults(ctx, sess, compaction.PruneOptions{})
// 默认 Max 4000 / Head 2400 / Tail 800 rune；head + marker + tail，rune 安全不劈 UTF-8
```

Over-budget tool result nodes are checkpoint-Replaced one by one (window = a single node); structured fields (ToolCallID/IsError) are preserved; **the original text stays complete in the raw log** (the UI can expand it). A deterministic operation, idempotent.

## Tests

```bash
go test -race -count=1 ./memory/compaction/...
```
