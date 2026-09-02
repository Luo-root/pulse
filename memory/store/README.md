[English](README.md) | [中文](README_zh.md)

# memory/store

The P2-C long-term memory canonical store: Put/Get/Search/Supersede/Revoke of `MemoryItem`.
Package docs (godoc) in `doc.go`; design source of truth [docs/design/memory-layer-research-and-v2-design.md](../../docs/design/memory-layer-research-and-v2-design.md) §6.5/§10/§13.1; implementation ticket #76 (C1). SQLite + FTS landed in C2, the Context Assembler in C3.

## Interface surface

```go
store := store.NewMemoryStore() // C1：内存实现
store, err := store.NewSQLiteStore(ctx, "file:/path/to/memory.db") // C2：SQLite + FTS5（CGO-free）
scope := store.MemoryScope{TenantID: "acme", UserID: "u1"}

it, err := store.Put(ctx, item, store.PutMemoryOptions{ExpectedRevision: 0}) // 0=新建，>0=CAS 更新
got, err := store.Get(ctx, scope.Namespace(), "d1")
hits, err := store.Search(ctx, store.MemoryQuery{Namespace: scope.Namespace(), Query: "toml"})
next, err := store.Supersede(ctx, "d1", newItem)
err := store.Revoke(ctx, "d1", "policy changed")
audit := s.AuditLog() // 实现特有：Supersede/Revoke 审计（reason 落点）
```

- `Revision / KnownAt / CreatedAt / UpdatedAt` are assigned by the store and callers do not set them; on update, immutable fields are preserved and `Revision` advances.
- `ExpectedRevision`: 0 = create (ID collision → `ErrItemExists`); >0 = CAS (mismatch → `ErrRevisionConflict` and the data is unchanged).

## Namespace visibility (the canonical key)

- `MemoryScope` is only a helper: it expands in fixed order into a self-describing hierarchy `tenant:<id>` → `user:<id>` → `project:<id>` → `workspace:<id>` → `agent:<id>` (empty fields skipped); new dimensions simply add a level.
- Visibility = **prefix matching**: a query namespace sees an item if it is a prefix of the item's namespace — a parent scope can read its child scopes, **sibling scopes never see each other** (the acceptance core); an empty query = global.

## State machine (physical DELETE forbidden)

```
Active ──Supersede──▶ 新 item（Active）；旧 item ──▶ Superseded
Active/Pending ──Revoke──▶ Revoked（终态；reason 走 store 审计）
```

- No Delete on the interface: Superseded/Revoked items remain queryable forever (`IncludeInactive`) — the precondition for audit and rollback.
- **Status transitions go only through Supersede/Revoke**: Put updates must not change Status (`ErrStatusTransition`) — otherwise active→pending bypasses the P2-D taint gate and active→superseded bypasses the supersede chain.
- Revoked is terminal: it can no longer be Superseded; Revoke is idempotent; Revoking a Superseded item → `ErrRevokeSuperseded` (wrong target — find the effective version first).
- Where `StatusPending` is produced and promoted is not this package's business: this package only defines its storage semantics — the write path is `candidate.Extract` (automatic distillation), Pending is **invisible** to the default Search (Active only), and promotion goes through Supersede, not Put (the host stamps); for the full semantics see the [candidate README](../candidate/README.md) and the [root README](../README.md) "State machine overview".

## Write validation (fail closed)

- At least one `SourceRefs` entry: session sources must carry `SessionID`+`Seq` (able to locate the canonical event); manual/external must carry `Ref` — **model inferences without provenance never enter active memory** (§10.2).
- `StatusActive` requires an explicit `Confidence > 0` (P2-C has no scoring producer, so ranking must not depend on values nobody writes; "default 1.0" is a documented suggestion).
- Kind/Taint are open strings (host-defined values allowed); an unknown Status is rejected; `Structured` must be valid JSON; `ValidUntil` must not precede `ValidFrom`.
- Audit and isolation first: the `Revoke` reason does not go into `MemoryItem` (§6.5 design freeze); it lands on the store audit surface — in C2, the SQLite audit table.

## Search semantics

- Namespace prefix + `Kinds` filter + keyword (**matches Content only**, case-insensitive substring; whether Structured joins the search domain is decided by C2 FTS) + status switch (default Active only).
- Ordering = UpdatedAt descending + ID tiebreak (stable, independent of Confidence — values nobody writes do not participate in ranking); `Limit` is a hard cap (keyset pagination is a C2 decision).
- No hits returns an empty slice, **never fabricated**.

## Error quick reference

| Sentinel | Meaning |
|---|---|
| `ErrItemExists` / `ErrItemNotFound` | Put create hits an existing ID / the Get, Supersede, Revoke target does not exist (including namespaces that do not see each other) |
| `ErrRevisionConflict` | CAS failed, the data is unchanged |
| `ErrInvalidItem` / `ErrInvalidQuery` | item validation failed (shape/provenance/confidence) / illegal Search conditions |
| `ErrSupersedeRevoked` / `ErrSupersedeSelf` | Superseding a Revoked item (terminal) / next.ID equals oldID |
| `ErrRevokeSuperseded` / `ErrStatusTransition` | Revoking a Superseded item / a Put update attempting to change Status |

## Tests

```bash
go test -race -count=1 ./memory/store/...
```

## SQLite backend (C2, #78)

- **CGO-free**: `modernc.org/sqlite` (FTS5 enabled by default); `sqlite.go`/`sqlite_test.go` carry the `//go:build !plan9 && !js` build constraint — on plan9/js the SQLite backend is absent but **the store main package still compiles** (core is never locked out; the memory implementation works).
- **On disk**: the `memory_items` table (the namespace joined with `\x1f` into `ns_key`, so prefix matching is safe at element boundaries) + an **FTS5 external-content table** (`content=`, triggers keep it in sync with inserts/updates/deletes) + the `memory_audit` table (where reasons land).
- **Schema version**: `PRAGMA user_version`; incompatible versions refuse to load (no migration guessing); `NewSQLiteStore` creates tables automatically.
- **Search shares the memory implementation's semantics** (escaped substring LIKE, status filter, UpdatedAt descending + ID tiebreak, hard `Limit` cap); **case folding is ASCII-only by unified convention** (SQLite `lower()` and the Go-side `asciiFold` agree — accented and other non-ASCII uppercase is not folded, so the two implementations are interchangeable without surprises); **FTS goes through the implementation-specific `SearchFTS(ctx, ns, match, limit)`** (token-prefix form like `"t"* AND "c"*`; the C3 Assembler's recall entry point, used via type assertion, not part of the §7.1 interface surface).
- **Concurrency trade-off**: `MaxOpenConns(1)` + `busy_timeout` + `_txlock=immediate` (BeginTx issues BEGIN IMMEDIATE right away, eliminating the deferred-upgrade window) — the most robust correctness under SQLite's write lock; single-machine throughput is not a concern; WAL and other tuning are up to the host's DSN.
- **Supersede / Revoke are both transactional writes**: the item write and the audit insert share one transaction, so a mid-way crash leaves no half-state; an INSERT PK conflict on the create path maps to `ErrItemExists` (the error funnel for concurrent-create TOCTOU).
- **Both Supersede writes happen inside a `BEGIN IMMEDIATE` transaction** — the supersede chain never breaks.
