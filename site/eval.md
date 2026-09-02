# 性能基准

框架基建开销有同机同任务的量化对比：`eval/war`（独立嵌套 module，[Issue #103](https://github.com/Luo-root/pulse/issues/103)）——Pulse 全家桶生产装配 vs Eino v0.9.19 官方生产入口，等薄 stub 模型对齐，正确性哨兵断言每个任务真实跑通。

!!! 口径：i9-14900HX / Go 1.25 / Windows。**比量级与倍数区间，不比个位数**（轮次间方差显著）。

## 跨框架对比（eval/war）

| 任务 | Pulse | Eino v0.9.19 | 倍数区间 |
|---|---|---|---|
| T1 文本回合（复用：装配一次，纯运行） | 3.6 µs / 22 allocs | 36.6–40.9 µs / 407 allocs | **~10–11×** |
| T1 文本回合（冷启动：每轮全套重建） | 10.5–10.7 µs / 139 allocs | 38.8–39.0 µs / 425 allocs | **~3.7×** |
| T2 工具往返（冷启动上界） | 15.1–15.9 µs / 177 allocs | 116.4–117.7 µs / 1364 allocs | **~7.4–7.7×** |
| T3 线性链编排（3 透传节点） | 8.6–8.9 µs / 73 allocs | 17.9–18.1 µs / 323 allocs | **~2.0×** |
| T4 分支汇聚 DAG（1 源→2 分支→AND join） | 9.0–9.3 µs / 73 allocs | 30.3–37.9 µs / 411–462 allocs（Graph 键化 fan-in / Workflow 字段映射两变体） | **~3.3–4.1×** |

## 与分层基线互证

eval 的分层基准（#102，L0–L3）与 war 同机数字交叉验证：

| 层 | ns/op | allocs/op | 口径 |
|---|---|---|---|
| L0 裸 Generate | ~23 | 1 | 无框架 |
| L1 Registry 装配 | ~73 | 3 | + observed 包装 |
| L2a Agent 单步（复用） | ~2.6 µs | 22 | + loop 回合循环 |
| L2b Agent 工具往返（每轮重建） | ~3.6 µs | 51 | + 工具往返 |
| L3 会话记账 | ~7.5 µs | 53 | + event-sourced 记账 |
| **war T1 复用** | **3.6 µs** | **22** | ≈ L2a 同口径复跑 ✓ |

## 怎么读这些数字

1. **Eino 的开销在每轮运行路径**——复用版 ≈ 冷启动版。其 ADK 路径每次 Run 走 compose 图执行 + 事件流迭代 + callback 分发，这是多 Agent 编排 / interrupt 能力的载体；Pulse 直调路径没有这层载体，也暂不需要它。
2. **编排 fan-out 免费**：flow 的 AND 槽位让分支汇聚几乎不加价——DAG（T4）与线性链（T3）同价同 allocs；对比方的 join 调度较其自身线性链贵 ~1.7×，Workflow 字段映射再 +15–20%。
3. **复用语义不对称**：flow Graph 一次性运行；Eino Runnable 可 Compile 一次 Invoke N 次——T3/T4 是单次运行口径，复用场景下对方每轮数字会下修。
4. **相对真实 LLM 调用（秒级）都可忽略**——量化的是**架构选择的基础价格**，差值在高频 / 高并发 / 长回合场景被乘起来。不构成「对方不可用」的判断。

## 复现

```bash
cd eval/war
go test -bench . -benchmem -run '^$' .   # 建议加 -count=2 看方差
go test -run TestWarSanity -count=1 .    # 正确性哨兵
```

另有 **19 条 property 不变式**（eval 四主题）持续校验词汇表 / fold / registry 的工程性质。详见 [eval 包文档](/packages/eval/) 与 [eval/war 包文档](/packages/eval/war/)。
