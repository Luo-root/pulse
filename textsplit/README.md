[English](README.md) | [中文](README_zh.md)

# textsplit

A standalone text-chunking module (Issue #86, D1.5): splits long text into a list of Chunks carrying source offsets under a size budget. **Pure functions, zero dependencies, zero IO** — deliberately not tied to memory/index: the current consumer is the oversized-input truncation of [memory/index/openai](../memory/index/README_zh.md); the D3 candidate extractor (splitting long episodes), future document/file ingestion, and similar modules import it on demand as well.

## Interface

```go
chunks, err := textsplit.Split(text, textsplit.Options{
    MaxLen:  800,   // 单块尺寸上限（按 Size 度量），必填
    Overlap: 100,   // 相邻块重叠预算，0 = 不重叠；必须 < MaxLen
    // Size: myTokenizer, // 尺寸度量，nil = rune 计数；假定对前缀单调不减
})
// Chunk{Text string, Start, End int} —— 字节 offset
```

## Conventions

- **Byte offsets**: `text[Start:End]` always equals `Chunk.Text` (Go byte-slice semantics); rune safety is guaranteed by the split boundaries — cut points always land on rune boundaries, not guaranteed by the offset unit.
- **Separator priority**: paragraphs (consecutive blank lines) → sentence punctuation (。．！？…!?; v1 heuristic, `.` splits `3.14`) → whitespace → hard rune cut; separators are kept at the end of the left chunk; each level takes the largest cut point within budget.
- **Size injection**: nil = rune counting; exact token budgets come from the host swapping in its own meter (no tokenizer dependency, matching the CharMeter precedent).
- **Overlap**: the overlapping region takes the largest span with Size ≤ Overlap, aligned to separator boundaries; paragraphs with no usable boundary degrade to no overlap.
- **Degenerate guarantees**: a single rune over budget becomes its own chunk (progress is guaranteed, no infinite loop); empty text returns an empty slice.

## Tests

```bash
go test -race -count=1 ./textsplit/...
```
