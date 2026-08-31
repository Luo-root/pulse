# 04-flow

flow 的编排课：**一张课两张图**——RAG 线性链（agent 作为图节点）+ 分支并行 DAG（Skip 裁剪 + 并行双召回 + YAML 同构）。先说结论：flow 在这里不是装饰，它把请求内的数据依赖、失败语义和可观测性从过程式代码变成声明式契约；但对话历史被有意排除在 flow 之外。

## 本课依赖

[02-react](../02-react/)：ReAct 循环（本课 answer 节点内嵌 loop.Agent）。

## 启动演示：YAML 同构

运行后先看到一段对拍：同一个 DAG 用**代码建图**与 **YAML 装图**（`flowyaml.Load`）各跑一次，Final 必须逐字相等。要点：

- 拓扑与边由 YAML 拥有；Factory 只给 `Run` 函数（E2 拓扑 A）——`buildDAG` 与 `registerDAGFactories` 共用同一套 `newDAGRuns`，否则会静默分叉（`TestYAMLIsomorphicFactAndChitchat` 钉死）。
- YAML 里的 `type` 标签来自 `flow.Registry.TypeTagOf`——类型标注是注册表的运行时事实，不是手写常量。

## 图 1：RAG 线性链（agent 在图里）

```text
flow.Seed(UserInput)
  ▼
extract_text ─ Requires(UserInput) ─ Provides(QueryText)
  ▼
retrieve ─── Requires(QueryText) ─── Provides(ContextDocs)
  ▼ AND 汇聚 UserInput + QueryText + ContextDocs
answer ─ prompt = 检索查询 + 检索上下文 + 用户原始 Parts
        │ agent.Run(ctx, history, …)   ← rc.Context() 可被图取消打断
        └ Set(FinalText)
```

**flow 承担的四件事**（对照 `rag.go`）：

1. **类型化 Key 是节点间唯一的数据通道**。节点间没有函数调用、没有共享变量；写未声明的 Key 得 `ErrUndeclared`——数据流在 `Add` 期钉死。
2. **AND 汇聚表达依赖，改图不用改调度**。`answer` 等三个输入全部 ready 才执行，且三个都真实消费。扩图时兑现：加一路并行召回只需声明新节点和新 Requires，调度代码零修改。
3. **失败显式 + 取消传播**。任一节点 error → 取消整图 → `g.Run()` 返回首错——`answer` 不会带着残缺上下文去调模型花钱；取消链经 `rc.Context()` 一路打断正在流式的模型调用。
4. **声明期校验挡住装配错误**。重复节点 ID、重复 Require/Provide、自环、多节点 Provide 同一 Key、来源身份冲突——启动前就炸。

**flow 有意不承担的事**：

- **history 不进槽位**：每轮新建一张图（「一次运行一个世界」），history 是跨运行状态，由 REPL 外层持有。
- **Seed 是外部输入**：`flow.Seed(g, RagUserInput, user)` 表达「这条数据来自系统之外」；与节点 Provide 互斥（`ErrDuplicateSource`）。
- **空命中是数据，不是 Skip**：检索无兜底，空切片照常到达、`answer` 照常执行并在 prompt 写明「无命中」——没有参考文档也是合法结果。

## 图 2：DAG 分支并行（Skip 的正确用法）

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

**Skip 是到达，不是失败**：classify 对不走的一侧调 `flow.Skip(rc, ChatGate)`，下游因任一 Require 被跳过而级联 Skip——「这条路径根本不该走」与「走了但空手而归」（空命中）是两件事。

**闭包写 Final（契约约束，不是输出惯例）**：`answer`/`smalltalk` 不能双 `Provides` 同一 FinalText（单 Key 只能一个生产者，`Add` 会拒）；AND 汇聚两个 Final 会被 Skip 级联跳过。因此两叶子经共用 Run 闭包写 `*string`——终端结果出图用闭包，不要抄成「输出都走闭包」。

**Timeout / Retry 是 Aspect**：`deps.localAspects = []flow.Aspect{flow.Timeout(30ms)}`、`flow.Retry(2, …)` 挂在真实节点上（测试钉死），E1 不双打点。

## 两图对比

| | RAG 线性链 | DAG 分支并行 |
|---|---|---|
| 拓扑 | extract → retrieve → answer | classify → 并行 retrieve ×2 → merge → answer/smalltalk |
| 模型 | loop.Agent 真回合（图内嵌 agent） | 节点内直接拼答案（聚焦 flow 本身） |
| Skip | 不演示（空命中 ≠ Skip） | classify 真实 Set/Skip |
| YAML | 无 | 运行时对拍 + 测试 Final 全等 |

## 时间统计怎么读

`WithObserver(bridge.FlowObserver(host.Peak))` 装观测；E1 三段事件驱动两条桥记录：

| 记录 | Duration 含义 |
|---|---|
| `flow.node_wait_finished` | Waiting → Running（或 Skip/超时直接 Finished）的 wait 墙钟 |
| `flow.node_run_finished` | Running → Finished 的 run 墙钟（仅发过 Running 时写出） |

Skip/超时打断 Wait：有 wait 记录与 Finished，**无** run 记录；Retry 不重复打 Waiting/Running。`alive_nodes_peak` 是同时存活节点峰值（≠ `WithMaxRunning` 执行并发）。

## 运行与测试

```powershell
go run ./examples/04-flow
go test ./examples/04-flow/ -v
```

测试清单：RAG 三态（空命中即数据 / 检索失败取消 / 命中 prompt 组装）+ DAG（并行 peak 与 wait/run 记录、Skip 分支、失败取消、Timeout、Retry、YAML 同构 Final 全等）。

## 下一课

[05-memory-session](../05-memory-session/)：会话真相——把「进程内的 history 切片」换成事件日志 + fold 投影。
