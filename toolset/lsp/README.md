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
- **Self-healing after a server dies**: a language server exiting on its own (configuration, OOM, lock files) is normal — the moment the connection breaks the server is marked dead (a request already waiting fails immediately with `connection closed` instead of sitting until timeout; `diagnostics` reports the error too when the server dies inside its wait window, rather than the soft "0 diagnostics / may still be indexing" result), and the next call drops the cache entry → tree-kills the old process as a backstop → spawns and handshakes a fresh one. The host does not need a restart and there is no "the tool is broken" black box (#216).
- **Two cleanup paths**: both explicit `dispose()` and scope Dispose (an independent Effect) run `shutdown → exit →` tree-kill (Windows `taskkill /T /F`; Unix process-group SIGKILL).
- **Writes are bounded by the caller's ctx**: each frame write happens on its own goroutine with the **caller's ctx** as the only bound — a server that stops reading stdin fills the pipe, and an unbounded write would hang forever. `Options.Timeout` covers the whole call (including the `didOpen`/`didChange` sync frames and the teardown `shutdown`/`exit`); when it fires the write fails, the error is returned, and the server is marked dead.
- **Every spawned process is reaped by `Wait`**: `Wait` runs immediately after spawn (stdout uses a self-made pipe, so `Wait` never closes the end we read; `readLoop` closes the read end once the process is gone) — without it, the stdin pipe parent end, the Windows process handle, and the `os/exec` ctx-watch goroutine each leak on every spawn / self-heal rebuild.
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
