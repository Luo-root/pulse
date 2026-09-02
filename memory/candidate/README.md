[English](README.md) | [中文](README_zh.md)

# memory/candidate

The P2-D3 candidate memory pipeline (Issue #90): candidate extractor (host-injected seam) → dedup → Pending storage → approval promotion/rejection. **This package is off by default** — no background loop, no automatic triggering (invocation timing belongs to the host; background reflection belongs to D4).

Design stance (§17 resolution 7 / ASI06): automatic memory only writes candidates, which become active items only after passing the provenance/taint/duplication/approval policies; anything that has not passed approval must not be promoted to active. Package documentation (godoc) is in `doc.go`.

## Wiring

```go
p, err := candidate.New(candidate.Options{
    Store:     memStore,
    Extractor: myLLMExtractor,            // 必填：LLM 提取协议归宿主 prompt（seam）
    Namespace: scope.Namespace(),         // 必填：候选作用域（scope 防污染同 selfedit）
    OriginFn:  func() store.SourceRef { … }, // 必填：当前 session 回链
    // Taint: store.TaintUntrustedExt,    // 默认（ASI06：自动提炼自工具/外部内容）
})

stored, report, err := p.Extract(ctx, surface) // 会话末/每 N 轮由宿主调
// report = {Extracted, Stored, Duplicates, Invalid}——可解释计数
pending, err := p.Pending(ctx)                 // 宿主审批面列表
active, err := p.Approve(ctx, pending[0].ID)   // 晋升
err = p.Reject(ctx, pending[0].ID, "noisy")    // 否决（reason 落审计）
```

## Key semantics

- **Approval = the existing state machine, zero changes to the store contract**: approve = `Supersede` (the old candidate stays behind as Superseded, the approved version gets a new ID as Active, `Confidence=1.0` i.e. host endorsement, SourceRefs inherited plus a manual-approval marker — the approval action is explicitly distinguishable in provenance); reject = `Revoke`. Anything not Pending returns `ErrNotPending` (fail closed).
- **Approval scope = exact namespace equality** (same standard as selfedit write permission): `Pending` does not list out-of-scope candidates; `Approve`/`Reject` return `ErrOutsideScope` for out-of-scope items (a parent scope must not reach down into child-scope candidates).
- **Invisibility for free**: Pending candidates are invisible to `store.Search` (Active only by default) — unapproved items never enter assemble/selfedit context; they appear naturally once approved.
- **Minimized model parameters**: only Kind/Content/Structured are taken from items returned by the extractor; namespace/status/taint/source/ID are pinned by the Pipeline.
- **Gatekeeping (ASI06)**: candidates default to `TaintUntrustedExt` (overridable); SourceRefs force the OriginFn session backlink; approval promotion **does not change taint** (approval is the promotion gate, taint is a data property).
- **Dedup v1 conservative rule**: after normalization (lowercase + whitespace tightening), if an existing item's Content contains the candidate → discard (substring redundancy counts as duplicate, **supersets are not blocked** — supersets belong to the Supersede revision semantics); the decision is made on an **in-memory double normalization** (the same rule on both the existing and candidate sides) — the store's ASCII folding does not tighten whitespace, so a coarse pre-filter query would miss dirty data on the existing side; vector-similarity dedup is not done (threshold semantics undecided, follow-up ticket).
- **Explainability**: the `Report{Extracted, Stored, Duplicates, Invalid}` counters — silent drops are forbidden; a dedup query failure aborts the batch (on store failure, prefer failing and letting the host retry).

## Metrics surface (D4)

`Pipeline.Metrics()` returns a snapshot of cumulative action counters (atomic, -race safe): `{Extracted, Stored, Duplicates, Invalid, Approved, Rejected, RejectedUntrusted}` — the data source for the extraction/approval/revocation/taint-rejection rates (rate computation belongs to the host or the presentation layer). Counters accumulate only when an action **fully succeeds** (batches interrupted by an error are not counted — after a successful retry, one full round is counted). `RejectedUntrusted` is only counted for the `TaintUntrustedExternal` tier (a rejected user-supplied item does not count as evidence for the external-taint gate; ticket #92 tightened this rule).

In-batch duplicates are deduped as well: the decision set = the normalized snapshot of existing items + **candidates stored this round** (appended as they are stored) — the snapshot does not include this round's writes; without the latter, in-batch duplicates would slip through.

The other two of the six D4 metric snapshots: `reflection.Metrics` (running token-cost counters) + `index.Counted` (recall hits).

## Error quick reference

| Sentinel | Meaning |
|---|---|
| `ErrNotPending` | The target item is not Pending (Active/Superseded/Revoked cannot be approved again — fail closed) |
| `ErrOutsideScope` | The item namespace is not exactly equal to the Pipeline scope (a parent scope must not reach down into child-scope candidates — same standard as selfedit write permission) |

Store-side sentinels (`ErrItemNotFound` / `ErrItemExists` / `ErrRevisionConflict` / `ErrStatusTransition` etc.) are passed through as-is.

## Tests

```bash
go test -race -count=1 ./memory/candidate/...
```
