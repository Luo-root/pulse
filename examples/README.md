[English](README.md) | [中文](README_zh.md)

# Pulse Examples: Eight Progressive Lessons

Eight progressive lessons; each one **runs standalone** (`go run ./examples/<lesson>`) and ships its own README. The path builds on itself: lessons 00–03 are framework fundamentals (kernel → vocabulary → ReAct → approval), lessons 05–06 are the memory layer (sessions → long-term memory), and lesson 07 is declarative orchestration plus production integration.

> Follow the numbers for the intended path; each lesson's README opens with "this lesson depends on", so you can also jump around.

| Lesson | Directory | Topic | Core packages | Depends on |
|---|---|---|---|---|
| 00 | [`00-hello-kernel`](00-hello-kernel/) | Kernel lifecycle and the first model call: `New/Use/Dispose`, reversible Effects, observability banner | `kernel` `llm` `observability` | — |
| 01 | [`01-chat`](01-chat/) | Full assembly chain and message vocabulary: Registry, multimodal Parts, REPL | `kernel` `llm` | 00 |
| 02 | [`02-react`](02-react/) | ReAct loop and tool calls: `toolset.Registry`, streaming, `AsToolSet` | `loop` `toolset` | 01 |
| 03 | [`03-hitl`](03-hitl/) | Human-in-the-loop approval: `before_tool_call`, deny/allow, session trust | `loop` (HITL events) | 02 |
| 04 | [`04-flow`](04-flow/) | Declarative and imperative orchestration: flow node graph, three-state slots, YAML loading | `kernel/flow` `flow/yaml` | 02 |
| 05 | [`05-memory-session`](05-memory-session/) | Session truth and compaction: event log, fold projection, JSONL persistence, compaction | `memory/session` `memory/compaction` | 01 |
| 06 | [`06-memory-agent`](06-memory-agent/) | Long-term memory end to end: store → index recall → candidate refinement → approval → assemble injection | 8 × `memory/*` | 05 |
| 07 | [`07-production`](07-production/) | Production integration: MCP/Skills multi-source, observability bridge, reflection & metrics | all | 03 04 06 |

## Running

Lessons auto-load a repository-root `.env` (gitignored) at startup.

```powershell
go run ./examples/00-hello-kernel
go run ./examples/01-chat
# ... and so on

# All example tests (offline unit tests, no real API)
go test ./examples/...
```

Without an API key each lesson falls back to `ScriptedModel` (scripted responses) — the lessons never require real credentials; with a real key configured, the same code hits the real model. Variable details live in each lesson's README.

## Architecture at a glance (how the lessons assemble the framework)

```text
00  kernel.New ──Use(observability)──Use(llm.Plugin)──Generate        ← ground
01      + Registry named instances / vocabulary Parts / REPL           ← can talk
02      + toolset.Registry + loop.Agent(ReAct) + RunStream             ← can act
03      + before_tool_call approval / trust modes                      ← controlled
04      + flow node graph / YAML loading (two orchestration forms)     ← composable
05      + session event log / fold / JSONL / compaction                ← remembers
06      + store + index + candidate + assemble (long-term memory loop) ← accumulates
07      + MCP / Skills multi-source + observability bridge + metrics   ← shippable
```

## Observability logs

- **Assembly time** (shared by all lessons): `observability.Bootstrap` → `MemorySink` / `SlogSink`; fields documented in [`observability/README.md`](../observability/README.md).
- **Runtime**: the observability bridge folds llm/loop/flow facts into the same Sink (`source=bridge`, `trace_id` required) — lesson 02 hand-writes a teaching `reqBridge`; from lesson 03 on, lessons reuse the packaged `demoapp.Bridge`.
- Privacy boundary: logs record **metadata** (counts, byte lengths, latency, status) — never prompt contents, attachment contents, keys, or chain-of-thought.

## Deliberately out of scope

- Lessons 00–04 intentionally skip `memory/` persistence (history lives only in-process) — that is a lesson boundary; see [`memory/README.md`](../memory/README.md).
- Real vector databases / embeddings (lesson 06 uses the in-memory index with keyword recall to demonstrate semantically equivalent structure).

## Design notes

- `examples/internal/demoapp` is lesson-private assembly scaffolding (REPL shell, `.env` loading, packaged Bridge/HITL); **the library packages themselves have no internal** — this does not violate the "no internal in library packages" convention.
- Teaching rhythm: lessons 01–03 hand-write the key implementations (assembly chain / Bridge / HITL) in-lesson, with the demoapp packaged versions as their reference prototypes; from lesson 04 on, lessons reuse the packaged versions, and every `main.go` stays deliberately "readable in one pass" while complexity settles into demoapp or the corresponding package.
- API questions: every package's `README_zh.md` is the source of truth, and `doc.go` is the godoc entry point.
