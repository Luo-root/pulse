# 渐进示例（8 课）

`examples/` 是一条从内核地基走到生产集成的渐进路线，每课可独立运行：

| 课 | 主题 | 内容 |
|---|---|---|
| 00-hello-kernel | kernel 地基 | `kernel.New` / `kernel.Use` / ServiceKey / Effect 的最小感知 |
| 01 | 装配链 + 词汇表 | llm.Registry 装配、named 实例、observed 包装 |
| 02 | ReAct | `loop.NewAgent` 文本回合与工具回合 |
| 03 | HITL | `before_tool_call` WaterfallLocal 决策事件 |
| 04 | flow 编排 | 槽位三态节点图（[examples/04-flow README](/packages/examples/)） |
| 05 | 会话记忆 | memory/session 事件溯源 + fold 投影 |
| 06 | 长期记忆 | memory/store + assemble 注入 |
| 07 | 生产集成 | 全家桶装配 + 观测 + 优雅退出 |

运行任一课：

```bash
go run ./examples/00-hello-kernel
```

示例共享 `examples/internal/demoapp` 装配层（示例私有脚手架，不违反「库无 internal」约定）。

各课完整说明见 [examples 包文档](/packages/examples/) 与各课子目录 README（包文档内全部收录）。
