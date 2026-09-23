[English](README.md) | [中文](README_zh.md)

# toolset

The reversible tool registration surface of pulse v2 (Accepted: [`docs/design/toolset-v1-design.md`](../docs/design/toolset-v1-design.md)).

It gives local tools and MCP sources ([`toolset/mcp`](mcp/README.md)) a single unified `pulse.tools` Registry; loop still only sees `loop.ToolSet`. Approval stays on the request-scoped `before_tool_call`; **no** separate `before_execute` bus is added.

## Deliberately out of scope

| Not doing | Belongs to |
|---|---|
| Model turns / HITL UI | `loop` + the assembly layer |
| Guaranteed SSE / Streamable HTTP | `ConnectSDK` accepts any Transport; tests default to InMemory + a stdio factory is provided |
| Skills loader | Landed: [`skills/`](../skills/README.md) (agentskills.io); Skill ≠ Tool / ≠ Source |
| Permissions inside `llm.ToolDef` | Risk/Source are host-side metadata |
| Standalone script sandbox | Scripts must map to registered tools in order to run |

## Getting started

```go
host := kernel.New()
defer host.Dispose()
_ = kernel.Use(host, toolset.Plugin())

reg, _ := kernel.Get(host, toolset.ServiceKey)
_, err := reg.Register(host, toolset.Registration{
    Def: llm.ToolDef{
        Name:        "lookup",
        Description: "查找本地知识",
        Parameters:  json.RawMessage(`{"type":"object"}`),
    },
    Fn:     lookupFn,
    Source: "local.lookup",
    Risk:   toolset.RiskReadonly, // 必填；零值拒绝
})
if err != nil {
    panic(err)
}

agent, err := loop.NewAgent(model, "react",
    loop.WithToolSet(reg.AsToolSet()),
    loop.WithEventScope(reqScope),
)
```

## Contract highlights

- **Primary key** = `Def.Name` (globally flat-unique); conflicts fail, no silent renaming.
- **`Def.Parameters`** (the model-visible parameter schema) must be **valid JSON** when non-empty — rejected at registration (`parameters is not valid JSON`), never deferred to persistence or request building. The criterion stops at "valid JSON": "must be a JSON **object**" is the adapter's call at request-build time (`ErrBadRequest`), and JSON Schema's boolean form (`true`) is legal on its own. A broken schema left in place kills the whole round at the `request.header` write (fail closed), with the error pointing at the session and the model never called once.
- **`DisposeSource(source)`** revokes in bulk by source; guessing the source from a Name prefix is forbidden.
- **`AsToolSet()`** is a live view; the Definitions snapshot for a given turn is taken once by loop at the start of Run.
- **`LookupMeta`** lets HITL/policies look up Source/Risk; a miss should fail closed.
- **`PreviewFn` / `LookupPreview` / `Preview`**: optional read-only pre-execution card (W2). loop does not alter it; HITL does its own Lookup. No PreviewFn = empty preview; the human is still asked according to Risk. The identity fields of `Preview()` (Tool/Source/Risk) come from the **same snapshot** as the PreviewFn — a concurrent revoke can never yield a card that is `ok=true` with an empty Source and a zero Risk.
- **Card field semantics**: `FileChange.Added/Removed` always count by the **positional interval outside the common prefix/suffix** (large files use the same convention, only the diff text is omitted and flagged `Truncated`) — multiset counting reports a whole-file rotation as 0, which makes "escalate approval on change size" policies fail open; `NetworkChange.HostClass` is one of `public|private|metadata|unknown`, decided from the **literal** address, with hostnames always `unknown` (no DNS in preview: nothing may produce external traffic before a human approves). It is a hint, not a security boundary.
- **Dependencies**: `toolset` → `loop`; `loop` does not import `toolset`. `MemToolSet` remains available for unit tests without a kernel.

**Migration**: a malformed `Def.Parameters` (invalid JSON) used to register fine; from the next minor on **`Register` rejects it outright** (`toolset: tool "x": parameters is not valid JSON`) — under the official assembly it was guaranteed to blow up anyway, only at the `request.header` write and with the error pointing at the session. Correctly written schemas are unaffected; sources that assemble a schema from external bytes (a host-written MCP `Client` adapter, say) should validate inside their own `ListTools` or produce it with `json.Marshal`.

## MCP source

Subpackage [`mcp`](mcp/README.md): the `Client` abstraction + `Source`/`Plugin` + the official go-sdk adapter (`ConnectSDK` / `ConnectCommand`). Skills are **not** Source plugins.

## Builtin tools

Subpackage [`builtins`](builtins/README.md): `read`/`ls`/`glob`/`grep`/`exec`/`edit`/`write`/`apply_patch`/`web_fetch`/`web_search`/`question`/`job_output`/`job_kill`, with `builtins.Register(scope, reg, Options{Root:...})`.

## LSP tools

Subpackage [`lsp`](lsp/README.md): an optional package (Issue [#64](https://github.com/Luo-root/pulse/issues/64)). It attaches external language servers as a read-only `lsp` tool (`diagnostics` / `definition` / `references` / `hover`); `Servers` explicitly maps extensions → launch commands, processes start lazily, and dispose / scope Dispose tree-kill is the backstop. Hand-written JSON-RPC stdio framing, zero new dependencies.

## Tests

```bash
go test -race ./toolset/...
```
