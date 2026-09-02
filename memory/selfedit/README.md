[English](README.md) | [中文](README_zh.md)

# memory/selfedit

The P2-C self-edit memory tool group (implementation ticket #82): adapts the `memory/store` write path into three model-visible tools, **registered by explicit opt-in** (never part of any default assembly; whether to expose them to the model is the host's call).
Package docs (godoc) in `doc.go`; design source of truth [docs/design/memory-layer-research-and-v2-design.md](../../docs/design/memory-layer-research-and-v2-design.md) §12/§17; prerequisite #56 (W2 Preview already implemented).

## Integration

```go
dispose, err := selfedit.Register(scope, reg, selfedit.Options{
    Store:     memStore,
    Namespace: scope.Namespace(),           // store.MemoryScope 展开——工具唯一作用域
    OriginFn:  func() store.SourceRef {     // 当前 session 回链（实时 Seq）
        return store.SourceRef{Type: store.SourceSession, SessionID: sessID, Seq: currentSeq()}
    },
    // Taint: store.TaintUntrustedExt,      // 默认（ASI06 对位）；可信写手显式升 trusted
    // NewID:  myIDGen,                     // 默认 crypto/rand 16B hex
    // Source: "memory.selfedit",           // Registration.Source 元数据
})
```

Approval belongs to the host's HITL (`before_tool_call`): the three tools are RiskReadWrite + PreviewFn (the #56 W2 surface); this package builds no auto-approval whitelist.

## The three tools

| Tool | Model parameters (full schema) | Effect |
|---|---|---|
| `memory_put` | `kind` / `content` / `structured?` | creates a new Active memory (Confidence=1.0, auto-assigned ID, back-linked via OriginFn) |
| `memory_supersede` | `id` / `content` / `kind?` | replaces (the old item is left behind as superseded; kind defaults to the old one; structured is not inherited; next keeps the original item's namespace) |
| `memory_revoke` | `id` / `reason` | invalidates (idempotent; rejects superseded targets; the reason goes into the store audit) |

## Invariants

- **Scope anti-contamination** (the counterpart to §17.1's Letta failure mode): namespace/provenance/trust level/confidence/status/revision are all pinned by the env; the model can neither supply nor change them — scope is a storage-layer boundary, not a prompt convention.
- **Write authority rule** (re-review verdict): supersede/revoke first does `Get(env.ns, id)` (invisible = nonexistent), and additionally requires item.Namespace and env.Namespace to be **exactly equal** — the store's prefix visibility is a read rule (visible downward), while writes are pinned to env.ns: a parent-scope tool must not drill down and rewrite a child-scope item (`ErrOutsideScope`). For the host to manage child-scope memories = configure one env per target scope (composition, not delegation).
- **Conservative taint default**: writes default to `TaintUntrustedExt` (§17.7 ASI06 counterpart — self-edit is the channel through which the model re-states tool output/external content into memory, so it must not default to the same level as host-authoritative writes; `before_tool_call` approval is the promotion gate and taint is an honest data attribute; trusted writers are an explicit host override).
- **Back-link mandatory**: write SourceRefs come only from `OriginFn()`; omitting it fails Register outright — never silently degraded to provenance-less writes.
- **Write-only, no read**: there is no retrieval tool — reading belongs to `memory/assemble` (§8) / the host pipeline, avoiding "memory editing replacing answering". The path by which model writes take effect = the next Assemble / RefreshStable.
- **No physical DELETE**: only supersede (trace-preserving replacement) and revoke (idempotent invalidation); state-machine errors pass through the store sentinels unchanged (`ErrSupersedeRevoked` / `ErrRevokeSuperseded` / `ErrItemNotFound`).
- **Preview is read-only**: the three tools use opaque/write cards (the envelope #56 is frozen; no fifth kind is added); Preview never persists.

## Tests

```bash
go test -race -count=1 ./memory/selfedit/...
```
