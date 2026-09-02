# eval/war：Go 框架内战对比

同机、同任务、等薄 stub 模型下测量各 agent 框架的基建开销（评测三步走·第一步第二阶段，[Issue #103](https://github.com/Luo-root/pulse/issues/103)）。**独立 go.mod**：Eino 等对比框架的依赖只在本 module，Pulse 主 module 零污染。

## 运行

```powershell
cd eval/war
go test -bench . -benchmem -run '^$' .
go test -run TestWarSanity -count=1 .   # 正确性哨兵：每任务真实跑通
```

## 参赛方与等价性声明

| | Pulse | Eino v0.9.19 |
|---|---|---|
| 生产入口 | `loop.Agent.Run` | `adk.Runner.Run`（ChatModelAgent） |
| stub 模型 | `llm.NewScripted`（锁+下标+浅拷贝） | `einoStubModel`（同语义实现） |
| 工具 | `MemToolSet`（计数+固定 JSON） | `tool.InvokableTool`（同语义） |
| 观测面 | 不挂（L1 已单独量化） | 不配 callback |
| 构造口径 | 重建版含构造；复用版剥离 | 同 |

## 结果（i9-14900HX / Go 1.25 / Windows，单机参考值）

```
BenchmarkWar_PulseTextRound-32          	  255176	      4934 ns/op	    1528 B/op	      22 allocs/op
BenchmarkWar_PulseTextRoundReused-32    	  272140	      4209 ns/op	    1216 B/op	      16 allocs/op
BenchmarkWar_EinoTextRoundReused-32     	   13586	     88419 ns/op	   27465 B/op	     407 allocs/op
BenchmarkWar_PulseToolRound-32          	   92257	     12566 ns/op	    3696 B/op	      53 allocs/op
BenchmarkWar_EinoTextRound-32           	   13087	     89833 ns/op	   28984 B/op	     425 allocs/op
BenchmarkWar_EinoToolRound-32           	    3962	    330982 ns/op	   89778 B/op	    1364 allocs/op
```

| 任务 | Pulse | Eino | 差距 |
|---|---|---|---|
| T1 文本回合（复用，纯运行） | 4.2 µs / 1.2 KB / 16 allocs | 88.4 µs / 27.5 KB / 407 allocs | **21× / 23× / 25×** |
| T1 文本回合（含构造） | 4.9 µs | 89.8 µs | 18× |
| T2 工具往返（含构造上界） | 12.6 µs / 3.7 KB / 53 allocs | 331.0 µs / 89.8 KB / 1364 allocs | **26× / 24× / 26×** |

## 解读

1. **Eino 的开销在每轮运行路径，不在构造**——复用版（88.4µs）≈ 重建版（89.8µs）。其 ADK 路径每次 Run 走 compose 图执行 + 事件流迭代 + callback 分发，这些是框架语义的一部分（多 Agent 编排、interrupt/resume 的载体）；Pulse 的直调路径（方法调用 + 显式消息聚合）没有这层载体，也暂不需要它。
2. **两边数字相对真实 LLM 调用都可忽略**（88µs vs 秒级）——单轮视角下两者都「够快」。本对比的意义是**架构选择的开销量化**：直调式（Pulse）vs 图执行器（Eino）在同任务下的基础价格差；差值在高频调用 / 高并发 / 多步长回合场景会被乘起来。
3. **T2 的 26× 与 T1 的 21× 几乎同比**——工具往返增量两边同量级（Pulse +8.4µs / Eino +243µs 含构造），差距主体仍来自每轮运行路径。
4. 上界口径声明：重建版数字含每轮模型/Agent 构造（脚本耗尽语义所迫，两边同口径）；复用版是纯运行成本。

## 加新参赛方

实现三个函数（参照 `contestants.go`）：`runXxxTextRound` / `runXxxToolRound`（生产装配 + stub 对齐声明）+ 两个 benchmark + Sanity 断言。装配等价性（stub 薄度、任务对齐、生产入口）写在注释里。
