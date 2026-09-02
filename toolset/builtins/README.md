[English](README.md) | [中文](README_zh.md)

# toolset/builtins

General-purpose builtin tools, mounted into `pulse.tools` via `toolset.Registry`.

Neutral naming: `read` / `ls` / `glob` / `grep` / `exec` / `edit` / `write` / `apply_patch` / `web_fetch` / `web_search` / `question` / `job_output` / `job_kill`.

## Getting started

```go
host := kernel.New()
defer host.Dispose()
_ = kernel.Use(host, toolset.Plugin())
reg, _ := kernel.Get(host, toolset.ServiceKey)

dispose, err := builtins.Register(host, reg, builtins.Options{
    Root: "/path/to/workspace",
    // WriteRoots: nil → 仅 Root
    // Enabled: []string{"read","grep"} → 子集
})
defer dispose()
```

## Contract highlights

| Item | Behavior |
|---|---|
| Paths | Relative paths resolve against `Root`; symlinks are resolved to the final target before confining; read and write roots are separate; `ForbidRead` refuses peeking |
| `read` | Line-number prefixes; `offset`/`limit`; returns a truncated notice with a continue-reading hint when over the limit |
| `ls`/`glob`/`grep` | **Collect and sort stably first, then paginate**; the over-limit trailer carries an `after` cursor |
| `edit`/`write`(overwrite) | **Must `read` first within the same process**; a newer mtime means stale and rejected; `edit` requires a unique match by default |
| `apply_patch` | Multi-file Add/Update/Delete (V4A text protocol); **verify before writing**: only when every hunk passes (context anchors, read-before-write, WriteRoots) is anything written to disk — one failure writes nothing. Update/Delete likewise require a prior `read`; CRLF is normalized and restored; NUL is rejected; leftover content after `*** End Patch` is an error; Add cannot express an empty file (at least one line). No `*** Move to:` rename, no fuzzy matching, no binary patches |
| `exec` | **Windows = PowerShell**; Unix = `sh -c`; timeout + head/tail output truncation; RiskDangerous. `background:true` starts a long-running command job: returns `job_id` immediately, exempt from the timeout and not canceled with the request |
| `job_output` | Reads a job's incremental merged output by **global byte offset** plus status (`running`/`exited exit_code=N`/`killed`); the ring buffer (`MaxExecBytes`) drops the head when over limit and reports `dropped`; the over-limit trailer says `pass offset=N` to continue reading |
| `job_kill` | Kills the whole tree: Windows `taskkill /T /F`, Unix process-group SIGKILL; returns only after the process has really exited; errors on an already-exited job. **Both dispose and scope Dispose kill all live jobs** (an independent Effect, a backstop even if the host forgets to dispose explicitly); `MaxJobs` (default 16) caps concurrency; when done jobs exceed `2*MaxJobs`, the oldest by creation order is evicted. When both `background` and `timeout_seconds` are given, the timeout is ignored |
| `web_fetch` | http(s) GET → extract text → line-based `offset`/`limit` (default limit=`ReadLimit`); lines longer than `MaxLineRunes` are truncated with `…` (same rule as `read`). Over-limit trailer `pass offset=N`; each continuation GETs again. Blocks file/ftp/data, NUL binary, and cloud metadata; **at Dial time** the actually resolved IP is re-checked (against redirect / DNS rebinding) and the connection goes to the checked IP rather than the hostname. Private networks are allowed by default (`BlockPrivate` to refuse). It is not a rendered browser DOM |
| `web_search` | A `Searcher` can be injected by default; if nil, DuckDuckGo Lite (HTML parsing, may hit anti-scraping) |
| `question` | Asks a human a question; requires an `Asker`. **Not** HITL approval |
| `glob`/`grep` | P0 does **not** apply `.gitignore` (explicit); invalid regexes return an error |
| Source | `builtins.<name>`; the `dispose()` returned by `Register` is reversible |

## Deliberately out of scope

- **Pre-write diff into HITL**: `edit`/`write`/`exec` register a `PreviewFn` (read-only disk access); the card goes to `before_tool_call` listeners, not into the tool result. See Issue [#56](https://github.com/Luo-root/pulse/issues/56)
- `apply_patch` does no `*** Move to:` (rename), no fuzzy context matching, no binary patches, no automatic rollback on write failure (verify already blocks content-level failures; an IO error aborts and lists the files already written)
- LSP → [`toolset/lsp`](../lsp/README.md) (a separate optional package)
- No job listing tool (the model remembers the id); no stdout/stderr split; no output persisted to disk; no cross-process / cross-restart jobs
- Real browser rendering (chromedp); `web_fetch` only extracts the HTTP body
- OS-level sandbox (bwrap / Seatbelt)
- Changing examples (examples are planned separately)
- Skills automatically becoming Tools
- A second execution event bus

## Tests

```bash
go test -race ./toolset/builtins/...
```
