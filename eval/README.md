[English](README.md) | [中文](README_zh.md)

# eval: Engineering-Capability Property Test Suite

This package turns the four engineering-capability themes from [`docs/design/agent-framework-evaluation.md`](../docs/design/agent-framework-evaluation.md) §5.2 into **executable assertions**: randomized input sequences + invariant checks (property tests), reproducible via fixed seeds, zero new dependencies, one command: `go test -race ./eval/`.

Positioning: this is **step two** of the three-step evaluation strategy — the "engineering reliability checklist". Step one (infrastructure benchmarks) and step three (GAIA / tau-bench integration) move in separate tickets. Division of labor with each package's `*_test.go`: in-package tests cover fixed cases; this package covers **invariants** (properties that must hold for any valid input). Complementary, not duplicated.

## Run

```powershell
go test -race -count=1 ./eval/    # full suite (~10s)
$env:EVAL_SEED = "12345"          # change seed to explore new paths (default seeds are fixed and identical in CI and locally)
go test -race -run TestPropertySessionTornRecovery -v ./eval/
```

Every failure message carries `seed=<N>`: set `EVAL_SEED` to that value to replay.

## Checklist

Each entry = invariant + corresponding test + competitor landscape (research conclusions as of 2026-09, see §4.2 of the evaluation survey).

### 1. Crash recovery (memory/session) — `TestPropertySessionTornRecovery`

| # | Invariant | Description |
|---|---|---|
| P1 | Torn-write detection | A JSONL truncated at any byte offset is recognized on Open: the valid prefix is kept intact, the damaged tail line dropped, Open never fails |
| P2 | Valid-prefix preservation | After a round-boundary cut the surface matches the baseline entry-by-entry, node count exactly equals retained event count (zero synthetic-event leakage) |
| P3 | fold validity | After any cut the Surface still folds — unclosed tool groups are closed by synthetic `interrupted` results, no orphans |
| P4 | Resumable | Appending new events after recovery succeeds and is reflected in the surface |
| P5 | Recovery idempotence | A second Open writes synthetic events exactly once (event count unchanged) |

Competitor landscape: event-sourced sessions + torn-write recovery + synthetic event write-back have no counterpart among Go frameworks (nor a public equivalent property suite in the Python camp).

### 2. Compaction transaction & token efficiency (memory/compaction) — `TestPropertyCompactionTransaction` / `TestPropertyCompactionShrinks`

| # | Invariant | Description |
|---|---|---|
| P1 | All-or-nothing | For any valid window, Compact either commits the transaction or fails with zero writes — never a partial state |
| P2 | raw log append-only | On success the original events are preserved byte-for-byte and exactly four events are appended: started/summarized/checkpoint/ended |
| P3 | Report cross-checked against the log | Replaced == window source seqs; CheckpointSeq == checkpoint event Seq |
| P4 | Version & fold | The post-compaction surface folds; FormatVersion is bumped |
| P5 | Token shrink | Under a reasonable summary engine, full-window compaction strictly reduces surface tokens and converges to a single node |

Competitor landscape: compaction is generally a "replace the list" with no transactional semantics; checkpoint transactions + immutable raw log + zero-write rejection are Pulse-specific.

### 3. Memory governance (memory/store + memory/candidate) — `TestPropertyStoreLifecycleInvariants` / `TestPropertyCandidateApprovalInvariants`

| # | Invariant | Description |
|---|---|---|
| G1 | No physical deletion | Under random operation sequences, every previously written item remains Gettable (Superseded / Revoked are statuses, not tombstone deletions) |
| G2 | Only Active is recallable | Search by default never returns a non-Active item |
| G3 | Legal state machine | Supersede: old → Superseded, new → Active; Revoke → Revoked |
| C1 | Pending invisible | Candidates are invisible to default Search before approval |
| C2 | Approval never changes taint | Approve promotes to Active + Confidence=1.0 while Taint is inherited unchanged (approval is a promotion gate; taint is a data property) |
| C3 | Reject is permanently invisible | Rejection = Revoke with an audit trail |
| C4 | Closed state machine | Pending drains after approval; repeated Approve/Reject → `ErrNotPending` |
| C5 | Scope pollution guard | A parent-scope pipeline approving a child-scope candidate → `ErrOutsideScope` |

Competitor landscape: Supersede/Revoke lifecycles + taint levels + an approval surface have no equivalent in current Go/Python framework memory implementations (aligned with the survey in `memory-layer-research-and-v2-design.md` §17).

### 4. Rejection semantics (loop) — `TestPropertyToolRejectionSemantics`

| # | Invariant | Description |
|---|---|---|
| R1 | Rejected never executes | A tool rejected at `before_tool_call` has zero handler side effects |
| R2 | IsError round-trip | Every rejected call produces a tool result containing the rejection reason — the model can self-correct |
| R3 | Rejection is not failure | Rejection ends the turn as `completed`, not `error` (a ruling is information, not a crash) |
| R4 | Allow executes exactly once | The Waterfall pass-through `next` path neither duplicates nor drops execution |

Competitor landscape: HITL approval generally stops at "interrupt and wait for input"; "rejection as a first-class returned result + zero side effects" has no public property-suite equivalent.

### 5. Wiring samples for the retrieval chain (memory/index + memory/assemble + memory/compaction) — `TestPropertyHybridRetrievalChain` / `TestPropertyPressureDrivesCompaction`

Not a fifth §5.2 theme but a **regression lock** for the D2 chain (audit ticket #224, item 3): the `Semantic` seam and `Pressure` previously had no executable consumer anywhere in the repo, so a signature change produced no production compile error — these samples turn "the seams inside the library really connect" into runnable, replayable assertions using public APIs only.

| # | Invariant | Description |
|---|---|---|
| H1 | Hybrid recall, two-way control | With one store / budget / query, a real `MemIndex` wired into the assembler through the `Semantic` seam lets a lexically-disjoint yet semantically-near item into the product; with the seam off (nil) it does not appear, while the keyword hit appears in both runs — the vector path carries it |
| H2 | Fusion scores are consumed | With the seam on, product order is fusion-score descending (double-path keyword > semantic-only > orthogonal decoy) — splitting `[]ScoredHit` into items/scores wrongly or a shape mismatch scrambles the order |
| H3 | Namespace pass-through | A semantically-near item in another namespace never enters the product (the seam must hand `AssembleInput.Namespace` to the filter-before-recall path) |
| P6 | Pressure gate, two-way | `Pressure` is strictly greater: at the threshold → false, and the host compacts nothing (zero new session events) |
| P7 | Compaction really relieves pressure | Above the threshold the two verdicts disagree, `Compact` really runs (+4 events, full-window `Replaced`), surface nodes and tokens both drop, and re-checking the same threshold falls back to false |

Competitor landscape: hybrid recall (keyword ∪ semantic with fusion ranking) and pressure-driven compaction are usually separate pieces glued together by an adapter layer; "the seams connect" is rarely asserted in CI at all.

## Reproducibility

- All random sequences are driven by `math/rand/v2` PCG, seeded from a fixed base + test-name hash (identical in CI and locally);
- The `EVAL_SEED` environment variable overrides the seed; failure messages carry `seed=<N>` and the iteration number for direct replay;
- This package imports only public APIs of the packages under test — if a property test catches a real bug, file a separate fix ticket instead of loosening the assertion here.
