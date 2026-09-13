[English](README.md) | [中文](README_zh.md)

# eval/war：Go 框架内战对比

同机、同任务、等薄 stub 模型下测量各 agent 框架的基建开销（评测三步走·第一步第二阶段，[Issue #103](https://github.com/Luo-root/pulse/issues/103)）。**独立 go.mod**：Eino 等对比框架的依赖只在本 module，Pulse 主 module 零污染。

## 运行

```powershell
cd eval/war
go test -bench . -benchmem -run '^$' . -count=2   # 即下表口径；加大 -count 看方差
go test -run TestWarSanity -count=1 .           # 正确性哨兵：每任务真实跑通
```

## 参赛方与等价性声明

| | Pulse | Eino v0.9.19 |
|---|---|---|
| 生产入口 | `loop.Agent.Run` | `adk.Runner.Run`（ChatModelAgent） |
| 生产装配 | kernel 宿主 + observability.Bootstrap（nopSink）+ llm.Registry（observed 包装）+ 请求 scope | ChatModelAgent（compose 工具节点内置）+ Runner |
| stub 模型 | `llm.NewScripted`（锁+下标+浅拷贝） | `einoStubModel`（同语义实现） |
| 工具 | `MemToolSet`（计数+固定 JSON） | `tool.InvokableTool`（同语义） |
| 编排执行器 | `kernel/flow` Graph（Add + Seed + Run，AND 槽位） | `compose.Chain`（T3）+ `compose.Graph`（AllPredecessor，T4）+ `compose.Workflow`（T4 变体：字段映射） |
| T3/T4 装配深度 | flow 独立运行（`flow.New` + Add/Seed/Run，无 kernel 宿主——flow 可独立用） | Chain/Graph/Workflow 独立 Compile/Invoke（同样无容器） |
| 构造口径 | 冷启动版（每轮全套重建）+ 复用版（装配一次） | 冷启动版（每轮重建 agent+runner）+ 复用版（装配一次） |

## 结果（i9-14900HX / Go 1.25 / Windows，`-count=2` 各两轮；**轮次间方差显著——比量级与倍数区间，不比个位数**。分配数跨运行稳定、是可信的一等证据，绝对 ns 不是。）

```
BenchmarkWar_PulseTextRound-32          	  149016	      8789 ns/op	    8225 B/op	     125 allocs/op
BenchmarkWar_PulseTextRound-32          	  126478	     10940 ns/op	    8233 B/op	     125 allocs/op
BenchmarkWar_PulseTextRoundReused-32    	  277581	      3680 ns/op	    2496 B/op	      22 allocs/op
BenchmarkWar_PulseTextRoundReused-32    	  385581	      3238 ns/op	    2496 B/op	      22 allocs/op
BenchmarkWar_EinoTextRoundReused-32     	   39688	     31168 ns/op	   27481 B/op	     407 allocs/op
BenchmarkWar_EinoTextRoundReused-32     	   32020	     36921 ns/op	   27481 B/op	     407 allocs/op
BenchmarkWar_PulseToolRound-32          	   81039	     13930 ns/op	   11744 B/op	     163 allocs/op
BenchmarkWar_PulseToolRound-32          	   89458	     12833 ns/op	   11744 B/op	     163 allocs/op
BenchmarkWar_EinoTextRound-32           	   40131	     30523 ns/op	   28997 B/op	     425 allocs/op
BenchmarkWar_EinoTextRound-32           	   36602	     31291 ns/op	   28998 B/op	     425 allocs/op
BenchmarkWar_EinoToolRound-32           	   12610	     94198 ns/op	   89803 B/op	    1364 allocs/op
BenchmarkWar_EinoToolRound-32           	   12528	     95960 ns/op	   89799 B/op	    1364 allocs/op
BenchmarkWar_PulseFlowChain-32          	  145460	      8309 ns/op	    5803 B/op	      73 allocs/op
BenchmarkWar_PulseFlowChain-32          	  143378	      8428 ns/op	    5803 B/op	      73 allocs/op
BenchmarkWar_EinoChain-32               	   69535	     17880 ns/op	   24369 B/op	     323 allocs/op
BenchmarkWar_EinoChain-32               	   69109	     17640 ns/op	   24369 B/op	     323 allocs/op
BenchmarkWar_PulseFlowDAG-32            	  137787	      8738 ns/op	    5846 B/op	      73 allocs/op
BenchmarkWar_PulseFlowDAG-32            	  138868	      8714 ns/op	    5847 B/op	      73 allocs/op
BenchmarkWar_EinoDAGGraph-32            	   41403	     30626 ns/op	   30009 B/op	     411 allocs/op
BenchmarkWar_EinoDAGGraph-32            	   38187	     30465 ns/op	   30008 B/op	     411 allocs/op
BenchmarkWar_EinoDAG-32                 	   35698	     34255 ns/op	   35432 B/op	     462 allocs/op
BenchmarkWar_EinoDAG-32                 	   32772	     35672 ns/op	   35433 B/op	     462 allocs/op
```

| 任务 | Pulse | Eino | 倍数区间 |
|---|---|---|---|
| T1 文本回合（复用：装配一次，纯运行） | 3.2–3.7 µs / 22 allocs | 31.2–36.9 µs / 407 allocs | **~8.5–11.4×** |
| T1 文本回合（冷启动：每轮全套重建） | 8.8–10.9 µs / 125 allocs | 30.5–31.3 µs / 425 allocs | **~2.8–3.6×** |
| T2 工具往返（冷启动上界） | 12.8–13.9 µs / 163 allocs | 94.2–96.0 µs / 1364 allocs | **~6.8–7.5×** |
| T3 线性链（3 透传节点，冷启动） | 8.3–8.4 µs / 73 allocs | 17.6–17.9 µs / 323 allocs | **~2.1×** |
| T4 分支汇聚（1 源→2 分支→AND join，冷启动） | 8.7 µs / 73 allocs | Graph 键化 fan-in 30.5–30.6 µs / 411 allocs；Workflow 字段映射 34.3–35.7 µs / 462 allocs | **~3.5–4.1×** |

## 与 #102 分层基线并排（同机同族数字）

| 层 | ns/op | allocs/op | 口径 |
|---|---|---|---|
| L0 裸 Generate | ~25 | 1 | 无框架 |
| L1 Registry 装配 | ~95 | 3 | + observed 包装 |
| L2a Agent 单步（复用） | ~2.9 µs | 22 | + loop 回合循环 |
| L2b Agent 工具往返（每轮重建） | ~4.0 µs | 51 | + 工具往返 |
| L3 会话记账 | ~7.2 µs | 53 | + event-sourced 记账 |
| **war T1 复用** | **3.2–3.7 µs** | **22** | ≈ L2a 同口径复跑 ✓ |
| **war T1 冷启动** | **8.8–10.9 µs** | **125** | L2a + kernel 装配冷启动 |

war 的 T1 复用数字与 #102 的 L2a 同口径互证（3.2–3.7 µs vs ~2.9 µs，同量级，且分配数同为 22；war 侧多一次 `kernel.Use` 链路校验与 Bootstrap 装配常量）——跨 PR 数字自洽。

## 解读

1. **Eino 的开销在每轮运行路径，不在构造**——复用版（31.2–36.9µs）≈ 冷启动版（30.5–31.3µs）。其 ADK 路径每次 Run 走 compose 图执行 + 事件流迭代 + callback 分发，这些是多 Agent 编排 / interrupt/resume 能力的载体；Pulse 的直调路径没有这层载体，也暂不需要它。
2. **两边相对真实 LLM 调用都可忽略**（~30–37µs vs 秒级）——本对比量化的是**架构选择的基础价格**（直调 vs 图执行器）；差值在高频调用 / 高并发 / 长回合场景被乘起来。不构成「Eino 不可用」的判断。
3. **T1 复用版（纯运行）8.5–11.4× vs 冷启动版 2.8–3.6×**——差距的大头来自每轮运行路径；冷启动差距收窄是因为 Eino 的构造相对便宜（同第 1 条）。
4. 上界口径：冷启动 T2 每轮重建全部装配（脚本耗尽语义所迫，两边同口径）。
5. **编排执行器（T3/T4）：Pulse 的 fan-out 免费，Eino 的 join 调度与字段映射都有价**——任务集全是零计算透传（测的是图执行机：调度、数据传递、汇聚同步）。Pulse flow 三节点链 8.3–8.4µs / 73 allocs，DAG 分支汇聚 8.7µs / 73 allocs（AND 槽位 fan-out 几乎不加价）；Eino Chain 17.6–17.9µs / 323 allocs（~2.1×），Graph 键化 fan-in（`WithOutputKey` + map 默认合并）30.5–30.6µs / 411 allocs——join 调度/合并较其自身 Chain 贵 ~1.7×；Workflow 字段映射 34.3–35.7µs / 462 allocs——较 Graph 再 +12–17%（字段映射/类型推断税，Compile 与 Invoke 两端都计）。相对真实 LLM 调用同样可忽略，量化的仍是架构选择的基础价格。
6. **复用语义不对称**：flow Graph 是一次性运行（`Run` 提交全部节点并阻塞到全部终止，实例即废）；Eino `Runnable` 可 Compile 一次、Invoke N 次。T3/T4 是单次运行口径（Eino 的 Compile 计入每轮）——宿主复用 Runnable 的场景下，Eino 每轮数字会低于表中值。

## 加新参赛方

实现三件（参照 `contestants.go`）：`runXxxTextRound` / `runXxxToolRound`（生产装配 + stub 对齐声明）+ benchmark + Sanity 断言；编排参赛方加 `runXxxFlowChain` / `runXxxFlowDAG`（任务集：零计算透传）。装配等价性（stub 薄度、任务对齐、生产入口）写在注释里。
