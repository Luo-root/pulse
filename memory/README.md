[English](README.md) | [中文](README_zh.md)

# memory

The P2 "memory & session" layer (design source of truth: [docs/design/memory-layer-research-and-v2-design.md](../docs/design/memory-layer-research-and-v2-design.md), Accepted). This package is Pulse v2's memory infrastructure: **9 sub-packages cover the full chain "session truth → context projection → compaction governance → long-term memory → retrieval injection → automatic distillation → approval promotion"**, and all P2 tickets (phases A/B/C/D) have landed and been merged.

Each sub-package has its own `README_zh.md` / `README.md` pair (interface surface / semantics / error quick reference) and `doc.go` (godoc) — this document is the **global view**: problem inventory, end-to-end data flow, the complete dependency graph, cross-package invariants, and assembly bridge points. Read this first to build the map before diving into any sub-package.

## What problem does this layer solve

Agent memory is not a "vector database + conversation history" component; it is five kinds of data that are independent of each other and cooperate through a unified projection (design doc §0):

| Data | Where this layer lands it |
|---|---|
| Session facts (Session Journal) | `session` — append-only event log, the single source of truth for one run |
| Model context (Model Context Projection) | `assemble` — computes the token-budget-constrained request input from log/memory/retrieval; **not a persisted truth** |
| Cross-session working memory (Working/Episodic) | `session` events + `assemble` retrieval injection (episode-kind items) |
| Stable memory (Semantic/Profile) | `store` (canonical) + `index` (derived vectors) + `candidate`/`selfedit` (the two write channels) |
| Procedural memory | not in this layer — `skills/` (Skill ≠ memory item, never mixed into the facts table) |

Design doc §1.1 lists six problems that must be covered: precise recovery and debugging of multi-turn tool-calling runs; context reuse across processes, sessions, and Agents; token budget control for long sessions; isolation of the three scope kinds — user, project, Agent; auditing of run facts such as model calls, tools, approvals, and task states; and plugin-based on-demand extension of events, storage, retrieval, and distillation policies. On top of these, poisoning protection for automatic memory (ASI06, §10.2/§17.7) is the security throughline spanning the two write channels and the approval surface.

### Four iron rules (preceding all sub-package semantics)

1. **event-sourced**: the append-only log is the single source of truth and `Surface()` is only a projection; projections can be replaced or rebuilt, original events are never modified or deleted.
2. **model-visible means logged**: everything shown to the model (including closure events synthesized during crash recovery) must be genuinely written back to the log — any divergence between projection and log is a bug.
3. **compaction is a transaction, not deletion**: compaction/pruning only appends + surface replace, `Replaced` records the full provenance of the replaced window, and failures leave an audit trail instead of pretending completion.
4. **memory management authority rests with the host pipeline + approval (HITL)**: automatic memory only writes candidates (Pending); promotion to Active requires the approver's stamp; model self-editing goes through explicit opt-in tools + `before_tool_call` approval. There is no "fully automatic memory".

## End-to-end data flow (the lifecycle of one memory)

```
【运行期】
loop.Run ⇄ 装配层桥 ⇄ session.Append(事件)          ← 每轮消息/工具调用/usage 落日志
                 │
                 ▼
          session.Surface()                          ← fold 投影（checkpoint Replace 已应用）
                 │
                 ▼
          assemble.Assemble                          ← 稳定前缀(frozen) + surface 尾部
                 │                                      + 混合召回(FTS∪向量) + injected
                 ▼
              模型请求                                    ← 预算按类、诊断可解释

【治理期——长会话】
compaction.Pressure ─▶ Compact（§9.1 八步事务）       ← surface 治理，raw log 只增不减
                  └─▶ PruneResults（§9.2）            ← 超长 tool result head+marker+tail

【提炼期——会话末/每 N 轮，宿主触发】
reflection.Reflect（预算截断）─▶ candidate.Extract ─▶ Pending 入库（双归一去重；检索不可见）
                                          │
宿主审批面 ◀── candidate.Pending   ─────────┘
   ├─ Approve ─▶ store.Supersede ─▶ Active（Confidence=1.0 + 审批标记）
   └─ Reject ──▶ store.Revoke（reason 落审计）

【索引与召回】
store 写入后 ─▶ index.Upsert（异步队列；Supersede/Revoke 后 Remove）
下一轮 assemble 召回：keyword(FTS/子串) ∪ semantic(向量) —— 只有 Active 可见

【模型自编辑通道（opt-in）】
模型 ─▶ selfedit 三工具（memory_put/supersede/revoke，HITL 审批）─▶ store 直写 Active（taint 保守标记）
```

The safety difference between the two write channels is deliberate: **automatic distillation** (reflection→candidate) output is always pending review (Pending is invisible); **model self-editing** (selfedit) writes Active directly but taint defaults to `untrusted-external` and every write passes HITL approval — the former isolates through invisibility, the latter through trust marking + the approval gate.

## Sub-package overview

| Package | Ticket/Phase | One-line responsibility | Required seam (host-injected) | Default state |
|---|---|---|---|---|
| [`session`](session/README.md) | #68/#70/#73 (A1+A2+B) | append-only event log + fold projection + cold recovery; two backends, memory/JSONL | none (Registry allows event extension) | infrastructure, no switch |
| [`compaction`](compaction/README.md) | #73 (B) | token meter + §9.1 eight-step compaction transaction + §9.2 tool result pruning | `Engine` (LLM/Deterministic) | manual entry point, no automatic trigger |
| [`store`](store/README.md) | #76/#78 (C1+C2) | MemoryItem canonical store: namespace isolation + Supersede/Revoke state machine + SQLite/FTS5 | none | memory version works out of the box; SQLite enabled via DSN |
| [`assemble`](assemble/README.md) | #80/#88 (C3+D2) | context assembly: per-class budgets + stable prefix caching + §8.2 hybrid fusion ranking + citation templates | `TokenCounter` (nil estimates), `Semantic` (optional vector path) | nil seam = keyword-only still works |
| [`selfedit`](selfedit/README.md) | #82 (C4) | self-edit memory tool group (put/supersede/revoke), model-visible write path | `OriginFn`, `toolset.Registry` | **explicit opt-in registration** |
| [`index`](index/README.md) | #84/#86 (D1+D1.5) | derived vector index (disposable, rebuildable) + async queue + counting decorator; `openai/` adapter | `EmbeddingProvider` | not wired = no vector recall, no missing feature |
| [`candidate`](candidate/README.md) | #90/#91 (D3) | candidate pipeline: extractor → dual-normalization dedup → Pending → approval promotion/rejection; `Metrics()` | `Extractor`, `OriginFn` | **off by default** (no background loop) |
| [`reflection`](reflection/README.md) | #92/#93 (D4) | configurable background reflection: input budget truncation → candidate distillation → counting | none new (reuses candidate's) | **off by default** (no background loop/timer) |

The table above has 8 rows corresponding to 9 package directories (`index/openai` is folded into the index row).

Design stance: **every layer may be absent independently** — without index it is keyword-only, without candidate/reflection there is no automatic memory, without selfedit the model cannot write memory; only session+store are the indispensable foundation.

## Complete dependency graph

Actual import relations (**direct-import basis** — transitive dependencies such as `toolset → loop` and `session → kernel` are not expanded; verified package by package, 2026-08-31):

```text
memory/session        → kernel, llm
memory/store          → kernel
memory/compaction     → llm, memory/session
memory/assemble       → kernel, llm, memory/store
memory/selfedit       → kernel, llm, memory/store, toolset
memory/index          → kernel, memory/store
memory/index/openai   → memory/index, textsplit
memory/candidate      → kernel, llm, memory/store
memory/reflection     → kernel, llm, memory/candidate, memory/store
```

Internal dependency layering (arrows only point downward):

```text
【投影与治理】  compaction ──────────────▶ session
【上下文组装】  assemble ─────────────────▶ store
【模型写通道】  selfedit ─────────────────▶ store
【召回索引】    index ────────────────────▶ store        index/openai ─▶ index + textsplit
【自动提炼】    reflection ─▶ candidate ──▶ store
```

Dependency rules (review verdicts, non-negotiable):

- **kernel does not import memory, loop does not import memory** — memory is a capability seam / plugin attached to `kernel.Context` (service keys belong to the `memory/*` packages: `SessionStoreKey` / `MemoryStoreKey` / `ContextAssemblerKey` / `VectorIndexKey` / `PipelineKey` / `ReflectorKey`); the assembly layer hands `session.Surface()` to `loop.Run`.
- **store does not know index exists** (index → store is one-way) — the index is a derivative; the writer is responsible for calling `Upsert/Remove` after store writes; deleting the index loses no canonical data.
- **assemble's production path does not import index** (§17 resolution 4, four-interface decoupling) — the vector path is wired by the assembly layer through the `DefaultAssembler.Semantic` function seam; E2E stitching exists only in tests.
- **memory/* does not import observability** — observability is a side channel: components expose return values/snapshots (`ReflectionResult`, `Metrics()`, `Counted.Metrics()`), and the bridge is built by the assembly layer (same precedent as `request.usage`).
- **reflection does not import session** — the surface is fetched and fed in by the host (compaction depends on session because it must fold/write back; reflection only reads input); it also does not import compaction (its char-counting basis aligns with `CharMeter` rather than reusing the type).
- **index/openai does not import llm** — embedding is not a cross-provider stable generation semantic, so it stays out of the llm vocabulary; the SDK is the same origin as `llm/openai` but an independent thin wrapper.

## Cross-package invariants (where the iron rules land)

| Invariant | Anchor |
|---|---|
| append-only: projections can be swapped, original events are never modified or deleted | `session` (fold reads the log only; checkpoint is an appended event) |
| model-visible means logged | closure events synthesized by `session.Open` cold recovery are **written back to the log** before folding |
| compaction is a transaction, not deletion | `compaction` §9.1 eight steps (failure stops at the unclosed state); §9.2 preserves the original text fully in the raw log |
| scope is a storage-layer boundary, not a prompt convention | `store` namespace prefix visibility (parent reads child, siblings never see each other); `selfedit`/`candidate` writes and approvals require **exact namespace equality** (`ErrOutsideScope`, the parent does not drill into the child) |
| no physical DELETE | `store` has no Delete API; status transitions go only through Supersede/Revoke (Put flipping status → `ErrStatusTransition`) |
| no provenance, no active memory | `store` validates SourceRefs mandatorily; a missing `OriginFn` in `candidate`/`selfedit` fails New outright |
| unapproved never enters context | Pending is invisible to default retrieval (`store.Search` returns Active only) — the assemble/index recall paths enforce this with zero extra wiring |
| taint is a data attribute; approval is the promotion gate | `candidate` approval does not change taint; `selfedit` writes default to `TaintUntrustedExt` (ASI06 counterpart) |
| the derived index is disposable and rebuildable | `index` stores only vector copies; `Rebuild` re-embeds everything from the store; queue-full drops are counted + Rebuild as the fallback |
| reflection is off by default, with budget and audit | `reflection` has no background loop/timer; `MaxInputChars` budget gate; `ReflectionResult` + `Metrics()` audit surface |

## Host assembly bridge points (what memory does not do, and who does it)

The glue that the memory layer deliberately does not do all belongs to the assembly layer/host:

| Bridge point | Owner | Notes |
|---|---|---|
| `session.Surface()` → `loop.Run` history | assembly layer | loop does not import memory |
| `request.header` / `request.usage` event writing | assembly layer | the system+ToolDef+model trio and token usage get logged (Ignorable but must be emitted) |
| `index.VectorIndex` → `assemble.Semantic` | assembly layer | production path decoupled, see the assemble README "Wiring the vector path" |
| `index.Upsert/Remove` after store writes | assembly layer/writer | the cost of one-way imports; a missed call only affects recall (Rebuild can recover) |
| `ReflectionResult` / the three `Metrics()` → observability surface | assembly layer | memory does not import observability |
| The three LLM seams: Extractor / Summarizer / EmbeddingProvider | host | extraction protocol, summary model, and embedding routing all belong to host prompts/config |
| Approval surface UI (Pending list / Approve/Reject buttons / HITL cards) | host | packages provide synchronous APIs, not panels |

## Metrics surface (D4, ticket #92)

Six metrics → three in-place snapshots (**no separate metrics aggregation package**; rate computation belongs to the host):

| Metric | Snapshot | Counting point |
|---|---|---|
| Distillation rate | `candidate.Metrics`: Stored/Extracted | `Pipeline.Extract` |
| Approval rate | Approved/(Approved+Rejected) | `Pipeline.Approve` |
| Revocation rate | Rejected/(Approved+Rejected) | `Pipeline.Reject` (= Revoke) |
| Recall hits | `index.Counted`: Searches/Hits (hits per search; **≠ offline Recall@K evaluation**) | `Counted.Search` |
| Token cost | `reflection.Metrics`: Runs/TotalInputChars/TruncatedChars; real LLM usage goes to the host bridge | `Reflector.Reflect` |
| Poisoning rejection rate | RejectedUntrusted/Rejected (only the untrusted-external tier counts) | `Pipeline.Reject` |

Counters accumulate only when an action **fully succeeds** (batches aborted by errors are not counted); all counters are atomic and concurrency-safe under `-race` (anchored by concurrency test cases).

## Platform and build constraints

- **Pure Go, zero CGO**: every sub-package compiles under `GOOS=plan9` / `GOOS=js GOARCH=wasm` — except the SQLite backend.
- **SQLite is the only platform-restricted piece**: `store/sqlite.go` carries `//go:build !plan9 && !js`, driver `modernc.org/sqlite` (CGO-free, FTS5 built in); on plan9/js SQLite is absent but the store main package still compiles (the memory implementation works), so core is never locked out.
- **JSONL is plaintext**: the file is the secret surface; the `blob:` URL prefix is reserved by this package; the file-lock stale threshold defaults to 1h (`Flush` doubles as heartbeat) — details in the session README.
- Test matrix: `go test -race -count=1 ./textsplit/... ./memory/...` (all ten packages green is the merge gate); live smoke tests are all env-gated (`PULSE_OPENAI_*`).

## JSONL backend caveats (P2-A2)

- **Plaintext storage**: JSONL/blobs/header are all plaintext — the file is the secret surface, paths are host-owned; P2-A does no encryption.
- **The `blob:` URL prefix is reserved by this package**: overflow bytes are referenced as `URL = "blob:<sha256>"`. Host-provided `Image.URL`/`Media.URL` starting with `blob:` would be mistaken for internal references and dereferenced at load time — use a different scheme.
- **File-lock heartbeat**: `Flush` doubles as the lock heartbeat (touches the lock file's mtime). A long-lived session that Flushes periodically is never stale-preempted; a session that never Flushes and holds the lock past the threshold (default 1h, configurable via `JSONLStale`) may be taken over by another process. The lock file's handle is closed immediately after creation and the lock relies on existence only — if this ever changes to a held-handle lock, the Windows preemption path would break.
- **List and corrupt directories**: directories whose header.json cannot be parsed disappear from `List` results (same for concurrently-creating and unrelated directories); the corresponding session reports `ErrCorruptLog` only at `Open`.
- **Cross-process Delete**: under normal conditions data is never silently lost — on Unix, unlink takes effect immediately and the other process's Append returns `ErrDeleted` via a pre-write stat check (only a tiny TOCTOU race window remains between stat and Write, which the library layer cannot eliminate); on Windows an open file cannot be deleted (Go handles lack FILE_SHARE_DELETE), so `RemoveAll` deletes what it can (blobs/, header.json) and then fails, leaving a half-deleted directory — the host coordinates and retries after the other side releases its handle. Neither path ever "reports success while losing data".
- **Behavior difference from in-memory**: A2's `Open` does not cold-recover on cache hits (repeated opens in the same process) — a live session is never mistakenly back-filled; the recovery path is `Close → Open` (triggered when the on-disk log is reloaded).

## State machine overview (store, including the full Pending path)

```text
                    candidate.Extract（自动提炼）
                              │
                              ▼
                         StatusPending          ← 对检索不可见（默认只 Active）
                        /           \
        candidate.Approve           candidate.Reject
        = Supersede（宿主盖章）      = Revoke（reason 落审计）
              /                         \
             ▼                           ▼
        StatusActive ──Supersede──▶ 新 item（Active）；旧 item ──▶ Superseded
             │
        （selfedit 三工具可直写 Active——HITL 审批 + taint 保守默认）
             │
        Active/Pending ──Revoke──▶ Revoked（终态；Superseded 不可 Revoke → ErrRevokeSuperseded）
```

- Approval promotion goes through **Supersede, not Put** (`ErrStatusTransition` blocks Put from flipping status — every path that bypasses the approval surface/supersede chain is sealed off); the approved version has Confidence=1.0, inherited SourceRefs + a manual approval marker.
- Full state-machine semantics and sentinel errors: see the [store README](store/README.md); candidate-side semantics: see the [candidate README](candidate/README.md).
