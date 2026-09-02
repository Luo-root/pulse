[English](README.md) | [中文](README_zh.md)

# eval/war：Go 框架内战对比

同机、同任务、等薄 stub 模型下测量各 agent 框架的基建开销（评测三步走·第一步第二阶段，[Issue #103](https://github.com/Luo-root/pulse/issues/103)）。**独立 go.mod**：Eino 等对比框架的依赖只在本 module，Pulse 主 module 零污染。

## 运行

```powershell
cd eval/war
go test -bench . -benchmem -run '^$' .          # 建议加 -count=2 看方差
go test -run TestWarSanity -count=1 .           # 正确性哨兵：每任务真实跑通
```

## 参赛方与等价性声明

| | Pulse | Eino v0.9.19 |
|---|---|---|
| 生产入口 | `loop.Agent.Run` | `adk.Runner.Run`（ChatModelAgent） |
| 生产装配 | kernel 宿主 + observability.Bootstrap（nopSink）+ llm.Registry（observed 包装）+ 请求 scope | ChatModelAgent（compose 工具节点内置）+ Runner |
| stub 模型 | `llm.NewScripted`（锁+下标+浅拷贝） | `einoStubModel`（同语义实现） |
| 工具 | `MemToolSet`（计数+固定 JSON） | `tool.InvokableTool`（同语义） |
| 编排执行器 | `kernel/flow` Graph（Add + Seed + Run，AND 槽位） | `compose.Chain`（T3）+ `compose.Workflow`（T4：字段映射 AND 汇聚） |
| 构造口径 | 冷启动版（每轮全套重建）+ 复用版（装配一次） | 冷启动版（每轮重建 agent+runner）+ 复用版（装配一次） |

## 结果（i9-14900HX / Go 1.25 / Windows，T1/T2 两轮、T3/T4 三轮；**轮次间方差显著——比量级与倍数区间，不比个位数**）

```
BenchmarkWar_PulseTextRound-32          	  126460	     10497 ns/op	    8369 B/op	     139 allocs/op
BenchmarkWar_PulseTextRoundReused-32    	  348265	      3596 ns/op	    2368 B/op	      22 allocs/op
BenchmarkWar_EinoTextRoundReused-32     	   33534	     36595 ns/op	   27480 B/op	     407 allocs/op
BenchmarkWar_PulseToolRound-32          	   73598	     15871 ns/op	   11729 B/op	     177 allocs/op
BenchmarkWar_EinoTextRound-32           	   31104	     38789 ns/op	   28999 B/op	     425 allocs/op
BenchmarkWar_EinoToolRound-32           	   10000	    116405 ns/op	   89804 B/op	    1364 allocs/op
BenchmarkWar_PulseFlowChain-32          	   131997	      9170 ns/op	    6208 B/op	      77 allocs/op
BenchmarkWar_EinoChain-32               	    66117	     17664 ns/op	   24367 B/op	     323 allocs/op
BenchmarkWar_PulseFlowDAG-32            	   134004	      8975 ns/op	    5831 B/op	      73 allocs/op
BenchmarkWar_EinoDAG-32                 	    37092	     33132 ns/op	   35433 B/op	     462 allocs/op
```

| 任务 | Pulse | Eino | 倍数区间 |
|---|---|---|---|
| T1 文本回合（复用：装配一次，纯运行） | 3.6 µs / 22 allocs | 36.6–40.9 µs / 407 allocs | **~10–11×** |
| T1 文本回合（冷启动：每轮全套重建） | 10.5–10.7 µs / 139 allocs | 38.8–39.0 µs / 425 allocs | **~3.7×** |
| T2 工具往返（冷启动上界） | 15.1–15.9 µs / 177 allocs | 116.4–117.7 µs / 1364 allocs | **~7.4–7.7×** |
| T3 线性链（3 透传节点，冷启动） | 8.9–9.3 µs / 77 allocs | 17.7–18.0 µs / 323 allocs | **~2.0×** |
| T4 分支汇聚（1 源→2 分支→AND join，冷启动） | 8.9–9.1 µs / 73 allocs | 32.5–35.3 µs / 462 allocs | **~3.6–3.9×** |

## 与 #102 分层基线并排（同机同族数字）

| 层 | ns/op | allocs/op | 口径 |
|---|---|---|---|
| L0 裸 Generate | ~23 | 1 | 无框架 |
| L1 Registry 装配 | ~73 | 3 | + observed 包装 |
| L2a Agent 单步（复用） | ~2.6 µs | 22 | + loop 回合循环 |
| L2b Agent 工具往返（每轮重建） | ~3.6 µs | 51 | + 工具往返 |
| L3 会话记账 | ~7.5 µs | 53 | + event-sourced 记账 |
| **war T1 复用** | **3.6 µs** | **22** | ≈ L2a 同口径复跑 ✓ |
| **war T1 冷启动** | **10.6 µs** | **139** | L2a + kernel 装配冷启动 |

war 的 T1 复用数字与 #102 的 L2a 同口径互证（3.6 µs vs 2.6 µs，同量级；war 侧多一次 `kernel.Use` 链路校验与 Bootstrap 装配常量）——跨 PR 数字自洽。

## 解读

1. **Eino 的开销在每轮运行路径，不在构造**——复用版（36.6–40.9µs）≈ 冷启动版（38.8–39.0µs）。其 ADK 路径每次 Run 走 compose 图执行 + 事件流迭代 + callback 分发，这些是多 Agent 编排 / interrupt/resume 能力的载体；Pulse 的直调路径没有这层载体，也暂不需要它。
2. **两边相对真实 LLM 调用都可忽略**（38µs vs 秒级）——本对比量化的是**架构选择的基础价格**（直调 vs 图执行器）；差值在高频调用 / 高并发 / 长回合场景被乘起来。不构成「Eino 不可用」的判断。
3. **T1 复用版（纯运行）10–11× vs 冷启动版 3.7×**——差距的大头来自每轮运行路径；冷启动差距收窄是因为 Eino 的构造相对便宜（同第 1 条）。
4. 上界口径：冷启动 T2 每轮重建全部装配（脚本耗尽语义所迫，两边同口径）。
5. **编排执行器（T3/T4）：图调度本身很便宜，Eino 的编排层比其 agent 路径更重**——任务集全是零计算透传（测的是图执行机：调度、数据传递、汇聚同步）。Pulse flow 三节点链 ~9µs / 77 allocs，DAG 分支汇聚 ~9µs / 73 allocs（AND 槽位 fan-out 几乎不加价）；Eino Chain ~18µs / 323 allocs（~2.0×），Workflow 字段映射 DAG ~33–35µs / 462 allocs（~3.6–3.9×）——字段映射与类型推断在 Compile 与 Invoke 两端都抬价。相对真实 LLM 调用同样可忽略，量化的仍是架构选择的基础价格。

## 加新参赛方

实现三件（参照 `contestants.go`）：`runXxxTextRound` / `runXxxToolRound`（生产装配 + stub 对齐声明）+ benchmark + Sanity 断言；编排参赛方加 `runXxxFlowChain` / `runXxxFlowDAG`（任务集：零计算透传）。装配等价性（stub 薄度、任务对齐、生产入口）写在注释里。
