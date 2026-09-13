# Contributing to Pulse

Pulse is an open-source Go library, still pre-1.0 (`v0.x`). Contributions are welcome.
The rules below are mostly about **how** work is proposed — not about who proposes it.

Both halves of this file are equivalent: English first, 中文在后. Read whichever you prefer;
if they ever disagree, the English text is the reference.

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

---

## English

### Before you start

- **Non-trivial changes start as an issue.** Anything that changes behavior, a public API, a
  documented contract or the repository layout opens an issue first — the issue is where the
  design gets settled, *before* code exists. Typo fixes and small patches that clearly belong
  to an existing issue may go straight to a pull request.
- **Keep pull requests reviewable.** Aim for roughly **~1000 changed lines** per PR. If it
  grows past that, split it into stacked PRs rather than one big drop.
- **Never push to `main`.** Work on a branch (`feat/…`, `fix/…`, `docs/…`, `chore/…`) and open
  a pull request.
- **Security problems do not go into the issue tracker** — see [SECURITY.md](SECURITY.md).

### What an issue must contain

Every issue — feature, design or chore — answers these, in this order:

1. **What to do** — the concrete deliverable.
2. **What *not* to do** — the explicit non-goals. This is what keeps a ticket from growing
   while it is being worked on.
3. **Why** — the problem it solves, and what goes wrong if it is not solved.
4. **Design rationale** — how it will be built, which contracts it touches, and which
   alternatives were rejected and why.
5. **Acceptance criteria** — a checklist that can be verified one item at a time.

Two kinds of issue carry extra requirements:

- **Bug report** — reproduction steps, expected vs. actual behavior, and the environment you
  ran in (OS, Go version, Pulse version or commit, plus the provider/model if the bug involves
  a model call). *A bug that cannot be reproduced cannot be fixed*: a report without a
  reproduction is likely to be closed as `question`.
- **Feature request** — a survey before a proposal. Look at how comparable projects solve the
  same problem, then lay the options out with their trade-offs — **including "do nothing"** —
  and say which one you recommend and why. Present options; don't assert a conclusion the
  evidence doesn't carry.

### What a pull request must contain

- **The issue it serves** — write `see #N`. Use a closing keyword only if the PR really
  finishes that issue.
- **What changed** — the diff in prose.
- **What is new** — new packages, new exported API, new behavior.
- **Impact / blast radius** — who is affected, whether a frozen contract moves, whether
  anything breaks.
- **How it was tested** — the exact commands and what you observed. "CI is green" is a result,
  not a test plan.
- **Review focus** — where you want a second pair of eyes, and what you are unsure about.

### Local development

Requires **Go 1.25+** — the toolchain downloads itself if it is missing.

```bash
go build ./...                                  # compilation
go test ./...                                   # all tests in the main module
go test -race -count=1 -skip TestLive ./...     # the regression CI runs on every PR
cd eval/war && go test -race -count=1 ./...     # nested module: cross-framework comparison suite
```

`TestLive*` in `llm/openai` and `llm/anthropic` call real provider APIs. They are gated by
environment variables (`PULSE_OPENAI_*`, `PULSE_ANTHROPIC_*`, `PULSE_MIMO_*`) and skip when
those are absent, so the default suite needs no credentials.

**Never commit credentials.** `.env`, `.env.*`, `*.pem` and `*.secrets` are gitignored, and
secret scanning with push protection is enabled on this repository.

### Conventions a reviewer will check

- **Functional options** for anything configurable — `loop.WithToolSet()`,
  `flow.WithMaxRunning()`.
- **No `internal/` or `cmd/`** in library packages: this is a library, not a binary, and
  everything published here is public API.
- **Chinese comments and docs are the norm.** When you edit near them, write in the same
  language instead of translating them.
- **The v2 vocabulary contract**: `llm.GenerateRequest` carries only cross-provider stable
  fields. When a provider wire format has no counterpart, the adapter returns `ErrBadRequest` —
  never silently drop a parameter, and never add a `map[string]any` escape hatch to the request
  vocabulary.
- **The flow contract**: slots are `pending | ready | skipped`; a skip is an arrival, not a
  failure; a node error cancels the graph and is never rewritten as a skip. Declarative graphs
  are YAML only.
- **The freeze contract** (v0.2.0+): under 0.x SemVer a breaking change may only ride a *minor*
  release, never a patch, and every breaking change is listed at the top of the release notes.
- **Line endings are LF**, enforced by `.gitattributes`. If your working copy predates that
  file, re-check it out once.

### Review and merge

- **Self-review first.** Read your own diff the way a reviewer would before asking anyone else
  to.
- **With a second person available, wait for their approval.** Security-related changes always
  need a human review.
- **Merging is the maintainer's call.** A green CI is a precondition, not an approval.

### Releases

Releases are cut from `main` as `vX.Y.Z` tags with notes on GitHub. Before tagging, every issue
and pull request that belongs to that release is merged or closed — a release does not ship
with half-open tickets.

---

## 中文

Pulse 是开源的 Go 库，目前仍在 1.0 之前（`v0.x`）。欢迎贡献。下面的规则主要约束**事情怎么被
提出来**，不约束**谁来提**。

本节与上面的英文内容等价；两者若有出入，以英文为准。

参与即表示你同意[行为准则](CODE_OF_CONDUCT.md)。

### 开工之前

- **非小改动先开 Issue。** 任何会改动行为、公开 API、已文档化的契约或仓库结构的事，都先开
  Issue——设计在 Issue 里定下来，再写代码。错字修正、以及明显属于某个既有 Issue 的小补丁，
  可以直接提 PR。
- **PR 要能被 review。** 有效改动按 **1000 行左右**控制；超过就拆成前后依赖的多个 PR，而不是
  一次性丢一大坨。
- **不要直接推 `main`。** 开分支（`feat/…`、`fix/…`、`docs/…`、`chore/…`）再提 PR。
- **安全问题不要开 Issue**——见 [SECURITY.md](SECURITY.md)。

### Issue 必须写清什么

任何 Issue（功能、设计、杂务）都按这个顺序回答：

1. **做什么**——具体的交付物。
2. **不做什么**——明确的非目标。这条决定了票据在执行过程中会不会膨胀。
3. **为什么**——它解决什么问题；不解决会怎样。
4. **设计理念**——打算怎么做、动了哪些契约、否掉了哪些替代方案以及为什么。
5. **验收标准**——能一条条核对的清单。

两类 Issue 有额外要求：

- **Bug 报告**——复现步骤、期望行为与实际行为、运行环境（操作系统、Go 版本、Pulse 版本或
  commit；涉及模型调用时附 provider / 模型）。**复现不了的 bug 修不了**：没有复现步骤的报告
  很可能被按 `question` 关闭。
- **功能需求**——先调研再提方案。看同类项目怎么解同一个问题，然后把各个选项连同取舍摆出来——
  **包括「什么都不做」**——并说明你推荐哪个、为什么。给可选方案，不要下证据撑不住的结论。

### PR 必须写清什么

- **关联 Issue**——写「见 #N」。除非这个 PR 真的把票干完，否则不要用自动关闭关键字。
- **改了什么**——用文字把 diff 讲清楚。
- **新增了什么**——新包、新导出 API、新行为。
- **影响范围**——谁受影响、有没有动冻结契约、有没有破坏性。
- **怎么验证的**——具体命令 + 观察到的结果。「CI 绿了」是结果，不是测试方案。
- **Review 关注点**——希望别人重点看哪里，以及你自己没把握的地方。

### 本地开发

需要 **Go 1.25+**，工具链缺失时会自动下载。

```bash
go build ./...                                  # 编译
go test ./...                                   # 主 module 全部测试
go test -race -count=1 -skip TestLive ./...     # CI 每个 PR 跑的回归集
cd eval/war && go test -race -count=1 ./...     # 嵌套 module：跨框架对比套件
```

`llm/openai`、`llm/anthropic` 里的 `TestLive*` 会调用真实 provider：由环境变量
（`PULSE_OPENAI_*`、`PULSE_ANTHROPIC_*`、`PULSE_MIMO_*`）门控，缺失时自动跳过，因此默认测试集
不需要任何凭据。

**绝不要提交凭据。** `.env`、`.env.*`、`*.pem`、`*.secrets` 都在 .gitignore 中，且本仓库已开启
secret scanning 与 push protection。

### Review 会检查的约定

- **可配置处一律用 functional options**——`loop.WithToolSet()`、`flow.WithMaxRunning()`。
- **库包不放 `internal/` 或 `cmd/`**：这是库不是可执行程序，这里发布的每个包都是公开 API。
- **中文注释与中文文档是本仓库常态**；在这些内容旁边改动时，用同一种语言写，不要顺手翻译。
- **v2 词汇表契约**：`llm.GenerateRequest` 只承载跨 provider 稳定的字段；provider 线格式没有
  对应物时，适配器返回 `ErrBadRequest`——既不静默丢参数，也不给请求词汇表加
  `map[string]any` 逃生舱。
- **flow 契约**：槽位是 `pending | ready | skipped`；skip 是「到达」不是「失败」；节点出错会取消
  整张图，绝不改写成 skip。声明式图只用 YAML。
- **freeze 契约**（v0.2.0 起）：0.x SemVer 下 breaking 只能随 **minor** 发布，patch 内永不破坏，
  且每次 breaking 都在 Release notes 顶部显式列出。
- **行尾是 LF**，由 `.gitattributes` 强制。如果你的工作副本早于该文件，重新检出一次即可。

### Review 与合并

- **先自审。**像 reviewer 那样读一遍自己的 diff，再交给别人。
- **有第二人时必须等人审过。**安全相关的改动永远需要人工 review。
- **合并由维护者决定。**CI 绿是前置条件，不是批准。

### 发版

从 `main` 打 `vX.Y.Z` tag 并在 GitHub 上写 notes。打 tag 之前，属于这次发版的 issue 与 PR 全部
合入或关闭——不带着半开的票据发版。
