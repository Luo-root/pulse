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
- **Markdown code blocks are not compiled** — when a signature changes, migrating only the Go call sites is not enough. `TestDocsExamplesUseCurrentSignatures` (root package) scans every tracked `*.md` for `New(` / `NewNode(` / `Sub(` and fails when an example has too few arguments or something other than a description in the description slot (`nil`, an option, a bind slice, a parameter-name placeholder). Migrate README (中/英), `site/`, `docs/design/` and the package READMEs together, then re-run it.
- No live-API tests and no network dependencies: `go.mod` requires only `gopkg.in/yaml.v3`.

## Repo layout

```
*.go            # package pulse — the graph engine（零依赖）
observe/        # package observe — graph observation (depends on pulse)
yaml/           # package yaml — declarative graph assembly (pulse + yaml.v3)
docs/design/    # pulse.md（编排 + 观测的实现级设计）/ philosophy.md（理念与设计公理）/ object-model.md（Graph vs Execution 定案）
```

## Key conventions

- **No `internal/` or `cmd/`** — this is a library, not a binary. All published packages are public API.
- **Chinese comments and doc are the norm**; preserve them when editing. **README naming**: `README.md` is the Chinese default, `README_en.md` is English — same for the root and every package (`observe/`, `yaml/`). The site builds from those two files.
- **Functional options pattern** throughout (`pulse.WithObserver()`, `observe.WithRenderer()`, `pulse.WithMaxRunning()`).
- **Dependency direction is one-way and must not reverse**: `pulse` ← `observe` / `yaml`. The engine never imports an observation package. Verify with `go list -deps .` — it must contain no other `github.com/Luo-root/pulse/*` entry.
- **Slot contract**: `pending` | `ready` | `skipped`. Skip is **arrival, not failure**; a node error cancels the graph and is never rewritten as skip. Unwritten `Provides` after a successful `Run` are auto-skipped.
- **Fan-in gate**: the gate is judged once every input has arrived (ready *or* skipped), and it asks "did any input bring a value" — a node enters `Run` as soon as one did (collect whatever arrives), and only skips itself when none did. A skipped input read inside `Run` yields `*SkipError`; returning `WaitAll`'s value is the explicit "all-or-skip me" opt-out.
- **One-shot contract**: a `Graph` runs once. `Start()` a second time returns `ErrGraphStarted`, slots never reopen, and `Seed` after start is rejected. **Do not add a `Reset`** — cross-run state belongs to the caller. *(This describes the implementation as of today; the object model is being migrated — see the next bullet.)*
- **Object model is being re-frozen (`#311`)**: the target is `Graph` = a **reusable static definition**, `Execution` = one run (own ctx / inputs / slots / node state / first error / cancellation domain), with composing ready-made child graphs as the default path and **transactional `Retry`** (attempt write-set, commit or discard). Until each stage lands, `docs/design/pulse.md` describes **what exists today**, while `docs/design/object-model.md` holds the target contract and the staged plan. **Do not keep extending the one-shot shape** — no `Reset`, no new runtime fields on `Graph`, and no new public surface that assumes `Graph.Child *Graph`. Philosophy (why the trade-offs are what they are): `docs/design/philosophy.md`.
- **Composition is static — `pulse.Sub` is the only public entry (stage 1, landed)**: `pulse.Sub(g, id, description, binds, child)` mounts a **ready-made** child graph — ports (`DeclareInput` / `DeclareOutput` + `NewPort` / `PortRef`) are the *only* wiring surface, the whole subtree is visible **without running** (`Topology().Nodes[i].Sub`, path derived from the mount position), and the child's static problems surface at assembly time (in `pulse/yaml`: at `Load`, with the **mount path** in the message). There is **no** dynamic builder: the old `SubFunc` / `SubCtx` form is **deleted**, not renamed — a fresh child per run needs the stage-2 `Execution`. `pulse/yaml` builds one child **instance per mount site** at `Load`; `retry:` on a `graph:` node is rejected at `Load` (a statically mounted child runs once, so a second attempt could not start it) — put the retry on the node inside the child that may actually fail.
- **Sealing**: `(*Graph).Validate()` **seals** the definition when it passes — `Add` / `DeclareInput` / `DeclareOutput` / `Sub` are then rejected (the second trigger point of `ErrGraphStarted`); a **failed** validation leaves the graph mutable. Do not add a `Seal()`. `Seed` is unaffected (it is this run's value, not part of the definition).
- **Aspect latch**: a node's `Run` must not be entered concurrently (overlapping `next` → `ErrNextCalledTwice`); sequential re-entry is legal and `Retry` depends on it.
- **Start validation**: `Start()` rejects the two statically-knowable dead graphs — a `Requires` with no source, and a dependency **cycle** (every input has a producer, yet no node can run first). Both would otherwise surface as a hang, or as the runtime's fatal all-goroutines-asleep deadlock. A cycle is reported as one concrete path: `pulse: dependency cycle: A -> B -> A (A requires "y", B requires "x")`.
- **Observation**: business dimensions go through `Attrs` only — **never add named fields to `Record`**. Attr keys use the `<component>.<field>` convention and are defined by the package that owns the fact (`pulse.AttrGraph` / `pulse.AttrNode`).
- **Encoding primitives are frozen**: `AppendDuration` / `AppendTextValue` / `AppendAttrs` / `AppendAttrsExcept` / `AppendPadding` / `DisplayWidth` and the `LineRenderer` contract — the fragments a host egress builds from these six primitives are byte-identical to the corresponding fragments of the built-in layout.
- **Declarative graphs are YAML only** (`yaml/`): the YAML owns topology, the registered factory only supplies `Run`.
- **Secrets**: never commit `.env`, API keys or tokens.

## Freeze contract (0.x SemVer)

Breaking changes ride **minor** releases only, never a patch; each is listed at the top of its release notes. Frozen contracts: the slot contract (three states + skip-is-arrival), the `Aspect` shape and its re-entrancy latch, the sentinel errors (`ErrUndeclared` / `ErrConflict` / `ErrGraphStarted` / `ErrGraphNotStarted` / `ErrDuplicateSource` / `ErrSkipped` / `ErrNextCalledTwice` — the same list lives in `docs/design/pulse.md` under 附 · 冻结面), the `Observer` callback-count contract (two run-level + three per node, and "observer panic never becomes a node failure"), and the six encoding primitives + `LineRenderer`.
