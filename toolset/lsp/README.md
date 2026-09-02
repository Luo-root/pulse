[English](README.md) | [中文](README_zh.md)

# toolset/lsp

An optional package that attaches external language servers (gopls, typescript-language-server, pyright…) as a tool (Issue [#64](https://github.com/Luo-root/pulse/issues/64)). **Not in builtins**: it depends on external processes and requires explicit per-language configuration.

A single `lsp` tool (RiskReadonly), dispatching by op:

| op | Params | Behavior |
|---|---|---|
| `diagnostics` | `path` | After didOpen, waits within `DiagWindow` for the server push and returns that file's diagnostics (severity/location/message/source); timeouts are reported as they are |
| `definition` | `path`,`line`,`column` | Symbol definition location `path:line:col` |
| `references` | `path`,`line`,`column`,`include_declaration?` | List of references |
| `hover` | `path`,`line`,`column` | Type/signature documentation (markdown preferred) |

## Getting started

```go
dispose, err := lsp.Register(host, reg, lsp.Options{
    Root: "/path/to/workspace",
    Servers: map[string]string{
        ".go": "gopls",
        ".ts": "typescript-language-server --stdio",
    },
    // Timeout: 30s；DiagWindow: 3s
})
defer dispose()
```

## Contract highlights

- **Lazy lifecycle**: the first call spawns by extension (`strings.Fields` tokenization, quoted paths unsupported) → `initialize`/`initialized` → `didOpen`; the process stays resident until dispose. A startup/handshake failure only errors for that language and is retried on the next call; it does not blow up Register.
- **Two cleanup paths**: both explicit `dispose()` and scope Dispose (an independent Effect) run `shutdown → exit →` tree-kill (Windows `taskkill /T /F`; Unix process-group SIGKILL).
- **Position convention**: `line`/`column` are 0-based; `column` is the LSP-native **UTF-16 code units** (identical to character counts in ASCII scenarios).
- **Ops are read-only, with two-way content sync**: no write-class ops such as formatting / rename / codeAction; but every call syncs the latest on-disk content to the server (first call `didOpen`, afterwards a full `didChange` on content-hash change with version++) — after `edit`/`apply_patch`, calling `diagnostics` yields the latest diagnostics.
- **Zero new dependencies**: hand-written JSON-RPC stdio framing (Content-Length header); no `golang.org/x/tools`.
- **conn seam**: the package-level `spawnServer` variable lets tests inject an in-memory fake that pins the protocol order (initialize → initialized → didOpen → request).
- Source: `lsp.lsp`; unconfigured extensions / failed server startups produce explicit errors (no silent empty results).

## Deliberately out of scope

- Write-class LSP ops (formatting / rename / codeAction)
- completion / code lens / a diagnostic subscription event surface
- Multi-root workspaces; automatic server discovery
- The `golang.org/x/tools` dependency

## Tests

```bash
go test -race ./toolset/lsp/...
```
