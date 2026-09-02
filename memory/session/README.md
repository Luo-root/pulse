[English](README.md) | [中文](README_zh.md)

# memory/session

The P2 memory layer's session event log: the append-only log is the single source of truth and `Surface() []*llm.Message` is only a projection.
Package docs (godoc) in `doc.go`; design source of truth [docs/design/memory-layer-research-and-v2-design.md](../../docs/design/memory-layer-research-and-v2-design.md) §6/§7/§9; implementation tickets #68 (A1), #70 (A2), #73 (P2-B Replace fold).

## Interface surface

```go
store := session.NewMemoryStore()               // A1：内存，进程内单写者
store, err := session.NewJSONLStore(root)       // A2：JSONL + blobs + 文件锁

sess, err := store.Create(ctx, session.SessionHeader{})
sess, err := store.Open(ctx, id)                // 非 live 打开 = 冷恢复入口
page, next, err := store.List(ctx, session.SessionFilter{After: cursor})
err := store.Delete(ctx, id)

sess.Header()                                   // SessionHeader 快照
env, err := sess.Append(ctx, session.EventDraft{Type, Data, Surface, Ignorable})
events, err := sess.Events(ctx, fromSeq)        // Seq >= fromSeq 的拷贝
msgs, err := sess.Surface(ctx)                  // []*llm.Message（不含 system）
child, err := sess.Fork(ctx, atSeq)             // 切 tool 组中间 → 拒绝
err := sess.Flush(ctx)                          // 内存版 no-op；JSONL 版 fsync
reg := sess.Registry()                          // 事件 codec 环境（FoldTrace 用）
```

- `Seq`/`Time` are assigned by the store; `Append` is **not idempotent** — after a Flush failure, do not replay the same batch of events verbatim.
- `Open` is cold recovery (there is no separate Recover method): unclosed turn/step and unpaired ToolCall get synthesized closure events that are **genuinely written back to the log** before folding; live sessions are never cold-patched; recovery is idempotent.
- The JSONL implementation additionally provides `Close() error` (use via type assertion): releases the file lock and handle; idempotent.

## The two backends

| | MemoryStore | JSONLStore |
|---|---|---|
| Flush | successful no-op (semantic placeholder) | `f.Sync()`; a crash is only guaranteed up to the Flush point |
| Single writer | in-process lock (concurrent Create on the same ID: exactly one wins) | in-process lock + file lock (`O_EXCL`, stale preemption, `Flush` doubles as heartbeat) |
| Torn-write recovery | not applicable (no file) | a trailing line without a newline is dropped and physically truncated; mid-file bad line / seq chain break → `ErrCorruptLog` |
| Persisted header | — | synchronously rewritten after writing `compaction.checkpoint` (FormatVersion raised to 2) |
| List | in-memory sort + cursor | scans `{root}/*/header.json` + cursor |

JSONL on-disk layout: `{root}/{sessionID}/header.json` + `events.jsonl` (one envelope per line) + `blobs/{sha256}` (inline byte overflow above 32KiB, content-addressed dedup, sha self-verification, missing blob = load error) + `lock`. **JSONL is plaintext: the file is the secret surface, paths are host-owned.** The `blob:` URL prefix is reserved by this package; host-provided `blob:` URLs must switch schemes.

## Event classification and adjudication

Each `EventType` binds payload validation and classification in the `Registry`; the adjudication table (§6.3 review verdict):

| Case | Behavior |
|---|---|
| Known Required (lifecycle/message/tool/compaction.*) | never skipped (envelope flag ignored) |
| Known Ignorable (chunk / request.header / route / usage) | skippable; fold does not read it |
| Unknown extension + `Ignorable=true` | written and skipped by fold |
| Unknown + flag defaulting false | **rejected at Append** (fail closed) |

Ignorable ≠ optional to record: `request.header` must still be emitted by the writer (the system + ToolDef + model trio); the writer is the assembly-layer bridge from session→loop.

## Common errors

| Sentinel | Meaning |
|---|---|
| `ErrSessionExists` / `ErrSessionNotFound` | Create hits an existing ID (second writer rejected) / Open, Delete on a nonexistent session |
| `ErrWriterBusy` | file lock held (fail-fast; preemptible after the stale threshold) |
| `ErrUnknownEvent` / `ErrUnknownRequired` | writing an unknown required event / the log contains an unknown required event at recovery |
| `ErrSurfaceNotAllowed` / `ErrReplaceNotSupported` / `ErrReplaceRange` | non-surface type carrying Surface / Replace type not registered / window reversed, out of range, or creating pairing orphans |
| `ErrCorruptLog` | persisted log corrupt (mid-file bad line, seq chain break, blob checksum mismatch) |
| `ErrForkSplitToolGroup` / `ErrForkBadAt` | Fork splits a tool group in the middle / the cut point is out of range |
| `ErrFormatVersion` | header version incompatible (only 1 and 2 accepted, no migration guessing) |
| `ErrSessionClosed` / `ErrInvalidSessionID` / `ErrCursorStale` / `ErrDeleted` | write after Close / invalid ID (must match `[A-Za-z0-9_-]{1,128}`) / stale List cursor / write to a deleted session |

## Tests

```bash
go test -race -count=1 ./memory/session/...
```
