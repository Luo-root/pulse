## What this is

Go library (`github.com/Luo-root/pulse`) — a **one-shot graph engine** plus a **graph-observation layer**. Nodes declare which keys they read and write; topology is implied by data production and consumption (no edge objects, no topological sort, no scheduler loop).

The engine is the root package `pulse`; `observe/` folds the engine's `Observer` callbacks into structured records; `yaml/` assembles graphs declaratively. **The engine's dependency closure is zero** (standard library only).

## Build & test

```bash
go build ./...          # verify compilation
go vet ./...
go test -race -count=1 ./...
```

- Requires **Go 1.25.0+** (toolchain auto-downloads if missing).
- No Makefile or linter config. GitHub Actions CI (`.github/workflows/ci.yml`) runs `go build ./...`, `go vet ./...`, a gofmt check (empty output = pass), plus the `-race` suite on every PR and on pushes to `main`.
- No live-API tests and no network dependencies: `go.mod` requires only `gopkg.in/yaml.v3`.

## Repo layout

```
*.go            # package pulse — the graph engine（零依赖）
observe/        # package observe — graph observation (depends on pulse)
yaml/           # package yaml — declarative graph assembly (pulse + yaml.v3)
docs/design/    # pulse.md — the single design doc (编排 + 观测)
```

## Key conventions

- **No `internal/` or `cmd/`** — this is a library, not a binary. All published packages are public API.
- **Chinese comments and doc are the norm**; preserve them when editing. **README naming**: `README.md` is the Chinese default, `README_en.md` is English — same for the root and every package (`observe/`, `yaml/`). The site builds from those two files.
- **Functional options pattern** throughout (`pulse.WithObserver()`, `observe.WithRenderer()`, `pulse.WithMaxRunning()`).
- **Dependency direction is one-way and must not reverse**: `pulse` ← `observe` / `yaml`. The engine never imports an observation package. Verify with `go list -deps .` — it must contain no other `github.com/Luo-root/pulse/*` entry.
- **Slot contract**: `pending` | `ready` | `skipped`. Skip is **arrival, not failure**; a node error cancels the graph and is never rewritten as skip. Unwritten `Provides` after a successful `Run` are auto-skipped.
- **Fan-in gate**: the gate is judged once every input has arrived (ready *or* skipped), and it asks "did any input bring a value" — a node enters `Run` as soon as one did (collect whatever arrives), and only skips itself when none did. A skipped input read inside `Run` yields `*SkipError`; returning `WaitAll`'s value is the explicit "all-or-skip me" opt-out.
- **One-shot contract**: a `Graph` runs once. `Start()` a second time returns `ErrGraphStarted`, slots never reopen, and `Seed` after start is rejected. **Do not add a `Reset`** — cross-run state belongs to the caller.
- **Aspect latch**: a node's `Run` must not be entered concurrently (overlapping `next` → `ErrNextCalledTwice`); sequential re-entry is legal and `Retry` depends on it.
- **Start validation**: `Start()` rejects the two statically-knowable dead graphs — a `Requires` with no source, and a dependency **cycle** (every input has a producer, yet no node can run first). Both would otherwise surface as a hang, or as the runtime's fatal all-goroutines-asleep deadlock. A cycle is reported as one concrete path: `pulse: dependency cycle: A -> B -> A (A requires "y", B requires "x")`.
- **Observation**: business dimensions go through `Attrs` only — **never add named fields to `Record`**. Attr keys use the `<component>.<field>` convention and are defined by the package that owns the fact (`pulse.AttrGraph` / `pulse.AttrNode`).
- **Encoding primitives are frozen**: `AppendDuration` / `AppendTextValue` / `AppendAttrs` / `AppendAttrsExcept` / `AppendPadding` / `DisplayWidth` and the `LineRenderer` contract — the fragments a host egress builds from these six primitives are byte-identical to the corresponding fragments of the built-in layout.
- **Declarative graphs are YAML only** (`yaml/`): the YAML owns topology, the registered factory only supplies `Run`.
- **Secrets**: never commit `.env`, API keys or tokens.

## Freeze contract (0.x SemVer)

Breaking changes ride **minor** releases only, never a patch; each is listed at the top of its release notes. Frozen contracts: the slot contract (three states + skip-is-arrival), the `Aspect` shape and its re-entrancy latch, the sentinel errors (`ErrUndeclared` / `ErrConflict` / `ErrGraphStarted` / `ErrGraphNotStarted` / `ErrDuplicateSource` / `ErrSkipped` / `ErrNextCalledTwice` — the same list lives in `docs/design/pulse.md` under 附 · 冻结面), the `Observer` callback-count contract (two run-level + three per node, and "observer panic never becomes a node failure"), and the six encoding primitives + `LineRenderer`.
