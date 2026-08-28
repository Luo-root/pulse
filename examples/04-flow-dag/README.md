# 04-flow-dag

在 `03-flow-agent` 线性链之上，验证 flow **真正值钱的扩图能力**：并行双召回、Skip 分支、AND 汇聚、E1 Observer，以及 E2 YAML 同构装图。

对应 [Issue #34](https://github.com/Luo-root/pulse/issues/34)。

## 图长什么样

```text
                    UserText (Seed)
                         │
                     classify
                    ╱         ╲
            Set FactGate    Set ChatGate
            Skip ChatGate   Skip FactGate
                 │               │
        ┌────────┴────────┐      │
        ▼                 ▼      ▼
 retrieve_local     retrieve_web  smalltalk
   (并行)              (并行)        │
        └────────┬────────┘         │
                 ▼                  │
               merge                │
                 │                  │
              answer ───────────────┘
           （闭包写 Final；两叶子不双 Provide）
```

| 意图 | 路径 | 验证什么 |
|---|---|---|
| `fact`（问 kernel/flow/observer…） | 两路 retrieve 并行 → merge → answer | `alive_nodes_peak ≥ 2`；local/web 均有 wait+run |
| `chitchat`（你好 / 晚饭…） | Skip 两路 retrieve → smalltalk | 被 Skip 节点无 `flow.node_run_finished`；整图成功 |

一路 retrieve 返回 error → 整图取消（与 03 失败语义一致）。

## 和 03 的差别

| | 03 | 04 |
|---|---|---|
| 拓扑 | 线性 extract→retrieve→answer | 分支 + 并行 + AND |
| 模型 | loop.Agent 真回合 | 节点内直接拼答案（聚焦 flow） |
| Skip | 文档说明，主路径不演示 | classify 真实 Set/Skip |
| YAML | 无 | 测试里 `flow/yaml.Load` 装同构图 |

history 仍在图外（一次运行一个世界）。

## 运行

```powershell
go run ./examples/04-flow-dag
go test ./examples/04-flow-dag/ -v
```

凭据缺省时走 Scripted 宿主（本例几乎不调模型）。stderr 会打 `intent` / `alive_nodes_peak`。

## 测试清单

- `TestFactPathParallelPeakAndRecords`
- `TestChitchatSkipsRetrieves`
- `TestRetrieveFailureCancelsGraph`
- `TestYAMLIsomorphicFactPath`（E2 拓扑 A：工厂只给 Run）
- `TestYAMLTimeoutOnSlowNode`
