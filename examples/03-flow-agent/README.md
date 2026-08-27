# 03-flow-agent

三层里最重的一层：验证 `kernel/flow` 在「一次用户请求」上到底承担了什么。**先说结论：flow 在这里不是装饰，它把请求内的数据依赖、失败语义和可观测性从过程式代码变成声明式契约；但对话历史被有意排除在 flow 之外。**

## flow 承担的四件事

### 1. 类型化 Key 是节点间唯一的数据通道

```go
var (
    UserInput   = flow.NewKey[*llm.Message]("demo.user_input")   // 外部 Seed
    QueryText   = flow.NewKey[string]("demo.query_text")
    ContextDocs = flow.NewKey[[]Document]("demo.context_docs")
    FinalText   = flow.NewKey[string]("demo.final_text")
)
```

节点之间没有函数调用、没有共享变量、没有全局黑板。`extract_text` 想把东西交给 `retrieve`，唯一的办法是 `Set(rc, QueryText, ...)`；`retrieve` 想读，必须先在声明里写 `Requires(QueryText)`。写未声明的 Key 会得到 `ErrUndeclared`——数据流在 `Add` 期就被钉死。

### 2. AND 汇聚表达依赖，改图不用改调度

```go
answer := NewNode("answer",
    Deps(Requires(UserInput), Requires(QueryText), Requires(ContextDocs)),
    Provides(FinalText), ...)
```

`answer` 只有等三个输入全部 ready 才会执行，且**三个都真实消费**：`UserInput` 提供原始多模态 Parts、`ContextDocs` 提供检索结果、`QueryText` 打进 prompt 首行作为「检索查询」标注——这是框架行为而非 demo 里写的等待循环。好处在扩图时兑现：想加一路并行召回（比如再开一个 `retrieve_web` 节点），只需声明新节点和新 Requires，调度代码零修改——节点全量提交，谁的数据到了谁走。

### 3. 失败显式 + 取消传播

任何一个节点返回 error（例如检索服务挂了）→ 记录首错并取消整图 → `g.Run()` 返回该错误 → **`answer` 节点不会带着残缺上下文去调模型花钱**。这就是「节点 error 取消整图」相对于「每个节点自己 try-catch 继续跑」的价值：失败语义集中在图级别。

取消链也是通的：`rc.Context()` 作为 ctx 传进 `agent.Run`——图的取消能一路打断正在流式的模型调用。

### 4. 声明期校验挡住装配错误

`Add` 会拒绝重复节点 ID、重复 Require/Provide、Require+Provide 自环、多个节点 Provide 同一 Key、来源身份冲突。这些错误发生在启动前，而不是跑到一半才炸。

## flow 有意不承担的事

### 对话 history 不进槽位

这是本 demo 最重要的架构决策。每轮输入新建一张 `flow.Graph`（「一次运行一个世界」：槽位、首错、取消状态随 Run 生灭），而 history 是**跨运行状态**，由 REPL 外层持有：

```go
res, dur, err := runGraph(host, agent, retriever, history, msg) // history 快照传入
history = append(history, msg)
history = append(history, res.Messages...)                      // 图外追加
```

如果把 history 放进 flow 槽位，就等于让一个一次性的世界持有跨轮生命周期状态，与设计契约直接冲突。flow 管「这一轮内部谁等谁」，不管「上一轮说了什么」。

### Seed 是外部输入，不是普通节点

用户消息用 `flow.Seed(g, UserInput, user)` 注入，而不是造一个 "user_input" 节点来生产它。来源身份唯一：这个 Key 要么外部 Seed，要么恰好一个节点 Provide，二者不可并存（违反会在 Add/Seed 时报 `ErrDuplicateSource`）。在这里 Seed 表达的就是字面意思：**这条数据来自系统之外，不由任何节点负责生产**。

### 空 RAG 命中是数据，不是 Skip

检索是纯关键词匹配，无兜底规则（不存在「问什么都能命中」的短路），所以随便问个不相干的问题（比如「晚饭吃什么」）就会真实走进空命中路径：`Set(rc, ContextDocs, []Document{})`——空切片照常到达，`answer` 照常执行并在 prompt 里写明「无命中」。这符合业务事实：没有参考文档也是一种合法结果，模型应该继续回答。

Skip 留给另一种情况：「这条路径根本不该走」。如果加了意图分类节点，判定纯闲聊不走检索，正确写法是对 `ContextDocs` 调 `flow.Skip(rc, ...)`——下游 `answer` 因任一 Require 被跳过而整体级联跳过，然后由另一条分支接手。本 demo 没有分类分支；要看 Skip 级联的实际效果见 `kernel/flow/flow_test.go` 的 `TestBranchSkip` / `TestCascadeSkip`，空命中与检索失败两条路径则由 `main_test.go` 的 `TestRunGraph*` 钉住。

### answer 必须写完 Provides

`Run` 返回 nil 后，框架把漏写的 Provides 自动 Skip（防止下游永远阻塞）。所以 `answer` 结束前显式 `Set(FinalText, ...)`；这一行不是仪式，是契约要求的「节点有责任写完输出」。

## 时间统计怎么读

生命周期观察通过 `WithObserver(bridge.FlowObserver(host.Peak))` 安装（`bridge` = 本请求 `demoapp.Bridge`）。E1 三段事件驱动两条桥记录，各用官方 `Duration`：

| 记录 | Duration 含义 |
|---|---|
| `flow.node_wait_finished` | Waiting → Running（或 Skip/超时直接 Finished）的 wait 墙钟 |
| `flow.node_run_finished` | Running → Finished 的 run 墙钟（仅发过 Running 时写出） |
| `alive_nodes_peak` | Waiting..Finished 之间同时存活的节点峰值（≠ `WithMaxRunning` 执行并发） |

Skip / 超时打断 Wait：有 wait 记录与 Finished，**无** `flow.node_run_finished`。Retry 不重复打 Waiting/Running。

## 数据流全貌

```text
REPL 输入
  │ (llm.Message, 已含多模态 Part)
  ▼
flow.Seed(UserInput)                        ← 外部输入
  ▼
┌─ extract_text ─────────────┐
│ Requires(UserInput)        │  抽取纯文本
│ Provides(QueryText)        │
└─────────────┬──────────────┘
              ▼
┌─ retrieve ─────────────────┐
│ Requires(QueryText)        │  内存关键词检索（Retriever 接口）
│ Provides(ContextDocs)      │  无命中 → Set(空切片)，非 Skip
└─────────────┬──────────────┘
              ▼ AND 汇聚 UserInput + QueryText + ContextDocs
┌─ answer ───────────────────┐
│ prompt = 检索查询（QueryText）│ ← 三个 Require 全部真实消费
│        + 检索上下文文本     │
│        + 用户原始 Parts     │  ← 多模态原样保留给模型
│ agent.Run(ctx, history, …) │  ← rc.Context() 可被图取消打断
│ Set(FinalText)             │
└────────────────────────────┘
```

> 注：`lookup` 是 loop 层演示工具（模型自愿调用），不是图的检索入口；flow 的知识通路只有 `retrieve → ContextDocs` 一条。prompt 组装契约由 `TestRunGraphEmptyHitIsData` / `TestRunGraphHitConsumesQueryAndDocs` 用截获式模型断言。

`Retriever` 是唯一的知识源边界：换真实向量库只需提供新的 `Search(ctx, query, limit)` 实现，图结构不动。

## 与手写顺序代码的诚实对比

当前三步是线性链（extract → retrieve → answer），说实话：写成三行顺序调用也能工作。flow 在线性链上是中性的，它的收益从扩图开始：

- 加并行召回 / rerank / 审计节点时只写新节点的 Requires/Provides，不动任何调度代码；
- 失败和取消集中在一个位置定义，不用在每个环节手写 try/catch 和 context 传递；
- 每个节点的状态和耗时自动进入观测，不用手工埋点；
- 声明错误在启动时炸，不在第三步才炸。

这正是 Issue #13 要验证的问题：「flow 是真的好，不只是概念新颖」——线性小链看不出差距，复杂 DAG 上才有答案，而扩图成本是本 demo 可以实际检验的部分。
