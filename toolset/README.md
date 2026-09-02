[English](README.md) | [中文](README_zh.md)

# toolset

The reversible tool registration surface of pulse v2 (Accepted: [`docs/design/toolset-v1-design.md`](../docs/design/toolset-v1-design.md)).

It gives local tools and future MCP sources a single unified `pulse.tools` Registry; loop still only sees `loop.ToolSet`. Approval stays on the request-scoped `before_tool_call`; **no** separate `before_execute` bus is added.

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

agent, err := loop.NewAgent(model,
    loop.WithToolSet(reg.AsToolSet()),
    loop.WithEventScope(reqScope),
)
```

## Contract highlights

- **Primary key** = `Def.Name` (globally flat-unique); conflicts fail, no silent renaming.
- **`DisposeSource(source)`** revokes in bulk by source; guessing the source from a Name prefix is forbidden.
- **`AsToolSet()`** is a live view; the Definitions snapshot for a given turn is taken once by loop at the start of Run.
- **`LookupMeta`** lets HITL/policies look up Source/Risk; a miss should fail closed.
- **`PreviewFn` / `LookupPreview` / `Preview`**: optional read-only pre-execution card (W2). loop does not alter it; HITL does its own Lookup. No PreviewFn = empty preview; the human is still asked according to Risk.
- **Dependencies**: `toolset` → `loop`; `loop` does not import `toolset`. `MemToolSet` remains available for unit tests without a kernel.

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
