# ReAct 与 Durable 适配试验（Draft，不合并）

本文仅作为未来独立 Pulse Agent 套件的设计讨论与试验索引，不是 Pulse/Durable 的正式 Agent API，不加入 README 或文档站导航。请保持对应 PR 为 Draft，不合并。

## 执行模型

一次用户输入对应一次图执行，模型请求工具时继续 ReAct，返回最终回答后结束。下一次用户输入携带会话信息，以新 RunID 执行；中断恢复使用原 RunID。Session ID 与 RunID 分离，不需要无限运行的 Agent。

## 试验结果（2026-10-26 至 27）

试验基于 Pulse v0.5.0 和包含 PR #109 的 Durable 代码；只新增测试，不修改生产架构，不把模型 SDK 或 Agent 套件加入核心依赖。

1. 普通节点内 for 循环：第二轮取消再 Resume，工具尝试为 `[1,2,1,2]`，未确认节点从头执行。
2. 消息状态提交：同一故障窗口工具尝试为 `[1,2,2]`，已确认轮次复用，未保存副作用可能重复。
3. 假模型、真实 PostgreSQL、独立进程强杀：模型回复提交后、工具结果提交后、工具执行但结果未提交三个窗口通过。接管 epoch 1→2；未保存工具尝试出现两次。
4. 真实 MiniMax-M3 动态 ReAct：3 次模型请求、2 次 lookup_value、1 次 add，最终回答后退出。单纯 API 成功不证明恢复。
5. 真实 MiniMax-M3 + PostgreSQL + 独立 worker：after_model 与 after_tool 两个强杀窗口均通过（含 race）。每个场景 3 次模型请求、3 次工具执行；接管 epoch 1→2，完整已提交 assistant/history 哈希不变，已确认请求和工具事件未重复。随机任务最终结果分别为 163546、159475，仅为当次样本，不是固定期望值。

这里“分布式接管”实测是同一 Windows 主机上的两个独立 OS 进程，不是该真实模型场景的跨机器验证。

## 适配方式与限制

每个模型回复和单个工具结果分别通过公开 HandleMessage 提交。持久状态保存完整 assistant content、history、pending tool calls、工具索引、部分结果、请求预算和最终结果，不保存认证信息、连接或闭包。

最新真实模型测试按持久状态决定下一步，不按奇偶消息编号或预设动作脚本执行。仍使用有限唤醒消息作为调度预算：最多 6 次模型请求，每次最多 16 个工具调用，共 102 个处理机会。模型最终回答提交后，通过 stream Stop 停止剩余输入。

这证明已有消息检查点可以承载显式 Agent 状态，但不代表应该把唤醒消息预算直接作为正式 Agent API。后续需评估通用“状态 + 单步提交 + 结束/等待”能力是否必要；模型厂商和聊天策略应留在独立 Agent 套件。

外部 HTTP 调用与检查点不共享事务。模型返回但未保存时可能重调、重复计费或生成不同回复；工具执行但未保存时可能重复副作用。业务工具应提供稳定幂等键、结果查询或事务/outbox，不承诺 exactly-once。

## 试验源码索引

源码现归档于本 Draft PR 的独立 Go 实验模块；正式 Agent 套件仍应在独立仓库开发，不把此试验当作 Pulse 核心 API 或合并承诺。来源基线、固定依赖、必要 helper 与完整命令见 [实验 README](../../experiments/react-durable/README.md)。

- [react_assessment_test.go](../../experiments/react-durable/react_assessment_test.go)：普通循环与消息恢复对照。
- [systemtest/react_recovery_assessment_test.go](../../experiments/react-durable/systemtest/react_recovery_assessment_test.go)：假模型三个强杀窗口。
- [live_react_assessment_test.go](../../experiments/react-durable/live_react_assessment_test.go)：真实 API 动态循环与离线协议断言。
- [systemtest/live_react_recovery_test.go](../../experiments/react-durable/systemtest/live_react_recovery_test.go)：真实模型步骤持久化、离线恢复与独立 worker 接管。

在 `experiments/react-durable` 目录设置 `GOWORK=off` 后运行：

```text
go mod download
go test -race -count=1 -buildvcs=false ./...
go vet -buildvcs=false ./...
```

默认只执行 MemoryStore 与本地 HTTP fixture，真实 API/PG 及子进程入口默认 skip；运行前清除继承的授权与子进程控制变量，具体 PowerShell 命令见实验 README。真实 API 需要显式提供 `PULSE_LIVE_MINIMAX_ALLOW`、`PULSE_LIVE_MINIMAX_KEY`、`PULSE_LIVE_MINIMAX_BASE`、`PULSE_LIVE_MINIMAX_MODEL`；PG 需要测试 allowlist 对应的 `PULSE_DISTRIBUTED_TEST_ALLOW`、`PULSE_DISTRIBUTED_TEST_DSN`。密钥只从进程环境注入，不打印 thinking 或原始响应，只发送合成数据。此轮归档仅重跑离线测试，不把历史真实 API/PG 结果表述为本轮重测。下节记录增长为历史观测，对应独立试验不在本次四文件归档范围内。

## 存储扩展观察

每条 payload 32 B，真实消息路径 + MemoryStore 成功 Save JSON 观测：

| 轮数 | 最终 JSON/B | Save 次数 | 累计 Save JSON/B |
| ---: | ---: | ---: | ---: |
| 10 | 5886 | 36 | 118977 |
| 100 | 34154 | 306 | 5355419 |
| 500 | 160155 | 1506 | 121191940 |

这不是磁盘写入、PG WAL、网络或吞吐基准。整份快照反复保存产生累计写放大，后续可评估快照+增量、已确认历史回收、大载荷引用；必须保留状态、确认和输出一致提交及 fencing/CAS。

## 未覆盖

真实模型跨机器接管、真实外部写入工具的幂等、模型返回未提交窗口、长稳、多轮对话的新 RunID 生命周期。两次恢复窗口通过不等于全部 Agent 使用场景已被验证。
