# AGENTS.md

## What this is

Go library (`github.com/Luo-root/pulse`) — an AI Agent framework under v2 reconstruction. The v2 core is `kernel/` (plugin kernel + `kernel/flow` dataflow), `llm/` (provider-neutral model vocabulary + adapters), `loop/` (stateless ReAct turn executor), `toolset/` (reversible tool registry adapting to `loop.ToolSet`) and `skills/` (Agent Skills loader per agentskills.io). The entire v1 `components/` tree has been removed. `pulse.go` at root is a stub (`package pulse`); it is not an entrypoint.

## Build & test

```bash
go build ./...          # verify compilation
go test ./...           # run all tests
go test -race -skip TestLive ./kernel/... ./llm/... ./loop/ ./toolset/... ./skills/ ./textsplit/... ./memory/... ./observability/ ./examples/03-flow-agent/ ./examples/04-flow-dag/ ./examples/05-tools-sources/   # v2 core + flow/tools examples
```

- Requires **Go 1.25.0+** (toolchain auto-downloads if missing).
- Provider adapter live-API smoke tests (`TestLive*` in `llm/openai`, `llm/anthropic`) are gated by environment variables (`PULSE_OPENAI_*`, `PULSE_ANTHROPIC_*`, `PULSE_MIMO_*`); without credentials they skip automatically.
- No Makefile, linter config, or CI pipeline exists. `go test ./...` is the only verification step.

## Repo layout

```
pulse.go                    # stub: package pulse (ignore)
kernel/                     # v2 plugin kernel: Context/ServiceKey/events/Plugin+Fiber/Loader
  flow/                     # node graph + Observer；yaml/ 为 E2 装图子包
llm/                        # v2 model layer: content-block vocabulary, ChatModel, Registry, openai/ + anthropic/ adapters
loop/                       # v2 stateless ReAct turn executor: ToolSet, HITL decision events
toolset/                    # v2 可逆工具注册（pulse.tools）+ AsToolSet；mcp/ Source；builtins/ 基础工具；lsp/ 可选 LSP 工具
memory/                     # P2 记忆与会话（设计 Accepted：docs/design/memory-layer-research-and-v2-design.md）；session/ 已落地（P2-A in-memory+JSONL/blobs/文件锁、P2-B Replace fold），compaction/ 已落地（P2-B meter+事务编排+pruning），store/ 已落地（P2-C 内存 item store + SQLite/FTS5 backend：namespace 隔离+Supersede/Revoke+CAS；SQLite 带 build tag，plan9/js 主包不锁死），assemble/ 已落地（P2-C3 Context Assembler：按类预算+stable snapshot+引用模板+D2 hybrid 融合排序（§8.2 子集，semantic 走函数 seam 不 import index）），selfedit/ 已落地（P2-C4 self-edit 记忆工具组：put/supersede/revoke opt-in 注册、模型参数最小化、scope env 钉死）；index/ 已落地（P2-D1 派生向量索引内存版：EmbeddingProvider seam+先过滤再召回+异步队列，openai/ 适配器 P2-D1.5 OpenAI 兼容 embeddings=SDK 薄包装），candidate/ 已落地（P2-D3 候选管线：extractor seam+去重+pending approval=Supersede/Revoke 状态机、taint 默认 untrusted-external），reflection/ 已落地（P2-D4 可配置 background reflection：预算截断+候选提炼编排、无后台循环默认关；D4 指标面=candidate Metrics/index Counted/reflection Metrics 三处快照）——P2 记忆层全部完结
skills/                     # v2 Skills 装载器（agentskills.io）；Skill ≠ Tool/Source
observability/              # v2 正式观测包：Bootstrap + Record + Sink（只依赖 kernel）
textsplit/                  # 独立文本分块：尺寸预算+分隔符优先级（段落>句读>空白>硬切）+字节 offset；index/openai 与未来长文本模块共用
docs/design/               # Accepted：plugin-kernel-v2 / flow-v2 / kernel-local-events / observability-v1 / toolset-v1 / skills-v1 / memory-layer-v1
                           # 未列出的 memory-layer 草稿不存在；P2 记忆层事实源是 memory-layer-research-and-v2-design.md（含 §17 补遗）
examples/                   # 01–05 渐进示例
  internal/demoapp/         # 示例私有装配层（库包本身无 internal/；此处不违反「库无 internal」）
  skills/                   # 示例 Skill 材料（历史私有 frontmatter 键会被装载器忽略）
```

## Key conventions

- **No `internal/` or `cmd/` in library packages** — this is a library, not a binary. All published packages are public API. `examples/internal/demoapp` is example-private scaffolding only.
- **Functional options pattern** used throughout (`loop.WithToolSet()`, `flow.WithMaxRunning()`, etc.).
- **Chinese comments and doc** are the norm; preserve them when editing.
- **v2 vocabulary contract**: `llm.GenerateRequest` only carries cross-provider stable fields; when a provider wire format has no counterpart, the adapter returns `ErrBadRequest` — never silently drop parameters and never add `map[string]any` escape hatches on the request vocabulary. `llm.Config.Options` is **client-level only** (org / timeout / headers / retries), not a request-parameter escape hatch.
- **flow contract**: slots are pending | ready | skipped; skip is arrival, not failure; node error cancels the graph and is never rewritten as skip. Declarative graphs are **YAML only** (`kernel/flow/yaml`).
- **Secrets**: never commit `.env`, API keys or tokens. Live tests are env-gated.
