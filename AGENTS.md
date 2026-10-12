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
- **Sealing**: `(*Graph).Validate()` **seals the whole subtree** when it passes — the mounted children are part of this definition, so `Add` / `DeclareInput` / `DeclareOutput` / `Sub` are rejected on **every** graph of that subtree (the second trigger point of `ErrGraphStarted`); a sealed child can still be mounted elsewhere (mounting does not change it). A **failed** validation seals **nothing**, in no layer — that is what "add the missing piece and validate again" needs. Do not add a `Seal()`. `Seed` is unaffected (it is this run's value, not part of the definition).
- **The static checks recurse with per-layer locking**: `Topology()` / `Validate()` hold the parent's lock and then ask each child through `child.problemsAt` / `child.topologyAt`, which take the child's own lock — never read a mounted child's fields while holding only the parent's lock (a test pins this under `-race`: `TestSubChildAssemblyVsParentReadOnly`). Corollary: when you already hold a child's lock, call the `...Locked` variants — `checkPort` locks, so it is not re-entrant. The mount-cycle criterion is a **global** invariant: `Sub` does "walk the subtree" and "mount" inside one package-level critical section (`subMountMu`), because a cycle is the composition of two graphs' separate actions; the recursion additionally carries the mount chain and reports the cycle instead of overflowing the stack or self-deadlocking on the child lock.
- **Declared outlets are checked statically** like inlets: `DeclareOutput(k)` where nothing in the graph provides/seeds `k` (and `k` is not also a declared inlet — an inlet/outlet pair is a **pass-through**) is reported by `Validate` / `Start` / `Topology().Problems` at assembly time, naming the mount path when the graph is mounted, instead of waiting for `bridgeOutputs` to say `never arrived` after the child ran. `Problems` order is sources → declared outlets → dependency cycles → streams → per-mount subtree; the outlet category is ordered by key name (map iteration must never reach a message).
- **In `pulse/yaml`, a spec nothing references is *not* judged for sources**: it has no mount site, hence no interface — "nothing feeds this key" is undecidable there (any key with no source is assumed to be an inlet, declared into a build-only throwaway check graph), so such a spec is checked for factory names / empty nodes / duplicate ids / source conflicts / cycles / streams only. Sources are judged per **reference site** once the spec is actually mounted.
- **Aspect latch**: a node's `Run` must not be entered concurrently (overlapping `next` → `ErrNextCalledTwice`); sequential re-entry is legal and `Retry` depends on it.
- **Start validation**: `Start()` rejects the statically-knowable dead graphs — a `Requires` with no source, a **declared outlet nobody produces**, and a dependency **cycle** (every input has a producer, yet no node can run first). Without those checks they surface as a hang, or as the runtime's fatal all-goroutines-asleep deadlock (or, for the outlet, as `never arrived` only after the child ran). A cycle is reported as one concrete path: `pulse: dependency cycle: A -> B -> A (A requires "y", B requires "x")`.
- **Observation**: business dimensions go through `Attrs` only — **never add named fields to `Record`**. Attr keys use the `<component>.<field>` convention and are defined by the package that owns the fact (`pulse.AttrGraph` / `pulse.AttrNode`).
- **Encoding primitives are frozen**: `AppendDuration` / `AppendTextValue` / `AppendAttrs` / `AppendAttrsExcept` / `AppendPadding` / `DisplayWidth` and the `LineRenderer` contract — the fragments a host egress builds from these six primitives are byte-identical to the corresponding fragments of the built-in layout.
- **Declarative graphs are YAML only** (`yaml/`): the YAML owns topology, the registered factory only supplies `Run`.
- **Secrets**: never commit `.env`, API keys or tokens.

## Freeze contract (0.x SemVer)

Breaking changes ride **minor** releases only, never a patch; each is listed at the top of its release notes. Frozen contracts: the slot contract (three states + skip-is-arrival), the `Aspect` shape and its re-entrancy latch, the sentinel errors (`ErrUndeclared` / `ErrConflict` / `ErrGraphStarted` / `ErrGraphNotStarted` / `ErrDuplicateSource` / `ErrSkipped` / `ErrNextCalledTwice` — the same list lives in `docs/design/pulse.md` under 附 · 冻结面), the `Observer` callback-count contract (two run-level + three per node, and "observer panic never becomes a node failure"), and the six encoding primitives + `LineRenderer`.
