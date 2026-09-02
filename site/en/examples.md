# Progressive examples (8 lessons)

`examples/` is a progressive path from kernel ground to production integration; every lesson runs standalone:

| Lesson | Topic | Content |
|---|---|---|
| 00-hello-kernel | kernel ground | minimal feel for `kernel.New` / `kernel.Use` / ServiceKey / Effect |
| 01 | Assembly + vocabulary | llm.Registry assembly, named instances, observed wrapper |
| 02 | ReAct | `loop.NewAgent` text and tool rounds |
| 03 | HITL | `before_tool_call` WaterfallLocal decision events |
| 04 | flow orchestration | three-state slot node graph ([examples/04-flow README](/en/packages/examples/)) |
| 05 | Session memory | memory/session event sourcing + fold projection |
| 06 | Long-term memory | memory/store + assemble injection |
| 07 | Production | full assembly + observability + graceful shutdown |

Run any lesson:

```bash
go run ./examples/00-hello-kernel
```

Lessons share the `examples/internal/demoapp` assembly layer (example-private scaffolding; does not violate the "no internal in library packages" convention).

Full per-lesson write-ups live in the [examples package docs](/en/packages/examples/) and each lesson's README (all included in the package docs).
