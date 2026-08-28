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
              answer                │
                 └──── 闭包写 Final ─┘
```

| 意图 | 路径 | 验证什么 |
|---|---|---|
| `fact`（问 kernel/flow/observer…） | 两路 retrieve 并行 → merge → answer | `alive_nodes_peak ≥ 2`；local/web 均有 wait+run |
| `chitchat`（你好 / 晚饭…） | Skip 两路 retrieve → smalltalk | 被 Skip 节点无 `flow.node_run_finished`；整图成功 |

一路 retrieve 返回 error → 整图取消（与 03 失败语义一致）。

## 闭包写 Final（契约约束，不是输出惯例）

`answer` / `smalltalk` **不能**双 `Provides` 同一 `FinalText`：flow 单 Key 只能有一个生产者（`Add` 会拒）。若用 AND 汇聚两个 Final，Skip 级联会把汇聚节点也跳过。图结束后也没有「从槽位 Get 输出」的通用 API。

因此本示例让两叶子经**共用 Run 闭包**写 `*string`。这是当前契约下的权宜：

1. 槽位仍是节点间数据通道（门闩 / 文档 / 查询）；
2. 终端结果用闭包出图，**不要**抄成「输出都走闭包」；
3. 代码建图与 YAML 装图必须共用同一套 `newDAGRuns`（否则会静默分叉）。

## 和 03 的差别

| | 03 | 04 |
|---|---|---|
| 拓扑 | 线性 extract→retrieve→answer | 分支 + 并行 + AND |
| 模型 | loop.Agent 真回合 | 节点内直接拼答案（聚焦 flow） |
| Skip | 文档说明，主路径不演示 | classify 真实 Set/Skip |
| YAML | 无 | 测试里 `flow/yaml.Load` 装同构图，**Final 与代码图逐字相等** |
| Run 同源 | — | `buildDAG` 与 Registry 共用 `newDAGRuns` |

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
- `TestYAMLIsomorphicFactAndChitchat`（代码图 vs YAML Final 全等）
- `TestRetrieveLocalTimeoutOnDAG`（Timeout 挂在真实 `retrieve_local`）
- `TestRetrieveWebRetryOnDAG`（Retry 挂在真实 `retrieve_web`；E1 不双打点）
