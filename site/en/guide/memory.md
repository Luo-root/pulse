# Memory layer

`memory/` is the P2 memory & session layer: nine sub-packages covering the full path from turn logs to long-term memory. Core invariant: **model-visible means logged** — everything the model sees lands in an append-only session log.

## Overview

| Sub-package | Responsibility | Phase |
|---|---|---|
| `memory/session` | Session core: event envelopes, codec registry, surface fold, JSONL store + blobs + file lock | P2-A |
| `memory/compaction` | Token metering + §9.1 eight-step transactional compaction + pruning | P2-B |
| `memory/store` | Long-term store: namespace isolation + Supersede/Revoke + CAS; SQLite/FTS5 backend (build-tag isolated) | P2-C |
| `memory/assemble` | Context Assembler: per-class budgets + stable snapshot + citation templates + hybrid ranking | P2-C3 |
| `memory/selfedit` | Self-edit memory tools (put/supersede/revoke), explicit opt-in | P2-C4 |
| `memory/index` | Derived vector index: EmbeddingProvider seam + filter-then-recall + async queue | P2-D1 |
| `memory/index/openai` | OpenAI-compatible embeddings adapter (thin SDK wrapper) | P2-D1.5 |
| `memory/candidate` | Candidate pipeline: extractor → dedup → pending-approval state machine | P2-D3 |
| `memory/reflection` | Configurable background reflection: budget truncation + orchestrated extraction, off by default | P2-D4 |

## Design stance

- **No physical DELETE**: long-term memory is only Superseded / Revoked — the audit chain never breaks;
- **KnownAt ingestion time**: distinguishes "when it happened" from "when the system knew it"; retrieval filters with bi-temporal semantics;
- **Progressive disclosure**: stable memory injection uses "small resident summary budget + on-demand full-text retrieval"; full injection is forbidden;
- **Management belongs to the host**: self-edit tools are explicit opt-in and candidate adoption runs through approval (aligned with the HITL stance) — not model-autonomous;
- **Graph backends stay out of core**: only a provider seam is kept.

## Sessions

A session is an event-sourced log: an append-only `EventEnvelope` stream + type-registered codecs + a fold projection. Key semantics:

- A complete line (with `\n`) = successful append; torn lines are rebuilt on cold recovery;
- Open performs cold recovery: synthesized events are written back to the log, keeping replay consistent;
- Pairing keys = `PartToolCall` on the assistant message (tool calls pair with results);
- Single writer: in-process lock + file lock `O_EXCL` (staleness via mtime, Flush doubles as heartbeat);
- Payloads >32KiB spill to blob storage with sha self-verification.

## Compaction

`CharMeter` meters → threshold triggers the §9.1 eight-step transaction: started → summary → checkpoint write (FormatVersion bump) → ended. Checkpoint fold Role is `user` (never disguised as system), and pruning validates by the "no new orphans" four rules — pre-check and fold replay share the same accounting.

## Context assembly

Per-class budgets (system / memory / history / tools account separately) + stable snapshot + citation templates; retrieval results are ranked with D2 hybrid fusion (the semantic channel goes through a function seam, never importing the index package).

## Retrieval & reflection (index / candidate / reflection)

- **index**: filter first (scope / namespace / budget), then vector recall; an async queue decouples writes; EmbeddingProvider is a seam — the in-memory implementation and the OpenAI adapter are interchangeable;
- **candidate**: memory candidates flow Extract → dual-normalization dedup → Pending → human Approve (=Supersede) / Reject (=Revoke); external sources default to an untrusted-external taint;
- **reflection**: budget truncation + candidate refinement orchestration; no background loop, off by default — the host decides.

See the [memory package docs](/en/packages/memory/) (global map: data flow, dependency list, cross-package invariants, bridge points).
